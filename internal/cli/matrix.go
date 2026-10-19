package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
)

// Matrix implements `mgw matrix`. It prints, explains, checks or writes the
// decision table for one install. The table is ordered candidates, never
// authorization.
func Matrix(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("matrix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strategy := fs.String("strategy", "balanced", "print one strategy: cheapest, balanced or best")
	all := fs.Bool("all", false, "print the three strategies side by side with blended price")
	explain := fs.String("explain", "", "explain one cell: <role>|<type>|<size>|<risk>|<strategy>")
	write := fs.Bool("write", false, "write the pinned registry to the output path")
	check := fs.Bool("check", false, "name empty cells and hard failures")
	providers := fs.String("providers", "", "comma-separated installed providers in preference order (default: all curated)")
	deny := fs.String("deny", "", "comma-separated provider ids or model ids that must never be routed")
	snapshot := fs.String("snapshot", deps.defaultSnapshot(), "pricing snapshot path")
	out := fs.String("out", deps.defaultRegistry(), "registry output path for --write")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	modes := 0
	for _, on := range []bool{*all, *explain != "", *write, *check} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		_, _ = fmt.Fprintln(stderr, "mgw matrix: choose one of --all, --explain, --write, --check")
		return ExitUsage
	}
	if !slices.Contains(policy.Strategies, *strategy) {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: unknown strategy %q (want cheapest, balanced or best)\n", *strategy)
		return ExitUsage
	}
	if explicit["strategy"] && modes > 0 {
		_, _ = fmt.Fprintln(stderr, "mgw matrix: --strategy only applies when printing one strategy")
		return ExitUsage
	}
	if explicit["out"] && !*write {
		_, _ = fmt.Fprintln(stderr, "mgw matrix: --out only applies to --write")
		return ExitUsage
	}
	if *explain != "" {
		if _, err := policy.ParseCell(*explain); err != nil {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: invalid cell %q: %v\n", *explain, err)
			return ExitUsage
		}
	}

	reg, code := buildRegistry(stderr, *snapshot, splitList(*providers), splitList(*deny))
	if reg == nil {
		return code
	}
	verr := policy.Validate(reg)

	switch {
	case *check:
		return runCheck(stdout, stderr, reg, verr)
	case verr != nil:
		_, _ = fmt.Fprintf(stderr, "mgw matrix: registry is invalid:\n%v\n", verr)
		return ExitError
	case *write:
		return runWrite(stdout, stderr, reg, *out)
	case *explain != "":
		return runExplain(stdout, stderr, reg, *explain)
	case *all:
		printAll(stdout, reg)
	default:
		printOne(stdout, reg, *strategy)
	}
	if len(reg.Unrated) > 0 {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %d unrated models are never routed: %s\n", len(reg.Unrated), strings.Join(reg.Unrated, ", "))
	}
	return ExitOK
}

// buildRegistry loads the snapshot and the embedded capabilities and builds the
// registry for the named providers. On failure it returns nil and an exit code.
func buildRegistry(stderr io.Writer, snapshotPath string, names, deny []string) (*policy.Registry, int) {
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return nil, ExitError
	}
	var installed []policy.ProviderPolicy
	if len(names) == 0 {
		for _, p := range caps.Providers {
			installed = append(installed, policy.ProviderPolicy{ID: p.ID, Billing: p.Billing})
		}
	}
	for _, name := range names {
		i := slices.IndexFunc(caps.Providers, func(p catalog.CapProvider) bool { return p.ID == name })
		if i < 0 {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: unknown provider %q\n", name)
			return nil, ExitUsage
		}
		installed = append(installed, policy.ProviderPolicy{ID: name, Billing: caps.Providers[i].Billing})
	}
	data, err := readFile(snapshotPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: no pricing snapshot at %s; run `mgw pricing refresh`\n", snapshotPath)
		} else {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		}
		return nil, ExitError
	}
	snap, err := catalog.ReadSnapshot(data)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return nil, ExitError
	}
	reg, err := policy.Build(caps, snap, policy.BuildOptions{Providers: installed, Deny: deny, Prices: policy.PricesCatalog})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return nil, ExitError
	}
	return reg, ExitOK
}

func providerList(reg *policy.Registry) string {
	ids := make([]string, len(reg.Providers))
	for i, p := range reg.Providers {
		ids[i] = p.ID
	}
	return strings.Join(ids, ",")
}

func header(w io.Writer, reg *policy.Registry, strategy string) {
	hash, _ := policy.RegistryHash(reg)
	if len(hash) > 12 {
		hash = hash[:12]
	}
	_, _ = fmt.Fprintf(w, "# mgw matrix  policy %s  registry %s  providers %s\n", reg.PolicyVersion, hash, providerList(reg))
	_, _ = fmt.Fprintf(w, "# strategy %s  blend %s  prices are blended USD per 1M tokens\n", strategy, catalog.BlendLabel)
}

