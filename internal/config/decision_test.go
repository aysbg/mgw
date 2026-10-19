package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDecisionConfigValidationOnLoad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Decision)
		reason string
	}{
		{"bad backend", func(d *Decision) { d.Backend = "unknown" }, "backend"},
		{"short timeout", func(d *Decision) { d.TimeoutMS = 99 }, "timeout_ms"},
		{"long timeout", func(d *Decision) { d.TimeoutMS = 10001 }, "timeout_ms"},
		{"zero timeout", func(d *Decision) { d.TimeoutMS = 0 }, "timeout_ms"},
		{"zero floor", func(d *Decision) { d.ConfidenceFloor = 0 }, "confidence_floor"},
		{"large floor", func(d *Decision) { d.ConfidenceFloor = 1 }, "confidence_floor"},
		{"zero choice floor", func(d *Decision) { d.ChoiceFloor = 0 }, "choice_floor"},
		{"negative choice floor", func(d *Decision) { d.ChoiceFloor = -0.1 }, "choice_floor"},
		{"large choice floor", func(d *Decision) { d.ChoiceFloor = 1 }, "choice_floor"},
		{"zero margin", func(d *Decision) { d.Margin = 0 }, "margin"},
		{"large margin", func(d *Decision) { d.Margin = 1 }, "margin"},
		{"sum", func(d *Decision) { d.Margin = 0.4 }, "confidence_floor+margin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sample()
			cfg.Decision = DefaultDecision()
			tc.edit(cfg.Decision)
			data, err := cfg.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(ConfigPath(dir), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); code(err) != CodeBadValue || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("expected named %s rejection: %v", tc.reason, err)
			}
		})
	}
}

func TestDecisionConfigDefaultsAbsentAndClone(t *testing.T) {
	cfg := New()
	if cfg.Decision == nil || cfg.Decision.Enabled || cfg.Decision.Model != "clef-flash" || cfg.Decision.KeyFile != "credentials/decision.key" || cfg.Decision.ChoiceFloor != 0.55 || cfg.Decision.ConfidenceFloor != 0.70 {
		t.Fatalf("init defaults: %+v", cfg.Decision)
	}
	clone := cfg.Clone()
	clone.Decision.Enabled = true
	if cfg.Decision.Enabled {
		t.Fatal("clone shares decision configuration")
	}
	for _, block := range []string{"", `,"decision":{"enabled":false}`, `,"decision":null`} {
		var got Config
		if err := json.Unmarshal([]byte(`{"schema":"mgw.config/1"`+block+`}`), &got); err != nil {
			t.Fatal(err)
		}
		if got.Decision != nil && (got.Decision.Enabled || got.Decision.Validate() != nil) {
			t.Fatalf("disabled block: %+v", got.Decision)
		}
		if got.Decision != nil && got.Decision.ChoiceFloor != 0.55 {
			t.Fatalf("omitted choice floor did not default: %+v", got.Decision)
		}
		if block == "" && got.Decision != nil {
			t.Fatal("absent block was enabled or invented")
		}
	}
	for _, timeout := range []int{100, 10000} {
		d := DefaultDecision()
		d.Backend, d.TimeoutMS, d.ConfidenceFloor, d.Margin = "jev", timeout, 0.75, 0.25
		if err := d.Validate(); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
}

func TestDecisionConfigRejectsUnknownFields(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"decision":{"enabled":true,"api_key":"not-allowed"}}`), &cfg); err == nil {
		t.Fatal("unknown decision field accepted")
	}
}
