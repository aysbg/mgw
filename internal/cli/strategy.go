package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
)

// pinFlag collects repeated --pin role=provider/model values.
type pinFlag struct {
	m   map[string]string
	bad *[]string
}

func (pinFlag) String() string { return "" }
func (f pinFlag) Set(v string) error {
	role, model, ok := strings.Cut(v, "=")
	if !ok || role == "" || model == "" {
		*f.bad = append(*f.bad, fmt.Sprintf("--pin %q: want role=provider/model", v))
		return nil
	}
	f.m[role] = model
	return nil
}

// strategyFlags are the flags `strategy set` stores and `strategy show` and
// `matrix` treat as dispatch-time overrides.
type strategyFlags struct {
	strategy, prefer, deny, dataClass string
	pins                              map[string]string
	bad                               []string
	given                             map[string]bool
}

func newStrategyFlags(fs *flag.FlagSet) *strategyFlags {
	sf := &strategyFlags{pins: map[string]string{}, given: map[string]bool{}}
	fs.StringVar(&sf.strategy, "strategy", "", "cheapest, balanced or best")
	fs.StringVar(&sf.prefer, "prefer", "", "comma-separated provider ids, most preferred first")
	fs.StringVar(&sf.deny, "deny", "", "comma-separated provider or model ids that must never be routed")
	fs.StringVar(&sf.dataClass, "data-class", "", "internal or customer")
	fs.Var(pinFlag{sf.pins, &sf.bad}, "pin", "role=provider/model (repeatable)")
	return sf
}

func (sf *strategyFlags) visit(fs *flag.FlagSet) {
	fs.Visit(func(f *flag.Flag) { sf.given[f.Name] = true })
}

// flags converts the given flags to dispatch overrides.
func (sf *strategyFlags) flags() config.Flags {
	var f config.Flags
	if sf.given["strategy"] {
		s := sf.strategy
		f.Strategy = &s
	}
	if sf.given["data-class"] {
		d := sf.dataClass
		f.DataClass = &d
	}
	if sf.given["prefer"] {
		f.Order = append([]string{}, splitList(sf.prefer)...)
	}
	if sf.given["deny"] {
		f.Deny = append([]string{}, splitList(sf.deny)...)
	}
	if len(sf.pins) > 0 {
		f.Pins = sf.pins
	}
	return f
}

// Strategy implements `mgw strategy set|show`.
func Strategy(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "mgw strategy: want a subcommand: set or show")
		return ExitUsage
	}
	switch args[0] {
	case "set":
		return strategySet(args[1:], stdout, stderr, deps)
	case "show":
		return strategyShow(args[1:], stdout, stderr, deps)
	default:
		_, _ = fmt.Fprintf(stderr, "mgw strategy: unknown subcommand %q (want set or show)\n", args[0])
		return ExitUsage
	}
}

