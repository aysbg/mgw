package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/parity"
	"github.com/aysbg/mgw/internal/policy"
)

var fixedNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func testDeps() Deps {
	return Deps{
		Now:     func() time.Time { return fixedNow },
		Getenv:  func(string) string { return "" },
		CAPaths: nil,
		Fetch: func(context.Context, string) ([]byte, string, error) {
			b := fixtureBytes(nil)
			return b, catalog.Digest(b), nil
		},
	}
}

func fixtureBytes(t testing.TB) []byte {
	if t != nil {
		t.Helper()
	}
	b, err := os.ReadFile("../catalog/testdata/models.json")
	if err != nil {
		panic(err)
	}
	return b
}

func writeSnapshot(t *testing.T, dir string, captured time.Time) string {
	t.Helper()
	raw := fixtureBytes(t)
	c, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := catalog.NewSnapshot(c, "fixture", catalog.Digest(raw), captured)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "pricing.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runMatrix(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Matrix(args, &out, &errb, testDeps())
	return code, out.String(), errb.String()
}

func TestMatrixPrintsOneStrategy(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	code, out, errs := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--strategy", "best")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "blend 90/10 input/output") || !strings.Contains(out, "strategy best") {
		t.Fatalf("header missing:\n%s", firstLines(out, 4))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// 2 comment lines, 1 column header, 210 rows.
	if len(lines) != 2+1+210 {
		t.Fatalf("lines = %d", len(lines))
	}
	var row string
	for _, l := range lines {
		if strings.HasPrefix(l, "build") && strings.Contains(l, " feature ") && strings.Contains(l, " M ") && strings.Contains(l, " false ") {
			row = l
		}
	}
	if !strings.Contains(row, "ollama-cloud/glm-5.3") {
		t.Fatalf("row: %q", row)
	}
	// Risk rows carry the effort rung.
	if !strings.Contains(out, "ollama-cloud/kimi-k3@high") {
		t.Fatal("risk rung not shown")
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("colour escape in output")
	}
	// Stable.
	_, out2, _ := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--strategy", "best")
	if out != out2 {
		t.Fatal("output not stable")
	}
	// Columns align: every row's second column starts at the same offset.
	col := strings.Index(lines[2], "TYPE")
	for _, l := range lines[3:] {
		if len(l) <= col || l[col-1] != ' ' || l[col] == ' ' {
			t.Fatalf("misaligned row %q", l)
		}
	}
}

func TestMatrixDefaultStrategyIsBalanced(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	_, out, _ := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud")
	if !strings.Contains(out, "strategy balanced") {
		t.Fatal(firstLines(out, 3))
	}
}

func TestMatrixAllShowsThreeStrategiesWithPrices(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	code, out, errs := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud,anthropic,openai", "--all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	hdr := firstLines(out, 3)
	for _, want := range []string{"CHEAPEST", "BALANCED", "BEST", "blend 90/10 input/output", "USD per 1M tokens"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, hdr)
		}
	}
	if !strings.Contains(out, "ollama-cloud/deepseek-v4.1-flash:1017 ($0.195)") || !strings.Contains(out, "openai/gpt-6.1-sol ($2.80)") {
		t.Fatalf("prices not shown:\n%s", firstLines(out, 12))
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "float") || strings.Contains(l, "e+") {
			t.Fatalf("suspicious number formatting: %q", l)
		}
	}
}

func TestMatrixExplain(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	code, out, errs := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--explain", "build|feature|M|false|balanced")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"target tier", "very_good", "used tier", "good", "selected #1", "ollama-cloud/glm-5.3", "not allowed", "PROVIDER", "PREF", "RANK", "PRICE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if code, _, errs := runMatrix(t, "--snapshot", snap, "--explain", "nonsense"); code != ExitUsage || !strings.Contains(errs, "cell") {
		t.Fatalf("bad cell: %d %s", code, errs)
	}
}

func TestMatrixCheck(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	code, out, _ := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--check")
	if code != 0 || !strings.Contains(out, "check: ok") {
		t.Fatalf("clean check: %d\n%s", code, out)
	}
	// Deny both seats that can review: balanced review/advise cells empty.
	code, out, errs := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--deny", "ollama-cloud/kimi-k3,ollama-cloud/glm-5.3", "--check")
	if code != ExitError {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "review|feature|M|false|balanced") || !strings.Contains(out, "hard failure") {
		t.Fatalf("empty cell not named:\n%s", firstLines(out, 8))
	}
	if !strings.Contains(errs, "no eligible seat") {
		t.Fatalf("deny cause not reported: %s", errs)
	}
}

