package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Wire error codes that are not policy sentinels.
const (
	codeNotFound    = audit.FailNotFound
	codeConcurrency = audit.FailConcurrency
)

// bodyReadTimeout bounds how long a caller may take to send the request body.
// http.Server.ReadTimeout is not used: it also kills the background read that
// detects client disconnects, which would cancel long streams.
const bodyReadTimeout = 30 * time.Second

// Config wires a Server. Fields marked "test seam" exist so tests can inject a
// clock, a dialer or a fake sleeper; production leaves them zero.
type Config struct {
	Providers []provider.Provider
	Authority *policy.Authority
	Ledger    *ledger.Ledger
	Audit     *audit.Writer

	// ReadyFile is written atomically after the listener is bound.
	ReadyFile string
	// Addr must be a 127.0.0.1 address. Default "127.0.0.1:0".
	Addr string
	// CleanupTimeout bounds settlement, audit and shutdown. Required.
	CleanupTimeout time.Duration
	// DefaultReserveTokens is reserved when a request declares no output cap.
	DefaultReserveTokens int64
	// MaxReservationTokens bounds each reservation. Zero defaults to 8000.
	MaxReservationTokens int64
	Logger               *slog.Logger

	// Now is a test seam for the clock.
	Now func() time.Time
	// Sleep is a test seam for the Retry-After wait.
	Sleep func(ctx context.Context, d time.Duration) error
	// DialContext is a test seam replacing the upstream dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSConfig is a test seam: extra trust for a fake HTTPS upstream.
	TLSConfig *tls.Config

	panicHook func(stage string) // tests only
}

// Server is the gateway. It is immutable after New apart from its listener.
type Server struct {
	cfg       Config
	upstreams map[string]*upstream
	sem       chan struct{}
	logger    *slog.Logger
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex // guards ln, hosts, srv, url at Listen time only
	ln    net.Listener
	srv   *http.Server
	url   string
	hosts map[string]struct{}

	inflight sync.WaitGroup
}

// New validates the configuration and builds a Server. It binds nothing.
func New(cfg Config) (*Server, error) {
	if cfg.Authority == nil || cfg.Ledger == nil || cfg.Audit == nil {
		return nil, errors.New("proxy: authority, ledger and audit are required")
	}
	if cfg.CleanupTimeout <= 0 {
		return nil, errors.New("proxy: cleanup timeout is required")
	}
	if err := provider.Validate(cfg.Providers); err != nil {
		return nil, err
	}
	if cfg.DefaultReserveTokens <= 0 {
		cfg.DefaultReserveTokens = defaultReserveTokens
	}
	if cfg.MaxReservationTokens == 0 {
		cfg.MaxReservationTokens = defaultMaxReservationTokens
	}
	if cfg.MaxReservationTokens < 0 {
		return nil, errors.New("proxy: max_reservation_tokens must be greater than zero")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if host, _, err := net.SplitHostPort(cfg.Addr); err != nil || host != "127.0.0.1" {
		return nil, errors.New("proxy: addr must be 127.0.0.1:port")
	}
	s := &Server{
		cfg:       cfg,
		upstreams: make(map[string]*upstream, len(cfg.Providers)),
		sem:       make(chan struct{}, cfg.Authority.Concurrency),
		logger:    cfg.Logger,
		now:       cfg.Now,
		sleep:     cfg.Sleep,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sleep == nil {
		s.sleep = sleepCtx
	}
	for _, p := range cfg.Providers {
		up, err := newUpstream(p, cfg)
		if err != nil {
			return nil, err
		}
		s.upstreams[p.ID] = up
	}
	return s, nil
}

// defaultReserveTokens is used when neither the request nor the runtime file
// declares an output cap.
const defaultReserveTokens = 4096

const defaultMaxReservationTokens = 8000

// reserveBudget caps a request's reservation to the remaining budget and the
// per-request ceiling. It does not cap measured usage at settlement.
func (s *Server) reserveBudget(id, runID string, limit int64) (int64, error) {
	for {
		remaining, err := s.cfg.Ledger.Remaining(runID)
		if err != nil {
			return 0, err
		}
		if remaining <= 0 {
			return 0, ledger.ErrBudgetExhausted
		}
		reserve := min(limit, remaining, s.cfg.MaxReservationTokens)
		err = s.cfg.Ledger.Reserve(id, runID, reserve)
		if errors.Is(err, ledger.ErrBudgetExhausted) {
			// Another request reserved tokens after our snapshot. Recheck and
			// shrink the reservation; refuse only once no budget remains.
			continue
		}
		return reserve, err
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Listen binds the loopback listener once (ctx bounds only the bind), keeps it, then writes the ready
// file atomically. The same listener serves the first request.
func (s *Server) Listen(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("proxy: already listening")
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("proxy: listen: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		_ = ln.Close()
		return errors.New("proxy: listener is not loopback")
	}
	hostport := addr.String()
	s.ln = ln
	s.url = "http://" + hostport
	s.hosts = map[string]struct{}{
		hostport:                             {},
		"localhost:" + fmt.Sprint(addr.Port): {},
	}
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	if s.cfg.ReadyFile != "" {
		if err := writeReadyFile(s.cfg.ReadyFile, s.url, s.now()); err != nil {
			_ = ln.Close()
			s.ln = nil
			return err
		}
	}
	return nil
}

// URL is the base URL of the bound listener.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Serve accepts connections on the listener Listen bound. It returns nil after
// Shutdown.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln, srv := s.ln, s.srv
	s.mu.Unlock()
	if ln == nil {
		return errors.New("proxy: Serve before Listen")
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("proxy: serve: %w", err)
	}
	return nil
}

// Shutdown stops accepting, lets in-flight requests finish, then forces the
// rest closed. It returns nil once every in-flight request has settled and
// been audited, all within the cleanup timeout.
func (s *Server) Shutdown(parent context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	// One cleanup budget covers the whole shutdown: half to let requests finish
	// on their own, the rest for forced ones to settle and audit.
	deadline := time.NewTimer(s.cfg.CleanupTimeout)
	defer deadline.Stop()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.cfg.CleanupTimeout/2)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close() // cancels the remaining handlers; they still settle and audit
	}
	idle := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(idle)
	}()
	select {
	case <-idle:
	case <-deadline.C:
		return errors.New("proxy: in-flight requests did not settle in time")
	}
	if s.cfg.ReadyFile != "" {
		_ = os.Remove(s.cfg.ReadyFile)
	}
	return nil
}

