package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Table errors.
var (
	// ErrBadCell is returned for a cell key that is not in the grid.
	ErrBadCell = errors.New("policy: not a decision-table cell")
	// ErrCellMissing is returned when a well-formed cell has no table entry.
	ErrCellMissing = errors.New("policy: cell missing from the decision table")
)

// typeTiers is the complexity ladder of the harness router, unchanged.
var typeTiers = map[string]int{
	"docs": 0, "test_only": 0, "scaffold": 1, "bugfix": 1, "feature": 2, "refactor": 2, "migration": 2,
}

// effortLadder orders effort rungs from the lowest to the highest.
var effortLadder = []string{"low", "medium", "high", "xhigh"}

// Cell is one decision-table cell.
type Cell struct {
	Role, Type, Size string
	Risk             bool
	Strategy         string
}

// Key returns the table key: <role>|<type>|<size>|<risk>|<strategy>.
func (c Cell) Key() string {
	return fmt.Sprintf("%s|%s|%s|%t|%s", c.Role, c.Type, c.Size, c.Risk, c.Strategy)
}

// ParseCell parses a cell key strictly. Anything outside the grid, including
// whitespace and extra fields, is ErrBadCell.
func ParseCell(key string) (Cell, error) {
	if len(key) > 64 {
		return Cell{}, fmt.Errorf("%w: key too long", ErrBadCell)
	}
	parts := strings.Split(key, "|")
	if len(parts) != 5 {
		return Cell{}, fmt.Errorf("%w: want 5 fields, got %d", ErrBadCell, len(parts))
	}
	c := Cell{Role: parts[0], Type: parts[1], Size: parts[2], Strategy: parts[4]}
	switch parts[3] {
	case "true":
		c.Risk = true
	case "false":
	default:
		return Cell{}, fmt.Errorf("%w: risk must be true or false", ErrBadCell)
	}
	if !slices.Contains(Roles, c.Role) || !slices.Contains(Types, c.Type) ||
		!slices.Contains(Sizes, c.Size) || !slices.Contains(Strategies, c.Strategy) {
		return Cell{}, fmt.Errorf("%w: unknown field value", ErrBadCell)
	}
	return c, nil
}

// Cells lists the whole grid in a stable order.
func Cells() []Cell {
	out := make([]Cell, 0, len(Roles)*len(Types)*len(Sizes)*2*len(Strategies))
	for _, role := range Roles {
		for _, typ := range Types {
			for _, size := range Sizes {
				for _, risk := range []bool{false, true} {
					for _, s := range Strategies {
						out = append(out, Cell{Role: role, Type: typ, Size: size, Risk: risk, Strategy: s})
					}
				}
			}
		}
	}
	return out
}

// Lookup returns a copy of the candidate list of one cell. The list is an
// ordered set of candidates and never an authorization.
func Lookup(r *Registry, key string) ([]string, error) {
	if _, err := ParseCell(key); err != nil {
		return nil, err
	}
	v, ok := r.DecisionTable[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCellMissing, key)
	}
	return slices.Clone(v), nil
}

type targetKind int

const (
	kindFixed targetKind = iota
	kindLadder
	kindMaxRank
)

func parseTarget(s string) (targetKind, int, error) {
	switch {
	case s == "max_rank":
		return kindMaxRank, 0, nil
	case strings.HasPrefix(s, "ladder:"):
		i := tierIndex(strings.TrimPrefix(s, "ladder:"))
		if i < 0 {
			return 0, 0, fmt.Errorf("unknown ladder floor in %q", s)
		}
		return kindLadder, i, nil
	default:
		i := tierIndex(s)
		if i < 0 {
			return 0, 0, fmt.Errorf("unknown target %q", s)
		}
		return kindFixed, i, nil
	}
}

// Row is how one model fared in one cell.
type Row struct {
	ID            string
	Provider      string
	Tier          string
	Rank          int
	PriceMicroUSD int64
	Preference    int
	Status        string
}

// Explanation says why the candidates of one cell won.
type Explanation struct {
	Key        string
	Rule       string
	Order      string
	TargetTier string
	UsedTier   string
	MinRank    int
	Candidates []string
	Rows       []Row
}

type evaluation struct {
	Explanation
	err error
}

func (r *Registry) prefIndex(provider string) int {
	if i := slices.Index(r.ProviderPolicy.Preference, provider); i >= 0 {
		return i
	}
	return len(r.ProviderPolicy.Preference)
}

