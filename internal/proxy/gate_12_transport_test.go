package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/provider"
)

// transportOf returns the transport behind a provider, for field assertions.
func transportOf(t *testing.T, h *harness, id string) *http.Transport {
	t.Helper()
	tr, ok := h.srv.upstreams[id].client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("provider transport is not *http.Transport")
	}
	return tr
}

// TestGate12_TransportDefaultsDisabled asserts the four defaults are off on
// every provider transport, then proves each by behaviour in the tests below.
func TestGate12_TransportDefaultsDisabled(t *testing.T) {
	h := newHarness(t, nil)
	for id, up := range h.srv.upstreams {
		tr := transportOf(t, h, id)
		if tr.Proxy != nil {
			t.Errorf("%s: Proxy must be nil", id)
		}
		if !tr.DisableCompression {
			t.Errorf("%s: DisableCompression must be true", id)
		}
		if !tr.DisableKeepAlives {
			t.Errorf("%s: DisableKeepAlives must be true (no reuse after an ambiguous write)", id)
		}
		if tr.ForceAttemptHTTP2 || tr.TLSNextProto == nil {
			t.Errorf("%s: HTTP/2 must stay off", id)
		}
		if up.client.CheckRedirect == nil || !errors.Is(up.client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
			t.Errorf("%s: CheckRedirect must return ErrUseLastResponse", id)
		}
		if tr.ResponseHeaderTimeout <= 0 || tr.TLSHandshakeTimeout <= 0 || up.p.Timeouts.Idle() <= 0 || up.p.Timeouts.Overall() <= 0 {
			t.Errorf("%s: a zero deadline is a defect", id)
		}
	}
}

// TestGate12_DecompressionNegative: a gzip response is relayed byte-exact, not
// inflated, and Accept-Encoding reaches upstream untouched.
func TestGate12_DecompressionNegative(t *testing.T) {
	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	_, _ = zw.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	_ = zw.Close()
	gz := zbuf.Bytes()

	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gz)
		}
	})
	resp, body := h.do(chatPath, chatBody, map[string]string{"Accept-Encoding": "gzip"})
	if !bytes.Equal(body, gz) {
		t.Fatalf("body was altered: %d bytes in, %d out", len(gz), len(body))
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding dropped: %v", resp.Header)
	}
	if got := h.upstream.request(0).Header.Get("Accept-Encoding"); got != "gzip" {
		t.Fatalf("upstream Accept-Encoding = %q", got)
	}
	// With no Accept-Encoding from the caller, the gateway must not add one.
	h.do(chatPath, chatBody, map[string]string{"Accept-Encoding": ""})
	if _, ok := h.upstream.request(1).Header["Accept-Encoding"]; ok {
		t.Fatalf("gateway invented Accept-Encoding: %v", h.upstream.request(1).Header)
	}
	// Compressed bytes cannot be parsed for usage, so usage stays unknown.
	if rec := h.records()[0]; rec.UsageStatus != audit.UsageUnknown {
		t.Fatalf("audit: %+v", rec)
	}
}

// TestGate12_EnvironmentProxyNegative: HTTP_PROXY/HTTPS_PROXY have no effect.
// net/http never proxies loopback destinations, so this uses an HTTPS provider
// on a fake host name and records where the dialer is asked to connect.
func TestGate12_EnvironmentProxyNegative(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyHits.Add(1) }))
	defer proxySrv.Close()
	t.Setenv("HTTP_PROXY", proxySrv.URL)
	t.Setenv("HTTPS_PROXY", proxySrv.URL)
	t.Setenv("http_proxy", proxySrv.URL)
	t.Setenv("https_proxy", proxySrv.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	upstreamTLS := httptest.NewTLSServer(http.HandlerFunc(okHandler))
	defer upstreamTLS.Close()
	pool := x509.NewCertPool()
	pool.AddCert(upstreamTLS.Certificate())

	var mu sync.Mutex
	var dialed []string
	h := newHarness(t, func(o *harnessOpts) {
		o.tls = &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12} // httptest cert is issued for example.com
		o.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, upstreamTLS.Listener.Addr().String())
		}
		o.providers = func(_ string, credFile string) []provider.Provider {
			return []provider.Provider{{
				ID: "openai", Dialect: provider.DialectOpenAI, BaseURL: "https://fake-upstream.example",
				CredentialFile: credFile, Timeouts: o.timeouts,
			}}
		}
	})
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != 200 || string(body) != usageJSON {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) == 0 {
		t.Fatal("the dialer was never used")
	}
	for _, a := range dialed {
		if a != "fake-upstream.example:443" {
			t.Fatalf("dialed %q; an environment proxy was used", a)
		}
	}
	if proxyHits.Load() != 0 {
		t.Fatal("the proxy named in the environment was contacted")
	}
	if tr := transportOf(t, h, "openai"); tr.Proxy != nil {
		t.Fatal("Proxy must be nil")
	}
}

// TestGate12_RedirectNegative: a redirect is returned, not followed, and the
// Location target is never contacted.
func TestGate12_RedirectNegative(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		h := newHarness(t, func(o *harnessOpts) {
			o.handler = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, code) }
		})
		h.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if resp, _ := h.do(chatPath, chatBody, nil); resp.StatusCode != code {
			t.Fatalf("%d: got %d", code, resp.StatusCode)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a redirect target was contacted")
	}
}

// TestGate12_ImplicitRetryNegative: net/http silently retries an idempotent
// request whose reused connection died. The gateway uses a fresh connection per
// attempt and never retries an ambiguous failure, so upstream sees the request
// exactly once and the failure reaches the caller.
func TestGate12_ImplicitRetryNegative(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				okHandler(w, r)
				return
			}
			killConn(t, w)
		}
	})
	do := func() int {
		req, _ := newTestRequest(http.MethodGet, h.url+"/p/openai/v1/models", ioReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if s := do(); s != 200 {
		t.Fatalf("first: %d", s)
	}
	if s := do(); s != 502 {
		t.Fatalf("second: %d", s)
	}
	if n := h.upstream.count(); n != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (no hidden retry)", n)
	}
	if c := h.upstream.conns.Load(); c != 2 {
		t.Fatalf("upstream accepted %d connections, want one fresh connection per attempt", c)
	}
	recs := h.records()
	if last := recs[len(recs)-1]; last.UpstreamAttempts != 1 || last.FailureCode != "commit_lost" {
		t.Fatalf("audit: %+v", last)
	}
}
