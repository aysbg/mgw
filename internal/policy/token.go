package policy

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// Error is a policy rejection with a stable machine code.
type Error struct{ code string }

// Error returns the stable machine code.
func (e *Error) Error() string { return e.code }

// Code returns the stable machine code.
func (e *Error) Code() string { return e.code }

// Policy rejections. Branch with errors.Is; map to a wire code with Code.
var (
	ErrMalformed          = &Error{"malformed"}
	ErrUnauthorized       = &Error{"unauthorized"}
	ErrModelNotAllowed    = &Error{"model_not_allowed"}
	ErrProviderNotAllowed = &Error{"provider_not_allowed"}
)

// ErrBodyTooLarge is a malformed request whose body exceeds MaxBodyBytes.
var ErrBodyTooLarge = fmt.Errorf("request body exceeds %d bytes: %w", MaxBodyBytes, ErrMalformed)

// Code returns the machine code of a policy rejection, or "" for other errors.
func Code(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.code
	}
	return ""
}

// MaxBodyBytes caps the request body the gateway reads. A larger body is
// rejected, never buffered.
const MaxBodyBytes = 64 << 10

const (
	minTokenBytes = 32
	maxModelLen   = 256
	maxAuthority  = 1 << 20
)

// Budget is the run's token budget.
type Budget struct {
	MaxTokens int64 `json:"max_tokens"`
}

// Authority is the run-scoped authority record, written 0600 by whoever starts
// the gateway. It is immutable after load. The Token field is a secret: do not
// log or marshal the record.
type Authority struct {
	Token          string              `json:"token"`
	RunID          string              `json:"run_id"`
	Attempt        int                 `json:"attempt"`
	ExpiresAt      time.Time           `json:"expires_at"`
	Providers      map[string][]string `json:"providers"`
	Budget         Budget              `json:"budget"`
	Concurrency    int                 `json:"concurrency"`
	PolicySHA256   string              `json:"policy_sha256"`
	RegistrySHA256 string              `json:"registry_sha256"`
}

// LoadAuthority reads and validates an authority record. The file must be
// mode 0600.
func LoadAuthority(path string) (*Authority, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path from the runtime file
	if err != nil {
		return nil, fmt.Errorf("authority: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("authority: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("authority: file must not be accessible to group or others")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAuthority+1))
	if err != nil {
		return nil, fmt.Errorf("authority: %w", err)
	}
	if len(data) > maxAuthority {
		return nil, errors.New("authority: file too large")
	}
	return ParseAuthority(data)
}

// ParseAuthority decodes and validates an authority record.
func ParseAuthority(data []byte) (*Authority, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var a Authority
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("authority: decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("authority: trailing data")
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *Authority) validate() error {
	raw, err := base64.RawURLEncoding.DecodeString(a.Token)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(a.Token)
	}
	if err != nil || len(raw) < minTokenBytes {
		return fmt.Errorf("authority: token must be base64url and at least %d bytes", minTokenBytes)
	}
	switch {
	case a.RunID == "":
		return errors.New("authority: empty run_id")
	case a.ExpiresAt.IsZero():
		return errors.New("authority: missing expires_at")
	case len(a.Providers) == 0:
		return errors.New("authority: no providers")
	case a.Budget.MaxTokens <= 0:
		return errors.New("authority: budget.max_tokens must be positive")
	case a.Concurrency <= 0:
		return errors.New("authority: concurrency must be positive")
	case a.PolicySHA256 == "" || a.RegistrySHA256 == "":
		return errors.New("authority: policy_sha256 and registry_sha256 are required")
	}
	return nil
}

// Authenticate checks the presented bearer token and the expiry. The compare
// is constant time over fixed-length digests, so length does not leak.
func (a *Authority) Authenticate(presented string, now time.Time) error {
	want := sha256.Sum256([]byte(a.Token))
	got := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return ErrUnauthorized
	}
	if !now.Before(a.ExpiresAt) {
		return fmt.Errorf("token expired: %w", ErrUnauthorized)
	}
	return nil
}

// CheckProvider denies a provider absent from the record.
func (a *Authority) CheckProvider(provider string) error {
	if _, ok := a.Providers[provider]; !ok {
		return ErrProviderNotAllowed
	}
	return nil
}

// CheckModel denies a model absent from the provider's list in the record. It
// reads only the record, never anything the caller supplied.
func (a *Authority) CheckModel(provider, model string) error {
	if err := a.CheckProvider(provider); err != nil {
		return err
	}
	if !slices.Contains(a.Providers[provider], model) {
		return ErrModelNotAllowed
	}
	return nil
}

// RequestMeta is what the gateway reads from a request body.
type RequestMeta struct {
	Model string
	// MaxOutputTokens is the caller-declared output cap, or 0 when absent.
	MaxOutputTokens int64
}

// ReadBody reads a request body of at most MaxBodyBytes. contentLength is the
// declared length, or -1 when unknown. An oversize body is rejected without
// being buffered beyond the cap.
func ReadBody(r io.Reader, contentLength int64) ([]byte, error) {
	if contentLength > MaxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", ErrMalformed)
	}
	if len(data) > MaxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

// ParseRequest reads the model and the declared output cap from a JSON body.
func ParseRequest(body []byte) (RequestMeta, error) {
	var in struct {
		Model               *string `json:"model"`
		MaxTokens           *int64  `json:"max_tokens"`
		MaxCompletionTokens *int64  `json:"max_completion_tokens"`
		MaxOutputTokens     *int64  `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return RequestMeta{}, fmt.Errorf("body is not a JSON object: %w", ErrMalformed)
	}
	if in.Model == nil || *in.Model == "" || len(*in.Model) > maxModelLen {
		return RequestMeta{}, fmt.Errorf("missing or invalid model: %w", ErrMalformed)
	}
	meta := RequestMeta{Model: *in.Model}
	for _, p := range []*int64{in.MaxTokens, in.MaxCompletionTokens, in.MaxOutputTokens} {
		if p != nil && *p > meta.MaxOutputTokens {
			meta.MaxOutputTokens = *p
		}
	}
	return meta, nil
}
