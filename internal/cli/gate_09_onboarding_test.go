package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
)

// Gate 9 drives the real CLI entry point into a temp MGW_CONFIG_DIR against a
// fake upstream, then runs the matrix from what init wrote.

func TestGate09_InitWritesFilesWithModesAndMatrixWorksFromThem(t *testing.T) {
	o := newOnboarding(t)
	code, out, errs := o.run(o.initArgs()...)
	if code != 0 {
		t.Fatalf("init exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}

	// Files and modes, not just contents.
	assertMode(t, o.dir, 0o700)
	assertMode(t, o.path("config.json"), 0o600)
	assertMode(t, o.path("credentials"), 0o700)
	assertMode(t, o.path("credentials", "openai.key"), 0o600)
	assertMode(t, o.path("credentials", "ollama-cloud.key"), 0o600)
	assertMode(t, o.path("registry.json"), 0o644)
	assertMode(t, o.path("pricing.json"), 0o644)

	if got := readFileT(t, o.path("credentials", "openai.key")); got != keyOpenAI+"\n" {
		t.Fatalf("credential file content %q", got)
	}
	cfg := readFileT(t, o.path("config.json"))
	for _, k := range allKeys {
		if strings.Contains(cfg, k) {
			t.Fatalf("key value written into config.json:\n%s", cfg)
		}
	}
	for _, want := range []string{`"schema": "mgw.config/1"`, `"id": "openai"`, `"id": "ollama-cloud"`, `"strategy": "balanced"`,
		`"data_class": "internal"`, `"profile": "default"`, `"billing": "included_credits"`, `"tool_grade": "pass"`, `"include_stream_usage": true`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config.json lacks %s:\n%s", want, cfg)
		}
	}

	// init printed the matrix.
	if !strings.Contains(out, "# mgw matrix") || !strings.Contains(out, "strategy balanced") {
		t.Fatalf("init did not print the matrix:\n%s", firstLines(out, 12))
	}

	// The registry on disk is valid, pinned, and carries the install's order.
	data := []byte(readFileT(t, o.path("registry.json")))
	reg, err := policy.DecodeRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Validate(reg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(reg.ProviderPolicy.Preference, ",") != "ollama-cloud,openai" {
		t.Fatalf("preference %v", reg.ProviderPolicy.Preference)
	}
	if pin := readFileT(t, o.path("registry.json.sha256")); !strings.HasPrefix(pin, parity.SHA256Hex(data)) {
		t.Fatalf("pin %q", pin)
	}

	// A subsequent matrix works from what init wrote: no flags, no snapshot path.
	for _, args := range [][]string{{"matrix", "--check"}, {"matrix", "--all"}, {"matrix"}, {"matrix", "--explain", "build|feature|M|false|balanced"}} {
		code, out, errs := o.run(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, firstLines(out, 5), errs)
		}
	}
	// Re-deriving the registry from the install reproduces the pinned bytes.
	code, out, errs = o.run("matrix", "--write", "--out", filepath.Join(t.TempDir(), "again.json"))
	if code != 0 || !strings.Contains(out, parity.SHA256Hex(data)) {
		t.Fatalf("matrix --write from config differs from init's registry: %d\n%s%s", code, out, errs)
	}
	// And doctor agrees.
	if code, out, errs := o.run("doctor"); code != 0 {
		t.Fatalf("doctor %d\n%s%s", code, out, errs)
	}
}

func TestGate09_BadKeyIsRefusedNamingTheProviderAndWritesNothing(t *testing.T) {
	o := newOnboarding(t)
	args := o.initArgs()
	for i, a := range args {
		if a == "--key-file" && args[i-1] == "ollama-cloud" {
			args[i+1] = writeKeyFile(t, "bad.key", keyBad, 0o600)
		}
	}
	code, out, errs := o.run(args...)
	if code != ExitError {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, ExitError, out, errs)
	}
	if !strings.Contains(errs, "provider_validation_failed") || !strings.Contains(errs, "ollama-cloud") {
		t.Fatalf("failure does not name the provider: %s", errs)
	}
	if strings.Contains(errs, "openai:") {
		t.Fatalf("the good provider was blamed: %s", errs)
	}
	if exists(o.dir) {
		entries, _ := os.ReadDir(o.dir)
		t.Fatalf("a failed validation wrote %d entries into %s", len(entries), o.dir)
	}
}

