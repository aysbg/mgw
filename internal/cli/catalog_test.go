package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/catalog"
)

func catalogFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../catalog/testdata/upstream-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCatalogDiffOutput(t *testing.T) {
	raw := catalogFixture(t)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
	defer srv.Close()
	d := Deps{Fetch: catalog.Fetch}
	var text string
	for _, mode := range []string{"text", "json", "quiet"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"catalog", "diff", "--catalog-url=" + srv.URL}
			if mode != "text" {
				args = append(args, "--"+mode)
			}
			var out, errb bytes.Buffer
			if code := Run(args, &out, &errb, d); code != ExitOK || errb.Len() != 0 {
				t.Fatalf("exit %d, stderr %q", code, errb.String())
			}
			if mode == "json" {
				var got map[string]any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{
					"source_url": srv.URL, "source_sha256": digest,
					"deltas": []any{
						map[string]any{"kind": "new_upstream", "provider": "ollama-cloud", "model": "alpha", "context": float64(100000), "input": float64(150000), "output": float64(600000)},
						map[string]any{"kind": "new_upstream", "provider": "ollama-cloud", "model": "zeta", "context": float64(200000), "input": float64(0), "output": float64(0)},
						map[string]any{"kind": "new_upstream", "provider": "openrouter", "model": "moonshotai/kimi-k3.1-pro", "context": float64(256000), "input": float64(500000), "output": float64(2000000)},
					},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("JSON = %#v; want %#v", got, want)
				}
				return
			}
			if mode == "quiet" {
				if out.String() != text {
					t.Fatalf("nonempty quiet output differs from text:\n%s", out.String())
				}
				return
			}
			text = out.String()
			lines := strings.Split(strings.TrimSpace(text), "\n")
			if len(lines) != 5 || !strings.Contains(lines[0], srv.URL) || !strings.Contains(lines[0], digest) {
				t.Fatalf("header/rows: %q", text)
			}
			if got := strings.Join(strings.Fields(lines[1]), " "); got != "KIND PROVIDER MODEL CTX INPUT/OUTPUT" {
				t.Fatalf("columns = %q", got)
			}
			if got := strings.Join(strings.Fields(lines[2]), " "); got != "new_upstream ollama-cloud alpha 100000 150000/600000" {
				t.Fatalf("row = %q", got)
			}
		})
	}
}

func TestCatalogDiffEmpty(t *testing.T) {
	raw := catalogFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
	defer srv.Close()
	for _, mode := range []string{"--quiet", "--json"} {
		var out, errb bytes.Buffer
		code := Catalog([]string{"diff", "--catalog-url=" + srv.URL, "--min-context=300000", mode}, &out, &errb, Deps{Fetch: catalog.Fetch})
		if code != ExitOK || errb.Len() != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		if mode == "--quiet" && out.Len() != 0 {
			t.Fatalf("quiet wrote %q", out.String())
		}
		if mode == "--json" && !strings.Contains(out.String(), `"deltas":[]`) {
			t.Fatalf("empty deltas must be an array: %s", out.String())
		}
	}
}

func TestCatalogDiffFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		cause  string
	}{
		{"server error", 500, "secret upstream body", "500"},
		{"decode error", 200, "not json", "decode"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			var out, errb bytes.Buffer
			code := Catalog([]string{"diff", "--catalog-url=" + srv.URL}, &out, &errb, Deps{Fetch: catalog.Fetch})
			if code != ExitError || out.Len() != 0 || !strings.Contains(errb.String(), tt.cause) || strings.Contains(errb.String(), "secret upstream body") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
			}
		})
	}
}

func TestCatalogUsage(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		message string
	}{
		{nil, "diff"}, {[]string{"bogus"}, "diff"},
		{[]string{"diff", "--json", "--quiet"}, "--quiet"},
		{[]string{"diff", "--min-context=50000"}, "100000"},
		{[]string{"diff", "--min-context=-1"}, "100000"},
		{[]string{"diff", "--min-context=oops"}, "min-context"},
		{[]string{"diff", "--bogus"}, "bogus"},
		{[]string{"diff", "unexpected"}, "unexpected"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			d := Deps{Fetch: func(context.Context, string) ([]byte, string, error) {
				t.Fatal("usage error fetched catalog")
				return nil, "", nil
			}}
			var out, errb bytes.Buffer
			code := Catalog(tt.args, &out, &errb, d)
			if code != ExitUsage || out.Len() != 0 || !strings.Contains(errb.String(), tt.message) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
			}
		})
	}
}

func TestCatalogDefaults(t *testing.T) {
	raw := catalogFixture(t)
	d := Deps{Fetch: func(_ context.Context, url string) ([]byte, string, error) {
		if url != "https://models.dev/api.json" {
			t.Fatalf("default URL = %q", url)
		}
		return raw, catalog.Digest(raw), nil
	}}
	var out, errb bytes.Buffer
	if code := Catalog([]string{"diff", "--json"}, &out, &errb, d); code != ExitOK || !strings.Contains(out.String(), `"model":"alpha"`) {
		t.Fatalf("default context: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}
