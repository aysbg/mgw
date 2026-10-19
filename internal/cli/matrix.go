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
	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Matrix implements `mgw matrix`. It prints, explains, checks or writes the
// decision table for one install. The table is ordered candidates, never
// authorization. The install comes from the config directory; --providers and
// --deny override it for one run, and an engagement profile (--profile before
// the command) sits between the flags and the install default.
func Matrix(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("matrix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strategy := fs.String("strategy", "balanced", "print one strategy: cheapest, balanced or best")
	all := fs.Bool("all", false, "print the three strategies side by side with blended price")
	explain := fs.String("explain", "", "explain one cell: <role>|<type>|<size>|<risk>|<strategy>")
	write := fs.Bool("write", false, "write the pinned registry to the output path")
	check := fs.Bool("check", false, "name empty cells and hard failures")
	providers := fs.String("providers", "", "comma-separated installed providers in preference order (default: the connected providers)")
	deny := fs.String("deny", "", "comma-separated provider ids or model ids that must never be routed")
	snapshot := fs.String("snapshot", deps.defaultSnapshot(), "pricing snapshot path")
	out := fs.String("out", deps.defaultRegistry(), "registry output path for --write")
	allowUngraded := fs.Bool("allow-ungraded-build", false, "with --write, accept a provider whose tool_grade is fail serving build")
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
	if explicit["allow-ungraded-build"] && !*write {
		_, _ = fmt.Fprintln(stderr, "mgw matrix: --allow-ungraded-build only applies to --write")
		return ExitUsage
	}
	if *explain != "" {
		if _, err := policy.ParseCell(*explain); err != nil {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: invalid cell %q: %v\n", *explain, err)
			return ExitUsage
		}
	}

	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return fail(stderr, "matrix", err)
	}
	// The install. A missing config is a named failure unless the providers are
	// given on the command line.
	dir, cfg, err := loadInstall(deps)
	if err != nil {
		var f *config.Failure
		if !explicit["providers"] || !errors.As(err, &f) || f.Code != config.CodeMissingConfig {
			return fail(stderr, "matrix", err)
		}
		cfg = nil
	}
	var flags config.Flags
	if explicit["strategy"] {
		flags.Strategy = strategy
	}
	if explicit["providers"] {
		flags.Order = append([]string{}, splitList(*providers)...)
	}
	if explicit["deny"] {
		flags.Deny = append([]string{}, splitList(*deny)...)
	}
	eff, profilePath, err := resolveEffective(dir, cfg, deps, caps, flags)
	if err != nil {
		return fail(stderr, "matrix", err)
	}
	var names []string
	if explicit["providers"] {
		names = splitList(*providers)
	} else {
		names = cfg.ProviderIDs()
	}
	var connected []config.Provider
	if cfg != nil {
		connected = cfg.Providers
	}
	installed, err := installedProviders(caps, names, connected)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return ExitUsage
	}
	pref := eff.Order
	if len(pref) == 0 {
		pref = names
	}
	var notes []string
	if cfg != nil || profilePath != "" {
		for _, l := range eff.SourceLines() {
			notes = append(notes, "# effective "+l)
		}
	}

	reg, code := buildRegistry(stderr, *snapshot, caps, installed, pref, eff.Deny)
	if reg == nil {
		return code
	}
	verr := policy.Validate(reg)
	issues := gradeIssues(reg, connected)
	for _, i := range issues {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: provider %s has tool_grade fail and would serve build in %d cells\n", i.Provider, i.Cells)
	}

	switch {
	case *check:
		return runCheck(stdout, stderr, reg, verr)
	case verr != nil:
		_, _ = fmt.Fprintf(stderr, "mgw matrix: registry is invalid:\n%v\n", verr)
		return ExitError
	case *write:
		if len(issues) > 0 && !*allowUngraded {
			_, _ = fmt.Fprintf(stderr, "mgw matrix: refusing to write: %s; pass --allow-ungraded-build to accept\n", describeIssues(issues))
			return ExitError
		}
		return runWrite(stdout, stderr, reg, *out, deps.Build)
	case *explain != "":
		return runExplain(stdout, stderr, reg, *explain)
	case *all:
		printAll(stdout, reg, notes)
	default:
		printOne(stdout, reg, eff.Strategy, notes...)
	}
	if len(reg.Unrated) > 0 {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %d unrated models are never routed: %s\n", len(reg.Unrated), strings.Join(reg.Unrated, ", "))
	}
	return ExitOK
}

