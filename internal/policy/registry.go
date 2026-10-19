package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/parity"
)

// Registry versions.
const (
	Version       = "2.0"
	PolicyVersion = "strategy-1.0"
)

// The decision grid. Order is the print order.
var (
	Roles      = []string{"build", "plan", "review", "advise", "explore"}
	Types      = []string{"docs", "test_only", "scaffold", "bugfix", "feature", "refactor", "migration"}
	Sizes      = []string{"S", "M", "L"}
	Strategies = []string{"cheapest", "balanced", "best"}
	// Tiers run from the lowest to the highest.
	Tiers = []string{"cheap", "good", "very_good", "top"}
)

// ErrFloat is returned for any non-integer number in a registry payload.
var ErrFloat = parity.ErrFloat

// Validation errors. Validate joins every problem it finds; branch with errors.Is.
var (
	ErrVersion         = errors.New("registry: unsupported version")
	ErrUnknownTier     = errors.New("registry: unknown tier")
	ErrUnknownRole     = errors.New("registry: unknown role")
	ErrRolesAllowed    = errors.New("registry: roles_allowed does not equal the roles with a non-zero rank")
	ErrRoleMissing     = errors.New("registry: roles_allowed is missing a role the seat is ranked for")
	ErrRankRange       = errors.New("registry: role rank outside 0..100")
	ErrUnrated         = errors.New("registry: unrated model present in models")
	ErrNoBilling       = errors.New("registry: provider has no billing mode")
	ErrUnknownProvider = errors.New("registry: model provider is not an installed provider")
	ErrDuplicateModel  = errors.New("registry: duplicate model id")
	ErrDuplicateProv   = errors.New("registry: duplicate provider id")
	ErrStrategyTarget  = errors.New("registry: invalid strategy target")
	ErrTieRules        = errors.New("registry: invalid tie rules")
	ErrNoEligibleSeat  = errors.New("registry: a deny entry leaves a role with no eligible seat")
)

// ProviderPolicy is one installed provider and how it is billed.
type ProviderPolicy struct {
	ID      string `json:"id"`
	Billing string `json:"billing"`
}

// providerPolicyIn is the install-level provider policy: the order in which
// providers are spent, and the deny list. A deny entry is a provider id or a
// full model id ("provider/model").
type providerPolicyIn struct {
	Preference []string `json:"preference"`
	Deny       []string `json:"deny"`
}

// TieRules pins how ties and fallbacks resolve.
type TieRules struct {
	MaxCandidates   int            `json:"max_candidates"`
	TierFallback    string         `json:"tier_fallback"`
	CheapestOrder   []string       `json:"cheapest_order"`
	RankedOrder     []string       `json:"ranked_order"`
	CheapestMinRank map[string]int `json:"cheapest_min_rank"`
}

// Model is one rated seat. PriceMicroUSD is the blended price at 90/10 in
// integer micro-USD per 1M tokens.
type Model struct {
	ID            string         `json:"id"`
	Provider      string         `json:"provider"`
	Family        string         `json:"family"`
	Tier          string         `json:"tier"`
	RoleRank      map[string]int `json:"role_rank"`
	RolesAllowed  []string       `json:"roles_allowed"`
	Efforts       []string       `json:"efforts"`
	Prohibited    []string       `json:"prohibited"`
	Evidence      string         `json:"evidence"`
	PriceMicroUSD int64          `json:"price_micro_usd"`
}

// Registry is registry v2: the pinned artifact the harness hashes. It holds no
// floats. DecisionTable maps a cell key to an ordered candidate list. RiskEfforts
// records, for risk=true cells only, the effort rung of each candidate whose
// seat supports a risk rung. Unrated lists curated models that are absent from
// the catalog or unpriced; they are never routed.
type Registry struct {
	Version         string                       `json:"version"`
	PolicyVersion   string                       `json:"policy_version"`
	Providers       []ProviderPolicy             `json:"providers"`
	Models          []Model                      `json:"models"`
	StrategyTargets map[string]map[string]string `json:"strategy_targets"`
	ProviderPolicy  providerPolicyIn             `json:"provider_policy"`
	TieRules        TieRules                     `json:"tie_rules"`
	Unrated         []string                     `json:"unrated"`
	RiskEfforts     map[string]map[string]string `json:"risk_efforts"`
	DecisionTable   map[string][]string          `json:"decision_table"`
}

// DefaultStrategyTargets returns the pinned strategy targets. A tier name is a
// fixed tier; "ladder:<floor>" is the complexity ladder with that floor;
// "max_rank" is the highest role_rank regardless of tier.
func DefaultStrategyTargets() map[string]map[string]string {
	return map[string]map[string]string{
		"cheapest": {"build": "cheap", "plan": "good", "review": "good", "advise": "good", "explore": "cheap"},
		"balanced": {"build": "ladder:cheap", "plan": "ladder:good", "review": "very_good", "advise": "top", "explore": "cheap"},
		"best":     {"build": "max_rank", "plan": "top", "review": "top", "advise": "top", "explore": "cheap"},
	}
}

