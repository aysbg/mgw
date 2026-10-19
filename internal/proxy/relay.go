package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/provider"
)

// chunkSize is the fixed relay buffer. One response never holds more.
const chunkSize = 16 << 10

// upstream is one provider with its own transport.
type upstream struct {
	p      provider.Provider
	base   *url.URL
	client *http.Client
}

func newUpstream(p provider.Provider, cfg Config) (*upstream, error) {
	base, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}
	dial := cfg.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: p.Timeouts.Connect()}
		dial = d.DialContext
	}
	var tlsCfg *tls.Config
	if cfg.TLSConfig != nil {
		tlsCfg = cfg.TLSConfig.Clone()
	} else {
		tlsCfg = &tls.Config{}
	}
	tlsCfg.MinVersion = tls.VersionTLS12

	// Go's transport defaults are wrong for a policy gateway. Each is turned
	// off explicitly and has a negative test in gate_12_transport_test.go.
	tr := &http.Transport{
		Proxy:                 nil,  // never ProxyFromEnvironment
		DisableCompression:    true, // Accept-Encoding passes through; bodies are never inflated
		DisableKeepAlives:     true, // every attempt dials fresh: no reuse after an ambiguous write, no implicit retry
		DialContext:           dial,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   p.Timeouts.Connect(),
		ResponseHeaderTimeout: p.Timeouts.TTFB(),
		ForceAttemptHTTP2:     false,
		// A non-nil empty map disables HTTP/2, whose transport retries on its own.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &upstream{p: p, base: base, client: client}, nil
}

// trackedWriter makes commit explicit. committed flips on the first header
// write (status line or a forwarded 1xx), the first body write or the first
// flush. After it flips the response can only be propagated or aborted, never
// restarted.
type trackedWriter struct {
	http.ResponseWriter
	committed atomic.Bool // also set from the transport's 1xx callback goroutine
	wroteHdr  bool        // handler goroutine only
	status    int
	hijacked  bool
}

// WriteHeader records the commit. Informational 1xx statuses commit but do not
// end the header phase.
func (t *trackedWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		t.committed.Store(true)
		t.ResponseWriter.WriteHeader(code)
		return
	}
	if t.wroteHdr {
		return
	}
	t.wroteHdr, t.status = true, code
	t.committed.Store(true)
	t.ResponseWriter.WriteHeader(code)
}

// Write commits, sending 200 first if no status was written.
func (t *trackedWriter) Write(p []byte) (int, error) {
	if !t.wroteHdr {
		t.WriteHeader(http.StatusOK)
	}
	return t.ResponseWriter.Write(p)
}

// flush commits and pushes buffered bytes to the client.
func (t *trackedWriter) flush() error {
	if !t.wroteHdr {
		t.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(t.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the real writer.
func (t *trackedWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// abort ends a committed response without writing another status line: the
// connection is closed so the client sees a failed stream, not a clean end.
func (t *trackedWriter) abort() {
	if t.hijacked {
		return
	}
	conn, _, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err != nil {
		return
	}
	t.hijacked = true
	_ = conn.Close()
}

// attemptTrace records what the transport managed to do for one attempt.
type attemptTrace struct {
	wroteHeaders atomic.Bool
}

var hopByHop = map[string]struct{}{
	"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {}, "Proxy-Authorization": {},
	"Proxy-Connection": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {},
}

// stripped reports whether a header must not cross the gateway in either
// direction. named is the set of header names the Connection header lists.
func stripped(canonical string, named map[string]struct{}) bool {
	if _, ok := hopByHop[canonical]; ok {
		return true
	}
	if strings.HasPrefix(canonical, "Proxy-") {
		return true
	}
	_, ok := named[canonical]
	return ok
}

// connectionNamed returns the headers a Connection header names.
func connectionNamed(h http.Header) map[string]struct{} {
	named := map[string]struct{}{}
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				named[http.CanonicalHeaderKey(tok)] = struct{}{}
			}
		}
	}
	return named
}

// upstreamHeaders builds the upstream request headers: hop-by-hop headers
// dropped, the caller's own credentials dropped, and the provider credential
// set for its dialect. Accept-Encoding passes through untouched.
func upstreamHeaders(src http.Header, dialect provider.Dialect, cred string) http.Header {
	named := connectionNamed(src)
	out := make(http.Header, len(src)+1)
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		switch ck {
		case "Authorization", "X-Api-Key", "Host", "Content-Length", "Expect", "Origin":
			continue
		}
		if stripped(ck, named) {
			continue
		}
		out[ck] = append([]string(nil), vv...)
	}
	if dialect == provider.DialectAnthropic {
		out.Set("X-Api-Key", cred)
	} else {
		out.Set("Authorization", "Bearer "+cred)
	}
	return out
}

