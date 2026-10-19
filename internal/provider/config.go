package provider

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Dialect names the wire protocol family a provider speaks.
type Dialect string

// Supported dialects.
const (
	DialectOpenAI     Dialect = "openai"
	DialectAnthropic  Dialect = "anthropic"
	DialectOpenRouter Dialect = "openrouter"
	DialectOllama     Dialect = "ollama"
)

func (d Dialect) valid() bool {
	switch d {
	case DialectOpenAI, DialectAnthropic, DialectOpenRouter, DialectOllama:
		return true
	}
	return false
}

// Provider is one runtime provider definition. It is immutable after load.
type Provider struct {
	ID             string   `json:"id"`
	Dialect        Dialect  `json:"dialect"`
	BaseURL        string   `json:"base_url"`        // scheme+host, no path
	CredentialFile string   `json:"credential_file"` // 0600, one line
	AllowedModels  []string `json:"allowed_models"`
	Timeouts       Timeouts `json:"timeouts"`
}

// Timeouts holds the four upstream deadlines, in milliseconds. Zero is a defect.
type Timeouts struct {
	ConnectMS int `json:"connect_ms"`
	TTFBMS    int `json:"ttfb_ms"`
	IdleMS    int `json:"idle_ms"`
	OverallMS int `json:"overall_ms"`
}

// Connect is the dial and TLS handshake deadline.
func (t Timeouts) Connect() time.Duration { return time.Duration(t.ConnectMS) * time.Millisecond }

// TTFB is the time-to-first-byte deadline, measured after the request is written.
func (t Timeouts) TTFB() time.Duration { return time.Duration(t.TTFBMS) * time.Millisecond }

// Idle is the longest allowed gap between two reads of a streaming response.
func (t Timeouts) Idle() time.Duration { return time.Duration(t.IdleMS) * time.Millisecond }

// Overall bounds one request, including any retry and the whole stream.
func (t Timeouts) Overall() time.Duration { return time.Duration(t.OverallMS) * time.Millisecond }

// idPattern is also the routing charset: the ID appears in /p/{provider}/....
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidID reports whether id is usable as a provider ID and route segment.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Validate checks one provider definition.
func (p Provider) Validate() error {
	if p.ID == "" {
		return errors.New("provider: empty id")
	}
	if !ValidID(p.ID) {
		return fmt.Errorf("provider %q: id must match %s", p.ID, idPattern)
	}
	if !p.Dialect.valid() {
		return fmt.Errorf("provider %q: unknown dialect %q", p.ID, p.Dialect)
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
	}
	if p.CredentialFile == "" {
		return fmt.Errorf("provider %q: empty credential_file", p.ID)
	}
	t := p.Timeouts
	if t.ConnectMS <= 0 || t.TTFBMS <= 0 || t.IdleMS <= 0 || t.OverallMS <= 0 {
		return fmt.Errorf("provider %q: every timeout must be greater than zero", p.ID)
	}
	return nil
}

// Validate checks every provider and rejects duplicate IDs.
func Validate(ps []Provider) error {
	seen := make(map[string]struct{}, len(ps))
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seen[p.ID]; dup {
			return fmt.Errorf("provider %q: duplicate id", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
	return nil
}

// validateBaseURL accepts https://host[:port], or the explicit test base
// http://127.0.0.1[:port]. Nothing else: no path, query, fragment or userinfo.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("base_url has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return errors.New("base_url must be scheme and host only")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if u.Hostname() == "127.0.0.1" {
			return nil
		}
		return errors.New("http base_url is allowed only for 127.0.0.1")
	}
	return errors.New("base_url must be https")
}

// maxCredentialBytes bounds the credential file read.
const maxCredentialBytes = 4096

// Credential reads the provider credential. The file must be mode 0600 and
// hold one line. The value is read per call so it never sits in the Provider.
// Error text never includes the credential.
func (p Provider) Credential() (string, error) {
	f, err := os.Open(p.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("provider %q: open credential file: %w", p.ID, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("provider %q: stat credential file: %w", p.ID, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("provider %q: credential file must not be accessible to group or others", p.ID)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil {
		return "", fmt.Errorf("provider %q: read credential file: %w", p.ID, err)
	}
	if len(data) > maxCredentialBytes {
		return "", fmt.Errorf("provider %q: credential file too large", p.ID)
	}
	cred := strings.TrimSuffix(string(data), "\n")
	if cred == "" || strings.ContainsAny(cred, "\r\n") {
		return "", fmt.Errorf("provider %q: credential file must hold exactly one non-empty line", p.ID)
	}
	return cred, nil
}
