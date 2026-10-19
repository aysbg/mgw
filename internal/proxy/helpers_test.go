package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

const (
	testCredential = "sk-upstream-credential-123"
	testRunID      = "run-test"
	chatPath       = "/p/openai/v1/chat/completions"
	msgPath        = "/p/anthropic/v1/messages"
	chatBody       = `{"model":"gpt-6-astra","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	msgBody        = `{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	usageJSON      = `{"id":"1","usage":{"prompt_tokens":11,"completion_tokens":7}}`
)

// upstreamRequest is what a fake upstream saw.
type upstreamRequest struct {
	Method string
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   []byte
}

// fakeUpstream is an httptest upstream that records every request.
type fakeUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []upstreamRequest
	conns   atomic.Int32
	handler atomic.Value // http.HandlerFunc
}

func newFakeUpstream(t *testing.T, h http.HandlerFunc) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.handler.Store(h)
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, upstreamRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Clone(), body})
		u.mu.Unlock()
		u.handler.Load().(http.HandlerFunc)(w, r)
	}))
	u.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			u.conns.Add(1)
		}
	}
	u.Start()
	t.Cleanup(u.Close)
	return u
}

func (u *fakeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *fakeUpstream) request(i int) upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[i]
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, usageJSON)
}

// harness is a running gateway wired to fake upstreams.
type harness struct {
	t         *testing.T
	srv       *Server
	url       string
	token     string
	auth      *policy.Authority
	led       *ledger.Ledger
	auditPath string
	upstream  *fakeUpstream
	sleeps    []time.Duration
	client    *http.Client
}

type harnessOpts struct {
	concurrency int
	budget      int64
	timeouts    provider.Timeouts
	expires     time.Time
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	providers   func(upstreamURL string, credFile string) []provider.Provider
	panicHook   func(string)
	handler     http.HandlerFunc
	cleanup     time.Duration
	tls         *tls.Config
}

func newToken() string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 33)) }

func newHarness(t *testing.T, mod func(*harnessOpts)) *harness {
	t.Helper()
	o := harnessOpts{
		concurrency: 2, budget: 50000, expires: time.Now().Add(time.Hour),
		timeouts: provider.Timeouts{ConnectMS: 2000, TTFBMS: 5000, IdleMS: 5000, OverallMS: 10000},
		handler:  okHandler, cleanup: 5 * time.Second,
	}
	if mod != nil {
		mod(&o)
	}
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	if err := os.WriteFile(credFile, []byte(testCredential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t, o.handler)
	provs := []provider.Provider{
		{ID: "openai", Dialect: provider.DialectOpenAI, BaseURL: up.URL, CredentialFile: credFile, Timeouts: o.timeouts},
		{ID: "anthropic", Dialect: provider.DialectAnthropic, BaseURL: up.URL, CredentialFile: credFile, Timeouts: o.timeouts},
		{ID: "extra", Dialect: provider.DialectOpenAI, BaseURL: up.URL, CredentialFile: credFile, Timeouts: o.timeouts},
	}
	if o.providers != nil {
		provs = o.providers(up.URL, credFile)
	}
	token := newToken()
	auth := &policy.Authority{
		Token: token, RunID: testRunID, Attempt: 3, ExpiresAt: o.expires,
		Providers:   map[string][]string{"openai": {"gpt-6-astra"}, "anthropic": {"claude-opus-5-5"}},
		Budget:      policy.Budget{MaxTokens: o.budget},
		Concurrency: o.concurrency, PolicySHA256: "policy-sha", RegistrySHA256: "registry-sha",
	}
	led, err := ledger.Open(filepath.Join(dir, "ledger"), o.budget, o.cleanup)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	aw, err := audit.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, token: token, auth: auth, led: led, auditPath: auditPath, upstream: up}
	srv, err := New(Config{
		Providers: provs, Authority: auth, Ledger: led, Audit: aw,
		ReadyFile: filepath.Join(dir, "ready.json"), CleanupTimeout: o.cleanup,
		DialContext: o.dial, TLSConfig: o.tls, panicHook: o.panicHook,
		Sleep: func(_ context.Context, d time.Duration) error { h.sleeps = append(h.sleeps, d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(context.Background()); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
		_ = aw.Close()
	})
	h.srv, h.url = srv, srv.URL()
	h.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}}
	t.Cleanup(h.client.CloseIdleConnections)
	return h
}

// reply is the part of a response the tests inspect. The body is already read
// and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

// do sends a POST with the harness token unless headers override it.
func (h *harness) do(path, body string, hdr map[string]string) (*reply, []byte) {
	h.t.Helper()
	req, err := newTestRequest(http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return &reply{StatusCode: resp.StatusCode, Header: resp.Header}, data
}

// idle waits for every started handler, including its settle and audit write.
func (h *harness) idle() { h.srv.inflight.Wait() }

// records returns every audit line, after all handlers have finished. It also
// checks that each line carries exactly the allowlisted fields.
func (h *harness) records() []audit.Record {
	h.t.Helper()
	h.idle()
	f, err := os.Open(h.auditPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []audit.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			h.t.Fatal(err)
		}
		if len(m) != len(audit.AllowedFields) {
			h.t.Fatalf("audit line has %d fields, want %d: %v", len(m), len(audit.AllowedFields), m)
		}
		for _, k := range audit.AllowedFields {
			if _, ok := m[k]; !ok {
				h.t.Fatalf("audit line missing %q", k)
			}
		}
		var r audit.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			h.t.Fatal(err)
		}
		if err := r.Validate(); err != nil {
			h.t.Fatal(err)
		}
		if r.RunID != testRunID || r.Attempt != 3 || r.PolicySHA256 != "policy-sha" || r.RegistrySHA256 != "registry-sha" {
			h.t.Fatalf("audit identity fields wrong: %+v", r)
		}
		out = append(out, r)
	}
	return out
}

func (h *harness) lastRecord() audit.Record {
	h.t.Helper()
	recs := h.records()
	if len(recs) == 0 {
		h.t.Fatal("no audit record")
	}
	return recs[len(recs)-1]
}

func (h *harness) totals() ledger.Totals {
	h.t.Helper()
	tot, err := h.led.Totals(testRunID)
	if err != nil {
		h.t.Fatal(err)
	}
	return tot
}

// errCode extracts error.code from a gateway error body.
func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not a gateway error body: %q", body)
	}
	return e.Error.Code
}

// rawRequest writes raw bytes to the gateway and returns the status line and body.
func rawRequest(t *testing.T, addr, raw string) (status string, body string) {
	t.Helper()
	conn, err := dialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.Status, string(b)
}

func hostPort(h *harness) string { return strings.TrimPrefix(h.url, "http://") }

func ioReader(s string) io.Reader { return strings.NewReader(s) }

func asExit(err error, target **exec.ExitError) bool { return errors.As(err, target) }

func newTestLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

// newTestRequest is http.NewRequest with a context, which the noctx linter wants.
func newTestRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(context.Background(), method, url, body)
}

func dialTCP(addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(context.Background(), "tcp", addr)
}

func listenTCP(addr string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(context.Background(), "tcp4", addr)
}

func command(name string, args ...string) *exec.Cmd {
	return exec.CommandContext(context.Background(), name, args...) // #nosec G204 -- tests run fixed commands
}