// copyResponseHeaders copies upstream response headers except hop-by-hop ones.
func copyResponseHeaders(dst, src http.Header) {
	named := connectionNamed(src)
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		if stripped(ck, named) {
			continue
		}
		dst[ck] = append([]string(nil), vv...)
	}
	if _, ok := dst["Content-Type"]; !ok {
		dst["Content-Type"] = nil // stop net/http sniffing a type the upstream never sent
	}
}

// roundTrip sends one upstream attempt. Informational responses are forwarded
// to the client as they arrive, which commits the response.
func (s *Server) roundTrip(ctx context.Context, tw *trackedWriter, r *http.Request, up *upstream, native string, body []byte, cred string, tr *attemptTrace) (*http.Response, error) {
	var rdr *bytes.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	trace := &httptrace.ClientTrace{
		// Conservative: once headers are handed to the transport we treat the
		// request as possibly on the wire.
		WroteHeaders: func() { tr.wroteHeaders.Store(true) },
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusSwitchingProtocols {
				return nil
			}
			hdr := tw.Header()
			var added []string
			for k, vv := range h {
				ck := http.CanonicalHeaderKey(k)
				if stripped(ck, nil) {
					continue
				}
				hdr[ck] = append([]string(nil), vv...)
				added = append(added, ck)
			}
			tw.WriteHeader(code)
			for _, k := range added {
				delete(hdr, k)
			}
			return nil
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	target := up.upstreamURL(native, r.URL.RawQuery)
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, r.Method, target, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, r.Method, target, http.NoBody)
	}
	if err != nil {
		return nil, err
	}
	req.GetBody = nil // the transport must never replay a body on its own
	req.ContentLength = int64(len(body))
	req.Host = up.base.Host // rewrite Host to the upstream
	req.Header = upstreamHeaders(r.Header, up.p.Dialect, cred)
	return up.client.Do(req) // #nosec G107 -- target is operator config plus a cleaned path
}

// retryAfter parses a Retry-After header as delta-seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// relay runs the upstream attempts and streams the response.
//
//	committed?  yes -> propagate, never restart
//	            no  -> retryable non-acceptance established?
//	                     yes -> retry once, same request, original deadline
//	                     no  -> fail, keep the uncertain charge
func (s *Server) relay(tw *trackedWriter, r *http.Request, st *reqState, up *upstream, native string, body []byte, cred string) {
	ctx, cancel := context.WithTimeout(r.Context(), up.p.Timeouts.Overall())
	defer cancel()

	const maxAttempts = 2
	for attempt := 1; ; attempt++ {
		st.attempts = attempt
		var tr attemptTrace
		resp, err := s.roundTrip(ctx, tw, r, up, native, body, cred, &tr)

		if err != nil {
			if s.failedAttempt(tw, r, ctx, st, err, &tr, attempt < maxAttempts) {
				continue
			}
			return
		}

		if attempt < maxAttempts && !tw.committed.Load() {
			if wait, ok := explicitNonAcceptance(resp, s.now()); ok && s.waitFits(ctx, wait) {
				_ = resp.Body.Close()
				if err := s.sleep(ctx, wait); err != nil {
					st.failure = failureForContext(r, ctx)
					return
				}
				continue
			}
		}
		s.stream(tw, r, ctx, cancel, st, up, resp)
		return
	}
}

// explicitNonAcceptance is a 429 or 503 with a usable Retry-After: the upstream
// said it did not take the request.
func explicitNonAcceptance(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0, false
	}
	return retryAfter(resp.Header, now)
}

