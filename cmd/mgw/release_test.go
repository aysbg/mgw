package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests exercise the Make recipe with explicit Git and compiler doubles.
// They do not claim to verify real release binaries from an uncommitted tree.
func releaseFixture(t *testing.T) (string, func(string, string) ([]byte, error)) {
	t.Helper()
	for _, tool := range []string{"make", "sh", "sha256sum", "sort", "cmp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release recipe needs %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	git := `#!/bin/sh
case "$1" in
  status) [ "$STATUS" != error ] || exit 1; printf '%s' "$STATUS" ;;
  describe) echo v1.2.3 ;;
  rev-parse) echo abcdef0 ;;
  show) echo 2026-10-19T10:00:00+00:00 ;;
  *) exit 1 ;;
esac
`
	compiler := `#!/bin/sh
set -eu
printf '%s|%s|%s|%s|%s\n' "$CGO_ENABLED" "$GOOS" "$GOARCH" "$GOTOOLCHAIN" "$*" >> "$BUILD_LOG"
out=
flags=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -ldflags) flags=$2; shift ;;
  esac
  shift
done
printf '%s %s %s\n' "$GOOS" "$GOARCH" "$flags" > "$out"
if [ "$NONREPRO" = yes ] && [ "${out##*/}" = repro ]; then echo different >> "$out"; fi
`
	for name, body := range map[string]string{"git": git, "go": compiler} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(status, nonrepro string) ([]byte, error) {
		cmd := exec.CommandContext(context.Background(), "make", "release", "GO="+filepath.Join(bin, "go"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"STATUS="+status, "NONREPRO="+nonrepro, "BUILD_LOG="+filepath.Join(dir, "build.log"))
		return cmd.CombinedOutput()
	}
	return dir, run
}

func TestReleaseRecipe(t *testing.T) {
	dir, run := releaseFixture(t)
	var first []byte
	for i := range 2 {
		if out, err := run("", "no"); err != nil {
			t.Fatalf("release: %v\n%s", err, out)
		}
		sums, err := os.ReadFile(filepath.Join(dir, "dist", "release", "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = sums
		} else if string(first) != string(sums) {
			t.Fatalf("manifest changed between runs:\n%s\n%s", first, sums)
		}
		var names []string
		for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("invalid checksum line: %s", line)
			}
			names = append(names, fields[1])
			data, err := os.ReadFile(filepath.Join(dir, "dist", "release", fields[1]))
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != fields[0] {
				t.Fatalf("digest %s, want %s", got, fields[0])
			}
		}
		want := []string{"mgw-v1.2.3-darwin-amd64", "mgw-v1.2.3-darwin-arm64", "mgw-v1.2.3-linux-amd64", "mgw-v1.2.3-windows-amd64.exe"}
		if !slices.Equal(names, want) {
			t.Fatalf("artifact order: %v", names)
		}
	}
	log, err := os.ReadFile(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 10 { // Four artifacts plus the Linux repeat, twice.
		t.Fatalf("builds: %s", log)
	}
	for _, line := range lines {
		for _, want := range []string{"0|", "|local|build -trimpath -buildvcs=true", "main.version=v1.2.3", "main.commit=abcdef0", "main.buildTime=2026-10-19T10:00:00+00:00"} {
			if !strings.Contains(line, want) {
				t.Fatalf("missing %q in %s", want, line)
			}
		}
	}
}

func TestReleaseRecipeRefusesDirtyOrUnknownTree(t *testing.T) {
	for _, status := range []string{" M tracked", "M  staged", "?? untracked", "error"} {
		t.Run(status, func(t *testing.T) {
			dir, run := releaseFixture(t)
			out, err := run(status, "no")
			if err == nil || (status != "error" && !strings.Contains(string(out), "refusing dirty tree")) {
				t.Fatalf("err %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
				t.Fatalf("compiler ran before dirty check: %v", err)
			}
		})
	}
}

func TestReleaseRecipeRejectsNonReproducibleBuild(t *testing.T) {
	dir, run := releaseFixture(t)
	if out, err := run("", "yes"); err == nil {
		t.Fatalf("non-reproducible release succeeded: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "release")); !os.IsNotExist(err) {
		t.Fatalf("published non-reproducible release: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "dist"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary build not cleaned up: %v %v", entries, err)
	}
}
