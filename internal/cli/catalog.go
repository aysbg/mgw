package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
)

type catalogDeltaJSON struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Context  int64  `json:"context"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
}

// Catalog implements `mgw catalog diff`, a read-only upstream comparison.
func Catalog(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "mgw catalog: want a subcommand: diff")
		return ExitUsage
	}
	if args[0] != "diff" {
		_, _ = fmt.Fprintf(stderr, "mgw catalog: unknown subcommand %q (want diff)\n", args[0])
		return ExitUsage
	}
	fs := flag.NewFlagSet("catalog diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("catalog-url", defaultCatalogURL, "catalog URL")
	minContext := fs.Int64("min-context", catalog.MinAgentContext, "minimum model context (at least 100000)")
	asJSON := fs.Bool("json", false, "emit JSON")
	quiet := fs.Bool("quiet", false, "emit nothing when there are no deltas")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "mgw catalog diff: unexpected arguments: %v\n", fs.Args())
		return ExitUsage
	}
	if *asJSON && *quiet {
		_, _ = fmt.Fprintln(stderr, "mgw catalog diff: --json and --quiet cannot be combined")
		return ExitUsage
	}
	if *minContext < catalog.MinAgentContext {
		_, _ = fmt.Fprintf(stderr, "mgw catalog diff: --min-context must be at least %d\n", catalog.MinAgentContext)
		return ExitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	body, sum, err := deps.Fetch(ctx, *url)
	if err != nil {
		return fail(stderr, "catalog diff", err)
	}
	cat, err := catalog.Decode(body)
	if err != nil {
		return fail(stderr, "catalog diff", err)
	}
	caps, err := catalog.LoadCapabilities()
	if err != nil {
		return fail(stderr, "catalog diff", err)
	}
	deltas, err := catalog.Diff(caps, cat, *minContext)
	if err != nil {
		return fail(stderr, "catalog diff", err)
	}
	if *quiet && len(deltas) == 0 {
		return ExitOK
	}
	if *asJSON {
		rows := make([]catalogDeltaJSON, len(deltas))
		for i, d := range deltas {
			rows[i] = catalogDeltaJSON{Kind: d.Kind, Provider: d.Provider, Model: d.Model, Context: d.Context, Input: d.InputMicro, Output: d.OutputMicro}
		}
		result := struct {
			SourceURL    string             `json:"source_url"`
			SourceSHA256 string             `json:"source_sha256"`
			Deltas       []catalogDeltaJSON `json:"deltas"`
		}{SourceURL: *url, SourceSHA256: sum, Deltas: rows}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return fail(stderr, "catalog diff", err)
		}
		return ExitOK
	}
	if _, err := fmt.Fprintf(stdout, "source_url %s source_sha256 %s\n", *url, sum); err != nil {
		return fail(stderr, "catalog diff", err)
	}
	w := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KIND\tPROVIDER\tMODEL\tCTX\tINPUT/OUTPUT")
	for _, d := range deltas {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d/%d\n", d.Kind, d.Provider, d.Model, d.Context, d.InputMicro, d.OutputMicro)
	}
	if err := w.Flush(); err != nil {
		return fail(stderr, "catalog diff", err)
	}
	return ExitOK
}