// writeReadyFile writes {"url","pid","started_at"} atomically: temp file in the
// same directory, fsync, rename.
func writeReadyFile(file, url string, now time.Time) error {
	data, err := json.Marshal(struct {
		URL       string `json:"url"`
		PID       int    `json:"pid"`
		StartedAt string `json:"started_at"`
	}{url, os.Getpid(), now.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, ".ready-*.tmp")
	if err != nil {
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("proxy: ready file: %w", err)
	}
	ok = true
	return nil
}

// RoutePath splits /p/{provider}/{native path...}. It accepts only paths that
// are already clean: no empty, "." or ".." segment, no backslash, no control
// byte, and at least one native segment.
func RoutePath(p string) (providerID, native string, ok bool) {
	rest, found := strings.CutPrefix(p, "/p/")
	if !found {
		return "", "", false
	}
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", false
	}
	providerID, native = rest[:i], rest[i:]
	if !provider.ValidID(providerID) || len(native) < 2 {
		return "", "", false
	}
	for _, c := range []byte(native) {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "", "", false
		}
	}
	for _, seg := range strings.Split(native[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", false
		}
	}
	if path.Clean(native) != native {
		return "", "", false
	}
	return providerID, native, true
}

// reqState is everything one request accumulates for settlement and audit.
type reqState struct {
	id        string
	start     time.Time
	method    string
	provider  string
	model     string
	path      string
	attempts  int
	failure   string
	ambiguous bool // request may have reached upstream but no response was committed
	local     bool // the gateway wrote its own error reply; no upstream response was committed

	reserved      bool
	reserveTokens int64
	maybeBilled   bool // upstream may have consumed tokens that nothing measured
	usage         provider.Usage
	streamInject  bool // the gateway added stream_options.include_usage to the body
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handle is the single http.Handler. Every request is audited exactly once.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Done()

	st := &reqState{id: newID(), start: s.now(), method: audit.CleanMethod(r.Method)}
	tw := &trackedWriter{ResponseWriter: w}
	func() { //nolint:contextcheck // ledger work is deliberately detached from the request context
		defer func() {
			if rec := recover(); rec != nil {
				s.onPanic(tw, st)
			}
		}()
		s.serveRequest(tw, r, st)
	}()
	s.finish(r, tw, st)
}

