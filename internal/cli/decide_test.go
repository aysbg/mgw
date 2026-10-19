package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/decision"
)

const decisionFixtureKey = "decision-SECRET-fixture-8421"

func decisionReply(confidence float64) string {
	return fmt.Sprintf(`{"model":"clef-flash","answers":{
		"change_type":{"type":"choice","choice":"bugfix","confidence":%g,"probabilities":{"bugfix":0.9,"docs":0.1}},
		"size":{"type":"choice","choice":"S","confidence":0.9,"probabilities":{"S":0.9,"M":0.1}},
		"schema_risk":{"type":"noul","probability":0.9},
		"billing_risk":{"type":"noul","probability":0.1},
		"auth_risk":{"type":"noul","probability":0.1},
		"privacy_risk":{"type":"noul","probability":0.1},
		"concurrency_risk":{"type":"noul","probability":0.1},
		"needs_scout":{"type":"noul","probability":0.9}}}`, confidence)
}

type decisionCLI struct {
	dir   string
	state string
	deps  Deps
	cfg   *config.Config
	calls atomic.Int64
}

func newDecisionCLI(t *testing.T, reply, state string) *decisionCLI {
	t.Helper()
	f := &decisionCLI{dir: t.TempDir(), deps: testDeps()}
	f.deps.Getenv = func(string) string { return "" }
	f.deps.Stdin = forbidStdin{t}
	f.state = filepath.Join(t.TempDir(), "state.txt")
	if err := os.WriteFile(f.state, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+decisionFixtureKey {
			t.Error("missing decision credential header")
		}
		var body struct {
			State string `json:"state"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State != state || body.Model == "" {
			t.Errorf("request did not carry file state and model: %v", err)
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	f.cfg = config.New()
	f.cfg.Strategy = "balanced"
	f.cfg.Decision.Enabled = true
	f.cfg.Decision.BaseURL = srv.URL
	if err := config.WriteFileAtomic(filepath.Join(f.dir, "credentials", "decision.key"), []byte(decisionFixtureKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.save(t)
	return f
}

func (f *decisionCLI) save(t *testing.T) {
	t.Helper()
	if err := config.Save(f.dir, f.cfg); err != nil {
		t.Fatal(err)
	}
}

func (f *decisionCLI) run(args ...string) (int, string, string) {
	var out, errs bytes.Buffer
	code := Run(append([]string{"--config", f.dir}, args...), &out, &errs, f.deps)
	return code, out.String(), errs.String()
}

func TestDecideJSONAcceptedAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		confidence float64
		fallback   bool
	}{
		{"accepted", 0.9, false},
		{"fallback", 0.6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionCLI(t, decisionReply(tc.confidence), "Repair a schema bug\nfrom file")
			code, out, errs := f.run("decide", "--state-file", f.state, "--json")
			if code != ExitOK || errs != "" || f.calls.Load() != 1 {
				t.Fatalf("exit %d calls %d\n%s%s", code, f.calls.Load(), out, errs)
			}
			var got decision.Result
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got.Fallback != tc.fallback || got.Backend != "ollama" || got.DecisionModel != "clef-flash" || got.LatencyMS < 0 {
				t.Fatalf("result: %+v", got)
			}
			if tc.fallback {
				if got.FallbackReason != decision.ReasonLowConfidence || got.Type != "" || got.Size != "" || got.Scout || len(got.RiskFlags) != 0 {
					t.Fatalf("fallback changed complexity: %+v", got)
				}
			} else if got.Type != "bugfix" || got.Size != "S" || !got.Scout || got.Confidence != 0.9 || strings.Join(got.RiskFlags, ",") != "schema" {
				t.Fatalf("gate result: %+v", got)
			}
			if strings.Contains(out+errs, decisionFixtureKey) {
				t.Fatal("key reached output")
			}
		})
	}
}

func TestDecideShadowBoundedPrivateAndNoKey(t *testing.T) {
	state := "first\nsecond\r\n" + strings.Repeat("界", 400) + "FULL-STATE-TAIL"
	f := newDecisionCLI(t, decisionReply(0.9), state)
	// A public install directory must not loosen the log's creation mode.
	if err := os.Chmod(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("decide", "--state-file", f.state, "--shadow", "--json")
	if code != ExitOK || out != "shadow: recorded\n" || errs != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	path := filepath.Join(f.dir, decision.ShadowFile)
	assertMode(t, path, 0o600)
	data := readFileT(t, path)
	if strings.Count(data, "\n") != 1 || strings.Contains(data, decisionFixtureKey) || strings.Contains(data, "FULL-STATE-TAIL") {
		t.Fatal("shadow log must have one line, no key, and no full state")
	}
	var got decision.ShadowRecord
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(state))
	wantExcerpt := "first second " + strings.Repeat("界", 287)
	if got.StateExcerpt != wantExcerpt || len([]rune(got.StateExcerpt)) != 300 || got.StateSHA256 != hex.EncodeToString(sum[:]) || got.PolicyVersion != "decision-1.0" || got.TS != fixedNow.Format("2006-01-02T15:04:05Z07:00") {
		t.Fatalf("bad shadow record: %+v", got)
	}
	// A second command appends; it must preserve the first record byte-for-byte.
	if code, _, errs := f.run("decide", "--state-file", f.state, "--shadow"); code != ExitOK {
		t.Fatalf("append: exit %d: %s", code, errs)
	}
	appended := readFileT(t, path)
	if !strings.HasPrefix(appended, data) || strings.Count(appended, "\n") != 2 || strings.Contains(appended, decisionFixtureKey) {
		t.Fatal("shadow append replaced the record or leaked the key")
	}
}

func TestDecideShadowRejectsWorldReadableFile(t *testing.T) {
	f := newDecisionCLI(t, decisionReply(0.9), "small state")
	path := filepath.Join(f.dir, decision.ShadowFile)
	const original = "existing record\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("decide", "--state-file", f.state, "--shadow")
	if code != ExitError || out != "" || !strings.Contains(errs, "decision_shadow_permissions") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	assertMode(t, path, 0o644)
	if readFileT(t, path) != original {
		t.Fatal("refused shadow log was modified")
	}
}

func TestDecideDisabledAndUnconfiguredNeverCallNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*config.Config)
		reason string
	}{
		{"disabled", func(c *config.Config) { c.Decision.Enabled = false }, "decision_disabled"},
		{"absent", func(c *config.Config) { c.Decision = nil }, "decision_unconfigured"},
		{"no key path", func(c *config.Config) { c.Decision.KeyFile = "" }, "decision_unconfigured"},
		{"missing key", func(c *config.Config) { c.Decision.KeyFile = "missing.key" }, "decision_unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionCLI(t, decisionReply(0.9), "state")
			tc.edit(f.cfg)
			f.save(t)
			code, out, errs := f.run("decide", "--state-file", f.state, "--shadow")
			if code != 3 || out != "" || !strings.Contains(errs, tc.reason) || f.calls.Load() != 0 {
				t.Fatalf("exit %d calls %d\n%s%s", code, f.calls.Load(), out, errs)
			}
			if exists(filepath.Join(f.dir, decision.ShadowFile)) {
				t.Fatal("disabled decision wrote a log")
			}
		})
	}
}

func TestDecideUsageRejectsArgvState(t *testing.T) {
	f := newDecisionCLI(t, decisionReply(0.9), "state")
	for _, args := range [][]string{
		{"decide"}, {"decide", "state from argv"},
		{"decide", "--state", "state from argv"},
		{"decide", "--state-file", f.state, "extra state"},
		{"decide", "--state-file", f.state, "--backend", "unknown"},
	} {
		if code, _, _ := f.run(args...); code != ExitUsage {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("usage error called network")
	}
}

func TestDecideFlagsOverrideBackendAndModel(t *testing.T) {
	// An omitted response model causes the client to report the requested model.
	f := newDecisionCLI(t, strings.Replace(decisionReply(0.9), `"model":"clef-flash",`, "", 1), "state")
	code, out, errs := f.run("decide", "--state-file", f.state, "--backend", "jev", "--model", "test-model", "--json")
	if code != ExitOK || !strings.Contains(out, `"backend":"jev"`) || !strings.Contains(out, `"decision_model":"test-model"`) {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

func TestDecidePlainIncludesTelemetryAndFallbackReason(t *testing.T) {
	f := newDecisionCLI(t, decisionReply(0.6), "state")
	code, out, errs := f.run("decide", "--state-file", f.state)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, field := range []string{"type:", "size:", "risk_flags:", "scout:", "confidence:", "backend: ollama", "decision_model: clef-flash", "latency_ms:", "fallback_reason: low_confidence"} {
		if !strings.Contains(out, field) {
			t.Errorf("missing %q in %s", field, out)
		}
	}
}

func TestDecideBadResponseFallbackExitsZero(t *testing.T) {
	f := newDecisionCLI(t, "invalid reply "+decisionFixtureKey, "state")
	code, out, errs := f.run("decide", "--state-file", f.state, "--json")
	var result decision.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || errs != "" || !result.Fallback || result.FallbackReason != decision.ReasonBadResponse {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if strings.Contains(out+errs, decisionFixtureKey) {
		t.Fatal("upstream error leaked the API key")
	}
}

func TestDecideMissingConfigExitsThree(t *testing.T) {
	f := newDecisionCLI(t, decisionReply(0.9), "state")
	if err := os.Remove(config.ConfigPath(f.dir)); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("decide", "--state-file", f.state)
	if code != 3 || out != "" || !strings.Contains(errs, "decision_unconfigured") || f.calls.Load() != 0 {
		t.Fatalf("exit %d calls %d\n%s%s", code, f.calls.Load(), out, errs)
	}
}
