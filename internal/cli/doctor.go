package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Doctor implements `mgw doctor`. Each check prints ok, warn or FAIL and any FAIL
// makes the exit code non-zero: the config directory and its credential files,
// the CA bundle, the pinned registry, the pricing snapshot age, the catalog
// (with --network), a tool_grade fail provider that would serve build, and the
// configured strategy. A warning does not fail the command.
func Doctor(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	snapshot := fs.String("snapshot", deps.defaultSnapshot(), "pricing snapshot path")
	registry := fs.String("registry", deps.defaultRegistry(), "pinned registry path")
	network := fs.Bool("network", false, "also check that the catalog is reachable")
	catalogURL := fs.String("catalog-url", defaultCatalogURL, "catalog URL for --network")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	var rows [][]string
	failed := false
	add := func(level, name, detail string) {
		rows = append(rows, []string{fmt.Sprintf("%-6s%s", level, name), detail})
		if level == "FAIL" {
			failed = true
		}
	}

	dir, dirErr := deps.configDir()
	var cfg *config.Config
	if dirErr != nil {
		add("FAIL", "config", dirErr.Error())
	} else {
		cfg = checkConfig(add, dir)
		if cfg != nil {
			checkCredentials(add, dir, cfg)
		}
	}
	checkCA(add, deps)
	checkRegistry(add, *registry)
	checkSnapshot(add, deps, *snapshot)
	if *network {
		checkReachable(add, deps, *catalogURL)
	}
	if cfg != nil {
		checkToolGrade(add, *registry, cfg)
		checkStrategy(add, dir, cfg, deps)
	}
	printTable(stdout, rows)
	if failed {
		return ExitError
	}
	return ExitOK
}

// checkConfig loads config.json for the report. It returns nil when there is
// nothing usable to check further. A bad strategy is left to checkStrategy.
func checkConfig(add func(level, name, detail string), dir string) *config.Config {
	cfg, err := config.LoadUnchecked(dir)
	if err != nil {
		add("FAIL", "config", err.Error())
		return nil
	}
	probe := cfg.Clone()
	probe.Strategy = "balanced"
	if err := probe.Validate(); err != nil {
		add("FAIL", "config", err.Error())
		return cfg
	}
	st, err := os.Stat(config.ConfigPath(dir))
	if err == nil && st.Mode().Perm()&0o077 != 0 {
		add("warn", "config", fmt.Sprintf("%s mode %04o, want 0600", config.ConfigPath(dir), st.Mode().Perm()))
		return cfg
	}
	add("ok", "config", config.ConfigPath(dir))
	return cfg
}

// checkCredentials requires every connected provider's key file to exist and
// every key file in credentials/ to be free of group and other access.
func checkCredentials(add func(level, name, detail string), dir string, cfg *config.Config) {
	var problems []string
	files := map[string]bool{}
	if entries, err := os.ReadDir(filepath.Join(dir, config.CredentialDir)); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".key") {
				files[strings.TrimSuffix(e.Name(), ".key")] = true
			}
		}
	}
	for _, p := range cfg.Providers {
		if !files[p.ID] {
			problems = append(problems, fmt.Sprintf("key file for provider %s is missing (%s)", p.ID, config.CredentialPath(dir, p.ID)))
		}
	}
	for id := range files {
		st, err := os.Stat(filepath.Join(dir, config.CredentialDir, id+".key"))
		if err != nil {
			problems = append(problems, err.Error())
		} else if st.Mode().Perm()&0o077 != 0 {
			problems = append(problems, fmt.Sprintf("%s/%s.key mode %04o, want 0600", config.CredentialDir, id, st.Mode().Perm()))
		}
	}
	slices.Sort(problems)
	if len(problems) > 0 {
		add("FAIL", "credentials", strings.Join(problems, "; "))
		return
	}
	add("ok", "credentials", fmt.Sprintf("%d key files, none readable by group or others", len(files)))
}

func checkReachable(add func(level, name, detail string), deps Deps, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, sum, err := deps.Fetch(ctx, url)
	if err != nil {
		add("FAIL", "catalog-reachability", err.Error())
		return
	}
	add("ok", "catalog-reachability", fmt.Sprintf("%s sha256 %s", url, sum))
}