func TestMatrixCheckNamesEveryEmptyCellButOnlyHardFailsOnReview(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir(), fixedNow)
	// Only the top-tier opus: no explore seat at all.
	code, out, _ := runMatrix(t, "--snapshot", snap, "--providers", "anthropic", "--deny", "anthropic/claude-sonnet-5-5,anthropic/claude-haiku-5-5", "--check")
	if code != ExitError {
		// haiku is the only explore seat and it was denied: Validate reports that.
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := strings.Count("\n"+out, "\nempty "); got != 126 {
		t.Fatalf("empty lines = %d, want 126", got)
	}
}

func TestMatrixWriteIsPinnedAndDoctorAgrees(t *testing.T) {
	dir := t.TempDir()
	snap := writeSnapshot(t, dir, fixedNow)
	reg := filepath.Join(dir, "out", "registry.json")
	code, out, errs := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--out", reg, "--write")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	data, err := os.ReadFile(reg)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(reg + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	want := parity.SHA256Hex(data)
	if string(sum) != want+"  registry.json\n" || !strings.Contains(out, want) {
		t.Fatalf("pin file %q, stdout %q", sum, out)
	}
	r, err := policy.DecodeRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Validate(r); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(reg); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}

	var dout, derr bytes.Buffer
	if code := Doctor([]string{"--snapshot", snap, "--registry", reg}, &dout, &derr, withInstall(t)); code != 0 {
		t.Fatalf("doctor %d\n%s%s", code, dout.String(), derr.String())
	}
	if !strings.Contains(dout.String(), "ok    registry") {
		t.Fatalf("registry not pinned:\n%s", dout.String())
	}
	// Tamper with the registry: doctor fails.
	if err := os.WriteFile(reg, append(data[:len(data)-1], []byte(" }")...), 0o600); err != nil {
		t.Fatal(err)
	}
	dout.Reset()
	if code := Doctor([]string{"--snapshot", snap, "--registry", reg}, &dout, &derr, withInstall(t)); code != ExitError {
		t.Fatalf("doctor %d on tampered registry\n%s", code, dout.String())
	}
	if !strings.Contains(dout.String(), "FAIL  registry") {
		t.Fatalf("tamper not reported:\n%s", dout.String())
	}
}

func TestMatrixWriteRefusesInvalid(t *testing.T) {
	dir := t.TempDir()
	snap := writeSnapshot(t, dir, fixedNow)
	reg := filepath.Join(dir, "registry.json")
	code, _, _ := runMatrix(t, "--snapshot", snap, "--providers", "ollama-cloud", "--deny", "ollama-cloud/kimi-k3,ollama-cloud/glm-5.3", "--out", reg, "--write")
	if code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatal("registry written despite hard failure")
	}
}

func TestMatrixUsageAndErrors(t *testing.T) {
	dir := t.TempDir()
	snap := writeSnapshot(t, dir, fixedNow)
	cases := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"bad strategy", []string{"--snapshot", snap, "--strategy", "fastest"}, ExitUsage, "strategy"},
		{"two modes", []string{"--snapshot", snap, "--all", "--check"}, ExitUsage, "one of"},
		{"unknown flag", []string{"--bogus"}, ExitUsage, ""},
		{"unknown provider", []string{"--snapshot", snap, "--providers", "nowhere"}, ExitUsage, "provider"},
		{"strategy with write", []string{"--snapshot", snap, "--strategy", "best", "--write"}, ExitUsage, "strategy"},
		{"no snapshot", []string{"--snapshot", filepath.Join(dir, "missing.json"), "--providers", "ollama-cloud"}, ExitError, "mgw pricing refresh"},
		{"no config and no providers", []string{"--snapshot", snap}, ExitError, "missing_config"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errs := runMatrix(t, c.args...)
			if code != c.code || !strings.Contains(errs, c.msg) {
				t.Fatalf("exit %d stderr %q", code, errs)
			}
		})
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestFormatUSD(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0.00", 1: "0.000001", 140000: "0.14", 195000: "0.195", 5600000: "5.60", 14000000: "14.00", 2800000: "2.80", 1234567: "1.234567",
	} {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%d) = %q, want %q", in, got, want)
		}
	}
}
