package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func good() Provider {
	return Provider{
		ID: "openai", Dialect: DialectOpenAI, BaseURL: "https://api.openai.com",
		CredentialFile: "/x", AllowedModels: []string{"m"},
		Timeouts: Timeouts{ConnectMS: 1, TTFBMS: 1, IdleMS: 1, OverallMS: 1},
	}
}

func TestValidate(t *testing.T) {
	mut := func(f func(*Provider)) Provider { p := good(); f(&p); return p }
	cases := []struct {
		name string
		p    Provider
		bad  string
	}{
		{"ok https", good(), ""},
		{"ok loopback http", mut(func(p *Provider) { p.BaseURL = "http://127.0.0.1:8080" }), ""},
		{"empty id", mut(func(p *Provider) { p.ID = "" }), "empty id"},
		{"bad id charset", mut(func(p *Provider) { p.ID = "Open/AI" }), "id must match"},
		{"unknown dialect", mut(func(p *Provider) { p.Dialect = "gemini" }), "unknown dialect"},
		{"http remote", mut(func(p *Provider) { p.BaseURL = "http://api.example.com" }), "127.0.0.1"},
		{"http localhost name", mut(func(p *Provider) { p.BaseURL = "http://localhost:1" }), "127.0.0.1"},
		{"ftp", mut(func(p *Provider) { p.BaseURL = "ftp://x" }), "https"},
		{"path", mut(func(p *Provider) { p.BaseURL = "https://x.example/v1" }), "scheme and host only"},
		{"userinfo", mut(func(p *Provider) { p.BaseURL = "https://u:p@x.example" }), "scheme and host only"},
		{"no host", mut(func(p *Provider) { p.BaseURL = "https://" }), "no host"},
		{"empty cred", mut(func(p *Provider) { p.CredentialFile = "" }), "credential_file"},
		{"zero connect", mut(func(p *Provider) { p.Timeouts.ConnectMS = 0 }), "timeout"},
		{"zero ttfb", mut(func(p *Provider) { p.Timeouts.TTFBMS = 0 }), "timeout"},
		{"zero idle", mut(func(p *Provider) { p.Timeouts.IdleMS = 0 }), "timeout"},
		{"zero overall", mut(func(p *Provider) { p.Timeouts.OverallMS = 0 }), "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]Provider{tc.p})
			switch {
			case tc.bad == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.bad != "" && (err == nil || !strings.Contains(err.Error(), tc.bad)):
				t.Fatalf("want error containing %q, got %v", tc.bad, err)
			}
		})
	}
}

func TestValidateDuplicate(t *testing.T) {
	err := Validate([]Provider{good(), good()})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestCredential(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := good()

	p.CredentialFile = write("ok", "sk-secret\n", 0o600)
	if got, err := p.Credential(); err != nil || got != "sk-secret" {
		t.Fatalf("got %q, %v", got, err)
	}
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"open perms": {"sk-secret\n", 0o644},
		"two lines":  {"a\nb\n", 0o600},
		"empty":      {"\n", 0o600},
	} {
		p.CredentialFile = write(strings.ReplaceAll(name, " ", "_"), tc.content, tc.mode)
		got, err := p.Credential()
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if got != "" || strings.Contains(err.Error(), "sk-secret") {
			t.Fatalf("%s: credential leaked", name)
		}
	}
}

func TestStreamUsageDefaultsFollowDialect(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		dialect Dialect
		setting *bool
		want    bool
	}{
		{DialectOpenAI, nil, true},
		{DialectOpenRouter, nil, true},
		{DialectOllama, nil, true},
		{DialectAnthropic, nil, false},
		{DialectOpenAI, &no, false},
		{DialectOpenRouter, &no, false},
		{DialectOllama, &no, false},
		{DialectAnthropic, &yes, true},
	}
	for _, tc := range cases {
		p := good()
		p.Dialect, p.IncludeStreamUsage = tc.dialect, tc.setting
		if got := p.StreamUsage(); got != tc.want {
			set := "unset"
			if tc.setting != nil {
				set = map[bool]string{true: "true", false: "false"}[*tc.setting]
			}
			t.Errorf("dialect %s, include_stream_usage %s: StreamUsage() = %v, want %v", tc.dialect, set, got, tc.want)
		}
	}
}

func TestIncludeStreamUsageDecodesFromConfig(t *testing.T) {
	var ps []Provider
	raw := `[{"id":"a","dialect":"openai"},{"id":"b","dialect":"openai","include_stream_usage":false},{"id":"c","dialect":"anthropic","include_stream_usage":true}]`
	if err := json.Unmarshal([]byte(raw), &ps); err != nil {
		t.Fatal(err)
	}
	if ps[0].IncludeStreamUsage != nil || !ps[0].StreamUsage() {
		t.Error("absent setting must stay nil and resolve to the dialect default")
	}
	if ps[1].IncludeStreamUsage == nil || ps[1].StreamUsage() {
		t.Error("explicit false must disable injection")
	}
	if ps[2].IncludeStreamUsage == nil || !ps[2].StreamUsage() {
		t.Error("explicit true must enable injection")
	}
}

func TestValidateAcceptsTheFourDialectsAndRejectsOthers(t *testing.T) {
	for _, d := range []Dialect{DialectOpenAI, DialectOpenRouter, DialectOllama, DialectAnthropic} {
		p := good()
		p.Dialect = d
		if err := p.Validate(); err != nil {
			t.Errorf("dialect %q rejected: %v", d, err)
		}
	}
	for _, d := range []Dialect{"", "OpenAI", "gemini", "openai-compatible"} {
		p := good()
		p.Dialect = d
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "unknown dialect") {
			t.Errorf("dialect %q accepted, err=%v", d, err)
		}
	}
}