func strategySet(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("strategy set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := newStrategyFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	sf.visit(fs)
	if len(sf.bad) > 0 {
		return fail(stderr, "strategy set", config.Fail(config.CodeBadValue, "%s", strings.Join(sf.bad, "; ")))
	}
	if len(sf.given) == 0 {
		return fail(stderr, "strategy set", config.Fail(config.CodeMissingValue, "nothing to set: pass --strategy, --prefer, --deny, --data-class or --pin"))
	}
	dir, cfg, err := loadInstall(deps)
	if err != nil {
		return fail(stderr, "strategy set", err)
	}
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return fail(stderr, "strategy set", err)
	}
	next := cfg.Clone()
	if sf.given["strategy"] {
		if err := config.CheckStrategy(sf.strategy); err != nil {
			return fail(stderr, "strategy set", err)
		}
		next.Strategy = sf.strategy
	}
	if sf.given["data-class"] {
		if err := config.CheckDataClass(sf.dataClass); err != nil {
			return fail(stderr, "strategy set", err)
		}
		next.DataClass = sf.dataClass
	}
	if sf.given["prefer"] {
		order := splitList(sf.prefer)
		for _, id := range order {
			if _, ok := next.Find(id); !ok {
				return fail(stderr, "strategy set", config.Fail(config.CodeUnknownProvider, "--prefer names %q, which is not connected", id))
			}
		}
		next.ProviderPolicy.Order = order
	}
	if sf.given["deny"] {
		deny := splitList(sf.deny)
		if err := config.CheckDeny(deny, caps); err != nil {
			return fail(stderr, "strategy set", err)
		}
		next.ProviderPolicy.Deny = deny
	}
	if err := config.CheckPolicyConflict(next.ProviderPolicy.Order, next.ProviderPolicy.Deny); err != nil {
		return fail(stderr, "strategy set", err)
	}
	if len(sf.pins) > 0 && next.Pins == nil {
		next.Pins = map[string]string{}
	}
	for role, model := range sf.pins {
		next.Pins[role] = model
	}
	// Every pin, new or old, must still pass admission under the new policy.
	for role, model := range next.Pins {
		if err := config.CheckPin(role, model, next.Providers, caps, next.ProviderPolicy.Deny); err != nil {
			return fail(stderr, "strategy set", err)
		}
	}
	if err := config.Save(dir, next); err != nil {
		return fail(stderr, "strategy set", err)
	}
	eff := config.Resolve(next, nil, config.Flags{})
	for _, l := range eff.SourceLines() {
		_, _ = fmt.Fprintln(stdout, l)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\nrun `mgw matrix --write` to pin a registry for this policy\n", config.ConfigPath(dir))
	return ExitOK
}

func strategyShow(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("strategy show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := newStrategyFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	sf.visit(fs)
	if len(sf.bad) > 0 {
		return fail(stderr, "strategy show", config.Fail(config.CodeBadValue, "%s", strings.Join(sf.bad, "; ")))
	}
	dir, cfg, err := loadInstall(deps)
	if err != nil {
		return fail(stderr, "strategy show", err)
	}
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return fail(stderr, "strategy show", err)
	}
	eff, profile, err := resolveEffective(dir, cfg, deps, caps, sf.flags())
	if err != nil {
		return fail(stderr, "strategy show", err)
	}
	_, _ = fmt.Fprintf(stdout, "config dir: %s\nprofile: %s\n", dir, orNone(profile))
	for _, l := range eff.SourceLines() {
		_, _ = fmt.Fprintln(stdout, l)
	}
	return ExitOK
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// resolveEffective applies the precedence (dispatch flags, engagement profile,
// install default) and checks the result: the flag values, the profile against
// the install, and every effective pin through admission. It returns the path
// of the profile in use, empty when there is none. cfg may be nil only when the
// caller has no install and is using explicit flags.
func resolveEffective(dir string, cfg *config.Config, deps Deps, caps *catalog.Capabilities, flags config.Flags) (config.Effective, string, error) {
	if flags.Strategy != nil {
		if err := config.CheckStrategy(*flags.Strategy); err != nil {
			return config.Effective{}, "", err
		}
	}
	if flags.DataClass != nil {
		if err := config.CheckDataClass(*flags.DataClass); err != nil {
			return config.Effective{}, "", err
		}
	}
	if err := config.CheckPolicyConflict(flags.Order, flags.Deny); err != nil {
		return config.Effective{}, "", err
	}
	if err := config.CheckDeny(flags.Deny, caps); err != nil {
		return config.Effective{}, "", err
	}
	if cfg != nil {
		for _, id := range flags.Order {
			if _, ok := cfg.Find(id); !ok {
				return config.Effective{}, "", config.Fail(config.CodeUnknownProvider, "order names %q, which is not connected", id)
			}
		}
	}
	for role, model := range flags.Pins {
		if err := config.CheckPinShape(role, model); err != nil {
			return config.Effective{}, "", err
		}
	}

	var profile *config.Profile
	path := ""
	if dir != "" || deps.Profile != "" {
		var err error
		if path, err = config.ProfilePath(dir, cfg, deps.Profile); err != nil {
			return config.Effective{}, "", err
		}
	}
	if path != "" {
		var err error
		if profile, err = config.LoadProfile(path); err != nil {
			return config.Effective{}, "", err
		}
		if cfg != nil {
			if err := profile.CheckAgainst(cfg, caps); err != nil {
				return config.Effective{}, "", fmt.Errorf("profile %s: %w", path, err)
			}
		} else if profile.ProviderPolicy != nil {
			if err := config.CheckDeny(profile.ProviderPolicy.Deny, caps); err != nil {
				return config.Effective{}, "", fmt.Errorf("profile %s: %w", path, err)
			}
		}
	}
	eff := config.Resolve(cfg, profile, flags)
	if cfg != nil {
		for role, model := range eff.Pins {
			if err := config.CheckPin(role, model, cfg.Providers, caps, eff.Deny); err != nil {
				return config.Effective{}, "", fmt.Errorf("%s: %w", eff.Sources["pin."+role], err)
			}
		}
	}
	return eff, path, nil
}
