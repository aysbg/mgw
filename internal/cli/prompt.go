package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/aysbg/mgw/internal/config"
)

// maxAttempts is how often an interactive question is asked before it fails.
const maxAttempts = 3

// prompter asks interactive questions. It reads one byte at a time, so it never
// consumes input beyond the line it was asked for.
type prompter struct {
	in      io.Reader
	out     io.Writer
	setEcho func(on bool) error
}

// line reads one line. End of input is a named failure.
func (p *prompter) line() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := p.in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimRight(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", config.Fail(config.CodeInputClosed, "input ended before the question was answered")
		}
	}
}

// ask prints prompt, reads an answer, applies the default to a blank answer and
// validates it. A rejected answer is explained and asked again, up to
// maxAttempts times; the last rejection is returned.
func (p *prompter) ask(prompt, def string, validate func(string) error) (string, error) {
	var last error
	for range maxAttempts {
		_, _ = fmt.Fprint(p.out, prompt)
		ans, err := p.line()
		if err != nil {
			return "", err
		}
		ans = strings.TrimSpace(ans)
		if ans == "" {
			ans = def
		}
		if validate == nil {
			return ans, nil
		}
		if last = validate(ans); last == nil {
			return ans, nil
		}
		_, _ = fmt.Fprintf(p.out, "  %v\n", last)
	}
	return "", last
}

// secret reads one line with terminal echo off. It refuses when the host cannot
// hide input: a key typed in the clear is worse than a failure.
func (p *prompter) secret(prompt string) (string, error) {
	if p.setEcho == nil {
		return "", config.Fail(config.CodeNoTerminal, "this terminal cannot hide input; pass --key-file instead")
	}
	_, _ = fmt.Fprint(p.out, prompt)
	if err := p.setEcho(false); err != nil {
		return "", config.Fail(config.CodeNoTerminal, "cannot turn terminal echo off (%v); pass --key-file instead", err)
	}
	ans, err := p.line()
	restoreErr := p.setEcho(true)
	_, _ = fmt.Fprintln(p.out) // the terminal did not echo the newline
	if err != nil {
		return "", err
	}
	if restoreErr != nil {
		return "", config.Fail(config.CodeNoTerminal, "cannot turn terminal echo back on: %v", restoreErr)
	}
	ans = strings.TrimSpace(ans)
	if ans == "" {
		return "", config.Fail(config.CodeMissingValue, "an empty key was entered")
	}
	return ans, nil
}
