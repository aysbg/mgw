package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Errors returned by the ledger. Codes are stable.
var (
	// ErrBudgetExhausted means settled plus reserved plus the request would exceed the budget.
	ErrBudgetExhausted = errors.New("budget_exhausted")
	// ErrTimeout means the ledger could not finish inside the cleanup timeout.
	// The caller records settle_timeout and usage unknown. The reservation, if
	// any, stays held: a charge is never dropped.
	ErrTimeout = errors.New("settle_timeout")
	// ErrUnknownReservation means Settle found no reservation with that id.
	ErrUnknownReservation = errors.New("unknown reservation")
	// ErrDuplicateReservation means Reserve found the id already held.
	ErrDuplicateReservation = errors.New("duplicate reservation")
	// ErrInvalid means the arguments were out of range.
	ErrInvalid = errors.New("invalid ledger argument")
)

const (
	stateFile = "ledger.json"
	lockFile  = "ledger.lock"
)

// Totals is one run's accounting.
type Totals struct {
	// Settled is measured usage.
	Settled int64 `json:"settled"`
	// Uncertain is usage charged without a measurement; counted against the budget.
	Uncertain int64 `json:"uncertain"`
	// Reserved is the sum of open reservations.
	Reserved int64 `json:"reserved"`
}

// Spent is everything counted against the budget.
func (t Totals) Spent() int64 { return t.Settled + t.Uncertain + t.Reserved }

type runState struct {
	Settled   int64            `json:"settled"`
	Uncertain int64            `json:"uncertain"`
	Reserved  map[string]int64 `json:"reserved"`
}

type state struct {
	Version int                  `json:"version"`
	Runs    map[string]*runState `json:"runs"`
}

// Ledger is a handle on one ledger directory. It holds no open files and no
// in-process lock: every operation takes the file lock itself.
type Ledger struct {
	dir     string
	budget  int64
	timeout time.Duration
}

// Open prepares the ledger directory. budget is the per-run token budget;
// timeout is the cleanup timeout that bounds every operation.
func Open(dir string, budget int64, timeout time.Duration) (*Ledger, error) {
	if budget <= 0 || timeout <= 0 {
		return nil, fmt.Errorf("%w: budget and timeout must be positive", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- operator-supplied dir
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	return &Ledger{dir: dir, budget: budget, timeout: timeout}, nil
}

// Reserve holds maxTokens against the run's budget under id. It fails with
// ErrBudgetExhausted when settled plus reserved plus maxTokens exceeds it.
func (l *Ledger) Reserve(id, runID string, maxTokens int64) error {
	if id == "" || runID == "" || maxTokens <= 0 {
		return fmt.Errorf("%w: reserve", ErrInvalid)
	}
	return l.update(func(s *state) error {
		r := s.run(runID)
		if _, dup := r.Reserved[id]; dup {
			return ErrDuplicateReservation
		}
		if r.totals().Spent()+maxTokens > l.budget {
			return ErrBudgetExhausted
		}
		r.Reserved[id] = maxTokens
		return nil
	})
}

// Settle releases the reservation id and adds usedTokens. With measured false
// the charge is recorded as uncertain: it still counts against the budget.
func (l *Ledger) Settle(id, runID string, usedTokens int64, measured bool) error {
	if id == "" || runID == "" || usedTokens < 0 {
		return fmt.Errorf("%w: settle", ErrInvalid)
	}
	return l.update(func(s *state) error {
		r := s.run(runID)
		if _, ok := r.Reserved[id]; !ok {
			return ErrUnknownReservation
		}
		delete(r.Reserved, id)
		if measured {
			r.Settled += usedTokens
		} else {
			r.Uncertain += usedTokens
		}
		return nil
	})
}

// Totals reads one run's accounting.
func (l *Ledger) Totals(runID string) (Totals, error) {
	var out Totals
	err := l.update(func(s *state) error {
		out = s.run(runID).totals()
		return errReadOnly
	})
	return out, err
}

func (s *state) run(id string) *runState {
	r, ok := s.Runs[id]
	if !ok {
		r = &runState{}
		s.Runs[id] = r
	}
	if r.Reserved == nil {
		r.Reserved = map[string]int64{}
	}
	return r
}

func (r *runState) totals() Totals {
	t := Totals{Settled: r.Settled, Uncertain: r.Uncertain}
	for _, n := range r.Reserved {
		t.Reserved += n
	}
	return t
}

// errReadOnly tells update to skip the write. It never escapes update.
var errReadOnly = errors.New("read only")

// update runs fn under the file lock and persists the result atomically. The
// whole operation is bounded by the cleanup timeout and is detached from any
// request context.
func (l *Ledger) update(fn func(*state) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), l.timeout)
	defer cancel()

	lock, err := os.OpenFile(filepath.Join(l.dir, lockFile), os.O_RDWR, 0o600) // #nosec G304 -- fixed name under operator dir
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	defer func() { _ = lock.Close() }() // closing the descriptor releases the flock

	if err := lockWithContext(ctx, lock); err != nil {
		return err
	}

	s, err := l.load()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		if errors.Is(err, errReadOnly) {
			return nil
		}
		return err
	}
	if ctx.Err() != nil {
		return ErrTimeout
	}
	return l.store(s)
}

// lockWithContext takes an exclusive flock, polling so the wait can time out.
func lockWithContext(ctx context.Context, f *os.File) error {
	fd := int(f.Fd()) // #nosec G115 -- file descriptors fit in int
	delay := time.Millisecond
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("ledger: flock: %w", err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ErrTimeout
		case <-t.C:
		}
		if delay < 20*time.Millisecond {
			delay *= 2
		}
	}
}

func (l *Ledger) load() (*state, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, stateFile)) // #nosec G304 -- fixed name under operator dir
	if errors.Is(err, os.ErrNotExist) {
		return &state{Version: 1, Runs: map[string]*runState{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("ledger: corrupt state: %w", err)
	}
	if s.Runs == nil {
		s.Runs = map[string]*runState{}
	}
	return &s, nil
}

// store writes a temp file in the same directory, fsyncs it, renames it over
// the state file and fsyncs the directory.
func (l *Ledger) store(s *state) (err error) {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	tmp, err := os.CreateTemp(l.dir, "ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ledger: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ledger: fsync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if err = os.Rename(tmp.Name(), filepath.Join(l.dir, stateFile)); err != nil {
		return fmt.Errorf("ledger: rename: %w", err)
	}
	dir, err := os.Open(l.dir) // #nosec G304 -- operator dir
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("ledger: fsync dir: %w", err)
	}
	return nil
}
