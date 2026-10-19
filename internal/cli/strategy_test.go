package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/policy"
)

func writeProfile(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engagement.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStrategySetAndShow(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	code, out, errs := o.run("strategy", "set", "--strategy", "cheapest", "--prefer", "openai,ollama-cloud",
		"--deny", "openai/gpt-6-astra", "--data-class", "customer", "--pin", "plan=openai/gpt-6.1-sol", "--pin", "review=ollama-cloud/kimi-k3")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "matrix --write") {
		t.Fatalf("no hint to repin the registry:\n%s", out)
	}
	assertMode(t, o.path("config.json"), 0o600)
	cfg, err := config.Load(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Strategy != "cheapest" || strings.Join(cfg.ProviderPolicy.Order, ",") != "openai,ollama-cloud" ||
		strings.Join(cfg.ProviderPolicy.Deny, ",") != "openai/gpt-6-astra" || cfg.DataClass != "customer" || cfg.Pins["plan"] != "openai/gpt-6.1-sol" {
		t.Fatalf("%+v", cfg)
	}

	code, out, errs = o.run("strategy", "show")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"strategy cheapest (install default)", "order openai,ollama-cloud (install default)", "deny openai/gpt-6-astra (install default)",
		"data_class customer (install default)", "pin plan=openai/gpt-6.1-sol (install default)", "pin review=ollama-cloud/kimi-k3 (install default)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestStrategyShowReportsTheWinnerOfThreeSources(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit() // install: balanced, order ollama-cloud,openai, internal
	prof := writeProfile(t, `{"strategy":"best","provider_policy":{"deny":["openai/gpt-6-astra"]},"data_class":"customer"}`)
	code, out, errs := o.run("--profile", prof, "strategy", "show", "--strategy", "cheapest")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"strategy cheapest (dispatch flag)",           // flag beats profile and install
		"order ollama-cloud,openai (install default)", // only the install sets it
		"deny openai/gpt-6-astra (engagement profile)",
		"data_class customer (engagement profile)", // profile beats install
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	// Without the flag the profile wins the strategy.
	_, out, _ = o.run("--profile", prof, "strategy", "show")
	if !strings.Contains(out, "strategy best (engagement profile)") {
		t.Fatalf("\n%s", out)
	}
	// Without the profile the install wins.
	_, out, _ = o.run("strategy", "show")
	if !strings.Contains(out, "strategy balanced (install default)") {
		t.Fatalf("\n%s", out)
	}
}

func TestStrategySetNamedFailures(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	before := readFileT(t, o.path("config.json"))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown strategy", []string{"--strategy", "fastest"}, "unknown_strategy"},
		{"unknown provider in prefer", []string{"--prefer", "nowhere"}, "unknown_provider"},
		{"unknown provider in deny", []string{"--deny", "nowhere"}, "unknown_provider"},
		{"unknown role", []string{"--pin", "janitor=openai/gpt-6.1-sol"}, "unknown_role"},
		{"unknown pin provider", []string{"--pin", "build=nowhere/x"}, "unknown_provider"},
		{"unknown pin model", []string{"--pin", "build=openai/nope"}, "unknown_model"},
		{"rank zero", []string{"--pin", "build=ollama-cloud/kimi-k3"}, "pin_not_admitted"},
		{"role not allowed", []string{"--pin", "explore=openai/gpt-6.1-sol"}, "pin_not_admitted"},
		{"pin on denied", []string{"--deny", "openai/gpt-6.1-sol", "--pin", "plan=openai/gpt-6.1-sol"}, "pin_not_admitted"},
		{"prefer and deny", []string{"--prefer", "openai", "--deny", "openai"}, "conflicting_policy"},
		{"bad data class", []string{"--data-class", "public"}, "bad_value"},
		{"bad pin shape", []string{"--pin", "build"}, "bad_value"},
		{"nothing to set", nil, "missing_value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errs := o.run(append([]string{"strategy", "set"}, c.args...)...)
			if code != ExitUsage || !strings.Contains(errs, c.want) {
				t.Fatalf("exit %d stderr %q, want %q", code, errs, c.want)
			}
			if readFileT(t, o.path("config.json")) != before {
				t.Fatal("a refused set changed config.json")
			}
		})
	}
}

