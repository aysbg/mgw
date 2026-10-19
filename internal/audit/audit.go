package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sync"
)

// SchemaVersion is the audit schema version carried by every record.
const SchemaVersion = 1

// Commit states.
const (
	CommitNone      = "uncommitted"
	CommitCommitted = "committed"
	CommitAmbiguous = "ambiguous"
)

// Usage statuses.
const (
	UsageMeasured = "measured"
	UsageUnknown  = "unknown"
)

// Failure codes. FailureCode is one of these or empty; free text is rejected.
const (
	FailMalformed         = "malformed"
	FailUnauthorized      = "unauthorized"
	FailModelNotAllowed   = "model_not_allowed"
	FailProviderNotAllow  = "provider_not_allowed"
	FailBudgetExhausted   = "budget_exhausted"
	FailUpstreamUnavail   = "upstream_unavailable"
	FailCommitLost        = "commit_lost"
	FailNotFound          = "not_found"
	FailConcurrency       = "concurrency_exceeded"
	FailInternalPanic     = "internal_panic"
	FailPanicAfterCommit  = "internal_panic_after_commit"
	FailSettleTimeout     = "settle_timeout"
	FailClientDisconnect  = "client_disconnect"
	FailStreamInterrupted = "stream_interrupted"
	FailUpstreamTimeout   = "upstream_timeout"
)

var failureCodes = []string{
	"", FailMalformed, FailUnauthorized, FailModelNotAllowed, FailProviderNotAllow,
	FailBudgetExhausted, FailUpstreamUnavail, FailCommitLost, FailNotFound,
	FailConcurrency, FailInternalPanic, FailPanicAfterCommit, FailSettleTimeout,
	FailClientDisconnect, FailStreamInterrupted, FailUpstreamTimeout,
}

// AllowedFields is the allowlist of audit field names. Adding a field is a
// code change: edit Record and this list together; a test keeps them equal.
var AllowedFields = []string{
	"schema", "ts", "request_id", "run_id", "attempt", "provider", "model",
	"method", "path", "upstream_attempts", "commit_state", "status_code",
	"duration_ms", "usage_status", "input_tokens", "output_tokens",
	"cache_read_tokens", "failure_code", "policy_sha256", "registry_sha256",
	"stream_usage_injected",
}

// Record is one audit line. Token counts are null when usage is unknown.
type Record struct {
	Schema           int    `json:"schema"`
	TS               string `json:"ts"`
	RequestID        string `json:"request_id"`
	RunID            string `json:"run_id"`
	Attempt          int    `json:"attempt"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	UpstreamAttempts int    `json:"upstream_attempts"`
	CommitState      string `json:"commit_state"`
	StatusCode       int    `json:"status_code"`
	DurationMS       int64  `json:"duration_ms"`
	UsageStatus      string `json:"usage_status"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	FailureCode      string `json:"failure_code"`
	PolicySHA256     string `json:"policy_sha256"`
	RegistrySHA256   string `json:"registry_sha256"`
	// StreamUsageInjected is true when the gateway added stream_options to the
	// request body (the sanctioned payload exception, MGW-001).
	StreamUsageInjected bool `json:"stream_usage_injected"`
}

var (
	modelPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
	providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	pathPattern     = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@/-]{0,255}$`)
	methods         = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
)

// CleanModel returns model when it looks like a model id, otherwise "". The
// model is caller-supplied text, so only a conservative charset is recorded.
func CleanModel(model string) string {
	if modelPattern.MatchString(model) {
		return model
	}
	return ""
}

// CleanMethod returns method when it is a standard HTTP method, otherwise "OTHER".
func CleanMethod(method string) string {
	if slices.Contains(methods, method) {
		return method
	}
	return "OTHER"
}

// CleanPath returns path when it is a short clean path, otherwise "".
func CleanPath(path string) string {
	if pathPattern.MatchString(path) {
		return path
	}
	return ""
}

// ErrInvalidRecord is returned when a record breaks the schema.
var ErrInvalidRecord = errors.New("invalid audit record")

// Validate enforces the schema. Every free-text field is constrained.
func (r Record) Validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrInvalidRecord, what) }
	switch {
	case r.Schema != SchemaVersion:
		return bad("schema")
	case r.TS == "" || r.RequestID == "" || r.RunID == "":
		return bad("ts, request_id and run_id are required")
	case r.Provider != "" && !providerPattern.MatchString(r.Provider):
		return bad("provider")
	case r.Model != "" && !modelPattern.MatchString(r.Model):
		return bad("model")
	case !slices.Contains(append([]string{"OTHER"}, methods...), r.Method):
		return bad("method")
	case r.Path != "" && !pathPattern.MatchString(r.Path):
		return bad("path")
	case r.UpstreamAttempts < 0 || r.DurationMS < 0:
		return bad("negative count")
	case !slices.Contains([]string{CommitNone, CommitCommitted, CommitAmbiguous}, r.CommitState):
		return bad("commit_state")
	case !slices.Contains([]string{UsageMeasured, UsageUnknown}, r.UsageStatus):
		return bad("usage_status")
	case r.UsageStatus == UsageUnknown && (r.InputTokens != nil || r.OutputTokens != nil || r.CacheReadTokens != nil):
		return bad("token counts with unknown usage")
	case !slices.Contains(failureCodes, r.FailureCode):
		return bad("failure_code")
	}
	return nil
}

type request struct {
	line []byte
	done chan error
}

// Writer appends records to one file. A single goroutine owns the file;
// callers hand it records over a channel and wait for the write.
type Writer struct {
	reqs chan request
	quit chan struct{}
	done chan struct{}
	once sync.Once
}

// Open opens or creates the audit file, mode 0600, append-only.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- operator-supplied path from the runtime file
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	w := &Writer{reqs: make(chan request), quit: make(chan struct{}), done: make(chan struct{})}
	go w.loop(f)
	return w, nil
}

// loop is the file's owner. It exits when quit closes.
func (w *Writer) loop(f *os.File) {
	defer close(w.done)
	defer func() { _ = f.Close() }()
	for {
		select {
		case req := <-w.reqs:
			_, err := f.Write(req.line) // one write per record: O_APPEND keeps lines whole
			if err == nil {
				err = f.Sync()
			}
			req.done <- err
		case <-w.quit:
			return
		}
	}
}

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("audit: writer closed")

// Write validates and appends one record. It returns when the line is durable
// or ctx ends.
func (w *Writer) Write(ctx context.Context, r Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	line = append(line, '\n')
	req := request{line: line, done: make(chan error, 1)}
	select {
	case w.reqs <- req:
	case <-w.quit:
		return ErrClosed
	case <-ctx.Done():
		return fmt.Errorf("audit: %w", ctx.Err())
	}
	select {
	case err := <-req.done:
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("audit: %w", ctx.Err())
	}
}

// Close stops the writer goroutine after any in-progress write and closes the file.
func (w *Writer) Close() error {
	w.once.Do(func() { close(w.quit) })
	<-w.done
	return nil
}