// evaluate is the single rule implementation. Generate and Explain both use it,
// so an explanation cannot drift from the table.
func evaluate(r *Registry, c Cell) evaluation {
	ev := evaluation{Explanation: Explanation{Key: c.Key(), TargetTier: "-", UsedTier: "-"}}
	kind, arg, err := parseTarget(r.StrategyTargets[c.Strategy][c.Role])
	if err != nil {
		ev.err = err
		ev.Candidates = []string{}
		return ev
	}
	riskTop := c.Risk && (c.Role == "build" || c.Role == "plan") && kind != kindMaxRank
	target := arg
	switch kind {
	case kindFixed:
		ev.Rule = fmt.Sprintf("%s: fixed tier %s for %s", c.Strategy, Tiers[arg], c.Role)
	case kindLadder:
		level := typeTiers[c.Type]
		if c.Size == "L" {
			level++
		}
		target = max(arg, min(len(Tiers)-1, level))
		ev.Rule = fmt.Sprintf("%s: ladder(type %s, size %s) = %s, floor %s", c.Strategy, c.Type, c.Size, Tiers[target], Tiers[arg])
	case kindMaxRank:
		ev.Rule = fmt.Sprintf("%s: highest role_rank among seats allowed for %s, any tier", c.Strategy, c.Role)
	}
	if riskTop {
		target = len(Tiers) - 1
		ev.Rule += "; risk forces top"
	}
	if kind != kindMaxRank {
		ev.TargetTier = Tiers[target]
	} else {
		ev.TargetTier = "any"
	}

	cheapest := c.Strategy == "cheapest"
	minRank := 0
	if cheapest {
		minRank = r.TieRules.CheapestMinRank[c.Role]
		ev.MinRank = minRank
		ev.Order = "provider preference, price asc, role_rank desc, id asc; drop rank below cheapest_min_rank"
	} else {
		ev.Order = "role_rank desc, provider preference, price asc, id asc"
	}

	type cand struct {
		m      Model
		status string
		usable bool
	}
	seats := make([]cand, 0, len(r.Models))
	for _, m := range r.Models {
		s := cand{m: m}
		switch {
		case r.denied(m):
			s.status = "excluded: denied"
		case !eligible(m, c.Role):
			s.status = fmt.Sprintf("excluded: role %s not allowed (rank %d)", c.Role, m.RoleRank[c.Role])
		case cheapest && m.RoleRank[c.Role] < minRank:
			s.status = fmt.Sprintf("excluded: below cheapest_min_rank %d (rank %d)", minRank, m.RoleRank[c.Role])
		default:
			s.usable = true
		}
		seats = append(seats, s)
	}

	less := func(a, b Model) int {
		ra, rb := a.RoleRank[c.Role], b.RoleRank[c.Role]
		pa, pb := r.prefIndex(a.Provider), r.prefIndex(b.Provider)
		if cheapest {
			return cmp.Or(cmp.Compare(pa, pb), cmp.Compare(a.PriceMicroUSD, b.PriceMicroUSD), cmp.Compare(rb, ra), strings.Compare(a.ID, b.ID))
		}
		return cmp.Or(cmp.Compare(rb, ra), cmp.Compare(pa, pb), cmp.Compare(a.PriceMicroUSD, b.PriceMicroUSD), strings.Compare(a.ID, b.ID))
	}

	used := -1
	pool := func(tier int) []int {
		var idx []int
		for i, s := range seats {
			if s.usable && (tier < 0 || tierIndex(s.m.Tier) == tier) {
				idx = append(idx, i)
			}
		}
		return idx
	}
	var chosen []int
	if kind == kindMaxRank {
		chosen = pool(-1)
		ev.UsedTier = "any"
	} else {
	search:
		for d := 0; d < len(Tiers); d++ {
			tryTiers := []int{target + d, target - d} // the higher tier wins a tie
			if d == 0 {
				tryTiers = tryTiers[:1]
			}
			for _, t := range tryTiers {
				if t < 0 || t >= len(Tiers) {
					continue
				}
				if p := pool(t); len(p) > 0 {
					chosen, used = p, t
					break search
				}
			}
		}
		if used >= 0 {
			ev.UsedTier = Tiers[used]
		} else {
			ev.UsedTier = "none"
		}
	}
	slices.SortFunc(chosen, func(a, b int) int { return less(seats[a].m, seats[b].m) })

	limit := r.TieRules.MaxCandidates
	ev.Candidates = []string{}
	position := map[int]int{}
	for n, i := range chosen {
		if n < limit {
			ev.Candidates = append(ev.Candidates, seats[i].m.ID)
			position[i] = n + 1
		}
	}
	inChosen := map[int]bool{}
	for _, i := range chosen {
		inChosen[i] = true
	}
	for i, s := range seats {
		status := s.status
		switch {
		case position[i] > 0:
			status = fmt.Sprintf("selected #%d", position[i])
		case inChosen[i]:
			status = fmt.Sprintf("beyond max_candidates %d", limit)
		case s.usable:
			status = fmt.Sprintf("tier %s (cell uses tier %s)", s.m.Tier, ev.UsedTier)
		}
		ev.Rows = append(ev.Rows, Row{
			ID: s.m.ID, Provider: s.m.Provider, Tier: s.m.Tier, Rank: s.m.RoleRank[c.Role],
			PriceMicroUSD: s.m.PriceMicroUSD, Preference: r.prefIndex(s.m.Provider), Status: status,
		})
	}
	// Selected rows first in candidate order, then the rest by id.
	slices.SortStableFunc(ev.Rows, func(a, b Row) int {
		ia, ib := slices.Index(ev.Candidates, a.ID), slices.Index(ev.Candidates, b.ID)
		switch {
		case ia >= 0 && ib >= 0:
			return cmp.Compare(ia, ib)
		case ia >= 0:
			return -1
		case ib >= 0:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return ev
}

// Regenerate rebuilds DecisionTable and RiskEfforts from the models and rules.
func (r *Registry) Regenerate() {
	table := make(map[string][]string, 630)
	efforts := map[string]map[string]string{}
	byID := make(map[string]Model, len(r.Models))
	for _, m := range r.Models {
		byID[m.ID] = m
	}
	for _, c := range Cells() {
		ev := evaluate(r, c)
		table[ev.Key] = ev.Candidates
		// The risk effort rung applies to build and plan only; review, advise
		// and explore keep their tier effort.
		if !c.Risk || (c.Role != "build" && c.Role != "plan") {
			continue
		}
		rungs := map[string]string{}
		for _, id := range ev.Candidates {
			if rung := riskRung(byID[id]); rung != "" {
				rungs[id] = rung
			}
		}
		if len(rungs) > 0 {
			efforts[ev.Key] = rungs
		}
	}
	r.DecisionTable = table
	r.RiskEfforts = efforts
}

// riskRung is the effort rung for a risk=true cell: xhigh clamped to the seat's
// highest declared, non-prohibited rung. A seat whose highest rung is below
// high has no risk rung and yields "".
func riskRung(m Model) string {
	best := -1
	for _, e := range m.Efforts {
		if slices.Contains(m.Prohibited, e) {
			continue
		}
		if i := slices.Index(effortLadder, e); i > best {
			best = i
		}
	}
	if best < slices.Index(effortLadder, "high") {
		return ""
	}
	return effortLadder[best]
}

// Explain describes how the candidates of one cell were chosen.
func Explain(r *Registry, key string) (Explanation, error) {
	c, err := ParseCell(key)
	if err != nil {
		return Explanation{}, err
	}
	ev := evaluate(r, c)
	if ev.err != nil {
		return Explanation{}, fmt.Errorf("policy: %s: %w", key, ev.err)
	}
	return ev.Explanation, nil
}

// CheckReport lists empty cells and hard failures.
type CheckReport struct {
	// Empty names every cell with no candidate, sorted.
	Empty []string
	// Failures names the empty review and advise cells under balanced, sorted.
	Failures []string
}

// Failed reports whether the check has a hard failure.
func (c CheckReport) Failed() bool { return len(c.Failures) > 0 }

// Check names every empty cell. An empty cell is allowed in the table, but an
// empty review or advise cell under balanced is a hard failure.
func Check(r *Registry) CheckReport {
	rep := CheckReport{Empty: []string{}, Failures: []string{}}
	for _, c := range Cells() {
		key := c.Key()
		if len(r.DecisionTable[key]) > 0 {
			continue
		}
		rep.Empty = append(rep.Empty, key)
		if c.Strategy == "balanced" && (c.Role == "review" || c.Role == "advise") {
			rep.Failures = append(rep.Failures, key)
		}
	}
	slices.Sort(rep.Empty)
	slices.Sort(rep.Failures)
	return rep
}