func TestGate09_IdempotentAcrossTwoRunsAndKeepsExistingProviders(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	snap := map[string]string{}
	files := []string{"config.json", "registry.json", "registry.json.sha256", "pricing.json", "credentials/openai.key", "credentials/ollama-cloud.key"}
	for _, f := range files {
		snap[f] = readFileT(t, o.path(f))
	}
	o.mustInit()
	for _, f := range files {
		got := readFileT(t, o.path(f))
		if got != snap[f] {
			t.Fatalf("%s changed on a rerun with the same flags", f)
		}
		if f == "config.json" && strings.Count(got, `"id": "openai"`) != 1 {
			t.Fatalf("provider duplicated:\n%s", got)
		}
	}
	assertMode(t, o.path("config.json"), 0o600)
	assertMode(t, o.path("credentials", "openai.key"), 0o600)

	// An added provider keeps the existing ones.
	anth := writeKeyFile(t, "anth.key", keyAnthropic, 0o600)
	code, out, errs := o.run("init", "--provider", "anthropic", "--key-file", anth, "--billing", "pay_per_token",
		"--base-url", "anthropic="+o.base(), "--yes")
	if code != 0 {
		t.Fatalf("add via init: %d\n%s\n%s", code, firstLines(out, 5), errs)
	}
	cfg := readFileT(t, o.path("config.json"))
	for _, id := range []string{"openai", "ollama-cloud", "anthropic"} {
		if strings.Count(cfg, `"id": "`+id+`"`) != 1 {
			t.Fatalf("provider %s count != 1:\n%s", id, cfg)
		}
	}
	if !strings.Contains(cfg, `"strategy": "balanced"`) {
		t.Fatalf("existing strategy lost:\n%s", cfg)
	}
	if got := readFileT(t, o.path("credentials", "openai.key")); got != snap["credentials/openai.key"] {
		t.Fatal("existing credential changed")
	}
	reg, err := policy.DecodeRegistry([]byte(readFileT(t, o.path("registry.json"))))
	if err != nil || len(reg.Providers) != 3 {
		t.Fatalf("registry providers %v %v", reg, err)
	}
}

func TestGate09_NonInteractiveMissingValueIsNamedFailureAndNeverPrompts(t *testing.T) {
	good := writeKeyFile(t, "k", keyOpenAI, 0o600)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no provider", []string{"init", "--strategy", "balanced", "--yes"}, "missing_value: --provider"},
		{"no key file", []string{"init", "--provider", "openai", "--billing", "pay_per_token", "--strategy", "balanced", "--yes"}, "missing_value: --key-file for provider openai"},
		{"no billing", []string{"init", "--provider", "openai", "--key-file", good, "--strategy", "balanced", "--yes"}, "missing_value: --billing for provider openai"},
		{"no strategy", []string{"init", "--provider", "openai", "--key-file", good, "--billing", "pay_per_token", "--yes"}, "missing_value: --strategy"},
		{"orphan key file", []string{"init", "--key-file", good, "--provider", "openai", "--billing", "pay_per_token", "--strategy", "balanced", "--yes"}, "--key-file"},
		{"unknown provider", []string{"init", "--provider", "nowhere", "--key-file", good, "--billing", "pay_per_token", "--strategy", "balanced", "--yes"}, "unknown_provider"},
		{"unknown strategy", []string{"init", "--provider", "openai", "--key-file", good, "--billing", "pay_per_token", "--strategy", "fastest", "--yes"}, "unknown_strategy"},
		{"bad billing", []string{"init", "--provider", "openai", "--key-file", good, "--billing", "free", "--strategy", "balanced", "--yes"}, "bad_value"},
		{"prefer and deny same", []string{"init", "--provider", "openai", "--key-file", good, "--billing", "pay_per_token", "--strategy", "balanced", "--prefer", "openai", "--deny", "openai", "--yes"}, "conflicting_policy"},
	}
	for _, c := range cases {
		for _, tty := range []bool{false, true} {
			t.Run(c.name, func(t *testing.T) {
				o := newOnboarding(t)
				// A prompt flag is always given here, so even a TTY stdin must
				// not be read: forbidStdin fails the test on any Read.
				o.deps.StdinIsTTY = func() bool { return tty }
				code, _, errs := o.run(c.args...)
				if code != ExitUsage || !strings.Contains(errs, c.want) {
					t.Fatalf("exit %d stderr %q, want %d containing %q", code, errs, ExitUsage, c.want)
				}
				if exists(o.dir) {
					t.Fatalf("config dir created by a failed init")
				}
				if o.fake.count() != 0 {
					t.Fatal("upstream was contacted before the arguments were valid")
				}
			})
		}
	}
}

