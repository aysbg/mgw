package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
)

// Default catalog source for `mgw pricing refresh`.
const defaultCatalogURL = "https://models.dev/api.json"

// Deps are the host dependencies of the commands, injectable for tests.
type Deps struct {
	// Build is the running binary's metadata, supplied by main's ldflags values.
	Build  BuildInfo
	Now    func() time.Time
	Fetch  func(ctx context.Context, url string) ([]byte, string, error)
	Getenv func(string) string
	// CAPaths are the places `mgw doctor` looks for a CA bundle.
	CAPaths []string
	// ConfigDir is the --config override of the config directory.
	ConfigDir string
	// Profile is the --profile override: an engagement profile path or name.
	Profile string
	// Stdin is read only by interactive init. StdinIsTTY says whether it is a
	// terminal; a nil StdinIsTTY means it is not.
	Stdin      io.Reader
	StdinIsTTY func() bool
	// SetEcho turns terminal echo on or off, so a key can be typed without being
	// shown. Nil means the host cannot hide input.
	SetEcho func(on bool) error
}

// DefaultDeps returns the production dependencies.
func DefaultDeps() Deps {
	return Deps{
		Now:        time.Now,
		Fetch:      catalog.Fetch,
		Getenv:     os.Getenv,
		Stdin:      os.Stdin,
		StdinIsTTY: func() bool { return isTerminal(os.Stdin) },
		SetEcho:    setEcho,
		CAPaths: []string{
			"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu
			"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, Fedora
			"/etc/ssl/ca-bundle.pem",             // openSUSE
			"/etc/ssl/cert.pem",                  // Alpine, macOS
		},
	}
}

// configDir resolves the config directory: --config, then MGW_CONFIG_DIR, then
// $XDG_CONFIG_HOME/mgw, then ~/.config/mgw. With none of them it is a named
// failure; there is no silent default.
func (d Deps) configDir() (string, error) {
	switch {
	case d.ConfigDir != "":
		return d.ConfigDir, nil
	case d.Getenv(config.DirEnv) != "":
		return d.Getenv(config.DirEnv), nil
	case d.Getenv("XDG_CONFIG_HOME") != "":
		return filepath.Join(d.Getenv("XDG_CONFIG_HOME"), "mgw"), nil
	case d.Getenv("HOME") != "":
		return filepath.Join(d.Getenv("HOME"), ".config", "mgw"), nil
	}
	return "", config.Fail(config.CodeMissingConfig, "cannot find the config directory; set %s or pass --config", config.DirEnv)
}

// stateDir is the directory of the default snapshot and registry paths. When no
// directory resolves it is "", and the commands that need a config fail by name.
func (d Deps) stateDir() string {
	dir, _ := d.configDir()
	return dir
}

func (d Deps) defaultSnapshot() string { return config.PricingPath(d.stateDir()) }
func (d Deps) defaultRegistry() string { return config.RegistryPath(d.stateDir()) }

// writePublicFile writes a registry or snapshot: mode 0644, atomic.
func writePublicFile(path string, data []byte) error {
	return config.WriteFileAtomic(path, data, config.ModePublic)
}

// printTable writes left-aligned columns separated by one space, with widths
// computed from the content. No colour, no merged cells, no trailing spaces.
func printTable(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], len(c))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(c)+1))
			}
		}
		_, _ = fmt.Fprintln(w, b.String())
	}
}

// formatUSD renders integer micro-USD as decimal USD with at least two decimals
// and no trailing zeros beyond that. Integer arithmetic only.
func formatUSD(micro int64) string {
	whole, frac := micro/1_000_000, micro%1_000_000
	f := fmt.Sprintf("%06d", frac)
	f = strings.TrimRight(f, "0")
	for len(f) < 2 {
		f += "0"
	}
	return fmt.Sprintf("%d.%s", whole, f)
}

func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 -- operator-supplied path
}
