package decision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowRedactsKeyFromStateAndUpstreamModel(t *testing.T) {
	const key = "decision-SECRET-shadow-fixture"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "decision.key")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{KeyFile: keyPath}
	path := filepath.Join(dir, ShadowFile)
	result := Result{Backend: BackendOllama, DecisionModel: "echo-" + key}
	if err := cfg.AppendShadow(path, "state\n"+key+"\nend", result, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Search the actual file, not just the captured CLI output.
	if strings.Contains(string(data), key) {
		t.Fatal("API key appeared in shadow file")
	}
	var got ShadowRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.StateExcerpt != "state [redacted] end" || got.DecisionModel != "echo-[redacted]" {
		t.Fatalf("secret was not redacted: %+v", got)
	}
}

func TestShadowRejectsSymlinkAndNonRegularFile(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ShadowFile)
			if kind == "symlink" {
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			err := (Config{}).AppendShadow(path, "state", Result{}, time.Unix(0, 0))
			if err == nil || !strings.Contains(err.Error(), "decision_shadow_file") {
				t.Fatalf("accepted %s: %v", kind, err)
			}
		})
	}
}