func TestGate09_NonTTYWithNoFlagsNeverPromptsEither(t *testing.T) {
	o := newOnboarding(t)
	code, _, errs := o.run("init", "--yes")
	if code != ExitUsage || !strings.Contains(errs, "missing_value: --provider") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestGate09_KeyIsNeverAcceptedAsAnArgument(t *testing.T) {
	for _, flag := range []string{"--key", "-key", "--api-key", "--token", "--secret"} {
		for _, form := range []string{"sep", "eq"} {
			o := newOnboarding(t)
			args := []string{"init", "--provider", "openai", "--billing", "pay_per_token", "--strategy", "balanced", "--yes"}
			if form == "eq" {
				args = append(args, flag+"="+keyOpenAI)
			} else {
				args = append(args, flag, keyOpenAI)
			}
			code, out, errs := o.run(args...) // run asserts the value is not echoed
			if code != ExitUsage || !strings.Contains(errs, "key_in_argv") || !strings.Contains(errs, "--key-file") {
				t.Fatalf("%s %s: exit %d stderr %q stdout %q", flag, form, code, errs, out)
			}
			if exists(o.dir) {
				t.Fatal("config written")
			}
		}
	}
}

func TestGate09_KeyFileWithLooseModeIsRefused(t *testing.T) {
	o := newOnboarding(t)
	args := o.initArgs()
	for i, a := range args {
		if a == "--key-file" && args[i-1] == "openai" {
			args[i+1] = writeKeyFile(t, "loose.key", keyOpenAI, 0o644)
		}
	}
	code, _, errs := o.run(args...)
	if code != ExitUsage || !strings.Contains(errs, "key_file") || !strings.Contains(errs, "openai") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if exists(o.dir) || o.fake.count() != 0 {
		t.Fatal("a loose key file must stop everything before any write or request")
	}
}

func TestGate09_WithoutYesNonInteractivePrintsThePlanAndWritesNothing(t *testing.T) {
	o := newOnboarding(t)
	args := o.initArgs()
	args = args[:len(args)-1] // drop --yes
	code, out, errs := o.run(args...)
	if code != 0 || !strings.Contains(out, "dry run; pass --yes") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"provider openai", "provider ollama-cloud", "billing included_credits", "strategy balanced", "credentials/openai.key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan lacks %q:\n%s", want, out)
		}
	}
	if exists(o.dir) {
		t.Fatal("dry run wrote files")
	}
	if o.fake.count() != 0 {
		t.Fatal("dry run contacted the upstream")
	}
}

func TestGate09_ToolGradeFailBlocksBuildUnlessAllowed(t *testing.T) {
	o := newOnboarding(t)
	o.fake.badTool[keyOllama] = true
	code, _, errs := o.run(o.initArgs()...)
	if code != ExitError || !strings.Contains(errs, "tool_grade_fail") || !strings.Contains(errs, "ollama-cloud") || !strings.Contains(errs, "--allow-ungraded-build") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if exists(o.dir) {
		t.Fatal("refused init wrote files")
	}
	code, out, errs := o.run(o.initArgs("--allow-ungraded-build")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, firstLines(out, 4), errs)
	}
	if !strings.Contains(readFileT(t, o.path("config.json")), `"tool_grade": "fail"`) {
		t.Fatal("tool grade not stored")
	}
	if !strings.Contains(errs+out, "tool_grade fail") {
		t.Fatalf("the failing provider is not reported:\n%s", errs)
	}
}

func TestGate09_CommandsRefuseAMissingConfigByName(t *testing.T) {
	o := newOnboarding(t)
	for _, args := range [][]string{{"matrix"}, {"matrix", "--all"}, {"providers", "list"}, {"providers", "test", "openai"},
		{"providers", "add", "--provider", "openai", "--yes"}, {"strategy", "show"}, {"strategy", "set", "--strategy", "best"}} {
		code, _, errs := o.run(args...)
		if code == 0 || !strings.Contains(errs, "missing_config") {
			t.Errorf("%v: exit %d stderr %q", args, code, errs)
		}
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"doctor"}, &out, &errb, o.deps); code == 0 || !strings.Contains(out.String(), "FAIL  config") || !strings.Contains(out.String(), "mgw init") {
		t.Fatalf("doctor on a missing config: %d %s", code, out.String())
	}
	if exists(o.dir) {
		t.Fatal("a read-only command created the config dir")
	}
}
