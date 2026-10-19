package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/catalog"
)

func sample() *Config {
	return &Config{
		Schema: Schema,
		Providers: []Provider{
			{ID: "openai", Dialect: "openai", BaseURL: "https://api.openai.com", Billing: "pay_per_token", IncludeStreamUsage: true},
			{ID: "ollama-cloud", Dialect: "ollama", BaseURL: "https://ollama.com", Billing: "included_credits", IncludeStreamUsage: true},
		},
		Strategy:       "balanced",
		ProviderPolicy: ProviderPolicy{Order: []string{"ollama-cloud", "openai"}, Deny: []string{}},
		DataClass:      "internal",
		Profile:        "default",
	}
}

func code(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return ""
}

func TestSaveLoadRoundTripAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mgw")
	if err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v err %v", st, err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != "balanced" || len(got.Providers) != 2 || got.Providers[1].ID != "ollama-cloud" {
		t.Fatalf("%+v", got)
	}
}

func TestLoadMissingConfigIsNamedFailure(t *testing.T) {
	_, err := Load(t.TempDir())
	if code(err) != CodeMissingConfig {
		t.Fatalf("code %q err %v", code(err), err)
	}
	if !strings.Contains(err.Error(), "mgw init") {
		t.Fatalf("no remedy in %v", err)
	}
}

func TestLoadRejectsUnknownFieldsBadSchemaAndValues(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, doc := range map[string]string{
		"unknown field": `{"schema":"mgw.config/1","providers":[],"strategy":"balanced","provider_policy":{"order":[],"deny":[]},"data_class":"internal","profile":"default","api_key":"x"}`,
		"bad schema":    `{"schema":"mgw.config/9","providers":[],"strategy":"balanced","provider_policy":{"order":[],"deny":[]},"data_class":"internal","profile":"default"}`,
		"bad strategy":  `{"schema":"mgw.config/1","providers":[],"strategy":"fastest","provider_policy":{"order":[],"deny":[]},"data_class":"internal","profile":"default"}`,
		"trailing":      `{"schema":"mgw.config/1"} {}`,
	} {
		write(doc)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateNamedFailures(t *testing.T) {
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"unknown strategy": {func(c *Config) { c.Strategy = "fastest" }, CodeUnknownStrategy},
		"bad billing":      {func(c *Config) { c.Providers[0].Billing = "free" }, CodeBadValue},
		"bad data class":   {func(c *Config) { c.DataClass = "public" }, CodeBadValue},
		"duplicate":        {func(c *Config) { c.Providers = append(c.Providers, c.Providers[0]) }, CodeBadValue},
		"order unknown":    {func(c *Config) { c.ProviderPolicy.Order = []string{"nowhere"} }, CodeUnknownProvider},
		"order and deny":   {func(c *Config) { c.ProviderPolicy.Deny = []string{"openai"} }, CodeConflictingPolicy},
		"bad base url":     {func(c *Config) { c.Providers[0].BaseURL = "http://example.com" }, CodeBadValue},
		"bad grade":        {func(c *Config) { c.Providers[0].ToolGrade = "maybe" }, CodeBadValue},
		"bad id":           {func(c *Config) { c.Providers[0].ID = "../x" }, CodeBadValue},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := sample()
			c.edit(cfg)
			if got := code(cfg.Validate()); got != c.want {
				t.Fatalf("code %q, want %q: %v", got, c.want, cfg.Validate())
			}
		})
	}
	if err := sample().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialIsWrittenAt0600AndNeverInConfig(t *testing.T) {
	dir := t.TempDir()
	const key = "sk-test-SECRET-value"
	if err := WriteCredential(dir, "openai", key); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "credentials", "openai.key")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "credentials")); st.Mode().Perm() != 0o700 {
		t.Fatalf("credentials dir mode %v", st.Mode().Perm())
	}
	got, err := ReadCredential(dir, "openai")
	if err != nil || got != key {
		t.Fatalf("read back: %q %v", got, err)
	}
	if err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(cfg), key) {
		t.Fatal("key value found in config.json")
	}
	// A credential file that grants group or other access is refused, and the
	// refusal does not print the value.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredential(dir, "openai"); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("0644 credential accepted or leaked: %v", err)
	}
	if err := WriteCredential(dir, "../evil", key); err == nil {
		t.Fatal("path traversal id accepted")
	}
	// Rewriting restores the mode even when the old file was loose.
	if err := WriteCredential(dir, "openai", key); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("rewrite left mode %v", st.Mode().Perm())
	}
}

func TestCheckDenyAndPins(t *testing.T) {
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	cfg := sample()
	if err := CheckDeny([]string{"openai", "ollama-cloud/glm-5.3"}, caps); err != nil {
		t.Fatal(err)
	}
	if code(CheckDeny([]string{"nowhere"}, caps)) != CodeUnknownProvider {
		t.Fatal("unknown deny entry accepted")
	}
	cfg.Providers[0].ToolGrade = "fail"
	cases := []struct {
		name        string
		role, model string
		deny        []string
		want        string
	}{
		{"ok", "build", "openai/gpt-6.1-sol", nil, ""},
		{"unknown role", "janitor", "openai/gpt-6.1-sol", nil, CodeUnknownRole},
		{"unknown provider", "build", "nowhere/model", nil, CodeUnknownProvider},
		{"unconfigured provider", "build", "anthropic/claude-opus-5-5", nil, CodeUnknownProvider},
		{"unknown model", "plan", "openai/nope", nil, CodeUnknownModel},
		{"rank zero", "build", "ollama-cloud/kimi-k3", nil, CodePinNotAdmitted},
		{"role not allowed", "explore", "openai/gpt-6.1-sol", nil, CodePinNotAdmitted},
		{"denied", "plan", "openai/gpt-6.1-sol", []string{"openai"}, CodePinNotAdmitted},
		{"malformed", "plan", "gpt", nil, CodeBadValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckPin(c.role, c.model, cfg.Providers, caps, c.deny)
			// openai carries tool_grade fail in this config: build is refused.
			want := c.want
			if c.name == "ok" {
				want = CodePinNotAdmitted
			}
			if code(err) != want {
				t.Fatalf("code %q want %q: %v", code(err), want, err)
			}
		})
	}
	cfg.Providers[0].ToolGrade = "pass"
	if err := CheckPin("build", "openai/gpt-6.1-sol", cfg.Providers, caps, nil); err != nil {
		t.Fatal(err)
	}
	// Tool grade only bars build.
	cfg.Providers[0].ToolGrade = "fail"
	if err := CheckPin("plan", "openai/gpt-6.1-sol", cfg.Providers, caps, nil); err != nil {
		t.Fatalf("plan pin on a tool-grade-fail provider: %v", err)
	}
}
