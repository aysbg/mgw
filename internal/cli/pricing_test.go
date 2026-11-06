package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
	for _, name := range []string{"pricing.json", "custom.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			snap := filepath.Join(dir, "state", name)
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
			if !s.Pinned || s.SourceSHA256 != independentSHA(raw) {
				t.Fatalf("approval/source digest: %+v", s)
			}
			assertMode(t, snap+".sha256", 0o644)
			if got, want := readFileT(t, snap+".sha256"), independentSHA(data)+"  "+name+"\n"; got != want {
				t.Fatalf("pin %q, want %q", got, want)
			}
		})
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

// independentSHA deliberately does not use the production digest helper.
func independentSHA(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func TestPricingRefreshProvenanceRejections(t *testing.T) {
	type entry struct {
		Mode fs.FileMode
		Data string
	}
	capture := func(t *testing.T, dir string) map[string]entry {
		t.Helper()
		state := map[string]entry{}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			var data []byte
			if !d.IsDir() {
				data, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
			state[strings.TrimPrefix(path, dir)] = entry{info.Mode(), string(data)}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	for _, existing := range []bool{false, true} {
		for _, pin := range []string{"", strings.Repeat("0", 64)} {
			t.Run(fmt.Sprintf("existing=%v/pin=%s", existing, pin), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "state", "pricing.json")
				var out, errs bytes.Buffer
				if existing {
					if code := Pricing([]string{"refresh", "--snapshot", path, "--sha256", independentSHA(fixtureBytes(t))}, &out, &errs, testDeps()); code != ExitOK {
						t.Fatalf("valid refresh: %d %s", code, errs.String())
					}
				}
				before := capture(t, dir)
				out.Reset()
				errs.Reset()
				args := []string{"refresh", "--snapshot", path}
				if pin != "" {
					args = append(args, "--sha256", pin)
				}
				if code := Pricing(args, &out, &errs, testDeps()); code != ExitError {
					t.Fatalf("rejected refresh: %d %s", code, errs.String())
				}
				if out.Len() != 0 {
					t.Fatalf("success output on refusal: %s", out.String())
				}
				if after := capture(t, dir); !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed bytes, modes or entries:\nbefore=%v\nafter=%v", before, after)
				}
				if !existing && (exists(path) || exists(path+".sha256")) {
					t.Fatal("fresh refusal wrote snapshot or pin")
				}
			})
		}
	}
	for _, target := range []string{"snapshot", "pin"} {
		t.Run(target+" write failure", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pricing.json")
			blocked := path
			if target == "pin" {
				blocked += ".sha256"
			}
			if err := os.Mkdir(blocked, 0o700); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			code := Pricing([]string{"refresh", "--snapshot", path, "--sha256", independentSHA(fixtureBytes(t))}, &out, &errs, testDeps())
			if code != ExitError || !strings.Contains(errs.String(), "write snapshot") || !strings.Contains(errs.String(), blocked) || out.Len() != 0 {
				t.Fatalf("write failure: %d stdout=%s stderr=%s", code, out.String(), errs.String())
			}
			if target == "pin" && !strings.Contains(errs.String(), "pin") {
				t.Fatal("missing pin-write error")
			}
		})
	}
}

func TestDoctorCatalogProvenancePrecedence(t *testing.T) {
	raw := fixtureBytes(t)
	c, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(approved bool, days int) []byte {
		s := catalog.NewSnapshot(c, "https://example.test/api.json", independentSHA(raw), fixedNow.Add(-time.Duration(days)*24*time.Hour), approved)
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	firstUse := "unpinned: catalog digest " + independentSHA(raw) + " was recorded on first use; run mgw pricing refresh to review the current digest, then rerun it with --sha256 <digest>"
	fresh := snapshot(true, 0)
	oldUnapproved := snapshot(false, 31)
	// A decodable one-byte mutation must fail integrity even before approval/age.
	tampered := bytes.Replace(oldUnapproved, []byte("https"), []byte("httpx"), 1)
	if bytes.Equal(tampered, oldUnapproved) {
		t.Fatal("missing tamper target")
	}
	if _, err := catalog.ReadSnapshot(tampered); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		data     []byte
		pin      string
		level    string
		detail   string
		contains string
	}{
		{name: "missing", level: "warn"},
		{name: "snapshot directory", level: "FAIL", contains: "is a directory"},
		{name: "invalid JSON without pin", data: []byte(`not json`), level: "FAIL", contains: "snapshot: decode:"},
		{name: "bad timestamp", data: []byte(`{"captured_at":"yesterday"}`), level: "FAIL", contains: "snapshot: captured_at:"},
		{name: "missing pin masks approval and age", data: oldUnapproved, level: "warn", detail: "unpinned: no pricing.json.sha256 next to the snapshot; run mgw pricing refresh"},
		{name: "pin directory", data: fresh, pin: "directory", level: "FAIL", contains: "is a directory"},
		{name: "mismatch masks approval and age", data: tampered, pin: independentSHA(oldUnapproved), level: "FAIL", detail: "snapshot sha256 " + independentSHA(tampered) + " does not match the pin " + independentSHA(oldUnapproved)},
		{name: "malformed digest", data: fresh, pin: "not-a-sha256", level: "FAIL", detail: "snapshot sha256 " + independentSHA(fresh) + " does not match the pin not-a-sha256"},
		{name: "false approval masks age", data: oldUnapproved, pin: "matching", level: "warn", detail: firstUse},
		{name: "approved exactly 30 days", data: snapshot(true, 30), pin: "matching", level: "ok", detail: "snapshot captured_at " + fixedNow.Add(-30*24*time.Hour).Format(time.RFC3339)},
		{name: "approved 31 days", data: snapshot(true, 31), pin: "matching", level: "warn", detail: fmt.Sprintf("snapshot captured_at %s is 31 days old (limit 30); run `mgw pricing refresh`", fixedNow.Add(-31*24*time.Hour).Format(time.RFC3339))},
		{name: "approved fresh", data: fresh, pin: "matching", level: "ok", detail: "snapshot captured_at " + fixedNow.Format(time.RFC3339)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pricing.json")
			if tc.name == "snapshot directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.data != nil {
				if err := os.WriteFile(path, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pin == "directory" {
				if err := os.Mkdir(path+".sha256", 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.pin != "" {
				pin := tc.pin
				if pin == "matching" {
					pin = independentSHA(tc.data)
				}
				if err := os.WriteFile(path+".sha256", []byte(" \n"+pin+"  pricing.json\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var rows []doctorCheck
			checkSnapshot(func(level, name, detail string) { rows = append(rows, doctorCheck{level, name, detail}) }, testDeps(), path)
			if len(rows) != 1 || rows[0].Name != "catalog" || rows[0].Level != tc.level {
				t.Fatalf("rows %+v", rows)
			}
			want := tc.detail
			if tc.name == "missing" {
				want = "no pricing snapshot at " + path + "; run `mgw pricing refresh`"
			}
			if tc.contains != "" {
				if !strings.Contains(rows[0].Detail, tc.contains) {
					t.Fatalf("detail %q lacks %q", rows[0].Detail, tc.contains)
				}
				if strings.Contains(tc.name, "directory") && !strings.Contains(rows[0].Detail, path) {
					t.Fatalf("read error lacks path: %+v", rows)
				}
			} else if rows[0].Detail != want {
				t.Fatalf("detail %q, want %q", rows[0].Detail, want)
			}
		})
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
