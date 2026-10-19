package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/provider"
)

func TestUpstreamURLPreservesBasePrefix(t *testing.T) {
	for _, tc := range []struct {
		name, base, path, query, want string
	}{
		{"openrouter prefix", "https://openrouter.ai/api", "/v1/chat/completions", "", "https://openrouter.ai/api/v1/chat/completions"},
		{"host root", "https://openrouter.ai", "/v1/chat/completions", "", "https://openrouter.ai/v1/chat/completions"},
		{"no double slash", "https://proxy.example/gateway/api", "/v1/chat/completions", "", "https://proxy.example/gateway/api/v1/chat/completions"},
		{"verbatim escaped prefix", "https://proxy.example/%61pi/a%20b", "/v1/chat/completions", "a=1&b=two%20words", "https://proxy.example/%61pi/a%20b/v1/chat/completions?a=1&b=two%20words"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, err := newUpstream(provider.Provider{BaseURL: tc.base}, Config{})
			if err != nil {
				t.Fatal(err)
			}
			got := up.prefixedURL(tc.path, tc.query)
			if got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(u.Path, "//") {
				t.Fatalf("URL path contains a double slash: %q", u.Path)
			}
		})
	}
}

func TestRoundTripPreservesBaseURLPrefix(t *testing.T) {
	const want = "/%61pi/v1/chat/completions?key=value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != want {
			t.Errorf("upstream RequestURI = %q, want %q", r.RequestURI, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	up, err := newUpstream(provider.Provider{BaseURL: srv.URL + "/%61pi"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions?key=value", nil)
	resp, err := (&Server{}).roundTrip(context.Background(), &trackedWriter{ResponseWriter: httptest.NewRecorder()}, req, up, "/v1/chat/completions", nil, "test-key", &attemptTrace{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