// candidates formats a cell's candidates; risk cells carry the effort rung.
func candidates(reg *policy.Registry, key string, withPrice bool, limit int) string {
	ids := reg.DecisionTable[key]
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	if len(ids) == 0 {
		return "-"
	}
	price := map[string]int64{}
	for _, m := range reg.Models {
		price[m.ID] = m.PriceMicroUSD
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		s := id
		if rung := reg.RiskEfforts[key][id]; rung != "" {
			s += "@" + rung
		}
		if withPrice {
			s += " ($" + formatUSD(price[id]) + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}

func printOne(w io.Writer, reg *policy.Registry, strategy string) {
	header(w, reg, strategy)
	rows := [][]string{{"ROLE", "TYPE", "SIZE", "RISK", "CANDIDATES"}}
	for _, c := range policy.Cells() {
		if c.Strategy != strategy {
			continue
		}
		rows = append(rows, []string{c.Role, c.Type, c.Size, fmt.Sprint(c.Risk), candidates(reg, c.Key(), false, 0)})
	}
	printTable(w, rows)
}

func printAll(w io.Writer, reg *policy.Registry) {
	header(w, reg, "all (first candidate of each; price is USD per 1M tokens)")
	rows := [][]string{{"ROLE", "TYPE", "SIZE", "RISK", "CHEAPEST", "BALANCED", "BEST"}}
	for _, role := range policy.Roles {
		for _, typ := range policy.Types {
			for _, size := range policy.Sizes {
				for _, risk := range []bool{false, true} {
					row := []string{role, typ, size, fmt.Sprint(risk)}
					for _, s := range policy.Strategies {
						key := policy.Cell{Role: role, Type: typ, Size: size, Risk: risk, Strategy: s}.Key()
						row = append(row, candidates(reg, key, true, 1))
					}
					rows = append(rows, row)
				}
			}
		}
	}
	printTable(w, rows)
}

func runExplain(stdout, stderr io.Writer, reg *policy.Registry, key string) int {
	ex, err := policy.Explain(reg, key)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "cell:        %s\n", ex.Key)
	_, _ = fmt.Fprintf(stdout, "rule:        %s\n", ex.Rule)
	_, _ = fmt.Fprintf(stdout, "target tier: %s\n", ex.TargetTier)
	_, _ = fmt.Fprintf(stdout, "used tier:   %s\n", ex.UsedTier)
	_, _ = fmt.Fprintf(stdout, "order:       %s\n", ex.Order)
	_, _ = fmt.Fprintf(stdout, "blend:       %s, USD per 1M tokens\n", catalog.BlendLabel)
	_, _ = fmt.Fprintf(stdout, "candidates:  %s\n\n", strings.Join(ex.Candidates, ", "))
	rows := [][]string{{"MODEL", "PROVIDER", "PREF", "TIER", "RANK", "PRICE", "STATUS"}}
	for _, r := range ex.Rows {
		rows = append(rows, []string{r.ID, r.Provider, fmt.Sprint(r.Preference), r.Tier, fmt.Sprint(r.Rank), "$" + formatUSD(r.PriceMicroUSD), r.Status})
	}
	printTable(stdout, rows)
	return ExitOK
}

func runCheck(stdout, stderr io.Writer, reg *policy.Registry, verr error) int {
	failed := false
	for _, id := range reg.Unrated {
		_, _ = fmt.Fprintf(stdout, "unrated %s (listed, never routed)\n", id)
	}
	if verr != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: registry is invalid:\n%v\n", verr)
		failed = true
	}
	rep := policy.Check(reg)
	hard := map[string]bool{}
	for _, k := range rep.Failures {
		hard[k] = true
	}
	for _, k := range rep.Empty {
		if hard[k] {
			_, _ = fmt.Fprintf(stdout, "empty %s  hard failure: no balanced candidate for review or advise\n", k)
		} else {
			_, _ = fmt.Fprintf(stdout, "empty %s\n", k)
		}
	}
	if rep.Failed() {
		failed = true
	}
	if failed {
		_, _ = fmt.Fprintf(stdout, "check: FAILED (%d empty cells, %d hard failures)\n", len(rep.Empty), len(rep.Failures))
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "check: ok (%d empty cells, 0 hard failures)\n", len(rep.Empty))
	return ExitOK
}

func runWrite(stdout, stderr io.Writer, reg *policy.Registry, path string) int {
	if rep := policy.Check(reg); rep.Failed() {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: refusing to write: %d balanced review/advise cells are empty; run --check\n", len(rep.Failures))
		return ExitError
	}
	data, err := policy.Marshal(reg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return ExitError
	}
	sum := parity.SHA256Hex(data)
	if err := writeFileAtomic(path, data); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: write registry: %v\n", err)
		return ExitError
	}
	pin := fmt.Sprintf("%s  %s\n", sum, filepath.Base(path))
	if err := writeFileAtomic(path+".sha256", []byte(pin)); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: write pin: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\nsha256 %s\n", path, sum)
	return ExitOK
}