// checkToolGrade fails when a provider with tool_grade fail would serve build in
// the pinned registry.
func checkToolGrade(add func(level, name, detail string), registryPath string, cfg *config.Config) {
	var failing []config.Provider
	for _, p := range cfg.Providers {
		if p.ToolGrade == provider.GradeFail {
			failing = append(failing, p)
		}
	}
	if len(failing) == 0 {
		add("ok", "tool-grade", "no provider has tool_grade fail")
		return
	}
	data, err := readFile(registryPath)
	var reg *policy.Registry
	if err == nil {
		reg, err = policy.DecodeRegistry(bytes.TrimSpace(data))
	}
	if err != nil {
		add("warn", "tool-grade", "a provider has tool_grade fail and no readable registry shows where it serves; run `mgw matrix --check`")
		return
	}
	if issues := gradeIssues(reg, failing); len(issues) > 0 {
		add("FAIL", "tool-grade", describeIssues(issues))
		return
	}
	add("ok", "tool-grade", "tool_grade fail providers serve no build cell")
}

// checkStrategy checks the effective strategy, after any engagement profile.
func checkStrategy(add func(level, name, detail string), dir string, cfg *config.Config, deps Deps) {
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		add("FAIL", "strategy", err.Error())
		return
	}
	eff, profile, err := resolveEffective(dir, cfg, deps, caps, config.Flags{})
	if err != nil {
		add("FAIL", "profile", err.Error())
		return
	}
	if err := config.CheckStrategy(eff.Strategy); err != nil {
		add("FAIL", "strategy", fmt.Sprintf("%q is not one of cheapest, balanced or best", eff.Strategy))
		return
	}
	detail := fmt.Sprintf("%s (%s)", eff.Strategy, eff.Sources["strategy"])
	if profile != "" {
		detail += "; profile " + profile
	}
	add("ok", "strategy", detail)
}

func checkSnapshot(add func(level, name, detail string), deps Deps, path string) {
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		add("warn", "catalog", "no pricing snapshot at "+path+"; run `mgw pricing refresh`")
		return
	}
	if err != nil {
		add("FAIL", "catalog", err.Error())
		return
	}
	s, err := catalog.ReadSnapshot(data)
	if err != nil {
		add("FAIL", "catalog", err.Error())
		return
	}
	stale, err := s.Stale(deps.Now())
	if err != nil {
		add("FAIL", "catalog", err.Error())
		return
	}
	if stale {
		// ReadSnapshot parsed CapturedAt, so this cannot fail.
		captured, _ := time.Parse(time.RFC3339, s.CapturedAt)
		days := int(deps.Now().Sub(captured).Hours() / 24)
		add("warn", "catalog", fmt.Sprintf("snapshot captured_at %s is %d days old (limit 30); run `mgw pricing refresh`", s.CapturedAt, days))
		return
	}
	add("ok", "catalog", "snapshot captured_at "+s.CapturedAt)
}

func checkCA(add func(level, name, detail string), deps Deps) {
	if f := deps.Getenv("SSL_CERT_FILE"); f != "" {
		if fileExists(f) {
			add("ok", "ca-bundle", f+" (SSL_CERT_FILE)")
		} else {
			add("warn", "ca-bundle", "SSL_CERT_FILE points to "+f+", which does not exist; HTTPS will fail")
		}
		return
	}
	for _, p := range deps.CAPaths {
		if fileExists(p) {
			add("ok", "ca-bundle", p)
			return
		}
	}
	add("warn", "ca-bundle", "no CA bundle found; a static binary still needs one on the host for HTTPS")
}

func checkRegistry(add func(level, name, detail string), path string) {
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		add("warn", "registry", "unpinned: no registry at "+path+"; run `mgw matrix --write`")
		return
	}
	if err != nil {
		add("FAIL", "registry", err.Error())
		return
	}
	pin, err := readFile(path + ".sha256")
	if errors.Is(err, os.ErrNotExist) {
		add("warn", "registry", "unpinned: no "+filepath.Base(path)+".sha256 next to the registry; run `mgw matrix --write`")
		return
	}
	if err != nil {
		add("FAIL", "registry", err.Error())
		return
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(pin)), " ")
	got := parity.SHA256Hex(data)
	if got != want {
		add("FAIL", "registry", fmt.Sprintf("registry sha256 %s does not match the pin %s", got, want))
		return
	}
	r, err := policy.DecodeRegistry(bytes.TrimSpace(data))
	if err != nil {
		add("FAIL", "registry", err.Error())
		return
	}
	if err := policy.Validate(r); err != nil {
		add("FAIL", "registry", "pinned but invalid: "+strings.ReplaceAll(err.Error(), "\n", "; "))
		return
	}
	add("ok", "registry", "pinned sha256 "+got)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
