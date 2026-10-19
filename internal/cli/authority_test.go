package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/proxy"
)

const authorityTestCredential = "secret-provider-value-never-print"

type authorityFixture struct {
	dir, routeFile, out string
	route               map[string]any
	providers           []map[string]any
	deps                Deps
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	root := t.TempDir()
	f := &authorityFixture{
		dir: filepath.Join(root, "config"), routeFile: filepath.Join(root, "route.json"), out: filepath.Join(root, "attempt"),
		route: map[string]any{
			"run_id": "run-123", "attempt": 3, "budget_max_tokens": 200000,
			"primary":        "ollama-cloud/glm-5.3#high",
			"chain":          []string{"ollama-cloud/glm-5.3#high", "openai/gpt-6.1-sol#high", "openai/gpt-other#low", "openai/gpt-6.1-sol#low"},
			"provider_order": []string{"ollama-cloud", "openai", "unused"},
		},
	}
	for _, id := range []string{"ollama-cloud", "openai", "unused"} {
		dialect, baseURL := "openai", "https://api.openai.com"
		if id == "ollama-cloud" {
			dialect, baseURL = "ollama", "https://ollama.com"
		}
		f.providers = append(f.providers, map[string]any{
			"id": id, "dialect": dialect, "base_url": baseURL,
			"allowed_models": []string{"glm-5.3", "gpt-6.1-sol", "gpt-other", "not-in-route"},
			"billing":        "pay_per_token", "include_stream_usage": id != "openai",
		})
		if err := config.WriteCredential(f.dir, id, authorityTestCredential); err != nil {
			t.Fatal(err)
		}
	}
	f.deps = Deps{Getenv: func(key string) string {
		if key == config.DirEnv {
			return f.dir
		}
		return ""
	}}
	return f
}

func (f *authorityFixture) save(t *testing.T) {
	t.Helper()
	for path, value := range map[string]any{
		f.routeFile:              f.route,
		config.ConfigPath(f.dir): map[string]any{"schema": config.Schema, "providers": f.providers, "strategy": "balanced"},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *authorityFixture) run(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--route", f.routeFile, "--out", f.out}, extra...)
	code := Authority(args, &stdout, &stderr, f.deps)
	for _, output := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(output, authorityTestCredential) {
			t.Fatal("credential appeared in command output")
		}
	}
	return code, stdout.String(), stderr.String()
}