// waitFits reports whether sleeping d still leaves time before the deadline.
func (s *Server) waitFits(ctx context.Context, d time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || s.now().Add(d).Before(dl)
}

// failedAttempt classifies a transport error. It returns true when the caller
// should retry; otherwise it has already answered the client (or cannot) and
// recorded the outcome.
func (s *Server) failedAttempt(tw *trackedWriter, r *http.Request, ctx context.Context, st *reqState, err error, tr *attemptTrace, canRetry bool) bool {
	transmitted := tr.wroteHeaders.Load()
	if transmitted {
		st.maybeBilled = true
	}
	switch {
	case r.Context().Err() != nil:
		// Client disconnect cancelled the upstream call. Still settle, still audit.
		st.failure = audit.FailClientDisconnect
		st.ambiguous = transmitted && !tw.committed.Load()
		return false
	case tw.committed.Load():
		// A forwarded 1xx already committed the response: propagate, never restart.
		st.failure = audit.FailCommitLost
		tw.abort()
		return false
	case !transmitted && ctx.Err() == nil:
		// Dial error, or a reset before any byte was written upstream: the
		// request was not accepted, so one retry is safe.
		if canRetry {
			return true
		}
		s.reject(tw, st, http.StatusBadGateway, audit.FailUpstreamUnavail)
		return false
	case !transmitted:
		// The overall deadline ran out before anything was sent.
		s.reject(tw, st, http.StatusGatewayTimeout, audit.FailUpstreamTimeout)
		return false
	}
	// Bytes may have reached upstream and nothing was committed: ambiguous. Do
	// not retry; keep the charge as uncertain.
	st.ambiguous = true
	var ne net.Error
	if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
		s.reject(tw, st, http.StatusGatewayTimeout, audit.FailUpstreamTimeout)
		return false
	}
	s.reject(tw, st, http.StatusBadGateway, audit.FailCommitLost)
	return false
}

func failureForContext(r *http.Request, ctx context.Context) string {
	if r.Context().Err() != nil {
		return audit.FailClientDisconnect
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return audit.FailUpstreamTimeout
	}
	return audit.FailStreamInterrupted
}

// stream commits the response and relays the body in fixed-size chunks, writing
// and flushing after each read. It never buffers the whole body.
func (s *Server) stream(tw *trackedWriter, r *http.Request, ctx context.Context, cancel context.CancelFunc, st *reqState, up *upstream, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()

	status := resp.StatusCode
	nonAccept := status >= 400 && status < 500
	if _, retryable := explicitNonAcceptance(resp, s.now()); retryable {
		nonAccept = true
	}
	if !nonAccept {
		st.maybeBilled = true
	}

	copyResponseHeaders(tw.Header(), resp.Header)
	tw.WriteHeader(status) // commit
	if s.cfg.panicHook != nil {
		s.cfg.panicHook("after_commit")
	}

	var scanner provider.UsageScanner
	scan := status >= 200 && status < 300

	var idleFired atomic.Bool
	idle := time.AfterFunc(up.p.Timeouts.Idle(), func() {
		idleFired.Store(true)
		cancel()
	})
	defer idle.Stop()

	buf := make([]byte, chunkSize)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(up.p.Timeouts.Idle())
			if scan {
				_, _ = scanner.Write(buf[:n])
			}
			if _, werr := tw.Write(buf[:n]); werr != nil {
				cancel() // client gone: cancel upstream
				st.failure = audit.FailClientDisconnect
				return
			}
			if ferr := tw.flush(); ferr != nil {
				cancel()
				st.failure = audit.FailClientDisconnect
				return
			}
		}
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) {
			if scan {
				st.usage = scanner.Usage()
			}
			return
		}
		// Upstream stream failed after commit: propagate, never restart.
		switch {
		case r.Context().Err() != nil:
			st.failure = audit.FailClientDisconnect
		case idleFired.Load() || errors.Is(ctx.Err(), context.DeadlineExceeded):
			st.failure = audit.FailUpstreamTimeout
		default:
			st.failure = audit.FailStreamInterrupted
		}
		tw.abort()
		return
	}
}
