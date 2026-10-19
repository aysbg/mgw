package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Distinct, recognisable key values. Every test asserts none of them reaches an
// output stream or config.json.
const (
	keyOpenAI    = "sk-openai-SECRET-1111aaaa"
	keyOllama    = "ollama-SECRET-2222bbbb"
	keyAnthropic = "ant-SECRET-3333cccc"
	keyBad       = "sk-wrong-SECRET-9999zzzz"
)

var allKeys = []string{keyOpenAI, keyOllama, keyAnthropic, keyBad}

// fakeUpstream serves every dialect on one 127.0.0.1 listener. A key is known to
// it by value; anything else is a 401. A key in badTool answers the tool-call
// request without a well-formed call.
type fakeUpstream struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	keys    map[string]string
	badTool map[string]bool
	reqs    int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{t: t, keys: map[string]string{}, badTool: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) allow(key, provider string) { f.keys[key] = provider }

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs++
	f.mu.Unlock()
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	anthropic := r.Header.Get("X-Api-Key") != ""
	if anthropic {
		key = r.Header.Get("X-Api-Key")
	}
	if _, ok := f.keys[key]; !ok {
		// An upstream that echoes the credential it was given: the CLI must
		// still never print a response body.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid key `+key+`"}`)
		return
	}
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"data":[]}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	switch {
	case req["stream"] == true:
		w.Header().Set("Content-Type", "text/event-stream")
		if anthropic {
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	case req["tools"] != nil:
		switch {
		case f.badTool[key]:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"no tool"}}],"content":[{"type":"text","text":"no tool"}]}`)
		case anthropic:
			_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","name":"record_result","input":{"value":7}}]}`)
		default:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"record_result","arguments":"{\"value\":7}"}}]}}]}`)
		}
	default:
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}],"content":[{"type":"text","text":"OK"}]}`)
	}
}

// writeKeyFile writes a key fixture with the given mode and returns its path.
func writeKeyFile(t *testing.T, name, key string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// forbidStdin fails the test if a command reads stdin. A CI run must never hang
// on input.
type forbidStdin struct{ t *testing.T }

func (f forbidStdin) Read([]byte) (int, error) {
	f.t.Helper()
	f.t.Error("command read stdin in non-interactive mode")
	return 0, io.EOF
}

// onboarding bundles one install under a temp MGW_CONFIG_DIR.
type onboarding struct {
	t    *testing.T
	dir  string
	fake *fakeUpstream
	deps Deps
}

func newOnboarding(t *testing.T) *onboarding {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mgw")
	t.Setenv("MGW_CONFIG_DIR", dir)
	d := withCA(t)
	d.Getenv = os.Getenv
	d.Stdin = forbidStdin{t}
	d.StdinIsTTY = func() bool { return false }
	o := &onboarding{t: t, dir: dir, fake: newFakeUpstream(t), deps: d}
	o.fake.allow(keyOpenAI, "openai")
	o.fake.allow(keyOllama, "ollama-cloud")
	o.fake.allow(keyAnthropic, "anthropic")
	return o
}

// run drives the real CLI entry point with the install's deps.
func (o *onboarding) run(args ...string) (int, string, string) {
	o.t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb, o.deps)
	assertNoSecret(o.t, out.String(), errb.String())
	return code, out.String(), errb.String()
}

func (o *onboarding) base() string { return o.fake.srv.URL }

// initArgs is a complete non-interactive init for the two given providers.
func (o *onboarding) initArgs(extra ...string) []string {
	openai := writeKeyFile(o.t, "openai.key", keyOpenAI, 0o600)
	ollama := writeKeyFile(o.t, "ollama.key", keyOllama, 0o600)
	args := []string{"init",
		"--provider", "openai", "--key-file", openai, "--billing", "pay_per_token",
		"--provider", "ollama-cloud", "--key-file", ollama, "--billing", "included_credits",
		"--base-url", "openai=" + o.base(), "--base-url", "ollama-cloud=" + o.base(),
		"--strategy", "balanced", "--prefer", "ollama-cloud,openai", "--data-class", "internal", "--yes"}
	return append(args, extra...)
}

func (o *onboarding) path(parts ...string) string {
	return filepath.Join(append([]string{o.dir}, parts...)...)
}

func (o *onboarding) mustInit() {
	o.t.Helper()
	if code, out, errs := o.run(o.initArgs()...); code != 0 {
		o.t.Fatalf("init exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
}

func assertNoSecret(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, k := range allKeys {
			if strings.Contains(s, k) {
				t.Fatalf("credential value reached an output stream:\n%s", s)
			}
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Fatalf("%s mode %04o, want %04o", path, got, want)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