func TestAuthorityEmitsRuntimeAndToken(t *testing.T) {
	f := newAuthorityFixture(t)
	// An unselected provider must not require a credential.
	if err := os.Remove(config.CredentialPath(f.dir, "unused")); err != nil {
		t.Fatal(err)
	}
	f.save(t)
	start := time.Now()
	code, stdout, stderr := f.run(t, "--deadline-s", "120")
	if code != ExitOK || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var output authorityOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	if output.Out != f.out || output.Runtime != filepath.Join(f.out, "runtime.json") || output.Authority != filepath.Join(f.out, "authority.json") {
		t.Fatalf("wrong output paths: %+v", output)
	}
	rt, err := proxy.LoadRuntime(output.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := policy.LoadAuthority(output.Authority)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{"ollama-cloud": {"glm-5.3"}, "openai": {"gpt-6.1-sol", "gpt-other"}}
	if !reflect.DeepEqual(auth.Providers, expected) || auth.Budget.MaxTokens != 200000 || auth.RunID != "run-123" || auth.Attempt != 3 || auth.Concurrency != 2 {
		t.Fatal("authority does not match route")
	}
	if auth.PolicySHA256 != strings.Repeat("0", 64) || auth.RegistrySHA256 != strings.Repeat("0", 64) {
		t.Fatal("missing pins must use zero hashes")
	}
	if auth.ExpiresAt.Before(start.Add(120*time.Second)) || auth.ExpiresAt.After(time.Now().Add(240*time.Second)) || !auth.ExpiresAt.Equal(output.ExpiresAt) || auth.ExpiresAt.Location() != time.UTC {
		t.Fatal("incorrect expiry")
	}
	raw, err := base64.RawURLEncoding.DecodeString(auth.Token)
	if err != nil || len(raw) != 32 || len(auth.Token) != 43 {
		t.Fatal("token must contain 32 random bytes as unpadded base64url")
	}
	if len(rt.Providers) != 2 || rt.AuthorityFile != output.Authority || rt.LedgerDir != filepath.Join(f.out, "ledger") || rt.AuditFile != filepath.Join(f.out, "audit.jsonl") || rt.ReadyFile != filepath.Join(f.out, "ready.json") || rt.CleanupTimeoutMS != 5000 || rt.DefaultReserveTokens != 2000 {
		t.Fatal("runtime fields do not match contract")
	}
	runtimeData, err := os.ReadFile(output.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var runtimeFields map[string]any
	if err := json.Unmarshal(runtimeData, &runtimeFields); err != nil {
		t.Fatal(err)
	}
	if runtimeFields["max_reservation_tokens"] != float64(2000) || rt.MaxReservationTokens != 2000 {
		t.Fatal("runtime must explicitly emit the 2000-token reservation ceiling")
	}
	for _, p := range rt.Providers {
		if !reflect.DeepEqual(p.AllowedModels, expected[p.ID]) || !filepath.IsAbs(p.CredentialFile) || p.CredentialFile != config.CredentialPath(f.dir, p.ID) {
			t.Fatal("runtime provider scope or credential path is incorrect")
		}
		if _, err := os.Stat(p.CredentialFile); err != nil {
			t.Fatal(err)
		}
		if p.IncludeStreamUsage == nil || *p.IncludeStreamUsage != (p.ID != "openai") {
			t.Fatal("include_stream_usage was not preserved")
		}
	}
	for path, mode := range map[string]os.FileMode{f.out: 0o700, rt.LedgerDir: 0o700, output.Runtime: 0o600, output.Authority: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong mode for %s: %v", path, err)
		}
	}
	for _, path := range []string{output.Runtime, output.Authority} {
		data, err := os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte(authorityTestCredential)) {
			t.Fatal("credential leaked to emitted files")
		}
	}
	// A second invocation can replace private artifacts but must mint a new token.
	ledgerFile := filepath.Join(rt.LedgerDir, "ledger.json")
	if err := os.WriteFile(ledgerFile, []byte("keep ledger"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.run(t); code != ExitOK {
		t.Fatalf("second emission failed: %s", stderr)
	}
	second, err := policy.LoadAuthority(output.Authority)
	if err != nil || second.Token == auth.Token {
		t.Fatal("token was reused")
	}
	if data, err := os.ReadFile(ledgerFile); err != nil || string(data) != "keep ledger" {
		t.Fatal("existing ledger was changed")
	}
}

func TestAuthorityAllowedModels(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		model   string
		refuse  bool
	}{
		{name: "absent allowlist", model: "glm-5.3"},
		{name: "empty allowlist", allowed: []string{}, model: "glm-5.3"},
		{name: "explicit list excludes model", allowed: []string{"glm-5.3"}, model: "deepseek", refuse: true},
		{name: "explicit list includes model", allowed: []string{"glm-5.3"}, model: "glm-5.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.providers = f.providers[:1]
			delete(f.providers[0], "allowed_models")
			if tt.allowed != nil {
				f.providers[0]["allowed_models"] = tt.allowed
			}
			f.route["primary"] = "ollama-cloud/" + tt.model + "#high"
			delete(f.route, "chain")
			f.save(t)
			code, stdout, stderr := f.run(t)
			if tt.refuse {
				if code != ExitUsage || stdout != "" || !strings.Contains(stderr, "model_not_allowed") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if len(authoritySnapshot(t, f.out)) != 0 {
					t.Fatal("output exists after model refusal")
				}
				return
			}
			if code != ExitOK || stderr != "" || strings.Count(stdout, "\n") != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			rt, err := proxy.LoadRuntime(filepath.Join(f.out, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(rt.Providers) != 1 || rt.Providers[0].ID != "ollama-cloud" || !reflect.DeepEqual(rt.Providers[0].AllowedModels, []string{tt.model}) {
				t.Fatal("runtime provider models do not match route")
			}
		})
	}
}

func TestAuthorityConfigDefaultsAndDeadline(t *testing.T) {
	f := newAuthorityFixture(t)
	delete(f.providers[0], "include_stream_usage")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.FixedZone("local", 3600))
	f.deps.Now = func() time.Time { return now }
	f.save(t)
	if code, _, stderr := f.run(t); code != ExitOK {
		t.Fatalf("failed: %s", stderr)
	}
	rt, err := proxy.LoadRuntime(filepath.Join(f.out, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rt.Providers[0].IncludeStreamUsage == nil || *rt.Providers[0].IncludeStreamUsage {
		t.Fatal("omitted install boolean must remain false")
	}
	auth, err := policy.LoadAuthority(rt.AuthorityFile)
	if err != nil || !auth.ExpiresAt.Equal(now.Add(960*time.Second)) {
		t.Fatal("default deadline or UTC conversion is wrong")
	}
}

func TestAuthorityPinsAndCredentialPaths(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "relative"}[relative], func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.route["policy_sha256"], f.route["registry_sha256"] = strings.Repeat("a", 64), strings.Repeat("b", 64)
			credential := config.CredentialPath(f.dir, "openai")
			if relative {
				credential = filepath.Join("credentials", "openai.key")
			}
			f.providers[1]["credential_file"] = credential
			f.save(t)
			if code, _, stderr := f.run(t); code != ExitOK {
				t.Fatalf("failed: %s", stderr)
			}
			auth, err := policy.LoadAuthority(filepath.Join(f.out, "authority.json"))
			if err != nil || auth.PolicySHA256 != strings.Repeat("a", 64) || auth.RegistrySHA256 != strings.Repeat("b", 64) {
				t.Fatal("caller pins were not preserved")
			}
		})
	}
}

