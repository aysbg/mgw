package cli

import (
	"io"
	"strings"
	"testing"
)

// scriptReader feeds scripted lines one byte per Read, the way a terminal in
// line mode delivers them, and records, for each completed line, whether echo
// was on when the line ended.
type scriptReader struct {
	s        string
	pos      int
	echo     *bool
	lineEcho []bool
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.pos]
	r.pos++
	if p[0] == '\n' {
		r.lineEcho = append(r.lineEcho, *r.echo)
	}
	return 1, nil
}

func interactiveOnboarding(t *testing.T, script string) (*onboarding, *scriptReader, *[]bool) {
	t.Helper()
	o := newOnboarding(t)
	echo := true
	var calls []bool
	rd := &scriptReader{s: script, echo: &echo}
	o.deps.Stdin = rd
	o.deps.StdinIsTTY = func() bool { return true }
	o.deps.SetEcho = func(on bool) error {
		echo = on
		calls = append(calls, on)
		return nil
	}
	return o, rd, &calls
}

const interactiveScript = "openai, ollama-cloud\n" + keyOpenAI + "\n" + keyOllama + "\n" +
	"pay_per_token\nincluded_credits\nbalanced\nollama-cloud,openai\n\ninternal\ny\n"

func TestInteractiveInitAsksInOrderAndNeverEchoesKeys(t *testing.T) {
	o, rd, calls := interactiveScript2(t)
	code, out, errs := o.run("init", "--base-url", "openai="+o.base(), "--base-url", "ollama-cloud="+o.base())
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}

	// Question order: providers, each key, billing for each, strategy, order, deny, data class.
	order := []string{"Providers to connect", "openai API key", "ollama-cloud API key", "Billing for openai", "Billing for ollama-cloud",
		"Strategy", "Provider order", "Deny list", "Data class", "Write these files?"}
	at := -1
	for _, q := range order {
		i := strings.Index(out, q)
		if i < 0 || i < at {
			t.Fatalf("question %q missing or out of order (at %d, previous %d) in:\n%s", q, i, at, out)
		}
		at = i
	}
	// The plan is printed before the confirmation.
	if strings.Index(out, "plan: mgw init") > strings.Index(out, "Write these files?") || !strings.Contains(out, "plan: mgw init") {
		t.Fatalf("plan not printed before confirmation:\n%s", out)
	}

	// Hidden input: echo off exactly while the two key lines were read, on for the rest.
	want := []bool{true, false, false, true, true, true, true, true, true, true}
	if len(rd.lineEcho) != len(want) {
		t.Fatalf("lines read %d, want %d (%v)", len(rd.lineEcho), len(want), rd.lineEcho)
	}
	for i := range want {
		if rd.lineEcho[i] != want[i] {
			t.Fatalf("echo state per line %v, want %v", rd.lineEcho, want)
		}
	}
	if n := len(*calls); n != 4 || (*calls)[0] || !(*calls)[1] || (*calls)[2] || !(*calls)[3] {
		t.Fatalf("echo calls %v, want off,on,off,on", *calls)
	}
	// The key is used (it validated against the upstream and was stored) yet never printed.
	if got := readFileT(t, o.path("credentials", "openai.key")); got != keyOpenAI+"\n" {
		t.Fatalf("credential %q", got)
	}
	assertMode(t, o.path("credentials", "openai.key"), 0o600)
	if !strings.Contains(readFileT(t, o.path("config.json")), `"strategy": "balanced"`) {
		t.Fatal("answers not written")
	}
}

func interactiveScript2(t *testing.T) (*onboarding, *scriptReader, *[]bool) {
	return interactiveOnboarding(t, interactiveScript)
}

func TestInteractiveDeclineWritesNothing(t *testing.T) {
	script := strings.TrimSuffix(interactiveScript, "y\n") + "n\n"
	o, _, _ := interactiveOnboarding(t, script)
	code, _, errs := o.run("init", "--base-url", "openai="+o.base(), "--base-url", "ollama-cloud="+o.base())
	if code != ExitError || !strings.Contains(errs, "aborted") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if exists(o.dir) || o.fake.count() != 0 {
		t.Fatal("declined init wrote files or called the upstream")
	}
}

func TestInteractiveEndOfInputIsANamedFailure(t *testing.T) {
	o, _, _ := interactiveOnboarding(t, "openai\n"+keyOpenAI+"\n") // stdin ends at the billing question
	code, _, errs := o.run("init")
	if code != ExitUsage || !strings.Contains(errs, "input_closed") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if exists(o.dir) {
		t.Fatal("files written")
	}
}

func TestInteractiveRefusesWhenInputCannotBeHidden(t *testing.T) {
	o, _, _ := interactiveOnboarding(t, "openai\n"+keyOpenAI+"\n")
	o.deps.SetEcho = nil
	code, _, errs := o.run("init")
	if code != ExitError || !strings.Contains(errs, "no_hidden_input") || !strings.Contains(errs, "--key-file") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestInteractiveInvalidAnswerIsReaskedThenFails(t *testing.T) {
	script := "openai\n" + keyOpenAI + "\npay_per_token\nfastest\nfastest\nfastest\n"
	o, _, _ := interactiveOnboarding(t, script)
	code, out, errs := o.run("init", "--base-url", "openai="+o.base())
	if code != ExitUsage || !strings.Contains(errs, "unknown_strategy") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Count(out, "Strategy (") != 3 {
		t.Fatalf("expected three attempts:\n%s", out)
	}
}

func TestInteractiveDefaultsAcceptBlankAnswers(t *testing.T) {
	// Blank billing, strategy, order, deny, data class: defaults apply.
	script := "ollama-cloud\n" + keyOllama + "\n\n\n\n\n\ny\n"
	o, _, _ := interactiveOnboarding(t, script)
	code, out, errs := o.run("init", "--base-url", "ollama-cloud="+o.base())
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	cfg := readFileT(t, o.path("config.json"))
	for _, want := range []string{`"billing": "included_credits"`, `"strategy": "balanced"`, `"data_class": "internal"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("lacks %s:\n%s", want, cfg)
		}
	}
}