func TestStrategyPinOnAToolGradeFailProviderIsRefusedForBuild(t *testing.T) {
	o := newOnboarding(t)
	o.fake.badTool[keyOpenAI] = true
	if code, _, errs := o.run(o.initArgs("--allow-ungraded-build")...); code != 0 {
		t.Fatalf("init %d: %s", code, errs)
	}
	code, _, errs := o.run("strategy", "set", "--pin", "build=openai/gpt-6.1-sol")
	if code != ExitUsage || !strings.Contains(errs, "pin_not_admitted") || !strings.Contains(errs, "tool_grade fail") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestProfileFlowsIntoMatrixAndPrintsSources(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	prof := writeProfile(t, `{"strategy":"best","provider_policy":{"order":["openai","ollama-cloud"],"deny":["openai/gpt-6-astra"]}}`)
	code, out, errs := o.run("--profile", prof, "matrix", "--all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"# effective strategy best (engagement profile)", "# effective order openai,ollama-cloud (engagement profile)", "# effective deny openai/gpt-6-astra (engagement profile)", "# effective data_class internal (install default)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, firstLines(out, 12))
		}
	}
	if strings.Contains(out, "gpt-6-astra ($") {
		t.Fatalf("denied model still a candidate:\n%s", firstLines(out, 40))
	}

	// matrix --write under the profile pins the profile's preference.
	outPath := filepath.Join(t.TempDir(), "engagement-registry.json")
	if code, _, errs := o.run("--profile", prof, "matrix", "--write", "--out", outPath); code != 0 {
		t.Fatalf("write %d: %s", code, errs)
	}
	reg, err := policy.DecodeRegistry([]byte(readFileT(t, outPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reg.ProviderPolicy.Preference, ",") != "openai,ollama-cloud" || strings.Join(reg.ProviderPolicy.Deny, ",") != "openai/gpt-6-astra" {
		t.Fatalf("profile policy not applied: %+v", reg.ProviderPolicy)
	}
	// A dispatch flag beats the profile.
	_, out, _ = o.run("--profile", prof, "matrix", "--strategy", "cheapest")
	if !strings.Contains(out, "# effective strategy cheapest (dispatch flag)") {
		t.Fatalf("\n%s", firstLines(out, 8))
	}
}

func TestProfileRejectedWhenOrderAndDenyConflict(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	prof := writeProfile(t, `{"provider_policy":{"order":["openai"],"deny":["openai"]}}`)
	for _, args := range [][]string{{"matrix", "--all"}, {"strategy", "show"}, {"doctor"}} {
		code, out, errs := o.run(append([]string{"--profile", prof}, args...)...)
		if code == 0 || !strings.Contains(out+errs, "conflicting_policy") {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errs)
		}
	}
	// A profile naming a provider that is not connected is rejected by name.
	prof = writeProfile(t, `{"provider_policy":{"order":["anthropic"]}}`)
	if code, _, errs := o.run("--profile", prof, "matrix", "--all"); code == 0 || !strings.Contains(errs, "unknown_provider") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// A pin in the profile goes through admission too.
	prof = writeProfile(t, `{"pins":{"build":"ollama-cloud/kimi-k3"}}`)
	if code, _, errs := o.run("--profile", prof, "matrix", "--all"); code == 0 || !strings.Contains(errs, "pin_not_admitted") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, errs := o.run("--profile", filepath.Join(t.TempDir(), "none.json"), "matrix", "--all"); code == 0 || !strings.Contains(errs, "missing_profile") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestParseGlobal(t *testing.T) {
	g, rest, err := ParseGlobal([]string{"--config", "/c", "--profile=/p.json", "matrix", "--all"})
	if err != nil || g.ConfigDir != "/c" || g.Profile != "/p.json" || strings.Join(rest, " ") != "matrix --all" {
		t.Fatalf("%+v %v %v", g, rest, err)
	}
	if _, _, err := ParseGlobal([]string{"--config"}); err == nil {
		t.Fatal("missing value accepted")
	}
	g, rest, _ = ParseGlobal([]string{"matrix", "--config", "x"})
	if g.ConfigDir != "" || strings.Join(rest, " ") != "matrix --config x" {
		t.Fatalf("flags after the command belong to the command: %+v %v", g, rest)
	}
}

func TestConfigFlagOverridesTheEnvironment(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	other := t.TempDir()
	if code, _, errs := o.run("--config", other, "providers", "list"); code == 0 || !strings.Contains(errs, "missing_config") || !strings.Contains(errs, other) {
		t.Fatalf("--config did not override MGW_CONFIG_DIR: %d %s", code, errs)
	}
}