func TestAuthorityRefusalsWriteNothing(t *testing.T) {
	tests := []struct {
		name, code string
		change     func(*testing.T, *authorityFixture)
	}{
		{"unknown provider", "provider_not_configured", func(_ *testing.T, f *authorityFixture) { f.providers = f.providers[:1] }},
		{"missing credential", "credential_missing", func(t *testing.T, f *authorityFixture) {
			if err := os.Remove(config.CredentialPath(f.dir, "openai")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unlisted model", "model_not_allowed", func(_ *testing.T, f *authorityFixture) { f.providers[1]["allowed_models"] = []string{"other"} }},
		{"missing primary", "route_malformed", func(_ *testing.T, f *authorityFixture) { delete(f.route, "primary") }},
		{"missing run", "route_malformed", func(_ *testing.T, f *authorityFixture) { delete(f.route, "run_id") }},
		{"bad reference", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["primary"] = "openai" }},
		{"bad chain", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["chain"] = []string{"openai/#high"} }},
		{"empty effort", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["primary"] = "openai/model#" }},
		{"invalid provider id", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["primary"] = "../model" }},
		{"bad budget", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["budget_max_tokens"] = 0 }},
		{"bad attempt", "route_malformed", func(_ *testing.T, f *authorityFixture) { f.route["attempt"] = 0 }},
		{"unwritable output", "out_not_private", func(t *testing.T, f *authorityFixture) {
			if err := os.Mkdir(f.out, 0o500); err != nil {
				t.Fatal(err)
			}
		}},
		{"public output", "out_not_private", func(t *testing.T, f *authorityFixture) {
			if err := os.Mkdir(f.out, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"output is file", "out_not_private", func(t *testing.T, f *authorityFixture) {
			if err := os.WriteFile(f.out, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"loose runtime", "out_not_private", func(t *testing.T, f *authorityFixture) { authorityExistingFile(t, f, "runtime.json", 0o644) }},
		{"loose authority", "out_not_private", func(t *testing.T, f *authorityFixture) { authorityExistingFile(t, f, "authority.json", 0o644) }},
		{"loose other file", "out_not_private", func(t *testing.T, f *authorityFixture) { authorityExistingFile(t, f, "other", 0o644) }},
		{"target is directory", "out_not_private", func(t *testing.T, f *authorityFixture) {
			if err := os.MkdirAll(filepath.Join(f.out, "runtime.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"target symlink", "out_not_private", func(t *testing.T, f *authorityFixture) {
			if err := os.Mkdir(f.out, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.routeFile, filepath.Join(f.out, "authority.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthorityFixture(t)
			tt.change(t, f)
			f.save(t)
			before := authoritySnapshot(t, f.out)
			code, stdout, stderr := f.run(t)
			if code != ExitUsage || stdout != "" || !strings.Contains(stderr, tt.code) {
				t.Fatalf("code=%d stdout=%q stderr=%q; want %s", code, stdout, stderr, tt.code)
			}
			if !reflect.DeepEqual(before, authoritySnapshot(t, f.out)) {
				t.Fatal("refusal changed output")
			}
		})
	}
}

func authorityExistingFile(t *testing.T, f *authorityFixture, name string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(f.out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.out, name), []byte("unchanged"), mode); err != nil {
		t.Fatal(err)
	}
}

func authoritySnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] += string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAuthorityMalformedDocumentsAndFlags(t *testing.T) {
	for _, doc := range []string{"{", "null", "[]", `{"run_id":"r","attempt":1,"budget_max_tokens":1,"primary":"openai/m"} {}`} {
		t.Run(doc, func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.save(t)
			if err := os.WriteFile(f.routeFile, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := f.run(t); code != ExitUsage || stdout != "" || !strings.Contains(stderr, "route_malformed") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if len(authoritySnapshot(t, f.out)) != 0 {
				t.Fatal("output exists after refusal")
			}
		})
	}
	for _, args := range [][]string{nil, {"--deadline-s", "0"}, {"--deadline-s", "-1"}, {"--deadline-s", "9223372036854775807"}, {"unexpected"}, {"--unknown"}} {
		f := newAuthorityFixture(t)
		f.save(t)
		var stdout, stderr bytes.Buffer
		if code := Authority(args, &stdout, &stderr, f.deps); code != ExitUsage {
			t.Fatalf("args=%v code=%d", args, code)
		}
		if stdout.Len() != 0 || len(authoritySnapshot(t, f.out)) != 0 {
			t.Fatal("invalid flags emitted output")
		}
	}
}
