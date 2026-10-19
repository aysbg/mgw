package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/decision"
)

func TestDoctorDecisionStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *decisionCLI)
		want  string
		code  int
	}{
		{"absent", func(_ *testing.T, f *decisionCLI) { f.cfg.Decision = nil }, "ok decision disabled", ExitOK},
		{"disabled", func(_ *testing.T, f *decisionCLI) { f.cfg.Decision.Enabled = false }, "ok decision disabled", ExitOK},
		{"enabled", func(*testing.T, *decisionCLI) {}, "ok decision clef-flash via ollama", ExitOK},
		{"missing", func(_ *testing.T, f *decisionCLI) { f.cfg.Decision.KeyFile = "missing.key" }, "fail decision key file missing:", ExitError},
		{"empty key path", func(_ *testing.T, f *decisionCLI) { f.cfg.Decision.KeyFile = "" }, "fail decision key file missing:", ExitError},
		{"world readable log", func(t *testing.T, f *decisionCLI) {
			path := filepath.Join(f.dir, decision.ShadowFile)
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "warn decision shadow log is world-readable", ExitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionCLI(t, decisionReply(0.9), "state")
			tc.setup(t, f)
			f.save(t)
			code, out, errs := f.run("doctor")
			normalized := strings.Join(strings.Fields(out), " ")
			if code != tc.code || !strings.Contains(normalized, tc.want) || f.calls.Load() != 0 {
				t.Fatalf("exit %d calls %d\n%s%s", code, f.calls.Load(), out, errs)
			}
			if strings.Contains(out+errs, decisionFixtureKey) {
				t.Fatal("doctor leaked key")
			}
		})
	}
}
