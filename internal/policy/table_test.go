package policy

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

var allProviders = []string{"ollama-cloud", "anthropic", "openai"}

func TestGridSizeAndKeyFormat(t *testing.T) {
	r := buildFixture(t, allProviders)
	if len(r.DecisionTable) != 630 {
		t.Fatalf("cells = %d, want 630", len(r.DecisionTable))
	}
	if got := cell("build", "feature", "M", false, "balanced"); got != "build|feature|M|false|balanced" {
		t.Fatalf("key = %q", got)
	}
	for k, v := range r.DecisionTable {
		if v == nil {
			t.Fatalf("%s: nil candidate list (must be empty slice)", k)
		}
		if len(v) > 4 {
			t.Fatalf("%s: %d candidates", k, len(v))
		}
		if _, err := ParseCell(k); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestMultiProviderRules(t *testing.T) {
	r := buildFixture(t, allProviders)
	cases := []struct {
		name string
		key  string
		want []string
	}{
		// Preference first, and the minimum rank drops flash (build 25 < 26).
		{"cheapest build keeps the preferred provider", cell("build", "feature", "M", false, "cheapest"),
			[]string{"deepseek-v4.1-flash:1017", "gpt-6-luna"}},
		// Capability first: the higher-ranked seat on another provider wins.
		{"balanced build reaches for rank", cell("build", "feature", "M", false, "balanced"),
			[]string{"gpt-6.1-sol", "claude-sonnet-5-5"}},
		{"balanced plan", cell("plan", "feature", "M", false, "balanced"),
			[]string{"gpt-6.1-sol", "claude-sonnet-5-5", "kimi-k3"}},
		{"balanced review uses its floor", cell("review", "docs", "S", false, "balanced"),
			[]string{"claude-sonnet-5-5", "gpt-6.1-sol", "kimi-k3"}},
		{"balanced advise uses its floor", cell("advise", "docs", "S", false, "balanced"),
			[]string{"claude-opus-5-5", "gpt-6-astra"}},
		{"best build ignores tier, caps at four", cell("build", "docs", "S", false, "best"),
			[]string{"gpt-6-astra", "claude-opus-5-5", "gpt-6.1-sol", "claude-sonnet-5-5"}},
		{"best plan tie goes to preference", cell("plan", "docs", "S", false, "best"),
			[]string{"claude-opus-5-5", "gpt-6-astra"}},
		{"cheapest explore: preference then price", cell("explore", "docs", "S", false, "cheapest"),
			[]string{"glm-5.3-flash", "deepseek-v4.1-flash:1017", "gpt-6-luna"}},
		{"balanced explore: rank first", cell("explore", "docs", "S", false, "balanced"),
			[]string{"deepseek-v4.1-flash:1017", "glm-5.3-flash", "gpt-6-luna"}},
		{"ladder: docs/S is cheap, no minimum rank", cell("build", "docs", "S", false, "balanced"),
			[]string{"gpt-6-luna", "deepseek-v4.1-flash:1017", "glm-5.3-flash"}},
		{"ladder: bugfix/L climbs one step", cell("build", "bugfix", "L", false, "balanced"),
			[]string{"gpt-6.1-sol", "claude-sonnet-5-5"}},
		{"ladder: migration/L is capped at top", cell("build", "migration", "L", false, "balanced"),
			[]string{"gpt-6-astra", "claude-opus-5-5"}},
		{"ladder: scaffold/S is good", cell("build", "scaffold", "S", false, "balanced"),
			[]string{"glm-5.3", "claude-haiku-5-5"}},
		{"risk forces top for build", cell("build", "docs", "S", true, "balanced"),
			[]string{"gpt-6-astra", "claude-opus-5-5"}},
		{"risk forces top for plan under cheapest", cell("plan", "docs", "S", true, "cheapest"),
			[]string{"claude-opus-5-5", "gpt-6-astra"}},
		{"risk does not move explore", cell("explore", "docs", "S", true, "cheapest"),
			[]string{"glm-5.3-flash", "deepseek-v4.1-flash:1017", "gpt-6-luna"}},
		{"cheapest plan stays at good", cell("plan", "migration", "L", false, "cheapest"),
			[]string{"glm-5.3"}}, // haiku plan 38 is below the plan minimum of 50
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shorts(r.DecisionTable[c.key])
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s\n got  %v\n want %v", c.key, got, c.want)
			}
		})
	}
}