var (
	cheapestOrder = []string{"provider_preference", "price_asc", "role_rank_desc", "id_asc"}
	rankedOrder   = []string{"role_rank_desc", "provider_preference", "price_asc", "id_asc"}
)

// DefaultTieRules returns the pinned tie rules, including cheapest_min_rank.
func DefaultTieRules() TieRules {
	return TieRules{
		MaxCandidates: 4, TierFallback: "nearest_prefer_higher",
		CheapestOrder: slices.Clone(cheapestOrder), RankedOrder: slices.Clone(rankedOrder),
		CheapestMinRank: map[string]int{"build": 26, "plan": 50, "review": 35, "advise": 26, "explore": 0},
	}
}

// PriceSource selects where Build takes seat prices from.
type PriceSource int

// Price sources.
const (
	// PricesCatalog uses the blended price in the pricing snapshot. A model the
	// snapshot cannot price is unrated.
	PricesCatalog PriceSource = iota
	// PricesFixture uses fixture_price from the capability file. It exists so
	// Gate 8 has a fixed, stated input.
	PricesFixture
)

// BuildOptions describes one install.
type BuildOptions struct {
	// Providers are the installed providers, in preference order.
	Providers []ProviderPolicy
	// Preference overrides the spend order; it defaults to Providers order.
	Preference []string
	// Deny lists provider ids or full model ids that must never be routed.
	Deny   []string
	Prices PriceSource
}

