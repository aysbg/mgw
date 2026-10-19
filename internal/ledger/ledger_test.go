package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T, budget int64) *Ledger {
	t.Helper()
	l, err := Open(t.TempDir(), budget, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReserveSettle(t *testing.T) {
	l := open(t, 100)
	if err := l.Reserve("a", "run", 60); err != nil {
		t.Fatal(err)
	}
	if err := l.Reserve("b", "run", 41); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want budget_exhausted, got %v", err)
	}
	if err := l.Reserve("a", "run", 1); !errors.Is(err, ErrDuplicateReservation) {
		t.Fatalf("want duplicate, got %v", err)
	}
	// Another run has its own budget.
	if err := l.Reserve("c", "other", 100); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle("a", "run", 25, true); err != nil {
		t.Fatal(err)
	}
	tot, err := l.Totals("run")
	if err != nil || tot != (Totals{Settled: 25}) {
		t.Fatalf("got %+v, %v", tot, err)
	}
	// 75 left: 75 fits, 76 does not.
	if err := l.Reserve("d", "run", 76); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v", err)
	}
	if err := l.Reserve("d", "run", 75); err != nil {
		t.Fatal(err)
	}
}

func TestSettleUnmeasuredIsKept(t *testing.T) {
	l := open(t, 100)
	if err := l.Reserve("a", "run", 40); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle("a", "run", 40, false); err != nil {
		t.Fatal(err)
	}
	tot, _ := l.Totals("run")
	if tot != (Totals{Uncertain: 40}) {
		t.Fatalf("uncertain charge dropped: %+v", tot)
	}
	if err := l.Reserve("b", "run", 61); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("uncertain charge must count against the budget, got %v", err)
	}
	if err := l.Settle("zzz", "run", 1, true); !errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("got %v", err)
	}
}

func TestRemaining(t *testing.T) {
	l := open(t, 100)
	check := func(runID string, want int64) {
		t.Helper()
		got, err := l.Remaining(runID)
		if err != nil || got != want {
			t.Fatalf("remaining(%q) = %d, %v; want %d", runID, got, err, want)
		}
	}
	check("run", 100)
	if err := l.Reserve("a", "run", 60); err != nil {
		t.Fatal(err)
	}
	check("run", 40)
	check("other", 100)
	if err := l.Settle("a", "run", 25, true); err != nil {
		t.Fatal(err)
	}
	check("run", 75)
	if err := l.Reserve("b", "run", 30); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle("b", "run", 30, false); err != nil {
		t.Fatal(err)
	}
	check("run", 45)
	if err := l.Reserve("c", "run", 45); err != nil {
		t.Fatal(err)
	}
	check("run", 0)
	if err := l.Settle("c", "run", 50, true); err != nil {
		t.Fatal(err)
	}
	check("run", -5)
	if tot, err := l.Totals("run"); err != nil || tot != (Totals{Settled: 75, Uncertain: 30}) {
		t.Fatalf("over-budget truth: %+v, %v", tot, err)
	}
	if _, err := l.Remaining(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run: %v", err)
	}
}

func TestInvalidArguments(t *testing.T) {
	l := open(t, 10)
	for _, err := range []error{
		l.Reserve("", "r", 1), l.Reserve("a", "", 1), l.Reserve("a", "r", 0),
		l.Settle("", "r", 1, true), l.Settle("a", "r", -1, true),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := Open(t.TempDir(), 0, time.Second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestStateFileMode(t *testing.T) {
	l := open(t, 10)
	if err := l.Reserve("a", "r", 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stateFile, lockFile} {
		info, err := os.Stat(filepath.Join(l.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode %v", name, info.Mode().Perm())
		}
	}
	matches, _ := filepath.Glob(filepath.Join(l.dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestConcurrentReserveInProcess(t *testing.T) {
	l := open(t, 100)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Reserve(fmt.Sprint("id", i), "run", 10) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 10 {
		t.Fatalf("granted %d reservations of 10 tokens against a 100 budget", ok)
	}
}

// TestTimeoutWhenLockHeld proves the ledger returns ErrTimeout, not a hang,
// and mutates nothing when another holder keeps the lock.
func TestTimeoutWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Reserve("a", "run", 10); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := lockWithContext(context.Background(), holder); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle("a", "run", 10, true); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want settle_timeout, got %v", err)
	}
	if _, err := l.Remaining("run"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("remaining while locked: want settle_timeout, got %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	tot, err := l.Totals("run")
	if err != nil || tot != (Totals{Reserved: 10}) {
		t.Fatalf("reservation must survive a settle timeout: %+v, %v", tot, err)
	}
}

const (
	helperEnv      = "MGW_LEDGER_HELPER_DIR"
	helperAttempts = 60
	helperTokens   = 10
	helperBudget   = 1000
	helperProcs    = 4
	helperRun      = "run-x"
)

// TestLedgerHelperProcess is not a test: it is the body of the subprocess that
// TestTwoProcessesNoOverspend starts. It is skipped unless the env var is set.
func TestLedgerHelperProcess(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	pid := os.Getpid()
	l, err := Open(dir, helperBudget, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	granted := 0
	for i := 0; i < helperAttempts; i++ {
		err := l.Reserve(fmt.Sprintf("p%d-%d", pid, i), helperRun, helperTokens)
		switch {
		case err == nil:
			granted++
		case errors.Is(err, ErrBudgetExhausted):
		default:
			t.Fatal(err)
		}
	}
	fmt.Printf("GRANTED %d\n", granted)
}

// TestTwoProcessesNoOverspend runs separate OS processes against one ledger.
// 4 processes x 60 attempts x 10 tokens ask for 2400 tokens against a 1000
// budget: exactly 100 reservations may be granted and the total may not exceed
// the budget. An in-process mutex could not pass this.
func TestTwoProcessesNoOverspend(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		out []byte
		err error
	}
	results := make(chan result, helperProcs)
	for i := 0; i < helperProcs; i++ {
		go func() {
			cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestLedgerHelperProcess$", "-test.v") // #nosec G204 -- re-exec of this test binary
			cmd.Env = append(os.Environ(), helperEnv+"="+dir)
			out, err := cmd.CombinedOutput()
			results <- result{out, err}
		}()
	}
	total := 0
	for i := 0; i < helperProcs; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("helper failed: %v\n%s", r.err, r.out)
		}
		for _, line := range strings.Split(string(r.out), "\n") {
			if rest, ok := strings.CutPrefix(line, "GRANTED "); ok {
				n, err := strconv.Atoi(strings.TrimSpace(rest))
				if err != nil {
					t.Fatal(err)
				}
				total += n
			}
		}
	}
	if want := helperBudget / helperTokens; total != want {
		t.Fatalf("processes were granted %d reservations, want exactly %d", total, want)
	}
	l, err := Open(dir, helperBudget, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tot, err := l.Totals(helperRun)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Spent() != helperBudget {
		t.Fatalf("ledger total %d, want %d", tot.Spent(), helperBudget)
	}
	t.Logf("two-process ledger: %d processes, %d reservations granted, spent %d of %d", helperProcs, total, tot.Spent(), helperBudget)
}
