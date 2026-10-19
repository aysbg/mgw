package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGate03_UnlistedProviderAndRoutes: anything that is not /p/{listed}/...
// is a 404 with zero upstream calls.
func TestGate03_UnlistedProviderAndRoutes(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{
		"/p/unlisted/v1/chat/completions", // valid token, provider not configured
		"/",
		"/v1/chat/completions",
		"/p/openai",
		"/p/openai/",
		"/p//v1/x",
		"/p/openai/../anthropic/v1/messages",
		"/p/openai/v1/../../x",
		"/p/openai/%2e%2e/x",
		"/p/openai//v1/chat/completions",
		"/p/openai/v1/chat/completions/",
		"/P/openai/v1/chat/completions",
	} {
		resp, body := h.do(path, chatBody, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d %s", path, resp.StatusCode, body)
		}
	}
	// CONNECT and absolute-form targets, sent raw because http.Client will not.
	for name, raw := range map[string]string{
		"connect":  "CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\n\r\n",
		"absolute": fmt.Sprintf("POST http://api.openai.com/p/openai/v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Length: 0\r\n\r\n", hostPort(h)),
	} {
		status, _ := rawRequest(t, hostPort(h), raw)
		if !strings.Contains(status, "404") && !strings.Contains(status, "400") {
			t.Errorf("%s: got %q", name, status)
		}
	}
	if n := h.upstream.count(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
}

// TestGate03_HostOriginAndFraming: unexpected Host, any Origin, and a request
// carrying both Transfer-Encoding and Content-Length are refused.
func TestGate03_HostOriginAndFraming(t *testing.T) {
	h := newHarness(t, nil)
	if resp, body := h.do(chatPath, chatBody, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("origin: %d %s", resp.StatusCode, body)
	}
	req, _ := newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(chatBody))
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("host: %d", resp.StatusCode)
	}
	// localhost with the right port is an expected Host.
	req, _ = newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(chatBody))
	req.Host = "localhost:" + strings.Split(hostPort(h), ":")[1]
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err = h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("localhost host: %d", resp.StatusCode)
	}
	before := h.upstream.count()

	both := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\nContent-Length: %d\r\n\r\n%x\r\n%s\r\n0\r\n\r\n",
		chatPath, hostPort(h), h.token, len(chatBody), len(chatBody), chatBody)
	if status, body := rawRequest(t, hostPort(h), both); !strings.Contains(status, "400") || errCode(t, []byte(body)) != "malformed" {
		t.Errorf("TE+CL: %q %q", status, body)
	}
	if n := h.upstream.count(); n != before {
		t.Fatalf("a refused request reached upstream (%d -> %d)", before, n)
	}
}

