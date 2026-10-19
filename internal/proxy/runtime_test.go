package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRuntime(t *testing.T, v map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(v)
	p := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodRuntime() map[string]any {
	return map[string]any{
		"authority_file": "authority.json", "ledger_dir": "ledger", "audit_file": "audit.jsonl", "ready_file": "ready.json",
		"cleanup_timeout_ms": 1000,
		"providers": []map[string]any{{
			"id": "openai", "dialect": "openai", "base_url": "https://api.openai.com", "credential_file": "cred",
			"allowed_models": []string{"gpt-6-astra"},
			"timeouts":       map[string]int{"connect_ms": 1, "ttfb_ms": 1, "idle_ms": 1, "overall_ms": 1},
		}},
	}
}

func TestLoadRuntime(t *testing.T) {
	path := writeRuntime(t, goodRuntime())
	rt, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if rt.ReadyFile != filepath.Join(dir, "ready.json") || rt.Providers[0].CredentialFile != filepath.Join(dir, "cred") {
		t.Fatalf("relative paths not resolved against the runtime file: %+v", rt)
	}
	if rt.MaxReservationTokens != 8000 {
		t.Fatalf("default reservation ceiling: %d", rt.MaxReservationTokens)
	}
	for name, mod := range map[string]func(map[string]any){
		"unknown field":    func(m map[string]any) { m["env"] = "X" },
		"no authority":     func(m map[string]any) { delete(m, "authority_file") },
		"no ready file":    func(m map[string]any) { delete(m, "ready_file") },
		"zero cleanup":     func(m map[string]any) { m["cleanup_timeout_ms"] = 0 },
		"zero ceiling":     func(m map[string]any) { m["max_reservation_tokens"] = 0 },
		"negative ceiling": func(m map[string]any) { m["max_reservation_tokens"] = -1 },
		"bad provider":     func(m map[string]any) { m["providers"].([]map[string]any)[0]["dialect"] = "gemini" },
		"zero timeout":     func(m map[string]any) { m["providers"].([]map[string]any)[0]["timeouts"] = map[string]int{} },
	} {
		m := goodRuntime()
		mod(m)
		if _, err := LoadRuntime(writeRuntime(t, m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := LoadRuntime(filepath.Join(t.TempDir(), "nope")); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRuntimeReservationCeiling(t *testing.T) {
	m := goodRuntime()
	m["max_reservation_tokens"] = 1234
	rt, err := LoadRuntime(writeRuntime(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if rt.MaxReservationTokens != 1234 {
		t.Fatalf("configured reservation ceiling: %d", rt.MaxReservationTokens)
	}
}
