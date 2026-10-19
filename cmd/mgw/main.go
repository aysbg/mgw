// Command mgw is the model gateway for coding agents.
//
// This file is deliberately thin: it parses the subcommand, prints help, and
// delegates. Every real command lives under internal/cli.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/aysbg/mgw/internal/cli"
)

// Set at build time with -ldflags. Defaults are for a local build.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

const usage = `mgw - model gateway for coding agents

Usage:
  mgw [--config DIR] [--profile FILE] <command> [flags]

Commands:
  serve      Run the gateway for one attempt or interactive session
  authority  Emit a runtime and run token for one attempt
  exec       Run a client under a gateway
  init       Connect providers and generate the decision table
  providers  Add, list or test providers
  strategy   Show or set the model strategy
  matrix     Print the capability matrix
  pricing    Refresh the pricing snapshot
  doctor     Check the host and the pinned artifacts
  decide     Classify a state file or record a shadow decision
  version    Print version information
`

func main() {
	// Global flags (--config, --profile) precede the subcommand.
	g, rest, err := cli.ParseGlobal(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgw: %v\n", err)
		os.Exit(2)
	}
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch cmd := rest[0]; cmd {
	case "version":
		printVersion(rest[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "serve":
		os.Exit(cli.Serve(rest[1:], os.Stdout, os.Stderr, version))
	case "authority":
		deps := cli.DefaultDeps()
		deps.ConfigDir = g.ConfigDir
		os.Exit(cli.Authority(rest[1:], os.Stdout, os.Stderr, deps))
	case "init", "providers", "strategy", "matrix", "pricing", "doctor", "decide":
		os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, cli.DefaultDeps()))
	case "exec":
		// Implemented by a later lane. Failing closed with a distinct exit code
		// keeps an unfinished build from looking functional.
		fmt.Fprintf(os.Stderr, "mgw: %s is not implemented yet\n", cmd)
		os.Exit(3)
	default:
		fmt.Fprintf(os.Stderr, "mgw: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func printVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print version information as JSON")
	_ = fs.Parse(args)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]string{
			"version":   version,
			"commit":    commit,
			"buildTime": buildTime,
		})
		return
	}

	fmt.Printf("mgw %s (commit %s, built %s)\n", version, commit, buildTime)
}
