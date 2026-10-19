package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/aysbg/mgw/internal/config"
)

// Global holds the flags that precede the subcommand: `mgw --config DIR
// --profile FILE <command>`.
type Global struct {
	ConfigDir string
	Profile   string
}

// ParseGlobal consumes leading --config and --profile flags (separate value or
// =value) and returns the rest, which starts with the subcommand. A flag after
// the subcommand belongs to the subcommand.
func ParseGlobal(args []string) (Global, []string, error) {
	var g Global
	for len(args) > 0 {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
		if !strings.HasPrefix(args[0], "-") || (name != "config" && name != "profile") {
			break
		}
		args = args[1:]
		if !hasVal {
			if len(args) == 0 {
				return Global{}, nil, fmt.Errorf("--%s needs a value", name)
			}
			val, args = args[0], args[1:]
		}
		if name == "config" {
			g.ConfigDir = val
		} else {
			g.Profile = val
		}
	}
	return g, args, nil
}

// Run parses the global flags and dispatches the onboarding-era commands:
// init, providers, strategy, matrix, pricing and doctor. It is the entry point
// cmd/mgw uses, so tests that call it drive the real command path.
func Run(args []string, stdout, stderr io.Writer, deps Deps) int {
	g, rest, err := ParseGlobal(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mgw: %v\n", err)
		return ExitUsage
	}
	deps.ConfigDir, deps.Profile = g.ConfigDir, g.Profile
	if len(rest) == 0 {
		_, _ = fmt.Fprintln(stderr, "mgw: want a command")
		return ExitUsage
	}
	switch cmd, tail := rest[0], rest[1:]; cmd {
	case "init":
		return Init(tail, stdout, stderr, deps)
	case "providers":
		return Providers(tail, stdout, stderr, deps)
	case "strategy":
		return Strategy(tail, stdout, stderr, deps)
	case "matrix":
		return Matrix(tail, stdout, stderr, deps)
	case "pricing":
		return Pricing(tail, stdout, stderr, deps)
	case "doctor":
		return Doctor(tail, stdout, stderr, deps)
	default:
		_, _ = fmt.Fprintf(stderr, "mgw: unknown command %q\n", cmd)
		return ExitUsage
	}
}

// failExit maps a named failure to an exit code: a wrong or missing argument is
// a usage error; a refused operation is an ordinary failure.
func failExit(err error) int {
	var f *config.Failure
	if errors.As(err, &f) {
		switch f.Code {
		case config.CodeMissingConfig, config.CodeMissingProfile, config.CodeProviderFailed,
			config.CodeToolGradeFail, config.CodeNoTerminal, config.CodeCredentialFile, config.CodeAborted:
			return ExitError
		}
		return ExitUsage
	}
	return ExitError
}

// fail prints one error line and returns its exit code. The line carries the
// failure code when the error is a named failure.
func fail(stderr io.Writer, cmd string, err error) int {
	_, _ = fmt.Fprintf(stderr, "mgw %s: %v\n", cmd, err)
	return failExit(err)
}

// isTerminal reports whether f is a character device other than /dev/null.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}

// setEcho turns terminal echo on or off through stty, which reads the terminal
// from stdin. While echo is off, an interrupt restores it before the process
// exits, so a Ctrl-C at the key prompt does not leave the terminal silent.
func setEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.CommandContext(context.Background(), "stty", arg) // #nosec G204 -- fixed arguments
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return err
	}
	echoGuard(on)
	return nil
}

var guardDone chan struct{}

// echoGuard starts or stops the interrupt watcher that restores echo.
func echoGuard(on bool) {
	if on {
		if guardDone != nil {
			close(guardDone)
			guardDone = nil
		}
		return
	}
	if guardDone != nil {
		return
	}
	done := make(chan struct{})
	guardDone = done
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		defer signal.Stop(sig)
		select {
		case <-sig:
			restore := exec.CommandContext(context.Background(), "stty", "echo") // #nosec G204 -- fixed arguments
			restore.Stdin = os.Stdin
			_ = restore.Run()
			os.Exit(130)
		case <-done:
		}
	}()
}
