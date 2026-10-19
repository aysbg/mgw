package proxy

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/provider"
)

func TestRoutePath(t *testing.T) {
	good := map[string][2]string{
		"/p/openai/v1/chat/completions": {"openai", "/v1/chat/completions"},
		"/p/anthropic/v1/messages":      {"anthropic", "/v1/messages"},
		"/p/a-b_c/x":                    {"a-b_c", "/x"},
		"/p/openai/v1/models/gpt-6":     {"openai", "/v1/models/gpt-6"},
		"/p/openai/a.b/c..d/.hidden":    {"openai", "/a.b/c..d/.hidden"},
	}
	for in, want := range good {
		p, n, ok := RoutePath(in)
		if !ok || p != want[0] || n != want[1] {
			t.Errorf("%q: got %q %q %v", in, p, n, ok)
		}
	}
	for _, in := range []string{
		"", "/", "/p", "/p/", "/p/openai", "/p/openai/", "/p//x", "/P/openai/x", "/q/openai/x",
		"/p/openai/../x", "/p/openai/./x", "/p/openai//x", "/p/openai/x/", "/p/openai/x/..",
		"/p/Open AI/x", "/p/open.ai/x", "/p/openai/a\\b", "/p/openai/a\x00b", "/p/openai/a\nb",
		"/p/../x", "p/openai/x", "//p/openai/x",
		"/p/" + strings.Repeat("a", 65) + "/x",
	} {
		if p, n, ok := RoutePath(in); ok {
			t.Errorf("%q accepted as %q %q", in, p, n)
		}
	}
}

func FuzzRoutePath(f *testing.F) {
	for _, s := range []string{
		"/p/openai/v1/chat/completions", "/p/anthropic/v1/messages", "/p/openai/../x", "/p//x", "/p/openai/",
		"/p/openai/%2e%2e/x", "/p/o/a/b/c", "", "/", "/p/openai/x\\y", "/p/openai/\x00", "/p/openai/./x",
		"/p/openai//x", "/p/UPPER/x", "/p/openai/a b", "/p/openai/…/ünï",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		p, n, ok := RoutePath(in)
		if !ok {
			if p != "" || n != "" {
				t.Fatalf("rejected input returned values %q %q", p, n)
			}
			return
		}
		if "/p/"+p+n != in {
			t.Fatalf("round trip: %q != /p/%s%s", in, p, n)
		}
		if !provider.ValidID(p) {
			t.Fatalf("invalid provider %q", p)
		}
		if !strings.HasPrefix(n, "/") || len(n) < 2 || path.Clean(n) != n {
			t.Fatalf("native path %q is not clean", n)
		}
		for _, seg := range strings.Split(n, "/") {
			if seg == ".." || seg == "." {
				t.Fatalf("traversal segment in %q", n)
			}
		}
		if strings.ContainsAny(n, "\\\x00\r\n") {
			t.Fatalf("control or separator byte in %q", n)
		}
	})
}

// TestLogsAndErrorsCarryNoContent: nothing the caller sent, no token and no
// credential reaches the logs or an error body.
func TestLogsAndErrorsCarryNoContent(t *testing.T) {
	var logs strings.Builder
	h := newHarness(t, func(o *harnessOpts) {
		o.providers = func(upstreamURL, credFile string) []provider.Provider {
			// A credential file readable by others makes Credential() fail.
			if err := os.Chmod(credFile, 0o644); err != nil {
				t.Fatal(err)
			}
			return []provider.Provider{{
				ID: "openai", Dialect: provider.DialectOpenAI, BaseURL: upstreamURL, CredentialFile: credFile, Timeouts: o.timeouts,
			}}
		}
	})
	h.srv.logger = newTestLogger(&logs)
	secretPrompt := "my-very-secret-prompt-text"
	resp, body := h.do(chatPath, `{"model":"gpt-6-astra","messages":[{"role":"user","content":"`+secretPrompt+`"}]}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	h.idle()
	for name, text := range map[string]string{"logs": logs.String(), "error body": string(body), "audit": mustRead(t, h.auditPath)} {
		for _, secret := range []string{secretPrompt, h.token, testCredential} {
			if strings.Contains(text, secret) {
				t.Errorf("%s contain %q", name, secret)
			}
		}
	}
	if logs.Len() == 0 {
		t.Fatal("expected the credential failure to be logged")
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
