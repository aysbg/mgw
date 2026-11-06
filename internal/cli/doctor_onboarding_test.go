package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
)

func TestDoctorPassesOnAFreshInstall(t *testing.T) {
	o := newOnboarding(t)
	if code, out, errs := o.run(o.initArgs("--catalog-sha256", independentSHA(fixtureBytes(t)))...); code != ExitOK {
		t.Fatalf("approved init: %d %s %s", code, out, errs)
	}
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
	if code, out, errs := o.run(o.initArgs("--catalog-sha256", independentSHA(fixtureBytes(t)))...); code != ExitOK {
		t.Fatalf("approved init: %d %s %s", code, out, errs)
	}
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

func requireCatalogReport(t *testing.T, o *onboarding, level, detail string, wantCode int) {
	t.Helper()
	code, out, errs := o.run("doctor", "--json")
	if code != wantCode {
		t.Fatalf("JSON doctor: %d, want %d\n%s%s", code, wantCode, out, errs)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.OK != (wantCode == ExitOK) {
		t.Fatalf("report ok=%v: %s", report.OK, out)
	}
	var rows []doctorCheck
	for _, row := range report.Checks {
		if row.Name == "catalog" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 || rows[0].Level != level || rows[0].Detail != detail {
		t.Fatalf("catalog rows %+v, want %s %q", rows, level, detail)
	}
	tableCode, table, tableErrs := o.run("doctor")
	tableLevel := level
	if level == "fail" {
		tableLevel = "FAIL"
	}
	label := tableLevel + strings.Repeat(" ", 6-len(tableLevel)) + "catalog"
	if tableCode != code || !strings.Contains(table, label) || !strings.Contains(table, detail) {
		t.Fatalf("table/JSON disagreement: %d %s%s", tableCode, table, tableErrs)
	}
}

func TestDoctorCatalogProvenanceLifecycle(t *testing.T) {
	t.Run("FirstUseRefreshTamperRestore", func(t *testing.T) {
		o := newOnboarding(t)
		o.mustInit()
		path := o.path("pricing.json")
		initial := []byte(readFileT(t, path))
		s, err := catalog.ReadSnapshot(initial)
		if err != nil || s.Pinned {
			t.Fatalf("first use: %+v %v", s, err)
		}
		warning := "unpinned: catalog digest " + s.SourceSHA256 + " was recorded on first use; run mgw pricing refresh to review the current digest, then rerun it with --sha256 <digest>"
		requireCatalogReport(t, o, "warn", warning, ExitOK)
		if code, out, errs := o.run("pricing", "refresh", "--sha256", independentSHA(fixtureBytes(t))); code != ExitOK {
			t.Fatalf("refresh: %d %s%s", code, out, errs)
		}
		original := []byte(readFileT(t, path))
		s, err = catalog.ReadSnapshot(original)
		if err != nil || !s.Pinned {
			t.Fatalf("approved snapshot: %+v %v", s, err)
		}
		okDetail := "snapshot captured_at " + s.CapturedAt
		requireCatalogReport(t, o, "ok", okDetail, ExitOK)
		if !bytes.Contains(original, []byte(`"source_url": "https`)) {
			t.Fatal("source_url has no https tamper target")
		}
		tampered := bytes.Replace(original, []byte(`"source_url": "https`), []byte(`"source_url": "httpx`), 1)
		if len(tampered) != len(original) {
			t.Fatal("tamper changed file length")
		}
		differences := 0
		for i := range original {
			if original[i] != tampered[i] {
				differences++
			}
		}
		if differences != 1 {
			t.Fatalf("tamper changed %d bytes", differences)
		}
		if _, err := catalog.ReadSnapshot(tampered); err != nil {
			t.Fatalf("tamper must remain valid JSON: %v", err)
		}
		originalDigest, changedDigest := independentSHA(original), independentSHA(tampered)
		if originalDigest == changedDigest {
			t.Fatal("tamper digests equal")
		}
		pin := readFileT(t, path+".sha256")
		if want := originalDigest + "  pricing.json\n"; pin != want {
			t.Fatalf("pin %q, want %q", pin, want)
		}
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readFileT(t, path+".sha256"); got != pin {
			t.Fatal("tamper changed pin")
		}
		requireCatalogReport(t, o, "fail", "snapshot sha256 "+changedDigest+" does not match the pin "+originalDigest, ExitError)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		requireCatalogReport(t, o, "ok", okDetail, ExitOK)
	})
	t.Run("ProvidersAddUnpinned", func(t *testing.T) {
		o := newOnboarding(t)
		o.mustInit()
		if code, out, errs := o.run("pricing", "refresh", "--sha256", independentSHA(fixtureBytes(t))); code != ExitOK {
			t.Fatalf("refresh: %d %s%s", code, out, errs)
		}
		path := o.path("pricing.json")
		before, err := catalog.ReadSnapshot([]byte(readFileT(t, path)))
		if err != nil || !before.Pinned {
			t.Fatalf("pre-add approval: %+v %v", before, err)
		}
		anth := writeKeyFile(t, "anth.key", keyAnthropic, 0o600)
		code, out, errs := o.run("providers", "add", "--provider", "anthropic", "--key-file", anth,
			"--billing", "pay_per_token", "--base-url", "anthropic="+o.base(), "--yes")
		if code != ExitOK {
			t.Fatalf("providers add: %d %s%s", code, out, errs)
		}
		data := []byte(readFileT(t, path))
		if got, want := readFileT(t, path+".sha256"), independentSHA(data)+"  pricing.json\n"; got != want {
			t.Fatalf("new snapshot pin %q, want %q", got, want)
		}
		after, err := catalog.ReadSnapshot(data)
		if err != nil || after.Pinned || after.SourceSHA256 != independentSHA(fixtureBytes(t)) {
			t.Fatalf("post-add provenance: %+v %v", after, err)
		}
		requireCatalogReport(t, o, "warn", "unpinned: catalog digest "+after.SourceSHA256+" was recorded on first use; run mgw pricing refresh to review the current digest, then rerun it with --sha256 <digest>", ExitOK)
	})
}

func TestInitPricingPinWriteFailure(t *testing.T) {
	o := newOnboarding(t)
	path := o.path("pricing.json.sha256")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, errs := o.run(o.initArgs()...)
	if code != ExitError || !strings.Contains(errs, "write pricing snapshot pin") || !strings.Contains(errs, path) {
		t.Fatalf("pin-write failure: %d %s%s", code, out, errs)
	}
	if strings.Contains(out, "wrote "+path+"\n") {
		t.Fatalf("printed pin success on failure: %s", out)
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
