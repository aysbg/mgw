package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
)

// Pricing implements `mgw pricing refresh` and `mgw pricing show`.
func Pricing(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "mgw pricing: want a subcommand: refresh or show")
		return ExitUsage
	}
	switch args[0] {
	case "refresh":
		return pricingRefresh(args[1:], stdout, stderr, deps)
	case "show":
		return pricingShow(args[1:], stdout, stderr, deps)
	default:
		_, _ = fmt.Fprintf(stderr, "mgw pricing: unknown subcommand %q (want refresh or show)\n", args[0])
		return ExitUsage
	}
}

func pricingRefresh(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("pricing refresh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultCatalogURL, "catalog URL")
	pin := fs.String("sha256", "", "pinned sha256 of the catalog body (required to write)")
	path := fs.String("snapshot", deps.defaultSnapshot(), "snapshot output path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	body, sum, err := deps.Fetch(ctx, *url)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: %v\n", err)
		return ExitError
	}
	if *pin == "" {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: no pinned digest; fetched sha256 %s\nreview the catalog, then rerun with --sha256 %s\n", sum, sum)
		return ExitError
	}
	if err := catalog.VerifyDigest(sum, *pin); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: catalog digest does not match the pin (%v)\n", err)
		return ExitError
	}
	c, err := catalog.Decode(body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: %v\n", err)
		return ExitError
	}
	snap := catalog.NewSnapshot(c, *url, sum, deps.Now(), true)
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: %v\n", err)
		return ExitError
	}
	data = append(data, '\n')
	if err := writePublicFile(*path, data); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: write snapshot: %v\n", err)
		return ExitError
	}
	pinData := []byte(fmt.Sprintf("%s  %s\n", catalog.Digest(data), filepath.Base(*path)))
	if err := writePublicFile(*path+".sha256", pinData); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing refresh: write snapshot pin: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s\ncaptured_at %s\nsource_sha256 %s\nmodels %d\n", *path, snap.CapturedAt, sum, len(snap.Models))
	return ExitOK
}

func pricingShow(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("pricing show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("snapshot", deps.defaultSnapshot(), "snapshot path")
	models := fs.Bool("models", false, "also list every model")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	data, err := readFile(*path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "mgw pricing show: no snapshot at %s; run `mgw pricing refresh`\n", *path)
		} else {
			_, _ = fmt.Fprintf(stderr, "mgw pricing show: %v\n", err)
		}
		return ExitError
	}
	s, err := catalog.ReadSnapshot(data)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw pricing show: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "captured_at %s\nsource_url %s\nsource_sha256 %s\nblend %s (9*input + 1*output)/10, USD per 1M tokens\npopulation %s\n\n",
		s.CapturedAt, s.SourceURL, s.SourceSHA256, s.Blend, s.Population)
	rows := [][]string{{"PROVIDER", "MEDIAN", "N"}}
	for _, p := range s.Providers {
		rows = append(rows, []string{p.ID, "$" + formatUSD(p.MedianMicroUSD), fmt.Sprint(p.N)})
	}
	printTable(stdout, rows)
	if *models {
		_, _ = fmt.Fprintln(stdout)
		rows = [][]string{{"MODEL", "INPUT", "OUTPUT", "CACHE_READ", "BLENDED"}}
		for _, m := range s.Models {
			if !m.Priced {
				rows = append(rows, []string{m.ID, "-", "-", "-", "-"})
				continue
			}
			rows = append(rows, []string{m.ID, "$" + formatUSD(m.InputMicroUSD), "$" + formatUSD(m.OutputMicroUSD),
				"$" + formatUSD(m.CacheReadMicroUSD), "$" + formatUSD(m.BlendedMicroUSD)})
		}
		printTable(stdout, rows)
	}
	return ExitOK
}
