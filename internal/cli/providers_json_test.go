package cli

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/config"
)

// providersJSON mirrors the documented machine-readable shape of
// `mgw providers list -json`.
type providersJSON struct {
	Providers []providerJSON `json:"providers"`
}

type providerJSON struct {
	ID        string     `json:"id"`
	Dialect   string     `json:"dialect"`
	Billing   string     `json:"billing"`
	KeyState  string     `json:"key_state"`
	ToolGrade string     `json:"tool_grade"`
	Probe     *probeJSON `json:"probe"`
}

type probeJSON struct {
	Auth        string `json:"auth"`
	Inference   string `json:"inference"`
	StreamUsage string `json:"stream_usage"`
	ToolCall    string `json:"tool_call"`
}

// decodeProvidersJSON requires stdout to hold exactly one JSON document.
func decodeProvidersJSON(t *testing.T, out string) providersJSON {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var got providersJSON
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries data after the JSON object: %v\n%s", err, out)
	}
	return got
}

func findProvider(t *testing.T, got providersJSON, id string) (providerJSON, int) {
	t.Helper()
	for i, p := range got.Providers {
		if p.ID == id {
			return p, i
		}
	}
	t.Fatalf("provider %s is missing", id)
	return providerJSON{}, 0
}

func TestProvidersListJSONCarriesTheProbeAndGrade(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	cfg, err := config.Load(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	// One provider keeps its probe and grade; the other has no probe at all and
	// no grade recorded.
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == "ollama-cloud" {
			cfg.Providers[i].Probe = nil
			cfg.Providers[i].ToolGrade = ""
		}
	}
	if err := config.Save(o.dir, cfg); err != nil {
		t.Fatal(err)
	}

	code, out, errs := o.run("providers", "list", "-json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(out, "DIALECT") {
		t.Fatalf("JSON mode printed the table:\n%s", out)
	}
	got := decodeProvidersJSON(t, out)
	if len(got.Providers) != len(cfg.Providers) {
		t.Fatalf("%d entries, want %d:\n%s", len(got.Providers), len(cfg.Providers), out)
	}
	for i, p := range cfg.Providers {
		if got.Providers[i].ID != p.ID {
			t.Fatalf("entry %d is %q, want %q: config order not preserved", i, got.Providers[i].ID, p.ID)
		}
		if got.Providers[i].Dialect != string(p.Dialect) {
			t.Fatalf("%s dialect %q, want %q", p.ID, got.Providers[i].Dialect, p.Dialect)
		}
		if got.Providers[i].Billing != p.Billing {
			t.Fatalf("%s billing %q, want %q", p.ID, got.Providers[i].Billing, p.Billing)
		}
		if got.Providers[i].KeyState != keyPresent {
			t.Fatalf("%s key_state %q, want %q", p.ID, got.Providers[i].KeyState, keyPresent)
		}
	}

	openai, _ := findProvider(t, got, "openai")
	if openai.Dialect != "openai" || openai.Billing != "pay_per_token" || openai.ToolGrade != "pass" {
		t.Fatalf("openai entry is wrong: %+v\n%s", openai, out)
	}
	if openai.Probe == nil {
		t.Fatalf("openai probe is null:\n%s", out)
	}
	if openai.Probe.Auth != "ok" || openai.Probe.Inference != "ok" ||
		openai.Probe.StreamUsage != "ok" || openai.Probe.ToolCall != "ok" {
		t.Fatalf("openai probe is wrong: %+v\n%s", openai.Probe, out)
	}

	ollama, _ := findProvider(t, got, "ollama-cloud")
	if ollama.Probe != nil {
		t.Fatalf("ollama-cloud probe is not null: %+v", ollama.Probe)
	}
	if ollama.ToolGrade != "" {
		t.Fatalf("ollama-cloud tool_grade %q, want the empty string", ollama.ToolGrade)
	}
	// R6 and R7 are about the wire form: a null probe and a present, empty grade.
	if !strings.Contains(out, `"probe":null`) {
		t.Fatalf("probe is not JSON null:\n%s", out)
	}
	if !strings.Contains(out, `"tool_grade":""`) {
		t.Fatalf("empty tool_grade is not carried:\n%s", out)
	}

	// Without the flag the table is what it always was.
	code, table, errs := o.run("providers", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.HasPrefix(table, "ID") || !strings.Contains(table, "PROBE") || strings.Contains(table, "{") {
		t.Fatalf("table output changed:\n%s", table)
	}
	if !strings.Contains(table, "grade=pass") || !strings.Contains(table, "ollama-cloud") {
		t.Fatalf("table lost a row or its grade:\n%s", table)
	}
}

func TestProvidersListJSONKeepsAProbeWithoutAGrade(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	cfg, err := config.Load(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Providers {
		cfg.Providers[i].ToolGrade = ""
	}
	if err := config.Save(o.dir, cfg); err != nil {
		t.Fatal(err)
	}
	code, out, errs := o.run("providers", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	got := decodeProvidersJSON(t, out)
	for _, p := range got.Providers {
		if p.ToolGrade != "" {
			t.Fatalf("%s tool_grade %q, want the empty string", p.ID, p.ToolGrade)
		}
		if p.Probe == nil {
			t.Fatalf("%s lost its probe", p.ID)
		}
	}
	if c := strings.Count(out, `"tool_grade":""`); c != len(cfg.Providers) {
		t.Fatalf("%d empty grades, want %d:\n%s", c, len(cfg.Providers), out)
	}
}

func TestProvidersListJSONMissingConfigFailsLikeTheTable(t *testing.T) {
	o := newOnboarding(t) // no init: there is no config to read.
	code, out, errs := o.run("providers", "list")
	jsonCode, jsonOut, jsonErrs := o.run("providers", "list", "-json")
	if code != ExitError || jsonCode != ExitError {
		t.Fatalf("exit %d and %d, want %d", code, jsonCode, ExitError)
	}
	if out != "" || jsonOut != "" {
		t.Fatalf("failure printed to stdout:\n%s\n%s", out, jsonOut)
	}
	if errs != jsonErrs {
		t.Fatalf("error message changed:\ntable: %s\njson:  %s", errs, jsonErrs)
	}
	if !strings.Contains(errs, "missing_config") {
		t.Fatalf("error does not name missing_config: %s", errs)
	}
}