// Build merges the curated capabilities with the catalog snapshot for one
// install and generates the decision table. A curated model that the snapshot
// does not rate is listed in Unrated and left out of Models. Build does not
// validate; call Validate.
func Build(caps *catalog.Capabilities, snap catalog.Snapshot, opt BuildOptions) (*Registry, error) {
	if caps == nil {
		return nil, errors.New("policy: nil capabilities")
	}
	if len(opt.Providers) == 0 {
		return nil, errors.New("policy: no providers installed")
	}
	installed := map[string]bool{}
	for _, p := range opt.Providers {
		installed[p.ID] = true
	}
	pref := slices.Clone(opt.Preference)
	if len(pref) == 0 {
		for _, p := range opt.Providers {
			pref = append(pref, p.ID)
		}
	}
	r := &Registry{
		Version: Version, PolicyVersion: PolicyVersion,
		Providers:       slices.Clone(opt.Providers),
		Models:          []Model{},
		StrategyTargets: DefaultStrategyTargets(),
		ProviderPolicy:  providerPolicyIn{Preference: pref, Deny: nonNil(opt.Deny)},
		TieRules:        DefaultTieRules(),
		Unrated:         []string{},
	}
	for _, c := range caps.Models {
		if !installed[c.Provider] {
			continue
		}
		sm, found := snap.Lookup(c.ID)
		var price int64
		switch opt.Prices {
		case PricesFixture:
			price = c.FixturePrice
		default:
			price = sm.BlendedMicroUSD
			found = found && sm.Priced
		}
		if !found {
			r.Unrated = append(r.Unrated, c.ID)
			continue
		}
		rank := make(map[string]int, len(c.RoleRank))
		for k, v := range c.RoleRank {
			rank[k] = v
		}
		r.Models = append(r.Models, Model{
			ID: c.ID, Provider: c.Provider, Family: c.Family, Tier: c.Tier, RoleRank: rank,
			RolesAllowed: nonNil(c.RolesAllowed), Efforts: nonNil(c.Efforts), Prohibited: nonNil(c.Prohibited),
			Evidence: c.Evidence, PriceMicroUSD: price,
		})
	}
	slices.SortFunc(r.Models, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(r.Unrated)
	r.Regenerate()
	return r, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// Marshal returns the canonical JSON of r. These are the bytes that are hashed
// and, when written to disk, the bytes of the pinned file.
func Marshal(r *Registry) ([]byte, error) {
	return parity.Canonical(r)
}

// RegistryHash is the sha256 of the canonical JSON of r, equal to what the
// Python harness computes over the same document.
func RegistryHash(r *Registry) (string, error) {
	return parity.Hash(r)
}

// DecodeRegistry parses a registry document. Any non-integer number is refused
// with ErrFloat; unknown fields and trailing data are errors.
func DecodeRegistry(data []byte) (*Registry, error) {
	if _, err := parity.CanonicalJSON(data); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Registry
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("registry: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("registry: decode: trailing data after document")
	}
	return &r, nil
}

func tierIndex(t string) int { return slices.Index(Tiers, t) }

// Validate checks a registry and returns every problem found, joined. It does
// not check the decision table against the rules; Regenerate does that.
func Validate(r *Registry) error {
	var errs []error
	add := func(base error, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: %s", base, fmt.Sprintf(format, args...)))
	}
	if r.Version != Version || r.PolicyVersion != PolicyVersion {
		add(ErrVersion, "version %q policy_version %q", r.Version, r.PolicyVersion)
	}

	providers := map[string]bool{}
	for _, p := range r.Providers {
		if providers[p.ID] {
			add(ErrDuplicateProv, "%s", p.ID)
		}
		providers[p.ID] = true
		if strings.TrimSpace(p.Billing) == "" {
			add(ErrNoBilling, "%s", p.ID)
		}
	}

	seen := map[string]bool{}
	for _, m := range r.Models {
		if seen[m.ID] {
			add(ErrDuplicateModel, "%s", m.ID)
		}
		seen[m.ID] = true
		if slices.Contains(r.Unrated, m.ID) {
			add(ErrUnrated, "%s is listed as unrated and as a model", m.ID)
		}
		if !providers[m.Provider] {
			add(ErrUnknownProvider, "%s has provider %q", m.ID, m.Provider)
		}
		if tierIndex(m.Tier) < 0 {
			add(ErrUnknownTier, "%s has tier %q", m.ID, m.Tier)
		}
		nonZero := map[string]bool{}
		for role, rank := range m.RoleRank {
			if !slices.Contains(Roles, role) {
				add(ErrUnknownRole, "%s ranks unknown role %q", m.ID, role)
				continue
			}
			if rank < 0 || rank > 100 {
				add(ErrRankRange, "%s %s rank %d", m.ID, role, rank)
			}
			if rank != 0 {
				nonZero[role] = true
			}
		}
		allowed := map[string]bool{}
		for _, role := range m.RolesAllowed {
			if !slices.Contains(Roles, role) {
				add(ErrUnknownRole, "%s allows unknown role %q", m.ID, role)
				continue
			}
			allowed[role] = true
		}
		for _, role := range Roles {
			switch {
			case nonZero[role] && !allowed[role]:
				errs = append(errs, fmt.Errorf("%w: %w: %s %s", ErrRolesAllowed, ErrRoleMissing, m.ID, role))
			case allowed[role] && !nonZero[role]:
				add(ErrRolesAllowed, "%s lists %s in roles_allowed with rank 0", m.ID, role)
			}
		}
	}

	for _, s := range Strategies {
		row, ok := r.StrategyTargets[s]
		if !ok {
			add(ErrStrategyTarget, "strategy %q missing", s)
			continue
		}
		for _, role := range Roles {
			if _, _, err := parseTarget(row[role]); err != nil {
				add(ErrStrategyTarget, "%s/%s: %v", s, role, err)
			}
		}
	}

	t := r.TieRules
	if t.MaxCandidates < 1 || t.MaxCandidates > 16 {
		add(ErrTieRules, "max_candidates %d", t.MaxCandidates)
	}
	if t.TierFallback != "nearest_prefer_higher" {
		add(ErrTieRules, "tier_fallback %q", t.TierFallback)
	}
	if !slices.Equal(t.CheapestOrder, cheapestOrder) || !slices.Equal(t.RankedOrder, rankedOrder) {
		add(ErrTieRules, "unsupported ordering")
	}
	for _, role := range Roles {
		if v, ok := t.CheapestMinRank[role]; !ok || v < 0 || v > 100 {
			add(ErrTieRules, "cheapest_min_rank[%s] = %d (present: %t)", role, v, ok)
		}
	}

	// A deny entry must not be the reason a role has no seat at all.
	withoutDeny := *r
	withoutDeny.ProviderPolicy = providerPolicyIn{Preference: r.ProviderPolicy.Preference, Deny: []string{}}
	for _, role := range Roles {
		if !hasSeat(r, role) && hasSeat(&withoutDeny, role) {
			add(ErrNoEligibleSeat, "role %s", role)
		}
	}
	return errors.Join(errs...)
}

func hasSeat(r *Registry, role string) bool {
	for _, m := range r.Models {
		if !r.denied(m) && eligible(m, role) {
			return true
		}
	}
	return false
}

func (r *Registry) denied(m Model) bool {
	for _, d := range r.ProviderPolicy.Deny {
		if d == m.Provider || d == m.ID {
			return true
		}
	}
	return false
}

// eligible reports whether a seat may take a role: listed in roles_allowed and
// holding a non-zero rank.
func eligible(m Model, role string) bool {
	return m.RoleRank[role] > 0 && slices.Contains(m.RolesAllowed, role) && tierIndex(m.Tier) >= 0
}