func (s *Server) onPanic(tw *trackedWriter, st *reqState) {
	if tw.committed.Load() {
		st.failure = audit.FailPanicAfterCommit
		tw.abort()
		return
	}
	st.failure = audit.FailInternalPanic
	s.reject(tw, st, http.StatusInternalServerError, audit.FailInternalPanic)
}

// reject writes a short JSON error and records the failure code. Nothing from
// upstream is ever echoed.
func (s *Server) reject(tw *trackedWriter, st *reqState, status int, code string) {
	st.failure = code
	if tw.committed.Load() || tw.hijacked {
		return
	}
	st.local = true
	h := tw.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	tw.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "request_id": st.id}})
	_, _ = tw.Write(append(body, '\n'))
}

func (s *Server) rejectPolicy(tw *trackedWriter, st *reqState, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, policy.ErrBodyTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, policy.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, policy.ErrProviderNotAllowed), errors.Is(err, policy.ErrModelNotAllowed):
		status = http.StatusForbidden
	}
	code := policy.Code(err)
	if code == "" {
		code = audit.FailMalformed
	}
	s.reject(tw, st, status, code)
}

// serveRequest runs the pipeline up to and including the relay.
func (s *Server) serveRequest(tw *trackedWriter, r *http.Request, st *reqState) {
	if status, code := s.preflight(r); code != "" {
		s.reject(tw, st, status, code)
		return
	}
	providerID, native, ok := RoutePath(r.URL.Path)
	if !ok {
		s.reject(tw, st, http.StatusNotFound, codeNotFound)
		return
	}
	st.path = audit.CleanPath(native)

	auth := s.cfg.Authority
	if err := auth.Authenticate(presentedToken(r), s.now()); err != nil {
		s.rejectPolicy(tw, st, err)
		return
	}
	up, known := s.upstreams[providerID]
	if !known {
		s.reject(tw, st, http.StatusNotFound, codeNotFound)
		return
	}
	st.provider = providerID
	if err := auth.CheckProvider(providerID); err != nil {
		s.rejectPolicy(tw, st, err)
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.reject(tw, st, http.StatusTooManyRequests, codeConcurrency)
		return
	}

	rc := http.NewResponseController(tw)
	_ = rc.SetReadDeadline(s.now().Add(bodyReadTimeout))
	body, err := policy.ReadBody(r.Body, r.ContentLength)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		s.rejectPolicy(tw, st, err)
		return
	}
	meta, err := policy.ParseRequest(body)
	if err != nil {
		s.rejectPolicy(tw, st, err)
		return
	}
	st.model = audit.CleanModel(meta.Model)
	if err := auth.CheckModel(providerID, meta.Model); err != nil {
		s.rejectPolicy(tw, st, err)
		return
	}
	if !up.allowsModel(meta.Model) {
		s.rejectPolicy(tw, st, policy.ErrModelNotAllowed)
		return
	}

	reserve := meta.MaxOutputTokens
	if reserve <= 0 {
		reserve = s.cfg.DefaultReserveTokens
	}
	reserve, err = s.reserveBudget(st.id, auth.RunID, reserve)
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrBudgetExhausted):
			s.reject(tw, st, http.StatusForbidden, audit.FailBudgetExhausted)
		case errors.Is(err, ledger.ErrTimeout):
			s.reject(tw, st, http.StatusServiceUnavailable, audit.FailSettleTimeout)
		default:
			s.logger.Error("ledger reserve failed", "request_id", st.id)
			s.reject(tw, st, http.StatusInternalServerError, audit.FailInternalPanic)
		}
		return
	}
	st.reserved, st.reserveTokens = true, reserve

	cred, err := up.p.Credential()
	if err != nil {
		s.logger.Error("credential unavailable", "provider", providerID, "request_id", st.id)
		s.reject(tw, st, http.StatusBadGateway, audit.FailUpstreamUnavail)
		return
	}
	if s.cfg.panicHook != nil {
		s.cfg.panicHook("before_commit")
	}
	if up.p.StreamUsage() {
		// The one sanctioned payload exception: see provider.InjectStreamUsage.
		body, st.streamInject = provider.InjectStreamUsage(body)
	}
	s.relay(tw, r, st, up, native, body, cred)
}

