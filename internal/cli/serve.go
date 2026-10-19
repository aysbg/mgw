// Package cli holds the mgw subcommands. cmd/mgw only dispatches to it.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/aysbg/mgw/internal/proxy"
)

// Exit codes for Serve.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Serve implements `mgw serve --runtime <file>`. It prints one JSON line on
// stderr once the ready file is written and exits 0 on SIGTERM or SIGINT after
// settling in-flight requests.
func Serve(args []string, stdout, stderr io.Writer, version string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runtimeFile := fs.String("runtime", "", "path to the runtime file (required)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "mgw %s\n", version)
		return ExitOK
	}
	if *runtimeFile == "" {
		_, _ = fmt.Fprintln(stderr, "mgw serve: --runtime is required")
		return ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := proxy.Run(ctx, *runtimeFile, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw serve: %v\n", err)
		return ExitError
	}
	return ExitOK
}
