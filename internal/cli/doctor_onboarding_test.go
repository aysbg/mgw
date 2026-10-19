package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorPassesOnAFreshInstall(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	code, out, errs := o.run("doctor")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"ok    config", "ok    credentials", "ok    ca-bundle", "ok    registry", "ok    catalog", "ok    tool-grade", "ok    strategy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestDoctorFailsOnALooseCredentialFile(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	if err := os.Chmod(o.path("credentials", "openai.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := o.run("doctor")
	if code != ExitError || !strings.Contains(out, "FAIL  credentials") || !strings.Contains(out, "openai.key") || !strings.Contains(out, "0644") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// A missing key for a connected provider fails as well.
	if err := os.Chmod(o.path("credentials", "openai.key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(o.path("credentials", "openai.key")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := o.run("doctor"); code != ExitError || !strings.Contains(out, "FAIL  credentials") || !strings.Contains(out, "missing") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorFailsOnARegistryHashMismatch(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	reg := readFileT(t, o.path("registry.json"))
	if err := os.WriteFile(o.path("registry.json"), []byte(strings.Replace(reg, "strategy-1.0", "strategy-1.1", 1)), 0o644); err != nil { // #nosec G306 -- test fixture
		t.Fatal(err)
	}
	code, out, _ := o.run("doctor")
	if code != ExitError || !strings.Contains(out, "FAIL  registry") || !strings.Contains(out, "does not match the pin") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorWarnsOnAnOldSnapshotAndChecksTheCatalogOnlyWithNetwork(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	o.deps.Now = func() time.Time { return fixedNow.Add(31 * 24 * time.Hour) }
	code, out, _ := o.run("doctor")
	if code != 0 || !strings.Contains(out, "warn  catalog") || !strings.Contains(out, "31 days") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "reachab") {
		t.Fatal("reachability checked without --network")
	}

	fetched := 0
	o.deps.Fetch = func(context.Context, string) ([]byte, string, error) {
		fetched++
		return nil, "", errors.New("no route")
	}
	code, out, _ = o.run("doctor", "--network")
	if code != ExitError || !strings.Contains(out, "FAIL  catalog-reachability") || fetched != 1 {
		t.Fatalf("exit %d fetched %d\n%s", code, fetched, out)
	}
	o.deps.Fetch = testDeps().Fetch
	if code, out, _ := o.run("doctor", "--network"); code != 0 || !strings.Contains(out, "ok    catalog-reachability") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorFailsOnAnUnknownStrategyInTheConfig(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	cfg := readFileT(t, o.path("config.json"))
	if err := os.WriteFile(o.path("config.json"), []byte(strings.Replace(cfg, `"strategy": "balanced"`, `"strategy": "fastest"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := o.run("doctor")
	if code != ExitError || !strings.Contains(out, "FAIL  strategy") || !strings.Contains(out, "fastest") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorWarnsWhenTheCABundleIsMissing(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	o.deps.CAPaths = []string{filepath.Join(t.TempDir(), "none.pem")}
	if code, out, _ := o.run("doctor"); code != 0 || !strings.Contains(out, "warn  ca-bundle") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