// installedProviders lists the named providers with their billing: the
// connected provider's own billing when there is one, else the curated default.
func installedProviders(caps *catalog.Capabilities, names []string, connected []config.Provider) ([]policy.ProviderPolicy, error) {
	var installed []policy.ProviderPolicy
	for _, name := range names {
		if i := slices.IndexFunc(connected, func(p config.Provider) bool { return p.ID == name }); i >= 0 {
			installed = append(installed, policy.ProviderPolicy{ID: name, Billing: connected[i].Billing})
			continue
		}
		i := slices.IndexFunc(caps.Providers, func(p catalog.CapProvider) bool { return p.ID == name })
		if i < 0 {
			return nil, fmt.Errorf("unknown provider %q", name)
		}
		installed = append(installed, policy.ProviderPolicy{ID: name, Billing: caps.Providers[i].Billing})
	}
	return installed, nil
}

// buildRegistry loads the snapshot and builds the registry for the installed
// providers. On failure it returns nil and an exit code.
func buildRegistry(stderr io.Writer, snapshotPath string, caps *catalog.Capabilities, installed []policy.ProviderPolicy, pref, deny []string) (*policy.Registry, int) {
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
	reg, err := buildFromSnapshot(caps, snap, installed, pref, deny)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: %v\n", err)
		return nil, ExitError
	}
	return reg, ExitOK
}

// buildFromSnapshot builds the registry of one install. The spend order is pref;
// a provider it leaves out sorts after the listed ones.
func buildFromSnapshot(caps *catalog.Capabilities, snap catalog.Snapshot, installed []policy.ProviderPolicy, pref, deny []string) (*policy.Registry, error) {
	return policy.Build(caps, snap, policy.BuildOptions{Providers: installed, Preference: pref, Deny: deny, Prices: policy.PricesCatalog})
}

// gradeIssue is a provider whose tool grade is fail and that the decision table
// would still place in build.
type gradeIssue struct {
	Provider string
	Cells    int
}

// gradeIssues finds the tool_grade fail providers that serve any build cell.
func gradeIssues(reg *policy.Registry, providers []config.Provider) []gradeIssue {
	var out []gradeIssue
	for _, p := range providers {
		if p.ToolGrade != provider.GradeFail {
			continue
		}
		cells := 0
		for key, ids := range reg.DecisionTable {
			if !strings.HasPrefix(key, "build|") {
				continue
			}
			if slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, p.ID+"/") }) {
				cells++
			}
		}
		if cells > 0 {
			out = append(out, gradeIssue{Provider: p.ID, Cells: cells})
		}
	}
	return out
}

func describeIssues(issues []gradeIssue) string {
	parts := make([]string, len(issues))
	for i, g := range issues {
		parts[i] = fmt.Sprintf("provider %s has tool_grade fail and would serve build in %d cells", g.Provider, g.Cells)
	}
	return strings.Join(parts, "; ")
}

func providerList(reg *policy.Registry) string {
	ids := make([]string, len(reg.Providers))
	for i, p := range reg.Providers {
		ids[i] = p.ID
	}
	return strings.Join(ids, ",")
}

func header(w io.Writer, reg *policy.Registry, strategy string, notes []string) {
	hash, _ := policy.RegistryHash(reg)
	if len(hash) > 12 {
		hash = hash[:12]
	}
	_, _ = fmt.Fprintf(w, "# mgw matrix  policy %s  registry %s  providers %s\n", reg.PolicyVersion, hash, providerList(reg))
	_, _ = fmt.Fprintf(w, "# strategy %s  blend %s  prices are blended USD per 1M tokens\n", strategy, catalog.BlendLabel)
	for _, n := range notes {
		_, _ = fmt.Fprintln(w, n)
	}
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

func printOne(w io.Writer, reg *policy.Registry, strategy string, notes ...string) {
	header(w, reg, strategy, notes)
	rows := [][]string{{"ROLE", "TYPE", "SIZE", "RISK", "CANDIDATES"}}
	for _, c := range policy.Cells() {
		if c.Strategy != strategy {
			continue
		}
		rows = append(rows, []string{c.Role, c.Type, c.Size, fmt.Sprint(c.Risk), candidates(reg, c.Key(), false, 0)})
	}
	printTable(w, rows)
}

func printAll(w io.Writer, reg *policy.Registry, notes []string) {
	header(w, reg, "all (first candidate of each; price is USD per 1M tokens)", notes)
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

func runWrite(stdout, stderr io.Writer, reg *policy.Registry, path string, build BuildInfo) int {
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
	if err := writePublicFile(path, data); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: write registry: %v\n", err)
		return ExitError
	}
	pin := fmt.Sprintf("%s  %s\n", sum, filepath.Base(path))
	if err := writePublicFile(path+".sha256", []byte(pin)); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: write pin: %v\n", err)
		return ExitError
	}
	if err := writeRegistryBuild(path, sum, build); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw matrix: write build pin: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\nsha256 %s\n", path, sum)
	return ExitOK
}
