package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
)

// Doctor implements `mgw doctor`: catalog staleness, a missing CA bundle, and an
// unpinned registry. A warning does not fail the command; a corrupt artifact does.
func Doctor(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	snapshot := fs.String("snapshot", deps.defaultSnapshot(), "pricing snapshot path")
	registry := fs.String("registry", deps.defaultRegistry(), "pinned registry path")
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
	checkSnapshot(add, deps, *snapshot)
	checkCA(add, deps)
	checkRegistry(add, *registry)
	printTable(stdout, rows)
	if failed {
		return ExitError
	}
	return ExitOK
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