// TestGate03_RedirectNotFollowed: a 3xx from upstream reaches the caller as is.
func TestGate03_RedirectNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
		}
	})
	h.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(resp.Header.Get("Location"), target.URL) {
		t.Fatalf("got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

// TestGate03_UpstreamRequestShape: the local token never appears upstream,
// the provider credential does, hop-by-hop headers are stripped, Host is
// rewritten, and Accept-Encoding passes through.
func TestGate03_UpstreamRequestShape(t *testing.T) {
	t.Run("openai dialect", func(t *testing.T) {
		h := newHarness(t, nil)
		resp, _ := h.do(chatPath+"?x=1", chatBody, map[string]string{
			"Connection":          "X-Hop, keep-alive",
			"X-Hop":               "secret-hop",
			"Keep-Alive":          "timeout=5",
			"Proxy-Authorization": "Basic abc",
			"Proxy-Foo":           "bar",
			"Te":                  "trailers",
			"Trailer":             "X-Trailer",
			"X-Api-Key":           h.token,
			"Accept-Encoding":     "gzip, br",
			"X-Keep-Me":           "yes",
			"Expect":              "100-continue",
		})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		got := h.upstream.request(0)
		if got.Path != "/v1/chat/completions" || got.Query != "x=1" || got.Method != "POST" {
			t.Fatalf("target %+v", got)
		}
		if a := got.Header.Get("Authorization"); a != "Bearer "+testCredential {
			t.Fatalf("Authorization = %q", a)
		}
		if got.Header.Get("X-Api-Key") != "" {
			t.Fatal("caller x-api-key was forwarded")
		}
		for k, vv := range got.Header {
			for _, v := range vv {
				if strings.Contains(v, h.token) {
					t.Fatalf("local token leaked in upstream header %s", k)
				}
			}
		}
		if strings.Contains(string(got.Body), h.token) || strings.Contains(got.Path+got.Query, h.token) {
			t.Fatal("local token leaked in upstream body or URL")
		}
		for _, k := range []string{"X-Hop", "Keep-Alive", "Proxy-Authorization", "Proxy-Foo", "Te", "Trailer", "Upgrade", "Expect"} {
			if _, ok := got.Header[k]; ok {
				t.Errorf("hop-by-hop header %s reached upstream", k)
			}
		}
		if c := got.Header.Get("Connection"); strings.Contains(strings.ToLower(c), "x-hop") || strings.Contains(strings.ToLower(c), "keep-alive") {
			t.Errorf("client Connection tokens forwarded: %q", c)
		}
		if got.Header.Get("X-Keep-Me") != "yes" || got.Header.Get("Accept-Encoding") != "gzip, br" {
			t.Errorf("end-to-end headers not preserved: %v", got.Header)
		}
		if got.Host == hostPort(h) || got.Host != strings.TrimPrefix(h.upstream.URL, "http://") {
			t.Errorf("Host not rewritten: %q (gateway %q, upstream %q)", got.Host, hostPort(h), h.upstream.URL)
		}
		if string(got.Body) != chatBody {
			t.Errorf("body changed: %q", got.Body)
		}
	})
	t.Run("anthropic dialect", func(t *testing.T) {
		h := newHarness(t, nil)
		resp, _ := h.do(msgPath, msgBody, map[string]string{"Authorization": "", "X-Api-Key": h.token, "Anthropic-Version": "2023-06-01"})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		got := h.upstream.request(0)
		if got.Header.Get("X-Api-Key") != testCredential || got.Header.Get("Authorization") != "" {
			t.Fatalf("credential headers: %v", got.Header)
		}
		if got.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Fatal("anthropic-version was not passed through")
		}
		for k, vv := range got.Header {
			for _, v := range vv {
				if strings.Contains(v, h.token) {
					t.Fatalf("local token leaked in %s", k)
				}
			}
		}
	})
}

// TestGate03_ResponseHeadersStripped: hop-by-hop response headers do not reach
// the caller and a missing Content-Type is not invented.
func TestGate03_ResponseHeadersStripped(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Keep-Alive", "timeout=1")
			w.Header().Set("Proxy-Authenticate", "Basic")
			w.Header().Set("X-Upstream", "kept")
			w.Header()["Content-Type"] = nil
			_, _ = io.WriteString(w, "plain")
		}
	})
	resp, body := h.do(chatPath, chatBody, nil)
	for _, k := range []string{"Keep-Alive", "Proxy-Authenticate"} {
		if resp.Header.Get(k) != "" {
			t.Errorf("%s reached the caller", k)
		}
	}
	if resp.Header.Get("X-Upstream") != "kept" || string(body) != "plain" {
		t.Errorf("headers %v body %q", resp.Header, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		t.Errorf("gateway invented Content-Type %q", ct)
	}
}

// TestGate03_ConnectionNamedHeadersStripped covers the header names a Connection
// header lists, in both directions. Done at function level for the response side
// because a Go upstream replaces Connection with "close" on our fresh connections.
func TestGate03_ConnectionNamedHeadersStripped(t *testing.T) {
	dst := http.Header{}
	copyResponseHeaders(dst, http.Header{
		"Connection": {"X-Resp-Hop, Upgrade"}, "X-Resp-Hop": {"1"}, "Upgrade": {"h2c"},
		"Transfer-Encoding": {"chunked"}, "Trailer": {"X-T"}, "Proxy-Connection": {"x"}, "X-Ok": {"1"},
	})
	if len(dst) != 2 || dst.Get("X-Ok") != "1" { // X-Ok plus the suppressed Content-Type entry
		t.Fatalf("got %v", dst)
	}
	got := upstreamHeaders(http.Header{
		"Connection": {"X-Hop"}, "X-Hop": {"1"}, "Authorization": {"Bearer local"}, "X-Ok": {"1"},
	}, "openai", "cred")
	if got.Get("X-Hop") != "" || got.Get("Connection") != "" || got.Get("Authorization") != "Bearer cred" || got.Get("X-Ok") != "1" {
		t.Fatalf("got %v", got)
	}
}
