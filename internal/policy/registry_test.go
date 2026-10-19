package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/parity"
)

func TestBuildOllamaOnly(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	if r.Version != "2.0" || r.PolicyVersion != "strategy-1.0" {
		t.Fatalf("versions: %s %s", r.Version, r.PolicyVersion)
	}
	var ids []string
	for _, m := range r.Models {
		ids = append(ids, m.ID)
	}
	want := []string{"ollama-cloud/deepseek-v4.1-flash:1017", "ollama-cloud/glm-5.3", "ollama-cloud/glm-5.3-flash", "ollama-cloud/kimi-k3"}
	if !slices.Equal(ids, want) {
		t.Fatalf("models = %v", ids)
	}
	if len(r.Providers) != 1 || r.Providers[0].Billing != "included_credits" {
		t.Fatalf("providers: %+v", r.Providers)
	}
	if err := Validate(r); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Models[0].PriceMicroUSD != 195000 {
		t.Fatalf("fixture price not applied: %d", r.Models[0].PriceMicroUSD)
	}
	if r.StrategyTargets["cheapest"]["build"] != "cheap" || r.TieRules.CheapestMinRank["build"] != 26 {
		t.Fatalf("pinned data: %+v %+v", r.StrategyTargets, r.TieRules)
	}
}

func TestBuildCatalogPricesAndUnrated(t *testing.T) {
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	snap := fixtureSnapshot(t)
	// Remove glm-5.3 and unprice deepseek: both become unrated.
	var models []catalog.SnapshotModel
	for _, m := range snap.Models {
		switch m.ID {
		case "ollama-cloud/glm-5.3":
			continue
		case "ollama-cloud/deepseek-v4.1-flash:1017":
			m.Priced = false
		}
		models = append(models, m)
	}
	snap.Models = models
	pp := []ProviderPolicy{{ID: "ollama-cloud", Billing: "included_credits"}}
	r, err := Build(caps, snap, BuildOptions{Providers: pp, Prices: PricesCatalog})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Unrated, []string{"ollama-cloud/deepseek-v4.1-flash:1017", "ollama-cloud/glm-5.3"}) {
		t.Fatalf("unrated = %v", r.Unrated)
	}
	for _, m := range r.Models {
		if slices.Contains(r.Unrated, m.ID) {
			t.Fatalf("unrated %s kept in models", m.ID)
		}
	}
	for k, v := range r.DecisionTable {
		for _, id := range v {
			if slices.Contains(r.Unrated, id) {
				t.Fatalf("%s routes unrated %s", k, id)
			}
		}
	}
	// Catalog prices are used in catalog mode: kimi is 4.2M in the fixture catalog too.
	for _, m := range r.Models {
		if m.ID == "ollama-cloud/glm-5.3-flash" && m.PriceMicroUSD != 185000 {
			t.Fatalf("flash price = %d", m.PriceMicroUSD)
		}
	}
	if err := Validate(r); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Tampering: an unrated id that is also in models is a named error.
	r.Models = append(r.Models, seat("ollama-cloud/glm-5.3", "good", 1, 1, 1, 1, 1, 1))
	if err := Validate(r); !errors.Is(err, ErrUnrated) {
		t.Fatalf("err = %v, want ErrUnrated", err)
	}
}

