package policy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
)

func fixtureSnapshot(t testing.TB) catalog.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("../catalog/testdata/models.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.NewSnapshot(c, "fixture", catalog.Digest(raw), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), true)
}

// buildFixture builds a registry from the seed capabilities and the fixture
// catalog for the named providers, in preference order, using fixture prices.
func buildFixture(t testing.TB, providers []string, deny ...string) *Registry {
	t.Helper()
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	var pp []ProviderPolicy
	for _, name := range providers {
		for _, cp := range caps.Providers {
			if cp.ID == name {
				pp = append(pp, ProviderPolicy{ID: cp.ID, Billing: cp.Billing})
			}
		}
	}
	if len(pp) != len(providers) {
		t.Fatalf("unknown provider in %v", providers)
	}
	r, err := Build(caps, fixtureSnapshot(t), BuildOptions{Providers: pp, Deny: deny, Prices: PricesFixture})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return r
}

// short drops the provider prefix of a model id.
func short(id string) string {
	_, m, _ := strings.Cut(id, "/")
	return m
}

func shorts(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = short(id)
	}
	return out
}

func cell(role, typ, size string, risk bool, strategy string) string {
	return Cell{Role: role, Type: typ, Size: size, Risk: risk, Strategy: strategy}.Key()
}

// seat builds a hand-made model for edge-case registries.
func seat(id, tier string, build, plan, review, advise, explore, price int) Model {
	rr := map[string]int{"build": build, "plan": plan, "review": review, "advise": advise, "explore": explore}
	var allowed []string
	for _, role := range Roles {
		if rr[role] > 0 {
			allowed = append(allowed, role)
		}
	}
	prov, _, _ := strings.Cut(id, "/")
	return Model{
		ID: id, Provider: prov, Family: prov, Tier: tier, RoleRank: rr, RolesAllowed: allowed,
		Efforts: []string{"low", "medium", "high", "xhigh"}, Prohibited: []string{}, Evidence: "test",
		PriceMicroUSD: int64(price),
	}
}

func handRegistry(models ...Model) *Registry {
	seen := map[string]bool{}
	var provs []ProviderPolicy
	var pref []string
	for _, m := range models {
		if !seen[m.Provider] {
			seen[m.Provider] = true
			provs = append(provs, ProviderPolicy{ID: m.Provider, Billing: "metered"})
			pref = append(pref, m.Provider)
		}
	}
	r := &Registry{
		Version: Version, PolicyVersion: PolicyVersion, Providers: provs, Models: models,
		StrategyTargets: DefaultStrategyTargets(), ProviderPolicy: providerPolicyIn{Preference: pref, Deny: []string{}},
		TieRules: DefaultTieRules(), Unrated: []string{},
	}
	r.Regenerate()
	return r
}