func TestCheapestMinimumRankDropsBelowFloor(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	got := shorts(r.DecisionTable[cell("build", "feature", "M", false, "cheapest")])
	if slices.Contains(got, "glm-5.3-flash") {
		t.Fatalf("glm-5.3-flash (build 25) must be below the build minimum of 26: %v", got)
	}
	// Explore has no floor: both clear it and price decides.
	got = shorts(r.DecisionTable[cell("explore", "feature", "M", false, "cheapest")])
	if !slices.Equal(got, []string{"glm-5.3-flash", "deepseek-v4.1-flash:1017"}) {
		t.Fatalf("explore: %v", got)
	}
}

func TestNearestTierPrefersHigher(t *testing.T) {
	// Balanced build/feature targets very_good; neither seat is there. Both
	// neighbours are one step away, so the higher tier wins.
	r := handRegistry(
		seat("p/up", "top", 70, 70, 70, 70, 0, 100),
		seat("p/down", "good", 99, 99, 99, 99, 0, 100),
	)
	got := r.DecisionTable[cell("build", "feature", "M", false, "balanced")]
	if !slices.Equal(got, []string{"p/up"}) {
		t.Fatalf("got %v, want [p/up]", got)
	}
	// Two steps down beats nothing: only a lower tier exists.
	r = handRegistry(seat("p/low", "cheap", 70, 70, 70, 70, 0, 100))
	if got := r.DecisionTable[cell("advise", "feature", "M", false, "balanced")]; !slices.Equal(got, []string{"p/low"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCandidateListIsNotPaddedAcrossTiers(t *testing.T) {
	r := handRegistry(
		seat("p/a", "very_good", 80, 80, 80, 80, 0, 100),
		seat("p/b", "top", 99, 99, 99, 99, 0, 100),
	)
	if got := r.DecisionTable[cell("build", "feature", "M", false, "balanced")]; !slices.Equal(got, []string{"p/a"}) {
		t.Fatalf("got %v, want only the target tier", got)
	}
}

func TestTieBreakOrder(t *testing.T) {
	// Equal rank and provider: cheaper price first, then id.
	r := handRegistry(
		seat("p/c", "good", 50, 60, 50, 50, 10, 300),
		seat("p/a", "good", 50, 60, 50, 50, 10, 200),
		seat("p/b", "good", 50, 60, 50, 50, 10, 200),
	)
	got := r.DecisionTable[cell("build", "scaffold", "S", false, "balanced")]
	if !slices.Equal(got, []string{"p/a", "p/b", "p/c"}) {
		t.Fatalf("got %v", got)
	}
	// Cheapest: price before rank.
	r = handRegistry(
		seat("p/hi", "cheap", 90, 60, 50, 50, 10, 300),
		seat("p/lo", "cheap", 40, 60, 50, 50, 10, 100),
	)
	got = r.DecisionTable[cell("build", "docs", "S", false, "cheapest")]
	if !slices.Equal(got, []string{"p/lo", "p/hi"}) {
		t.Fatalf("cheapest got %v", got)
	}
	// Balanced: rank before price.
	got = r.DecisionTable[cell("build", "docs", "S", false, "balanced")]
	if !slices.Equal(got, []string{"p/hi", "p/lo"}) {
		t.Fatalf("balanced got %v", got)
	}
}

func TestEmptyCellsAllowedAndNamed(t *testing.T) {
	r := handRegistry(seat("p/only", "top", 90, 90, 90, 90, 0, 100))
	if got := r.DecisionTable[cell("explore", "docs", "S", false, "balanced")]; got == nil || len(got) != 0 {
		t.Fatalf("explore cell = %#v, want empty non-nil slice", got)
	}
	rep := Check(r)
	if len(rep.Empty) != 7*3*2*3 {
		t.Fatalf("empty cells = %d, want 126", len(rep.Empty))
	}
	for _, k := range rep.Empty {
		if !strings.HasPrefix(k, "explore|") {
			t.Fatalf("unexpected empty cell %s", k)
		}
	}
	if !slices.IsSorted(rep.Empty) {
		t.Fatal("empty cells not sorted")
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("explore under balanced is not a hard failure: %v", rep.Failures)
	}
	if rep.Failed() {
		t.Fatal("Failed() must be false")
	}
}

func TestRiskEffortRung(t *testing.T) {
	r := buildFixture(t, allProviders)
	re := r.RiskEfforts[cell("build", "docs", "S", true, "balanced")]
	if re["openai/gpt-6-astra"] != "xhigh" || re["anthropic/claude-opus-5-5"] != "xhigh" {
		t.Fatalf("risk efforts: %v", re)
	}
	if _, ok := r.RiskEfforts[cell("build", "docs", "S", false, "balanced")]; ok {
		t.Fatal("risk efforts recorded for a non-risk cell")
	}
	// Seat that declares only low and high: clamped to high.
	o := buildFixture(t, []string{"ollama-cloud"})
	re = o.RiskEfforts[cell("plan", "feature", "M", true, "balanced")]
	if re["ollama-cloud/kimi-k3"] != "high" {
		t.Fatalf("kimi rung: %v", re)
	}
	for _, c := range []struct {
		efforts, prohibited []string
		want                string
	}{
		{[]string{"low", "medium", "high", "xhigh"}, nil, "xhigh"},
		{[]string{"low", "medium", "high", "xhigh"}, []string{"xhigh"}, "high"},
		{[]string{"low", "high"}, nil, "high"},
		{[]string{"low", "medium"}, nil, ""},
		{[]string{"low"}, nil, ""},
		{nil, nil, ""},
		{[]string{"turbo"}, nil, ""},
	} {
		if got := riskRung(Model{Efforts: c.efforts, Prohibited: c.prohibited}); got != c.want {
			t.Errorf("riskRung(%v,%v) = %q want %q", c.efforts, c.prohibited, got, c.want)
		}
	}
}

func TestGate08_RiskEffortOnlyForBuildAndPlan(t *testing.T) {
	for _, providers := range [][]string{allProviders, {"ollama-cloud"}} {
		r := buildFixture(t, providers)
		byID := map[string]Model{}
		for _, m := range r.Models {
			byID[m.ID] = m
		}
		for _, role := range Roles {
			for _, typ := range Types {
				for _, size := range Sizes {
					for _, strategy := range Strategies {
						off := cell(role, typ, size, false, strategy)
						on := cell(role, typ, size, true, strategy)
						switch role {
						case "build", "plan":
							for _, id := range r.DecisionTable[on] {
								m := byID[id]
								want := ""
								for i := len(effortLadder) - 1; i >= slices.Index(effortLadder, "high"); i-- {
									if slices.Contains(m.Efforts, effortLadder[i]) && !slices.Contains(m.Prohibited, effortLadder[i]) {
										want = effortLadder[i]
										break
									}
								}
								if got := r.RiskEfforts[on][id]; got != want {
									t.Errorf("%s %s: risk rung %q want %q", on, id, got, want)
								}
							}
							if len(r.RiskEfforts[off]) != 0 {
								t.Errorf("%s: risk efforts on a non-risk cell: %v", off, r.RiskEfforts[off])
							}
						default:
							if len(r.RiskEfforts[on]) != 0 {
								t.Errorf("%s: risk=true must not set an effort rung: %v", on, r.RiskEfforts[on])
							}
							for _, id := range r.DecisionTable[on] {
								if r.RiskEfforts[on][id] != r.RiskEfforts[off][id] {
									t.Errorf("%s %s: effort %q differs from risk=false %q", on, id, r.RiskEfforts[on][id], r.RiskEfforts[off][id])
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestDenyListRemovesSeats(t *testing.T) {
	r := buildFixture(t, allProviders, "openai", "anthropic/claude-opus-5-5")
	for k, v := range r.DecisionTable {
		for _, id := range v {
			if strings.HasPrefix(id, "openai/") || id == "anthropic/claude-opus-5-5" {
				t.Fatalf("%s routes denied seat %s", k, id)
			}
		}
	}
	// Top is empty once opus and openai are denied; the nearest tier is very_good,
	// where kimi (advise 70) outranks sonnet (65) despite the lower preference.
	if got := r.DecisionTable[cell("advise", "docs", "S", false, "balanced")]; !slices.Equal(got, []string{"ollama-cloud/kimi-k3", "anthropic/claude-sonnet-5-5"}) {
		t.Fatalf("advise fell back to %v", got)
	}
}

func TestExplainMatchesTable(t *testing.T) {
	for _, set := range [][]string{{"ollama-cloud"}, allProviders} {
		r := buildFixture(t, set)
		for k, want := range r.DecisionTable {
			ex, err := Explain(r, k)
			if err != nil {
				t.Fatalf("%s: %v", k, err)
			}
			if !slices.Equal(ex.Candidates, want) {
				t.Fatalf("%s: explain %v != table %v", k, ex.Candidates, want)
			}
		}
	}
	r := buildFixture(t, []string{"ollama-cloud"})
	ex, err := Explain(r, cell("build", "feature", "M", false, "cheapest"))
	if err != nil {
		t.Fatal(err)
	}
	if ex.TargetTier != "cheap" || ex.UsedTier != "cheap" || ex.Rule == "" {
		t.Fatalf("header: %+v", ex)
	}
	status := map[string]string{}
	for _, row := range ex.Rows {
		status[short(row.ID)] = row.Status
	}
	if !strings.Contains(status["glm-5.3-flash"], "below cheapest_min_rank") {
		t.Fatalf("flash: %q", status["glm-5.3-flash"])
	}
	if !strings.HasPrefix(status["deepseek-v4.1-flash:1017"], "selected #1") {
		t.Fatalf("deepseek: %q", status["deepseek-v4.1-flash:1017"])
	}
	if !strings.Contains(status["kimi-k3"], "not allowed") {
		t.Fatalf("kimi: %q", status["kimi-k3"])
	}
	if !strings.Contains(status["glm-5.3"], "tier good") {
		t.Fatalf("glm: %q", status["glm-5.3"])
	}
	if _, err := Explain(r, "bogus"); !errors.Is(err, ErrBadCell) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCellRejectsHostileKeys(t *testing.T) {
	for _, k := range []string{
		"", "|", "||||", "build|feature|M|false", "build|feature|M|false|balanced|x", "Build|feature|M|false|balanced",
		"build|feature|m|false|balanced", "build|feature|M|False|balanced", "build|feature|M|1|balanced",
		"build|feature|M|false|balanced\n", " build|feature|M|false|balanced", "build|feature|M|false|best ", "build|nope|M|false|best",
		"\x00|\x00|\x00|\x00|\x00", strings.Repeat("a|", 100000),
	} {
		if _, err := ParseCell(k); !errors.Is(err, ErrBadCell) {
			t.Errorf("%.30q: err = %v, want ErrBadCell", k, err)
		}
	}
	c, err := ParseCell("build|feature|M|true|best")
	if err != nil || !c.Risk || c.Key() != "build|feature|M|true|best" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLookup(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	got, err := Lookup(r, "build|feature|M|false|balanced")
	if err != nil || !slices.Equal(shorts(got), []string{"glm-5.3"}) {
		t.Fatalf("%v %v", got, err)
	}
	got[0] = "mutated"
	if again, _ := Lookup(r, "build|feature|M|false|balanced"); again[0] == "mutated" {
		t.Fatal("Lookup returned the table's own slice")
	}
	if _, err := Lookup(r, "build|feature|M|maybe|balanced"); !errors.Is(err, ErrBadCell) {
		t.Fatalf("err = %v", err)
	}
	delete(r.DecisionTable, "build|feature|M|false|balanced")
	if _, err := Lookup(r, "build|feature|M|false|balanced"); !errors.Is(err, ErrCellMissing) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzTableLookup(f *testing.F) {
	r := buildFixture(f, allProviders)
	for _, k := range []string{"build|feature|M|false|balanced", "", "|||||", "explore|docs|S|true|cheapest", "build|feature|M|false|balanced|", "\xff|\xfe", "a|b|c|d|e"} {
		f.Add(k)
	}
	f.Fuzz(func(t *testing.T, key string) {
		got, err := Lookup(r, key)
		if err != nil {
			if !errors.Is(err, ErrBadCell) && !errors.Is(err, ErrCellMissing) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		c, perr := ParseCell(key)
		if perr != nil || c.Key() != key {
			t.Fatalf("lookup accepted a non-canonical key %q", key)
		}
		if len(got) > r.TieRules.MaxCandidates {
			t.Fatalf("%q: %d candidates", key, len(got))
		}
		if _, err := Explain(r, key); err != nil {
			t.Fatalf("explain failed for a valid key %q: %v", key, err)
		}
	})
}
