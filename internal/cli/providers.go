package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/provider"
)

const (
	keyPresent = "present"
	keyMissing = "missing"
)

// keyState reports whether a provider's credential file exists and is safe to
// read: present, missing, or insecure with its mode.
func keyState(dir, id string) string {
	st, err := os.Stat(config.CredentialPath(dir, id))
	switch {
	case err != nil || st.IsDir():
		return keyMissing
	case st.Mode().Perm()&0o077 != 0:
		return fmt.Sprintf("insecure (%04o)", st.Mode().Perm())
	}
	return keyPresent
}

// Providers implements `mgw providers add|list|test`.
func Providers(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "mgw providers: want a subcommand: add, list or test")
		return ExitUsage
	}
	switch args[0] {
	case "add":
		return providersAdd(args[1:], stdout, stderr, deps)
	case "list":
		return providersList(args[1:], stdout, stderr, deps)
	case "test":
		return providersTest(args[1:], stdout, stderr, deps)
	default:
		_, _ = fmt.Fprintf(stderr, "mgw providers: unknown subcommand %q (want add, list or test)\n", args[0])
		return ExitUsage
	}
}

// loadInstall resolves the config directory and loads the config.
func loadInstall(deps Deps) (string, *config.Config, error) {
	dir, err := deps.configDir()
	if err != nil {
		return "", nil, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return dir, nil, err
	}
	return dir, cfg, nil
}

func providersList(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("providers list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	dir, cfg, err := loadInstall(deps)
	if err != nil {
		return fail(stderr, "providers list", err)
	}
	rows := [][]string{{"ID", "DIALECT", "BILLING", "KEY", "PROBE"}}
	for _, p := range cfg.Providers {
		probe := "-"
		if p.Probe != nil {
			probe = fmt.Sprintf("auth=%s inference=%s stream_usage=%s tool_call=%s", p.Probe.Auth, p.Probe.Inference, p.Probe.StreamUsage, p.Probe.ToolCall)
			if p.ToolGrade != "" {
				probe += " grade=" + p.ToolGrade
			}
		}
		rows = append(rows, []string{p.ID, string(p.Dialect), p.Billing, keyState(dir, p.ID), probe})
	}
	printTable(stdout, rows)
	return ExitOK
}

func providersTest(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("providers test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("model", "", "upstream model name to probe (default: the provider's cheapest curated model)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() == 0 {
		return fail(stderr, "providers test", config.Fail(config.CodeMissingValue, "a provider id is required: mgw providers test <provider>"))
	}
	id := fs.Arg(0)
	if fs.NArg() > 1 { // flags after the provider id
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return ExitUsage
		}
		if fs.NArg() > 0 {
			return fail(stderr, "providers test", config.Fail(config.CodeBadValue, "unexpected argument %q", fs.Arg(0)))
		}
	}
	dir, cfg, err := loadInstall(deps)
	if err != nil {
		return fail(stderr, "providers test", err)
	}
	p, ok := cfg.Find(id)
	if !ok {
		return fail(stderr, "providers test", config.Fail(config.CodeUnknownProvider, "provider %q is not connected (connected: %s)", id, strings.Join(cfg.ProviderIDs(), ", ")))
	}
	key, err := config.ReadCredential(dir, id)
	if err != nil {
		return fail(stderr, "providers test", err)
	}
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return fail(stderr, "providers test", err)
	}
	res := provider.Probe(context.Background(), provider.ProbeTarget{
		ID: id, Dialect: p.Dialect, BaseURL: p.BaseURL, Key: key,
		Model: probeModel(caps, id, *model), IncludeStreamUsage: p.IncludeStreamUsage,
	})
	_, _ = fmt.Fprintf(stdout, "provider %s\n", id)
	for _, s := range []struct {
		name string
		step provider.ProbeStep
	}{{"auth", res.Auth}, {"inference", res.Inference}, {"stream_usage", res.StreamUsage}, {"tool_call", res.ToolCall}} {
		detail := s.step.Detail
		if detail == "" {
			detail = "-"
		}
		_, _ = fmt.Fprintf(stdout, "  %-13s%-8s%s\n", s.name, s.step.Result, detail)
	}
	applyProbe(p, res)
	if err := config.Save(dir, cfg); err != nil {
		return fail(stderr, "providers test", fmt.Errorf("store the result: %w", err))
	}
	switch {
	case !res.Usable():
		_, _ = fmt.Fprintf(stderr, "mgw providers test: provider %s failed: authentication and inference must both pass\n", id)
		return ExitError
	case res.ToolGrade == "":
		_, _ = fmt.Fprintln(stdout, "tool_grade not graded")
	default:
		_, _ = fmt.Fprintf(stdout, "tool_grade %s\n", res.ToolGrade)
	}
	return ExitOK
}
