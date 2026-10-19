package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDoctorReportsBinaryAndRegistryBuildPin(t *testing.T) {
	build := BuildInfo{Version: "v1.2.3", Commit: "abcdef0123456789", BuildTime: "2026-10-19T10:00:00Z"}
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *onboarding)
		want string
	}{
		{name: "matching", want: "ok    binary-registry"},
		{name: "mixed install", edit: func(_ *testing.T, o *onboarding) {
			o.deps.Build.Commit = "123456789abcdef0"
		}, want: "mixed install: binary commit 123456789abcdef0 does not match registry pin commit abcdef0123456789"},
		{name: "abbreviated commit", edit: func(_ *testing.T, o *onboarding) {
			o.deps.Build.Commit = "abcdef0"
		}, want: "ok    binary-registry"},
		{name: "legacy registry", edit: func(t *testing.T, o *onboarding) {
			if err := os.Remove(o.path("registry.json.build.json")); err != nil {
				t.Fatal(err)
			}
		}, want: "cannot compare binary commit"},
		{name: "invalid pin", edit: func(t *testing.T, o *onboarding) {
			if err := os.WriteFile(o.path("registry.json.build.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "no usable commit"},
		{name: "stale pin", edit: func(t *testing.T, o *onboarding) {
			if err := writeRegistryBuild(o.path("registry.json"), strings.Repeat("0", 64), build); err != nil {
				t.Fatal(err)
			}
		}, want: "build pin does not identify the current registry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOnboarding(t)
			o.deps.Build = build
			o.mustInit()
			var pin registryBuild
			if err := json.Unmarshal([]byte(readFileT(t, o.path("registry.json.build.json"))), &pin); err != nil {
				t.Fatal(err)
			}
			if pin.BuildInfo != build || len(pin.SHA256) != 64 {
				t.Fatalf("build pin: %+v", pin)
			}
			assertMode(t, o.path("registry.json.build.json"), 0o644)
			if tc.edit != nil {
				tc.edit(t, o)
			}
			code, out, errs := o.run("doctor")
			if code != ExitOK || !strings.Contains(out, tc.want) || !strings.Contains(out, "version v1.2.3, commit "+o.deps.Build.Commit+", built "+build.BuildTime) {
				t.Fatalf("exit %d\n%s%s", code, out, errs)
			}
			if tc.name != "matching" && tc.name != "abbreviated commit" && !strings.Contains(out, "warn  binary-registry") {
				t.Fatalf("missing warning: %s", out)
			}
		})
	}
}

func TestMatrixRecordsBuildPinForCustomRegistry(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	o.deps.Build = BuildInfo{Version: "v2", Commit: "1234567", BuildTime: "2026-10-19T12:00:00Z"}
	path := o.path("custom", "registry.json")
	if code, out, errs := o.run("matrix", "--write", "--out", path); code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	code, out, errs := o.run("doctor", "--registry", path)
	if code != ExitOK || !strings.Contains(out, "binary commit matches registry pin commit 1234567") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

func TestDoctorLocalBuildDoesNotCompareCommits(t *testing.T) {
	o := newOnboarding(t)
	o.mustInit()
	o.deps.Build = BuildInfo{Version: "dev", Commit: "none", BuildTime: "unknown"}
	code, out, errs := o.run("doctor")
	if code != ExitOK || !strings.Contains(out, "version dev, commit none, built unknown") || strings.Contains(out, "binary-registry") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}
