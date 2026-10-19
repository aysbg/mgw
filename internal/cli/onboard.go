package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// providerSpec is one provider named on the command line or at a prompt. Key
// holds the credential value in memory only; it is never printed.
type providerSpec struct {
	ID      string
	KeyFile string
	Billing string
	Key     string
}

// specCollector records --provider flags in order, and attaches --key-file and
// --billing to the --provider that precedes them.
type specCollector struct {
	specs []*providerSpec
	bad   []string
}

type providerFlag struct{ c *specCollector }

func (providerFlag) String() string { return "" }
func (f providerFlag) Set(v string) error {
	f.c.specs = append(f.c.specs, &providerSpec{ID: strings.TrimSpace(v)})
	return nil
}

type groupFlag struct {
	c    *specCollector
	name string
}

func (groupFlag) String() string { return "" }
func (f groupFlag) Set(v string) error {
	if len(f.c.specs) == 0 {
		f.c.bad = append(f.c.bad, "--"+f.name+" must follow the --provider it belongs to")
		return nil
	}
	last := f.c.specs[len(f.c.specs)-1]
	target := &last.Billing
	if f.name == "key-file" {
		target = &last.KeyFile
	}
	if *target != "" {
		f.c.bad = append(f.c.bad, fmt.Sprintf("--%s given twice for provider %s", f.name, last.ID))
		return nil
	}
	*target = v
	return nil
}

type baseURLFlag struct {
	m   map[string]string
	bad *[]string
}

func (baseURLFlag) String() string { return "" }
func (f baseURLFlag) Set(v string) error {
	id, url, ok := strings.Cut(v, "=")
	if !ok || id == "" || url == "" {
		*f.bad = append(*f.bad, "--base-url wants <provider>=<url>")
		return nil
	}
	f.m[id] = url
	return nil
}

// connectOpts are the parsed flags of `mgw init` and `mgw providers add`.
type connectOpts struct {
	add           bool
	cmd           string
	col           specCollector
	baseURLs      map[string]string
	bad           []string
	strategy      string
	prefer        string
	deny          string
	dataClass     string
	given         map[string]bool
	yes           bool
	allowUngraded bool
	catalogURL    string
	catalogSHA    string
}

// keyFlagNames are flags that would carry a credential value on the command
// line. They are refused outright and their value is never echoed.
var keyFlagNames = map[string]bool{"key": true, "api-key": true, "apikey": true, "token": true, "secret": true, "password": true}

func argvCarriesKey(args []string) (string, bool) {
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if keyFlagNames[name] {
			return "--" + name, true
		}
	}
	return "", false
}

// Init implements `mgw init`.
func Init(args []string, stdout, stderr io.Writer, deps Deps) int {
	return connectCmd(false, args, stdout, stderr, deps)
}

func providersAdd(args []string, stdout, stderr io.Writer, deps Deps) int {
	return connectCmd(true, args, stdout, stderr, deps)
}

