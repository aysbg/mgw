package catalog_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/policy"
)

// Gate 8, matrix fidelity half: the fixture catalog plus the seed capabilities
// reproduce the CTO's worked example. The table is the acceptance fixture; if a
// change to ranks, prices or rules breaks it, the change is wrong.

func buildInstall(t *testing.T, providers ...string) *policy.Registry {
	t.Helper()
	raw, err := os.ReadFile("testdata/models.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	snap := catalog.NewSnapshot(c, "fixture", catalog.Digest(raw), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), true)
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	var pp []policy.ProviderPolicy
	for _, name := range providers {
		for _, cp := range caps.Providers {
			if cp.ID == name {
				pp = append(pp, policy.ProviderPolicy{ID: cp.ID, Billing: cp.Billing})
			}
		}
	}
	r, err := policy.Build(caps, snap, policy.BuildOptions{Providers: pp, Prices: policy.PricesFixture})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Validate(r); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return r
}

func first(t *testing.T, r *policy.Registry, role, typ, size string, risk bool, strategy string) string {
	t.Helper()
	key := policy.Cell{Role: role, Type: typ, Size: size, Risk: risk, Strategy: strategy}.Key()
	got, err := policy.Lookup(r, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatalf("%s is empty", key)
	}
	_, m, _ := strings.Cut(got[0], "/")
	return m
}

// TestGate08_OllamaOnlyDefaultCell asserts the Gate 8 table cell by cell.
func TestGate08_OllamaOnlyDefaultCell(t *testing.T) {
	r := buildInstall(t, "ollama-cloud")
	want := []struct {
		role                      string
		cheapest, balanced, bestM string
	}{
		{"build", "deepseek-v4.1-flash:1017", "glm-5.3", "glm-5.3"},
		{"plan", "glm-5.3", "kimi-k3", "kimi-k3"},
		{"review", "glm-5.3", "kimi-k3", "kimi-k3"},
		{"advise", "glm-5.3", "kimi-k3", "kimi-k3"},
		{"explore", "glm-5.3-flash", "deepseek-v4.1-flash:1017", "deepseek-v4.1-flash:1017"},
	}
	for _, w := range want {
		for strategy, expect := range map[string]string{"cheapest": w.cheapest, "balanced": w.balanced, "best": w.bestM} {
			t.Run(w.role+"/"+strategy, func(t *testing.T) {
				if got := first(t, r, w.role, "feature", "M", false, strategy); got != expect {
					t.Fatalf("%s %s = %s, want %s", w.role, strategy, got, expect)
				}
			})
		}
	}
}

// TestGate08_BalancedBuildDegradesToGood: the ladder targets very_good, no
// very_good seat may build, so the cell falls to the nearest tier.
func TestGate08_BalancedBuildDegradesToGood(t *testing.T) {
	r := buildInstall(t, "ollama-cloud")
	ex, err := policy.Explain(r, "build|feature|M|false|balanced")
	if err != nil {
		t.Fatal(err)
	}
	if ex.TargetTier != "very_good" || ex.UsedTier != "good" {
		t.Fatalf("target %s used %s, want very_good -> good", ex.TargetTier, ex.UsedTier)
	}
	ex, err = policy.Explain(r, "plan|feature|M|false|balanced")
	if err != nil {
		t.Fatal(err)
	}
	if ex.TargetTier != "very_good" || ex.UsedTier != "very_good" {
		t.Fatalf("plan: target %s used %s", ex.TargetTier, ex.UsedTier)
	}
}

// TestGate08_MultiProviderPreferenceVersusRank: cheapest keeps the work on the
// preferred provider; balanced reaches for the higher-ranked seat elsewhere.
func TestGate08_MultiProviderPreferenceVersusRank(t *testing.T) {
	r := buildInstall(t, "ollama-cloud", "anthropic", "openai")
	if got := first(t, r, "build", "feature", "M", false, "cheapest"); got != "deepseek-v4.1-flash:1017" {
		t.Fatalf("cheapest build = %s", got)
	}
	// gpt-6-luna is cheaper than deepseek and ranks higher for build (30 vs 28),
	// yet the prepaid provider is spent first.
	if got := first(t, r, "build", "feature", "M", false, "balanced"); got != "gpt-6.1-sol" {
		t.Fatalf("balanced build = %s", got)
	}
	if got := first(t, r, "review", "feature", "M", false, "balanced"); got != "claude-sonnet-5-5" {
		t.Fatalf("balanced review = %s", got)
	}
}

// TestGate08_TableShape: 630 cells on every install, at most four candidates.
func TestGate08_TableShape(t *testing.T) {
	for _, set := range [][]string{{"ollama-cloud"}, {"ollama-cloud", "anthropic", "openai"}} {
		r := buildInstall(t, set...)
		if len(r.DecisionTable) != 630 {
			t.Fatalf("%v: cells = %d", set, len(r.DecisionTable))
		}
		if rep := policy.Check(r); rep.Failed() {
			t.Fatalf("%v: %v", set, rep.Failures)
		}
	}
}