func TestValidateNamedErrors(t *testing.T) {
	mut := func(f func(r *Registry)) error {
		r := buildFixture(t, []string{"ollama-cloud"})
		f(r)
		return Validate(r)
	}
	cases := []struct {
		name string
		f    func(r *Registry)
		want error
	}{
		{"unknown tier", func(r *Registry) { r.Models[0].Tier = "legendary" }, ErrUnknownTier},
		{"allowed but rank zero", func(r *Registry) { r.Models[0].RolesAllowed = append(r.Models[0].RolesAllowed, "review") }, ErrRolesAllowed},
		{"ranked but not allowed", func(r *Registry) { r.Models[0].RolesAllowed = []string{"build"} }, ErrRolesAllowed},
		{"ranked but not allowed is named", func(r *Registry) { r.Models[0].RolesAllowed = []string{"build"} }, ErrRoleMissing},
		{"rank above 100", func(r *Registry) { r.Models[0].RoleRank["build"] = 101 }, ErrRankRange},
		{"rank below 0", func(r *Registry) { r.Models[0].RoleRank["explore"] = -1 }, ErrRankRange},
		{"unknown role", func(r *Registry) { r.Models[0].RoleRank["janitor"] = 5 }, ErrUnknownRole},
		{"unknown allowed role", func(r *Registry) { r.Models[0].RolesAllowed = append(r.Models[0].RolesAllowed, "janitor") }, ErrUnknownRole},
		{"no billing mode", func(r *Registry) { r.Providers[0].Billing = "" }, ErrNoBilling},
		{"model provider not installed", func(r *Registry) { r.Models[0].Provider = "elsewhere" }, ErrUnknownProvider},
		{"duplicate model", func(r *Registry) { r.Models = append(r.Models, r.Models[0]) }, ErrDuplicateModel},
		{"bad version", func(r *Registry) { r.Version = "1.0" }, ErrVersion},
		{"bad target", func(r *Registry) { r.StrategyTargets["cheapest"]["build"] = "platinum" }, ErrStrategyTarget},
		{"missing strategy", func(r *Registry) { delete(r.StrategyTargets, "best") }, ErrStrategyTarget},
		{"bad tie rules", func(r *Registry) { r.TieRules.MaxCandidates = 0 }, ErrTieRules},
		{"bad min rank", func(r *Registry) { r.TieRules.CheapestMinRank["build"] = 200 }, ErrTieRules},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := mut(c.f); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if err := mut(func(*Registry) {}); err != nil {
		t.Fatalf("unmutated registry invalid: %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	r.Models[0].Tier = "x"
	r.Models[1].RoleRank["build"] = 500
	err := Validate(r)
	if !errors.Is(err, ErrUnknownTier) || !errors.Is(err, ErrRankRange) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryHashRoundTrip(t *testing.T) {
	for _, set := range [][]string{{"ollama-cloud"}, allProviders} {
		r := buildFixture(t, set)
		h1, err := RegistryHash(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if parity.SHA256Hex(data) != h1 {
			t.Fatal("file bytes do not hash to the registry hash")
		}
		back, err := DecodeRegistry(data)
		if err != nil {
			t.Fatalf("DecodeRegistry: %v", err)
		}
		h2, err := RegistryHash(back)
		if err != nil || h1 != h2 {
			t.Fatalf("hash changed on round trip: %s %s %v", h1, h2, err)
		}
		data2, _ := Marshal(back)
		if !bytes.Equal(data, data2) {
			t.Fatal("bytes changed on round trip")
		}
		if err := Validate(back); err != nil {
			t.Fatal(err)
		}
		// Rebuilding gives the same pin.
		h3, _ := RegistryHash(buildFixture(t, set))
		if h3 != h1 {
			t.Fatal("build is not deterministic")
		}
	}
}

func TestRegistryHashMovesWhenTableMoves(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	h1, _ := RegistryHash(r)
	r.DecisionTable[cell("build", "docs", "S", false, "best")] = []string{"x"}
	h2, _ := RegistryHash(r)
	if h1 == h2 {
		t.Fatal("hash did not change")
	}
}

func TestDecodeRegistryRejectsFloats(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	data, _ := Marshal(r)
	for _, repl := range []struct{ from, to string }{
		{`"price_micro_usd":195000`, `"price_micro_usd":195000.5`},
		{`"price_micro_usd":195000`, `"price_micro_usd":1.95e5`},
		{`"build":26`, `"build":26.0`},
		{`"max_candidates":4`, `"max_candidates":4.0`},
	} {
		if !bytes.Contains(data, []byte(repl.from)) {
			t.Fatalf("fixture lacks %s", repl.from)
		}
		bad := bytes.Replace(data, []byte(repl.from), []byte(repl.to), 1)
		if _, err := DecodeRegistry(bad); !errors.Is(err, ErrFloat) {
			t.Errorf("%s: err = %v, want ErrFloat", repl.to, err)
		}
	}
	// A string that looks like a float is fine; unknown fields are not.
	if _, err := DecodeRegistry(bytes.Replace(data, []byte(`"version":"2.0"`), []byte(`"version":"2.0","extra":1`), 1)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := DecodeRegistry(append(slicesClone(data), []byte(" {}")...)); err == nil {
		t.Fatal("trailing data accepted")
	}
	// Hashing a registry whose struct was somehow fed a float is refused too.
	if _, err := parity.Canonical(map[string]any{"p": json.Number("1.5")}); !errors.Is(err, parity.ErrFloat) {
		t.Fatalf("err = %v", err)
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

func TestRegistryHasNoFloatTypes(t *testing.T) {
	data, err := Marshal(buildFixture(t, allProviders))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case json.Number:
			if strings.ContainsAny(y.String(), ".eE") {
				t.Fatalf("non-integer number %s", y)
			}
		case []any:
			for _, e := range y {
				walk(e)
			}
		case map[string]any:
			for _, e := range y {
				walk(e)
			}
		}
	}
	walk(v)
}

func TestSeedCapabilitiesAreValid(t *testing.T) {
	r := buildFixture(t, allProviders)
	if err := Validate(r); err != nil {
		t.Fatal(err)
	}
	if len(r.Models) != 10 {
		t.Fatalf("models = %d", len(r.Models))
	}
	for _, m := range r.Models {
		var nz []string
		for _, role := range Roles {
			if m.RoleRank[role] != 0 {
				nz = append(nz, role)
			}
		}
		got := slices.Clone(m.RolesAllowed)
		slices.Sort(nz)
		slices.Sort(got)
		if !slices.Equal(nz, got) {
			t.Errorf("%s: roles_allowed %v != non-zero ranks %v", m.ID, got, nz)
		}
	}
}
