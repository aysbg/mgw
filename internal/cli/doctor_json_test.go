package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

type doctorJSONCheck struct {
	Level  string `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type doctorJSONReport struct {
	OK     bool              `json:"ok"`
	Checks []doctorJSONCheck `json:"checks"`
}

// decodeDoctorJSON parses the one JSON object `doctor --json` must print and
// fails if stdout carries anything else.
func decodeDoctorJSON(t *testing.T, out string) doctorJSONReport {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var report doctorJSONReport
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\nstdout:\n%s", err, out)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("doctor --json printed more than one JSON object: %v\nstdout:\n%s", err, out)
	}
	return report
}

func assertJSONLevel(t *testing.T, c doctorJSONCheck) {
	t.Helper()
	switch c.Level {
	case "ok", "warn", "fail":
	default:
		t.Errorf("check %q level %q is not one of ok, warn, fail", c.Name, c.Level)
	}
	if c.Level != strings.ToLower(c.Level) {
		t.Errorf("check %q level %q is not lowercase", c.Name, c.Level)
	}
}

func TestDoctorJSONReportsAMissingConfig(t *testing.T) {
	o := newOnboarding(t) // no init: the config directory does not exist

	code, out, errs := o.run("doctor", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, out, errs)
	}
	report := decodeDoctorJSON(t, out)
	if report.OK {
		t.Fatalf("ok must be false when a check failed:\n%s", out)
	}
	if report.Checks == nil {
		t.Fatalf("checks must be a JSON array:\n%s", out)
	}
	var config *doctorJSONCheck
	for i := range report.Checks {
		c := &report.Checks[i]
		assertJSONLevel(t, *c)
		if c.Name == "config" {
			config = c
		}
	}
	if config == nil {
		t.Fatalf("missing the config check:\n%s", out)
	}
	if config.Level != "fail" || config.Detail == "" {
		t.Fatalf("config check = %+v, want level fail and a detail", *config)
	}
	if strings.Contains(out, `"level":"FAIL"`) || strings.Contains(out, "ok    ") {
		t.Fatalf("JSON mode must not print the table:\n%s", out)
	}

	tableCode, tableOut, _ := o.run("doctor")
	if tableCode != code {
		t.Fatalf("table exit %d, JSON exit %d", tableCode, code)
	}
	if !strings.Contains(tableOut, "FAIL  config") {
		t.Fatalf("table mode lost its FAIL spelling:\n%s", tableOut)
	}
}

func TestDoctorJSONReportsAPassingInstall(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()

	code, out, errs := o.run("doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, out, errs)
	}
	report := decodeDoctorJSON(t, out)
	if !report.OK {
		t.Fatalf("ok must be true when no check failed:\n%s", out)
	}
	if len(report.Checks) == 0 {
		t.Fatalf("expected at least one check:\n%s", out)
	}
	names := map[string]bool{}
	for _, c := range report.Checks {
		assertJSONLevel(t, c)
		if c.Level == "fail" {
			t.Fatalf("check %q failed in a passing install:\n%s", c.Name, out)
		}
		names[c.Name] = true
	}
	for _, want := range []string{"config", "credentials", "binary", "ca-bundle", "registry", "catalog", "tool-grade", "strategy"} {
		if !names[want] {
			t.Fatalf("missing check %q:\n%s", want, out)
		}
	}

	tableCode, tableOut, _ := o.run("doctor")
	if tableCode != code {
		t.Fatalf("table exit %d, JSON exit %d", tableCode, code)
	}
	if strings.HasPrefix(strings.TrimSpace(tableOut), "{") {
		t.Fatalf("table mode printed JSON:\n%s", tableOut)
	}
}

func TestDoctorJSONLevelIsLowercaseWhenTablePrintsFAIL(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if err := os.Chmod(o.path("credentials", "openai.key"), 0o644); err != nil {
		t.Fatal(err)
	}

	tableCode, tableOut, _ := o.run("doctor")
	if tableCode != ExitError || !strings.Contains(tableOut, "FAIL  credentials") {
		t.Fatalf("table exit %d\n%s", tableCode, tableOut)
	}
	jsonCode, jsonOut, _ := o.run("doctor", "--json")
	if jsonCode != tableCode {
		t.Fatalf("table exit %d, JSON exit %d", tableCode, jsonCode)
	}
	report := decodeDoctorJSON(t, jsonOut)
	if report.OK {
		t.Fatalf("ok must be false for a failed credentials check:\n%s", jsonOut)
	}
	var found bool
	for _, c := range report.Checks {
		if c.Name != "credentials" {
			continue
		}
		found = true
		if c.Level != "fail" {
			t.Fatalf("credentials level = %q, want fail", c.Level)
		}
		if !strings.Contains(c.Detail, "openai.key") {
			t.Fatalf("credentials detail %q lost the table detail", c.Detail)
		}
	}
	if !found {
		t.Fatalf("missing the credentials check:\n%s", jsonOut)
	}
	if strings.Contains(jsonOut, `"level":"FAIL"`) {
		t.Fatalf("JSON mode must use lowercase fail:\n%s", jsonOut)
	}
}

func TestDoctorJSONWorksWithTheOtherFlags(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	const catalogURL = "https://example.test/api.json"
	fetched := 0
	baseFetch := testDeps().Fetch
	o.deps.Fetch = func(ctx context.Context, url string) ([]byte, string, error) {
		fetched++
		if url != catalogURL {
			t.Errorf("catalog URL = %q, want %q", url, catalogURL)
		}
		return baseFetch(ctx, url)
	}

	args := []string{"--snapshot", o.path("pricing.json"), "--registry", o.path("registry.json"), "--network", "--catalog-url", catalogURL}
	jsonArgs := append([]string{"doctor", "--json"}, args...)
	code, out, errs := o.run(jsonArgs...)
	if code != ExitOK {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, out, errs)
	}
	report := decodeDoctorJSON(t, out)
	if !report.OK {
		t.Fatalf("ok must be true:\n%s", out)
	}
	var reachable *doctorJSONCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "catalog-reachability" {
			reachable = &report.Checks[i]
		}
	}
	if reachable == nil || reachable.Level != "ok" || !strings.Contains(reachable.Detail, catalogURL) {
		t.Fatalf("catalog-reachability check = %+v\n%s", reachable, out)
	}
	if fetched != 1 {
		t.Fatalf("fetched %d times, want 1", fetched)
	}

	tableArgs := append([]string{"doctor"}, args...)
	tableCode, _, _ := o.run(tableArgs...)
	if tableCode != code {
		t.Fatalf("table exit %d, JSON exit %d", tableCode, code)
	}
}

func TestDoctorJSONLeavesFlagErrorsOnTheUsageExit(t *testing.T) {
	o := newOnboarding(t)
	code, out, errs := o.run("doctor", "--json", "--not-a-flag")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	if out != "" {
		t.Fatalf("stdout on a flag error:\n%s", out)
	}
	if !strings.Contains(errs, "not-a-flag") {
		t.Fatalf("stderr does not name the bad flag:\n%s", errs)
	}
}