// preflight applies the checks that need no token: method, target form, Host,
// Origin and framing. It returns the status and code to reject with.
func (s *Server) preflight(r *http.Request) (int, string) {
	if r.Method == http.MethodConnect || r.URL.IsAbs() || r.URL.Host != "" {
		return http.StatusNotFound, codeNotFound
	}
	if raw := strings.ToLower(r.URL.RawPath); strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(raw, "%2e") {
		return http.StatusNotFound, codeNotFound
	}
	if _, ok := s.hosts[strings.ToLower(r.Host)]; !ok {
		return http.StatusBadRequest, audit.FailMalformed
	}
	if _, present := r.Header["Origin"]; present {
		return http.StatusForbidden, audit.FailUnauthorized
	}
	// net/http deletes Content-Length when Transfer-Encoding is present, so a
	// request carrying both can no longer be told apart from a chunked one.
	// Refusing every Transfer-Encoding request is the strictest safe reading.
	if len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" {
		return http.StatusBadRequest, audit.FailMalformed
	}
	return 0, ""
}

// presentedToken is the caller's bearer token. It is only ever compared with
// the authority record; it is never forwarded.
func presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.Header.Get("X-Api-Key")
}

// finish settles the reservation and writes the audit record. Both run under a
// context detached from the request and bounded by the cleanup timeout, so a
// client disconnect cannot skip them.
func (s *Server) finish(r *http.Request, tw *trackedWriter, st *reqState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.CleanupTimeout)
	defer cancel()
	auth := s.cfg.Authority

	measured := st.usage.Measured()
	known := measured || !st.maybeBilled // known zero cost counts as measured
	if st.reserved {
		used, isMeasured := st.reserveTokens, false // unknown: keep the conservative charge
		switch {
		case measured:
			used, isMeasured = st.usage.Total(), true
		case !st.maybeBilled:
			used, isMeasured = 0, true
		}
		if err := s.cfg.Ledger.Settle(st.id, auth.RunID, used, isMeasured); err != nil { //nolint:contextcheck // the ledger applies its own cleanup timeout, detached from the request
			known = false
			st.failure = audit.FailSettleTimeout
			s.logger.Error("settle failed", "request_id", st.id, "timeout", errors.Is(err, ledger.ErrTimeout))
		}
	}

	rec := audit.Record{
		Schema: audit.SchemaVersion, TS: s.now().UTC().Format(time.RFC3339Nano),
		RequestID: st.id, RunID: auth.RunID, Attempt: auth.Attempt,
		Provider: st.provider, Model: st.model, Method: st.method, Path: st.path,
		UpstreamAttempts: st.attempts, CommitState: commitState(tw, st),
		StatusCode: tw.status, DurationMS: s.now().Sub(st.start).Milliseconds(),
		UsageStatus: audit.UsageUnknown, FailureCode: st.failure,
		PolicySHA256: auth.PolicySHA256, RegistrySHA256: auth.RegistrySHA256,
		StreamUsageInjected: st.streamInject,
	}
	if known {
		rec.UsageStatus = audit.UsageMeasured
		zero := int64(0)
		rec.InputTokens, rec.OutputTokens = &zero, &zero
		if measured {
			rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens = st.usage.InputTokens, st.usage.OutputTokens, st.usage.CacheReadTokens
			if rec.InputTokens == nil {
				rec.InputTokens = &zero
			}
			if rec.OutputTokens == nil {
				rec.OutputTokens = &zero
			}
		}
	}
	if err := s.cfg.Audit.Write(ctx, rec); err != nil {
		s.logger.Error("audit write failed", "request_id", st.id)
	}
}

func commitState(tw *trackedWriter, st *reqState) string {
	switch {
	case tw.committed.Load() && !st.local:
		return audit.CommitCommitted
	case st.ambiguous:
		return audit.CommitAmbiguous
	}
	return audit.CommitNone
}

// allowsModel applies the provider-level allowlist when one is configured. An
// empty list defers to the token record, which is always enforced.
func (u *upstream) allowsModel(model string) bool {
	if len(u.p.AllowedModels) == 0 {
		return true
	}
	for _, m := range u.p.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// upstreamURL joins the provider base and the native path.
func (u *upstream) upstreamURL(native, rawQuery string) string {
	return (&url.URL{Scheme: u.base.Scheme, Host: u.base.Host, Path: native, RawQuery: rawQuery}).String()
}
