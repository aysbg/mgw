package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Runtime is the supervisor contract: the JSON file `mgw serve --runtime`
// reads. Whoever starts the gateway writes it, together with the authority
// record it points at. Relative paths resolve against the runtime file's
// directory. Unknown fields are rejected.
type Runtime struct {
	// AuthorityFile is the 0600 run-scoped authority record (policy.Authority).
	AuthorityFile string `json:"authority_file"`
	// Providers are the runtime provider definitions.
	Providers []provider.Provider `json:"providers"`
	// LedgerDir holds ledger.json and ledger.lock. Gateways sharing a run share it.
	LedgerDir string `json:"ledger_dir"`
	// AuditFile is the append-only JSON Lines audit file.
	AuditFile string `json:"audit_file"`
	// ReadyFile is written atomically once the listener is bound.
	ReadyFile string `json:"ready_file"`
	// CleanupTimeoutMS bounds settlement, audit and shutdown.
	CleanupTimeoutMS int `json:"cleanup_timeout_ms"`
	// DefaultReserveTokens is reserved for a request that declares no output cap.
	DefaultReserveTokens int64 `json:"default_reserve_tokens"`
	// MaxReservationTokens bounds each reservation. Defaults to 2000 if omitted.
	MaxReservationTokens int64 `json:"max_reservation_tokens"`
}

// LoadRuntime reads and validates a runtime file.
func LoadRuntime(file string) (*Runtime, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- operator-supplied --runtime path
	if err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	rt := Runtime{MaxReservationTokens: defaultMaxReservationTokens}
	if err := dec.Decode(&rt); err != nil {
		return nil, fmt.Errorf("runtime: decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("runtime: trailing data")
	}
	for name, v := range map[string]string{
		"authority_file": rt.AuthorityFile, "ledger_dir": rt.LedgerDir,
		"audit_file": rt.AuditFile, "ready_file": rt.ReadyFile,
	} {
		if v == "" {
			return nil, fmt.Errorf("runtime: %s is required", name)
		}
	}
	if rt.CleanupTimeoutMS <= 0 {
		return nil, errors.New("runtime: cleanup_timeout_ms must be greater than zero")
	}
	if rt.MaxReservationTokens <= 0 {
		return nil, errors.New("runtime: max_reservation_tokens must be greater than zero")
	}
	if err := provider.Validate(rt.Providers); err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}
	base := filepath.Dir(file)
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	rt.AuthorityFile, rt.LedgerDir = resolve(rt.AuthorityFile), resolve(rt.LedgerDir)
	rt.AuditFile, rt.ReadyFile = resolve(rt.AuditFile), resolve(rt.ReadyFile)
	for i := range rt.Providers {
		rt.Providers[i].CredentialFile = resolve(rt.Providers[i].CredentialFile)
	}
	return &rt, nil
}

// Run loads the runtime file, binds the listener, writes the ready file, prints
// the serving line and serves until ctx ends. It then shuts down, settling
// in-flight requests within the cleanup timeout.
func Run(ctx context.Context, runtimeFile string, stderr io.Writer) error {
	rt, err := LoadRuntime(runtimeFile)
	if err != nil {
		return err
	}
	auth, err := policy.LoadAuthority(rt.AuthorityFile)
	if err != nil {
		return err
	}
	cleanup := time.Duration(rt.CleanupTimeoutMS) * time.Millisecond
	led, err := ledger.Open(rt.LedgerDir, auth.Budget.MaxTokens, cleanup)
	if err != nil {
		return err
	}
	aw, err := audit.Open(rt.AuditFile)
	if err != nil {
		return err
	}
	defer func() { _ = aw.Close() }()

	srv, err := New(Config{
		Providers: rt.Providers, Authority: auth, Ledger: led, Audit: aw,
		ReadyFile: rt.ReadyFile, CleanupTimeout: cleanup, DefaultReserveTokens: rt.DefaultReserveTokens,
		MaxReservationTokens: rt.MaxReservationTokens,
		Logger:               slog.New(slog.NewJSONHandler(stderr, nil)),
	})
	if err != nil {
		return err
	}
	if err := srv.Listen(ctx); err != nil {
		return err
	}
	line, err := json.Marshal(map[string]any{"serving": map[string]any{"url": srv.URL(), "pid": os.Getpid()}})
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "%s\n", line); err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutErr := srv.Shutdown(ctx)
	if err := <-serveErr; err != nil {
		return err
	}
	return shutErr
}
