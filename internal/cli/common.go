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
)

// Default catalog source for `mgw pricing refresh`.
const defaultCatalogURL = "https://models.dev/api.json"

// Deps are the host dependencies of the commands, injectable for tests.
type Deps struct {
	Now    func() time.Time
	Fetch  func(ctx context.Context, url string) ([]byte, string, error)
	Getenv func(string) string
	// CAPaths are the places `mgw doctor` looks for a CA bundle.
	CAPaths []string
}

// DefaultDeps returns the production dependencies.
func DefaultDeps() Deps {
	return Deps{
		Now:    time.Now,
		Fetch:  catalog.Fetch,
		Getenv: os.Getenv,
		CAPaths: []string{
			"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu
			"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, Fedora
			"/etc/ssl/ca-bundle.pem",             // openSUSE
			"/etc/ssl/cert.pem",                  // Alpine, macOS
		},
	}
}

// stateDir is where the snapshot and the pinned registry live by default.
func (d Deps) stateDir() string {
	if v := d.Getenv("MGW_HOME"); v != "" {
		return v
	}
	if v := d.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "mgw")
	}
	if v := d.Getenv("HOME"); v != "" {
		return filepath.Join(v, ".config", "mgw")
	}
	return "."
}

func (d Deps) defaultSnapshot() string { return filepath.Join(d.stateDir(), "pricing.json") }
func (d Deps) defaultRegistry() string { return filepath.Join(d.stateDir(), "registry.json") }

// writeFileAtomic writes data to path through a temporary file in the same
// directory and a rename, with mode 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mgw-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
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