func connectCmd(add bool, args []string, stdout, stderr io.Writer, deps Deps) int {
	o := &connectOpts{add: add, cmd: "init", baseURLs: map[string]string{}, given: map[string]bool{}}
	if add {
		o.cmd = "providers add"
	}
	if name, bad := argvCarriesKey(args); bad {
		return fail(stderr, o.cmd, config.Fail(config.CodeKeyInArgv,
			"%s is not accepted: a key on the command line lands in the process list and shell history; use --key-file PATH", name))
	}
	fs := flag.NewFlagSet(o.cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(providerFlag{&o.col}, "provider", "provider id to connect (repeatable); --key-file and --billing apply to the one before them")
	fs.Var(groupFlag{&o.col, "key-file"}, "key-file", "file holding the provider's key; a key is never accepted as an argument")
	fs.Var(groupFlag{&o.col, "billing"}, "billing", "billing shape of the provider: pay_per_token or included_credits")
	fs.Var(baseURLFlag{o.baseURLs, &o.bad}, "base-url", "<provider>=<url> override of a provider's base URL (repeatable)")
	fs.BoolVar(&o.yes, "yes", false, "write without asking for confirmation")
	fs.BoolVar(&o.allowUngraded, "allow-ungraded-build", false, "write a registry that places a tool_grade fail provider in build")
	fs.StringVar(&o.catalogURL, "catalog-url", defaultCatalogURL, "catalog URL")
	fs.StringVar(&o.catalogSHA, "catalog-sha256", "", "pinned sha256 of the catalog body; without it the digest is recorded and printed")
	if !add {
		fs.StringVar(&o.strategy, "strategy", "", "cheapest, balanced or best")
		fs.StringVar(&o.prefer, "prefer", "", "comma-separated provider ids, most preferred first")
		fs.StringVar(&o.deny, "deny", "", "comma-separated provider or model ids that must never be routed")
		fs.StringVar(&o.dataClass, "data-class", "", "internal or customer")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	fs.Visit(func(f *flag.Flag) { o.given[f.Name] = true })
	if fs.NArg() > 0 {
		return fail(stderr, o.cmd, config.Fail(config.CodeBadValue, "unexpected argument %q", fs.Arg(0)))
	}
	bad := append(slices.Clone(o.col.bad), o.bad...)
	if len(bad) > 0 {
		return fail(stderr, o.cmd, config.Fail(config.CodeBadValue, "%s", strings.Join(bad, "; ")))
	}
	if err := connect(o, stdout, deps); err != nil {
		return fail(stderr, o.cmd, err)
	}
	return ExitOK
}

// promptFlagGiven reports whether any question already has a flag. Then the run
// is non-interactive, even on a terminal.
func (o *connectOpts) promptFlagGiven() bool {
	for _, n := range []string{"provider", "key-file", "billing", "strategy", "prefer", "deny", "data-class"} {
		if o.given[n] {
			return true
		}
	}
	return false
}

// connect runs init or providers add. Nothing is written until every check, the
// provider probes and the registry build have passed.
func connect(o *connectOpts, stdout io.Writer, deps Deps) error {
	dir, err := deps.configDir()
	if err != nil {
		return err
	}
	existing, err := config.Load(dir)
	if err != nil {
		var f *config.Failure
		if !errors.As(err, &f) || f.Code != config.CodeMissingConfig || o.add {
			return err
		}
		existing = nil
	}
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return err
	}

	interactive := !o.promptFlagGiven() && deps.StdinIsTTY != nil && deps.StdinIsTTY()
	if interactive {
		p := &prompter{in: deps.Stdin, out: stdout, setEcho: deps.SetEcho}
		if err := o.askAll(p, existing, caps); err != nil {
			return err
		}
	}

	next, err := o.resolve(existing, caps, dir)
	if err != nil {
		return err
	}
	o.printPlan(stdout, dir, existing, next)

	if !o.yes {
		if !interactive {
			_, _ = fmt.Fprintln(stdout, "dry run; pass --yes to write these files")
			return nil
		}
		p := &prompter{in: deps.Stdin, out: stdout}
		ans, err := p.ask("Write these files? [y/N]: ", "n", nil)
		if err != nil {
			return err
		}
		if a := strings.ToLower(ans); a != "y" && a != "yes" {
			return config.Fail(config.CodeAborted, "nothing was written")
		}
	}

	// Validate every new key against its provider before anything is written.
	for _, s := range o.col.specs {
		if s.Key == "" {
			continue
		}
		pv, _ := next.Find(s.ID)
		res := provider.Probe(context.Background(), provider.ProbeTarget{
			ID: s.ID, Dialect: pv.Dialect, BaseURL: pv.BaseURL, Key: s.Key,
			Model: probeModel(caps, s.ID, ""), IncludeStreamUsage: pv.IncludeStreamUsage,
		})
		_, _ = fmt.Fprintf(stdout, "probe %s: auth %s, inference %s, stream_usage %s, tool_call %s\n",
			s.ID, res.Auth.Result, res.Inference.Result, res.StreamUsage.Result, res.ToolCall.Result)
		if !res.Usable() {
			step, detail := "authentication", res.Auth.Detail
			if res.Auth.OK() {
				step, detail = "inference", res.Inference.Detail
			}
			return config.Fail(config.CodeProviderFailed, "provider %s: %s failed: %s; nothing was written", s.ID, step, detail)
		}
		applyProbe(pv, res)
	}

	snap, snapData, err := fetchSnapshot(deps, o.catalogURL, o.catalogSHA)
	if err != nil {
		return err
	}
	eff := config.Resolve(next, nil, config.Flags{})
	reg, err := buildFromSnapshot(caps, snap, installedFrom(next.Providers), eff.Order, eff.Deny)
	if err != nil {
		return err
	}
	if err := policy.Validate(reg); err != nil {
		return fmt.Errorf("registry is invalid; nothing was written:\n%w", err)
	}
	if rep := policy.Check(reg); rep.Failed() {
		return fmt.Errorf("registry has %d empty balanced review/advise cells; nothing was written; run `mgw matrix --check` after adjusting the policy", len(rep.Failures))
	}
	for role, model := range next.Pins {
		if err := config.CheckPin(role, model, next.Providers, caps, eff.Deny); err != nil {
			return err
		}
	}
	if issues := gradeIssues(reg, next.Providers); len(issues) > 0 {
		for _, i := range issues {
			_, _ = fmt.Fprintf(stdout, "warn: provider %s has tool_grade fail and serves build in %d cells\n", i.Provider, i.Cells)
		}
		if !o.allowUngraded {
			return config.Fail(config.CodeToolGradeFail, "%s; nothing was written; pass --allow-ungraded-build to accept", describeIssues(issues))
		}
	}

	if err := config.Save(dir, next); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\n", config.ConfigPath(dir))
	for _, s := range o.col.specs {
		if s.Key == "" {
			continue
		}
		if err := config.WriteCredential(dir, s.ID, s.Key); err != nil {
			return fmt.Errorf("write credential for %s: %w", s.ID, err)
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", config.CredentialPath(dir, s.ID))
	}
	if err := writePublicFile(config.PricingPath(dir), snapData); err != nil {
		return fmt.Errorf("write pricing snapshot: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\ncatalog sha256 %s", config.PricingPath(dir), snap.SourceSHA256)
	if o.catalogSHA == "" {
		_, _ = fmt.Fprint(stdout, " (not pinned; pass --catalog-sha256 to pin it)")
	}
	_, _ = fmt.Fprintln(stdout)
	var werr bytes.Buffer
	if code := runWrite(stdout, &werr, reg, config.RegistryPath(dir), deps.Build); code != ExitOK {
		return fmt.Errorf("write registry: %s", strings.TrimSpace(werr.String()))
	}
	_, _ = fmt.Fprintln(stdout)
	printOne(stdout, reg, eff.Strategy)
	return nil
}

// applyProbe stores a probe outcome on the provider. The tool grade changes
// only when the fidelity probe ran.
func applyProbe(p *config.Provider, res provider.ProbeResult) {
	p.Probe = &config.ProbeRecord{
		Auth: res.Auth.Result, Inference: res.Inference.Result,
		StreamUsage: res.StreamUsage.Result, ToolCall: res.ToolCall.Result,
	}
	if res.ToolGrade != "" {
		p.ToolGrade = res.ToolGrade
	}
}

// probeModel picks the model name a probe uses: the override, or the cheapest
// curated seat of the provider.
func probeModel(caps *catalog.Capabilities, id, override string) string {
	if override != "" {
		return override
	}
	best, bestTier := "", len(policy.Tiers)
	for _, m := range caps.Models {
		if m.Provider != id {
			continue
		}
		if t := slices.Index(policy.Tiers, m.Tier); t >= 0 && t < bestTier {
			best, bestTier = m.ID, t
		}
	}
	return strings.TrimPrefix(best, id+"/")
}

func installedFrom(ps []config.Provider) []policy.ProviderPolicy {
	out := make([]policy.ProviderPolicy, len(ps))
	for i, p := range ps {
		out[i] = policy.ProviderPolicy{ID: p.ID, Billing: p.Billing}
	}
	return out
}

// fetchSnapshot downloads the catalog and builds the pricing snapshot. The
// body is verified against a pin when one is given.
func fetchSnapshot(deps Deps, url, pin string) (catalog.Snapshot, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	body, sum, err := deps.Fetch(ctx, url)
	if err != nil {
		return catalog.Snapshot{}, nil, fmt.Errorf("catalog fetch failed; nothing was written: %w", err)
	}
	if pin != "" {
		if err := catalog.VerifyDigest(sum, pin); err != nil {
			return catalog.Snapshot{}, nil, fmt.Errorf("catalog digest does not match the pin; nothing was written: %w", err)
		}
	}
	c, err := catalog.Decode(body)
	if err != nil {
		return catalog.Snapshot{}, nil, err
	}
	snap := catalog.NewSnapshot(c, url, sum, deps.Now())
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return catalog.Snapshot{}, nil, err
	}
	return snap, append(data, '\n'), nil
}

// resolve merges the flags (or the interactive answers) into the existing
// install and validates the result. It reads each key file; it touches no
// network and writes nothing.
func (o *connectOpts) resolve(existing *config.Config, caps *catalog.Capabilities, dir string) (*config.Config, error) {
	fresh := existing == nil
	next := config.New()
	if !fresh {
		next = existing.Clone()
	}
	if len(o.col.specs) == 0 && (fresh || o.add) {
		return nil, config.Fail(config.CodeMissingValue, "--provider is required")
	}
	seen := map[string]bool{}
	for _, s := range o.col.specs {
		if seen[s.ID] {
			return nil, config.Fail(config.CodeBadValue, "provider %q is given twice", s.ID)
		}
		seen[s.ID] = true
		dialect, baseURL, ok := config.Endpoint(s.ID)
		if !ok || !slices.ContainsFunc(caps.Providers, func(p catalog.CapProvider) bool { return p.ID == s.ID }) {
			return nil, config.Fail(config.CodeUnknownProvider, "unknown provider %q (want %s)", s.ID, strings.Join(capProviderIDs(caps), ", "))
		}
		cur, connected := next.Find(s.ID)
		if s.Billing == "" {
			if !connected {
				return nil, config.Fail(config.CodeMissingValue, "--billing for provider %s is required", s.ID)
			}
			s.Billing = cur.Billing
		}
		if err := config.CheckBilling(s.Billing); err != nil {
			return nil, err
		}
		keyStored := connected && keyState(dir, s.ID) == keyPresent
		if s.Key == "" && s.KeyFile == "" && (o.add || !keyStored) {
			return nil, config.Fail(config.CodeMissingValue, "--key-file for provider %s is required", s.ID)
		}
		if s.KeyFile != "" {
			key, err := config.ReadKeyFile(s.ID, s.KeyFile)
			if err != nil {
				return nil, config.Fail(config.CodeKeyFile, "key file for provider %s: %v", s.ID, err)
			}
			s.Key = key
		}
		if u := o.baseURLs[s.ID]; u != "" {
			baseURL = u
		} else if connected {
			baseURL = cur.BaseURL
		}
		if err := provider.ValidateEndpoint(s.ID, dialect, baseURL); err != nil {
			return nil, config.Fail(config.CodeBadValue, "%v", err)
		}
		if connected {
			cur.Dialect, cur.BaseURL, cur.Billing = dialect, baseURL, s.Billing
		} else {
			next.Providers = append(next.Providers, config.Provider{
				ID: s.ID, Dialect: dialect, BaseURL: baseURL, Billing: s.Billing,
				IncludeStreamUsage: dialect != provider.DialectAnthropic,
			})
			if !o.add {
				next.ProviderPolicy.Order = append(next.ProviderPolicy.Order, s.ID)
			}
		}
	}
	for id := range o.baseURLs {
		if !seen[id] {
			return nil, config.Fail(config.CodeBadValue, "--base-url %s needs a --provider %s", id, id)
		}
	}
	if o.add {
		return validated(next)
	}

	switch {
	case o.given["strategy"]:
		if err := config.CheckStrategy(o.strategy); err != nil {
			return nil, err
		}
		next.Strategy = o.strategy
	case fresh:
		return nil, config.Fail(config.CodeMissingValue, "--strategy is required (cheapest, balanced or best)")
	}
	if o.given["data-class"] {
		if err := config.CheckDataClass(o.dataClass); err != nil {
			return nil, err
		}
		next.DataClass = o.dataClass
	}
	if o.given["prefer"] {
		order := splitList(o.prefer)
		for _, id := range order {
			if _, ok := next.Find(id); !ok {
				return nil, config.Fail(config.CodeUnknownProvider, "--prefer names %q, which is not connected", id)
			}
		}
		next.ProviderPolicy.Order = order
	}
	if o.given["deny"] {
		deny := splitList(o.deny)
		if err := config.CheckDeny(deny, caps); err != nil {
			return nil, err
		}
		next.ProviderPolicy.Deny = deny
	}
	return validated(next)
}

func validated(c *config.Config) (*config.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func capProviderIDs(caps *catalog.Capabilities) []string {
	ids := make([]string, len(caps.Providers))
	for i, p := range caps.Providers {
		ids[i] = p.ID
	}
	return ids
}

func (o *connectOpts) printPlan(w io.Writer, dir string, existing, next *config.Config) {
	_, _ = fmt.Fprintf(w, "plan: mgw %s\nconfig dir: %s\n", o.cmd, dir)
	named := map[string]*providerSpec{}
	for _, s := range o.col.specs {
		named[s.ID] = s
	}
	for _, p := range next.Providers {
		key := "kept"
		if s := named[p.ID]; s != nil && s.Key != "" {
			key = "new, written to " + strings.TrimPrefix(config.CredentialPath("", p.ID), "/")
		}
		_, _ = fmt.Fprintf(w, "provider %s  dialect %s  base_url %s  billing %s  key %s\n", p.ID, p.Dialect, p.BaseURL, p.Billing, key)
	}
	list := func(v []string) string {
		if len(v) == 0 {
			return "-"
		}
		return strings.Join(v, ",")
	}
	_, _ = fmt.Fprintf(w, "strategy %s\nprovider order %s\ndeny %s\ndata_class %s\n",
		next.Strategy, list(next.ProviderPolicy.Order), list(next.ProviderPolicy.Deny), next.DataClass)
	_, _ = fmt.Fprintln(w, "files: config.json (0600), credentials/<provider>.key (0600), pricing.json (0644), registry.json (0644), registry.json.sha256 (0644)")
	if existing != nil {
		_, _ = fmt.Fprintln(w, "existing providers are kept")
	}
}

// askAll runs the interactive questions in order: providers, each key (hidden),
// billing for each, then (init only) strategy, provider order, deny list and
// data class. The answers land in the same fields the flags fill.
func (o *connectOpts) askAll(p *prompter, existing *config.Config, caps *catalog.Capabilities) error {
	known := capProviderIDs(caps)
	validProviders := func(s string) error {
		ids := splitList(s)
		if len(ids) == 0 || (o.add && len(ids) != 1) {
			return config.Fail(config.CodeMissingValue, "name %s provider", map[bool]string{true: "one", false: "at least one"}[o.add])
		}
		for _, id := range ids {
			if !slices.Contains(known, id) {
				return config.Fail(config.CodeUnknownProvider, "unknown provider %q (want %s)", id, strings.Join(known, ", "))
			}
		}
		return nil
	}
	label := "Providers to connect"
	if o.add {
		label = "Provider to connect"
	}
	ans, err := p.ask(fmt.Sprintf("%s (%s; comma-separated): ", label, strings.Join(known, ", ")), "", validProviders)
	if err != nil {
		return err
	}
	for _, id := range splitList(ans) {
		o.col.specs = append(o.col.specs, &providerSpec{ID: id})
	}
	for _, s := range o.col.specs {
		if s.Key, err = p.secret(fmt.Sprintf("%s API key (input hidden): ", s.ID)); err != nil {
			return err
		}
	}
	for _, s := range o.col.specs {
		def := config.DefaultBilling(s.ID)
		if existing != nil {
			if cur, ok := existing.Find(s.ID); ok {
				def = cur.Billing
			}
		}
		if s.Billing, err = p.ask(fmt.Sprintf("Billing for %s (pay_per_token or included_credits) [%s]: ", s.ID, def), def, config.CheckBilling); err != nil {
			return err
		}
	}
	if o.add {
		return nil
	}

	connected := []string{}
	if existing != nil {
		connected = existing.ProviderIDs()
	}
	for _, s := range o.col.specs {
		if !slices.Contains(connected, s.ID) {
			connected = append(connected, s.ID)
		}
	}
	defStrategy, defOrder, defDeny, defClass := "balanced", strings.Join(connected, ","), "none", "internal"
	if existing != nil {
		defStrategy, defClass = existing.Strategy, existing.DataClass
		if len(existing.ProviderPolicy.Order) > 0 {
			defOrder = strings.Join(append(slices.Clone(existing.ProviderPolicy.Order), notIn(connected, existing.ProviderPolicy.Order)...), ",")
		}
		if len(existing.ProviderPolicy.Deny) > 0 {
			defDeny = strings.Join(existing.ProviderPolicy.Deny, ",")
		}
	}
	if o.strategy, err = p.ask(fmt.Sprintf("Strategy (cheapest, balanced or best) [%s]: ", defStrategy), defStrategy, config.CheckStrategy); err != nil {
		return err
	}
	if o.prefer, err = p.ask(fmt.Sprintf("Provider order, most preferred first [%s]: ", defOrder), defOrder, nil); err != nil {
		return err
	}
	if o.deny, err = p.ask(fmt.Sprintf("Deny list (provider or model ids, comma-separated) [%s]: ", defDeny), defDeny, nil); err != nil {
		return err
	}
	if o.deny == "none" {
		o.deny = ""
	}
	if o.dataClass, err = p.ask(fmt.Sprintf("Data class (internal or customer) [%s]: ", defClass), defClass, config.CheckDataClass); err != nil {
		return err
	}
	for _, n := range []string{"strategy", "prefer", "deny", "data-class"} {
		o.given[n] = true
	}
	return nil
}

func notIn(all, have []string) []string {
	var out []string
	for _, a := range all {
		if !slices.Contains(have, a) {
			out = append(out, a)
		}
	}
	return out
}
