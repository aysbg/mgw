package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/config"
)

func withCA(t *testing.T) Deps {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := testDeps()
	d.CAPaths = []string{ca}
	return d
}

// withInstall is withCA plus a minimal valid install, so doctor has a config.
func withInstall(t *testing.T) Deps {
	t.Helper()
	d := withCA(t)
	dir := t.TempDir()
	cfg := config.New()
	cfg.Providers = []config.Provider{{ID: "ollama-cloud", Dialect: "ollama", BaseURL: "https://ollama.com", Billing: "included_credits", IncludeStreamUsage: true}}
	cfg.Strategy = "balanced"
	cfg.ProviderPolicy.Order = []string{"ollama-cloud"}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteCredential(dir, "ollama-cloud", "fixture-key-not-a-secret"); err != nil {
		t.Fatal(err)
	}
	d.ConfigDir = dir
	return d
}

func TestPricingRefreshVerifiesDigestAndWrites(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "state", "pricing.json")
	raw := fixtureBytes(t)
	var gotURL string
	d := testDeps()
	d.Fetch = func(_ context.Context, url string) ([]byte, string, error) {
		gotURL = url
		return raw, catalog.Digest(raw), nil
	}
	var out, errb bytes.Buffer
	code := Pricing([]string{"refresh", "--url", "https://example.test/api.json", "--sha256", catalog.Digest(raw), "--snapshot", snap}, &out, &errb, d)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if gotURL != "https://example.test/api.json" {
		t.Fatalf("url = %q", gotURL)
	}
	data, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	s, err := catalog.ReadSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.CapturedAt != "2026-10-19T12:00:00Z" || s.SourceSHA256 != catalog.Digest(raw) || s.SourceURL != "https://example.test/api.json" {
		t.Fatalf("snapshot header: %+v", s)
	}
	if st, _ := os.Stat(snap); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestPricingRefreshRefusesWrongOrMissingDigest(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "pricing.json")
	raw := fixtureBytes(t)

	var out, errb bytes.Buffer
	code := Pricing([]string{"refresh", "--sha256", strings.Repeat("0", 64), "--snapshot", snap}, &out, &errb, testDeps())
	if code != ExitError || !strings.Contains(errb.String(), "digest") {
		t.Fatalf("wrong digest: %d %s", code, errb.String())
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatal("snapshot written despite digest mismatch")
	}

	errb.Reset()
	code = Pricing([]string{"refresh", "--snapshot", snap}, &out, &errb, testDeps())
	if code != ExitError || !strings.Contains(errb.String(), catalog.Digest(raw)) || !strings.Contains(errb.String(), "--sha256") {
		t.Fatalf("missing digest: %d %s", code, errb.String())
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatal("snapshot written without a pinned digest")
	}
}

func TestPricingRefreshFetchFailureAndBadCatalog(t *testing.T) {
	d := testDeps()
	d.Fetch = func(context.Context, string) ([]byte, string, error) { return nil, "", errors.New("boom") }
	var out, errb bytes.Buffer
	if code := Pricing([]string{"refresh", "--sha256", "aa", "--snapshot", filepath.Join(t.TempDir(), "p.json")}, &out, &errb, d); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	d.Fetch = func(context.Context, string) ([]byte, string, error) {
		b := []byte(`not json`)
		return b, catalog.Digest(b), nil
	}
	p := filepath.Join(t.TempDir(), "p.json")
	if code := Pricing([]string{"refresh", "--sha256", catalog.Digest([]byte(`not json`)), "--snapshot", p}, &out, &errb, d); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("snapshot written for an undecodable catalog")
	}
}

func TestPricingShow(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	var out, errb bytes.Buffer
	if code := Pricing([]string{"show", "--snapshot", snap}, &out, &errb, testDeps()); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"captured_at 2026-10-19T12:00:00Z", "blend 90/10 input/output", "population agent_capable", "ollama-cloud", "0.195"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	Pricing([]string{"show", "--snapshot", snap, "--models"}, &out, &errb, testDeps())
	if !strings.Contains(out.String(), "ollama-cloud/deepseek-v4.1-flash:1017") {
		t.Fatalf("--models missing the model:\n%s", out.String())
	}
	if code := Pricing([]string{"show", "--snapshot", filepath.Join(t.TempDir(), "nope.json")}, &out, &errb, testDeps()); code != ExitError {
		t.Fatalf("exit %d", code)
	}
}

func TestPricingUsage(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{{}, {"bogus"}, {"refresh", "--nope"}} {
		if code := Pricing(args, &out, &errb, testDeps()); code != ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestDoctor(t *testing.T) {
	dir := t.TempDir()
	run := func(d Deps, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Doctor(args, &out, &errb, d)
		return code, out.String() + errb.String()
	}
	fresh := writeSnapshot(t, dir, fixedNow.Add(-29*24*time.Hour))
	reg := filepath.Join(dir, "registry.json")

	code, out := run(withInstall(t), "--snapshot", fresh, "--registry", reg)
	if code != 0 {
		t.Fatalf("warnings must not fail doctor: %d\n%s", code, out)
	}
	if !strings.Contains(out, "ok    catalog") || !strings.Contains(out, "warn  registry") || !strings.Contains(out, "unpinned") {
		t.Fatalf("fresh snapshot / unpinned registry:\n%s", out)
	}

	old := writeSnapshot(t, t.TempDir(), fixedNow.Add(-31*24*time.Hour))
	_, out = run(withInstall(t), "--snapshot", old, "--registry", reg)
	if !strings.Contains(out, "warn  catalog") || !strings.Contains(out, "31 days") {
		t.Fatalf("stale snapshot not warned:\n%s", out)
	}

	_, out = run(withInstall(t), "--snapshot", filepath.Join(dir, "none.json"), "--registry", reg)
	if !strings.Contains(out, "warn  catalog") || !strings.Contains(out, "mgw pricing refresh") {
		t.Fatalf("missing snapshot:\n%s", out)
	}

	noCA := withInstall(t)
	noCA.CAPaths = []string{filepath.Join(dir, "absent.pem")}
	_, out = run(noCA, "--snapshot", fresh, "--registry", reg)
	if !strings.Contains(out, "warn  ca-bundle") {
		t.Fatalf("missing CA bundle not warned:\n%s", out)
	}

	env := withInstall(t)
	env.CAPaths = nil
	env.Getenv = func(k string) string {
		if k == "SSL_CERT_FILE" {
			return filepath.Join(dir, "absent.pem")
		}
		return ""
	}
	if _, out = run(env, "--snapshot", fresh, "--registry", reg); !strings.Contains(out, "warn  ca-bundle") {
		t.Fatalf("SSL_CERT_FILE pointing nowhere must warn:\n%s", out)
	}
}
