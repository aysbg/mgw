package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/config"
)

func TestProvidersListShowsKeyStateAndLastProbe(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	code, out, errs := o.run("providers", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"ID", "DIALECT", "BILLING", "KEY", "PROBE", "openai", "ollama", "included_credits", "present", "grade=pass", "tool_call=ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if err := os.Chmod(o.path("credentials", "openai.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(o.path("credentials", "ollama-cloud.key")); err != nil {
		t.Fatal(err)
	}
	_, out, _ = o.run("providers", "list")
	if !strings.Contains(out, "insecure") || !strings.Contains(out, "missing") {
		t.Fatalf("key state not shown:\n%s", out)
	}
}

func TestProvidersTestReportsEachStepAndStoresTheGrade(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	code, out, errs := o.run("providers", "test", "openai")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"auth", "inference", "stream_usage", "tool_call", "tool_grade pass"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}

	// The provider starts returning a malformed tool call: the grade flips and is stored.
	o.fake.badTool[keyOpenAI] = true
	code, out, _ = o.run("providers", "test", "openai")
	if code != 0 || !strings.Contains(out, "tool_grade fail") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg := readFileT(t, o.path("config.json"))
	if !strings.Contains(cfg, `"tool_grade": "fail"`) {
		t.Fatalf("grade not stored:\n%s", cfg)
	}
	assertMode(t, o.path("config.json"), 0o600)

	// A failing provider that would serve build is reported by matrix, refused on
	// --write, and failed by doctor. The flag is the only way past it.
	_, _, errs = o.run("matrix", "--all")
	if !strings.Contains(errs, "tool_grade fail") || !strings.Contains(errs, "openai") || !strings.Contains(errs, "build") {
		t.Fatalf("matrix did not report the failing provider: %s", errs)
	}
	before := readFileT(t, o.path("registry.json"))
	code, _, errs = o.run("matrix", "--write")
	if code != ExitError || !strings.Contains(errs, "--allow-ungraded-build") {
		t.Fatalf("write not refused: %d %s", code, errs)
	}
	if readFileT(t, o.path("registry.json")) != before {
		t.Fatal("registry rewritten despite the refusal")
	}
	if code, out, _ := o.run("doctor"); code != ExitError || !strings.Contains(out, "FAIL  tool-grade") {
		t.Fatalf("doctor %d\n%s", code, out)
	}
	if code, _, errs := o.run("matrix", "--write", "--allow-ungraded-build"); code != 0 {
		t.Fatalf("flag did not allow the write: %d %s", code, errs)
	}
}

func TestProvidersTestFailsOnARejectedKeyWithoutPrintingIt(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if err := os.WriteFile(o.path("credentials", "openai.key"), []byte(keyBad+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := o.run("providers", "test", "openai") // run asserts no key value in output
	if code != ExitError || !strings.Contains(out+errs, "openai") || !strings.Contains(out+errs, "rejected") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestProvidersTestRefusesALooseCredentialFile(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if err := os.Chmod(o.path("credentials", "openai.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := o.fake.count()
	code, _, errs := o.run("providers", "test", "openai")
	if code != ExitError || !strings.Contains(errs, "credential_file") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if o.fake.count() != before {
		t.Fatal("a loose credential file was read and used")
	}
}

func TestProvidersTestUnknownProvider(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if code, _, errs := o.run("providers", "test", "nowhere"); code != ExitUsage || !strings.Contains(errs, "unknown_provider") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, errs := o.run("providers", "test"); code != ExitUsage || !strings.Contains(errs, "missing_value") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, _ := o.run("providers", "bogus"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestProvidersAddLeavesStrategyAndOrderAlone(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if code, _, errs := o.run("strategy", "set", "--strategy", "best", "--deny", "openai/gpt-6-astra"); code != 0 {
		t.Fatalf("strategy set %d: %s", code, errs)
	}
	anth := writeKeyFile(t, "a.key", keyAnthropic, 0o600)
	code, out, errs := o.run("providers", "add", "--provider", "anthropic", "--key-file", anth, "--billing", "pay_per_token",
		"--base-url", "anthropic="+o.base(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, firstLines(out, 5), errs)
	}
	cfg := readFileT(t, o.path("config.json"))
	for _, want := range []string{`"id": "anthropic"`, `"strategy": "best"`, `"openai/gpt-6-astra"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("lacks %s:\n%s", want, cfg)
		}
	}
	loaded, err := config.Load(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(loaded.ProviderPolicy.Order, ",") != "ollama-cloud,openai" {
		t.Fatalf("add touched the provider order: %v", loaded.ProviderPolicy.Order)
	}
	assertMode(t, o.path("credentials", "anthropic.key"), 0o600)
	// add without the provider's key is a named failure, never a prompt.
	if code, _, errs := o.run("providers", "add", "--provider", "openai"); code != ExitUsage || !strings.Contains(errs, "missing_value") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// add must not accept strategy flags.
	if code, _, _ := o.run("providers", "add", "--provider", "openai", "--strategy", "best"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}
