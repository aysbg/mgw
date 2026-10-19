package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

func TestResolvePrecedenceThreeSourcesOneWinner(t *testing.T) {
	install := sample()
	install.Strategy = "balanced"
	install.DataClass = "internal"
	install.Pins = map[string]string{"build": "openai/gpt-6.1-sol", "plan": "openai/gpt-6.1-sol"}
	profile := &Profile{
		Strategy:       "best",
		ProviderPolicy: &ProfilePolicy{Deny: []string{"openai"}},
		Pins:           map[string]string{"plan": "ollama-cloud/kimi-k3"},
	}
	flags := Flags{Strategy: strp("cheapest"), Pins: map[string]string{"review": "ollama-cloud/kimi-k3"}}

	e := Resolve(install, profile, flags)
	want := map[string]string{
		"strategy":   SourceFlag,    // flag beats profile beats install
		"order":      SourceInstall, // only the install has an order
		"deny":       SourceProfile, // profile beats install
		"data_class": SourceInstall,
		"pin.build":  SourceInstall,
		"pin.plan":   SourceProfile,
		"pin.review": SourceFlag,
	}
	for k, v := range want {
		if e.Sources[k] != v {
			t.Errorf("source[%s] = %q, want %q", k, e.Sources[k], v)
		}
	}
	if e.Strategy != "cheapest" || e.DataClass != "internal" {
		t.Fatalf("%+v", e)
	}
	if strings.Join(e.Order, ",") != "ollama-cloud,openai" || strings.Join(e.Deny, ",") != "openai" {
		t.Fatalf("order %v deny %v", e.Order, e.Deny)
	}
	if e.Pins["plan"] != "ollama-cloud/kimi-k3" || e.Pins["build"] != "openai/gpt-6.1-sol" || e.Pins["review"] != "ollama-cloud/kimi-k3" {
		t.Fatalf("pins %v", e.Pins)
	}

	line := e.SourceLines()
	for _, w := range []string{"strategy cheapest (dispatch flag)", "order ollama-cloud,openai (install default)", "deny openai (engagement profile)", "data_class internal (install default)", "pin plan=ollama-cloud/kimi-k3 (engagement profile)"} {
		if !strings.Contains(strings.Join(line, "\n"), w) {
			t.Errorf("missing %q in\n%s", w, strings.Join(line, "\n"))
		}
	}

	// No flag: profile wins the strategy. No profile either: install wins.
	if e := Resolve(install, profile, Flags{}); e.Strategy != "best" || e.Sources["strategy"] != SourceProfile {
		t.Fatalf("%+v", e)
	}
	if e := Resolve(install, nil, Flags{}); e.Strategy != "balanced" || e.Sources["strategy"] != SourceInstall {
		t.Fatalf("%+v", e)
	}
	// No install either: built-in default, honestly labelled.
	if e := Resolve(nil, nil, Flags{}); e.Strategy != "balanced" || e.Sources["strategy"] != SourceBuiltin {
		t.Fatalf("%+v", e)
	}
}

func TestProfileRejectsConflictingOrderAndDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engagement.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"strategy":"best","provider_policy":{"order":["openai","ollama-cloud"],"deny":["openai"]}}`)
	_, err := LoadProfile(path)
	if code(err) != CodeConflictingPolicy || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("code %q err %v", code(err), err)
	}
	write(`{"strategy":"best","provider_policy":{"order":["openai"],"deny":["ollama-cloud"]},"data_class":"customer","pins":{"build":"openai/gpt-6.1-sol"}}`)
	p, err := LoadProfile(path)
	if err != nil || p.Strategy != "best" || p.DataClass != "customer" || p.ProviderPolicy.Order[0] != "openai" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, doc := range map[string]string{
		"unknown strategy": `{"strategy":"fastest"}`,
		"unknown field":    `{"strategy":"best","credentials":"x"}`,
		"bad data class":   `{"data_class":"public"}`,
		"unknown role pin": `{"pins":{"janitor":"openai/gpt-6.1-sol"}}`,
		"not json":         `{`,
	} {
		write(doc)
		if _, err := LoadProfile(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadProfile(filepath.Join(t.TempDir(), "missing.json")); code(err) != CodeMissingProfile {
		t.Fatalf("missing profile: %v", err)
	}
}

func TestProfilePathResolution(t *testing.T) {
	dir := t.TempDir()
	cfg := sample()
	if got, _ := ProfilePath(dir, cfg, ""); got != "" {
		t.Fatalf("default profile with no file must resolve to none, got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "profiles", "default.json")
	if err := os.WriteFile(p, []byte(`{"strategy":"best"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ProfilePath(dir, cfg, ""); got != p {
		t.Fatalf("got %q", got)
	}
	cfg.Profile = "acme"
	if _, err := ProfilePath(dir, cfg, ""); code(err) != CodeMissingProfile {
		t.Fatalf("a named profile that does not exist must fail: %v", err)
	}
	if got, _ := ProfilePath(dir, cfg, "/x/e.json"); got != "/x/e.json" {
		t.Fatalf("explicit path: %q", got)
	}
	if got, _ := ProfilePath(dir, cfg, "acme"); got != filepath.Join(dir, "profiles", "acme.json") {
		t.Fatalf("bare name: %q", got)
	}
}
