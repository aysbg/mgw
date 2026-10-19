// Package config owns the on-disk install layout of mgw: one directory holding
// config.json, one credential file per provider, the pinned registry, the
// pricing snapshot and the engagement profiles. A credential value lives only
// in its own 0600 file; config.json carries the provider id and nothing else.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Schema is the config.json schema tag.
const Schema = "mgw.config/1"

// Layout under the config directory.
const (
	// DirEnv overrides the config directory.
	DirEnv = "MGW_CONFIG_DIR"

	ConfigFile    = "config.json"
	RegistryFile  = "registry.json"
	PricingFile   = "pricing.json"
	CredentialDir = "credentials"
	ProfileDir    = "profiles"

	// DefaultProfile is the profile name an install carries until told otherwise.
	DefaultProfile = "default"

	// ModeSecret is the mode of config.json, credentials and profiles.
	ModeSecret os.FileMode = 0o600
	// ModePublic is the mode of the registry and the pricing snapshot.
	ModePublic os.FileMode = 0o644
	// ModeDir is the mode of every directory mgw creates.
	ModeDir os.FileMode = 0o700
)

// Allowed values.
var (
	DataClasses  = []string{"internal", "customer"}
	BillingModes = []string{"pay_per_token", "included_credits"}
)

// Stable failure codes. Every named failure prints its code.
const (
	CodeMissingConfig     = "missing_config"
	CodeMissingProfile    = "missing_profile"
	CodeMissingValue      = "missing_value"
	CodeBadValue          = "bad_value"
	CodeUnknownStrategy   = "unknown_strategy"
	CodeUnknownProvider   = "unknown_provider"
	CodeUnknownRole       = "unknown_role"
	CodeUnknownModel      = "unknown_model"
	CodeConflictingPolicy = "conflicting_policy"
	CodePinNotAdmitted    = "pin_not_admitted"
	CodeKeyInArgv         = "key_in_argv"
	CodeKeyFile           = "key_file"
	CodeCredentialFile    = "credential_file" // #nosec G101 -- an error code name, not a credential
	CodeProviderFailed    = "provider_validation_failed"
	CodeToolGradeFail     = "tool_grade_fail"
	CodeInputClosed       = "input_closed"
	CodeNoTerminal        = "no_hidden_input"
	CodeAborted           = "aborted"
)

// Failure is a named failure: a stable code and a message that never carries a
// credential value.
type Failure struct {
	Code string
	Msg  string
}

// Error implements error.
func (f *Failure) Error() string { return f.Code + ": " + f.Msg }

// Fail builds a named failure.
func Fail(code, format string, args ...any) error {
	return &Failure{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ProbeRecord is the last probe result of a provider. It holds outcomes only.
type ProbeRecord struct {
	Auth        string `json:"auth"`
	Inference   string `json:"inference"`
	StreamUsage string `json:"stream_usage"`
	ToolCall    string `json:"tool_call"`
}

// Provider is one connected provider. There is no credential field: the key is
// found by deriving credentials/<id>.key.
type Provider struct {
	ID                 string           `json:"id"`
	Dialect            provider.Dialect `json:"dialect"`
	BaseURL            string           `json:"base_url"`
	Billing            string           `json:"billing"`
	IncludeStreamUsage bool             `json:"include_stream_usage"`
	ToolGrade          string           `json:"tool_grade,omitempty"`
	Probe              *ProbeRecord     `json:"probe,omitempty"`
}

// ProviderPolicy is the install provider policy: the spend order and the deny
// list. A deny entry is a provider id or a full model id.
type ProviderPolicy struct {
	Order []string `json:"order"`
	Deny  []string `json:"deny"`
}

// Config is config.json.
type Config struct {
	Schema         string            `json:"schema"`
	Providers      []Provider        `json:"providers"`
	Strategy       string            `json:"strategy"`
	ProviderPolicy ProviderPolicy    `json:"provider_policy"`
	DataClass      string            `json:"data_class"`
	Profile        string            `json:"profile"`
	Pins           map[string]string `json:"pins,omitempty"`
}

// Endpoint returns the dialect and default base URL of a curated provider.
func Endpoint(id string) (provider.Dialect, string, bool) {
	switch id {
	case "openai":
		return provider.DialectOpenAI, "https://api.openai.com", true
	case "anthropic":
		return provider.DialectAnthropic, "https://api.anthropic.com", true
	case "ollama-cloud":
		return provider.DialectOllama, "https://ollama.com", true
	}
	return "", "", false
}

// DefaultBilling is the billing shape offered for a provider when none is given.
func DefaultBilling(id string) string {
	if id == "ollama-cloud" {
		return "included_credits"
	}
	return "pay_per_token"
}

// Paths of one config directory.

// ConfigPath is the path of config.json.
func ConfigPath(dir string) string { return filepath.Join(dir, ConfigFile) }

// RegistryPath is the path of the pinned registry.
func RegistryPath(dir string) string { return filepath.Join(dir, RegistryFile) }

// PricingPath is the path of the pricing snapshot.
func PricingPath(dir string) string { return filepath.Join(dir, PricingFile) }

// CredentialPath is the path of one provider's key file. The id must be valid.
func CredentialPath(dir, id string) string {
	return filepath.Join(dir, CredentialDir, id+".key")
}

// New returns an empty install config with the defaults that need no question.
func New() *Config {
	return &Config{
		Schema: Schema, Providers: []Provider{}, DataClass: "internal", Profile: DefaultProfile,
		ProviderPolicy: ProviderPolicy{Order: []string{}, Deny: []string{}},
	}
}

// Find returns the provider with the given id.
func (c *Config) Find(id string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// ProviderIDs lists the connected provider ids in config order.
func (c *Config) ProviderIDs() []string {
	ids := make([]string, len(c.Providers))
	for i, p := range c.Providers {
		ids[i] = p.ID
	}
	return ids
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	out := *c
	out.Providers = make([]Provider, len(c.Providers))
	for i, p := range c.Providers {
		if p.Probe != nil {
			rec := *p.Probe
			p.Probe = &rec
		}
		out.Providers[i] = p
	}
	out.ProviderPolicy = ProviderPolicy{Order: slices.Clone(c.ProviderPolicy.Order), Deny: slices.Clone(c.ProviderPolicy.Deny)}
	if c.Pins != nil {
		out.Pins = make(map[string]string, len(c.Pins))
		for k, v := range c.Pins {
			out.Pins[k] = v
		}
	}
	return &out
}

// CheckStrategy returns a named failure for a strategy that is not one of the three.
func CheckStrategy(s string) error {
	if !slices.Contains(policy.Strategies, s) {
		return Fail(CodeUnknownStrategy, "unknown strategy %q (want cheapest, balanced or best)", s)
	}
	return nil
}

// CheckDataClass returns a named failure for an unknown data class.
func CheckDataClass(s string) error {
	if !slices.Contains(DataClasses, s) {
		return Fail(CodeBadValue, "unknown data class %q (want internal or customer)", s)
	}
	return nil
}

// CheckBilling returns a named failure for an unknown billing shape.
func CheckBilling(s string) error {
	if !slices.Contains(BillingModes, s) {
		return Fail(CodeBadValue, "unknown billing %q (want pay_per_token or included_credits)", s)
	}
	return nil
}

// CheckPolicyConflict rejects a provider id that is both preferred and denied.
func CheckPolicyConflict(order, deny []string) error {
	for _, id := range order {
		if slices.Contains(deny, id) {
			return Fail(CodeConflictingPolicy, "%q is both preferred and denied", id)
		}
	}
	return nil
}

// Validate checks the config without the capability file: shapes, vocabulary,
// endpoints and the order/deny conflict.
func (c *Config) Validate() error {
	if c.Schema != Schema {
		return Fail(CodeBadValue, "config schema %q, want %q", c.Schema, Schema)
	}
	if err := CheckStrategy(c.Strategy); err != nil {
		return err
	}
	if err := CheckDataClass(c.DataClass); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if seen[p.ID] {
			return Fail(CodeBadValue, "provider %q is listed twice", p.ID)
		}
		seen[p.ID] = true
		if err := provider.ValidateEndpoint(p.ID, p.Dialect, p.BaseURL); err != nil {
			return Fail(CodeBadValue, "%v", err)
		}
		if err := CheckBilling(p.Billing); err != nil {
			return Fail(CodeBadValue, "provider %q: %v", p.ID, err)
		}
		if p.ToolGrade != "" && p.ToolGrade != provider.GradePass && p.ToolGrade != provider.GradeFail {
			return Fail(CodeBadValue, "provider %q: tool_grade %q (want pass or fail)", p.ID, p.ToolGrade)
		}
	}
	for _, id := range c.ProviderPolicy.Order {
		if !seen[id] {
			return Fail(CodeUnknownProvider, "provider_policy.order names %q, which is not connected", id)
		}
	}
	if err := CheckPolicyConflict(c.ProviderPolicy.Order, c.ProviderPolicy.Deny); err != nil {
		return err
	}
	if c.Profile != "" && !provider.ValidID(c.Profile) {
		return Fail(CodeBadValue, "profile name %q is not a valid name", c.Profile)
	}
	return nil
}

// Load reads and validates config.json. A missing config is a named failure.
func Load(dir string) (*Config, error) {
	c, err := LoadUnchecked(dir)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(dir), err)
	}
	return c, nil
}

// LoadUnchecked decodes config.json strictly but does not validate its values.
// `mgw doctor` uses it so it can name a bad value instead of stopping at it.
func LoadUnchecked(dir string) (*Config, error) {
	path := ConfigPath(dir)
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, Fail(CodeMissingConfig, "no config at %s; run `mgw init`", path)
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, Fail(CodeBadValue, "%s: %v", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, Fail(CodeBadValue, "%s: trailing data after document", path)
	}
	return &c, nil
}

// Marshal returns the bytes of config.json for c.
func (c *Config) Marshal() ([]byte, error) {
	out := c.Clone()
	if out.Providers == nil {
		out.Providers = []Provider{}
	}
	if out.ProviderPolicy.Order == nil {
		out.ProviderPolicy.Order = []string{}
	}
	if out.ProviderPolicy.Deny == nil {
		out.ProviderPolicy.Deny = []string{}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Save validates c and writes config.json with mode 0600.
func Save(dir string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := c.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(ConfigPath(dir), data, ModeSecret)
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory and a rename. The mode is set explicitly, so the umask cannot loosen
// it and an older looser file is replaced, not reused. Missing parent
// directories are created with mode 0700.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, ModeDir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mgw-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// WriteCredential writes one provider key as a single line to
// credentials/<id>.key with mode 0600.
func WriteCredential(dir, id, key string) error {
	if !provider.ValidID(id) {
		return Fail(CodeBadValue, "provider id %q is not valid", id)
	}
	if key == "" {
		return Fail(CodeMissingValue, "empty key for provider %q", id)
	}
	return WriteFileAtomic(CredentialPath(dir, id), []byte(key+"\n"), ModeSecret)
}

// ReadCredential reads one provider key. The file must not grant group or other
// access and must hold exactly one line; the error never carries the value.
func ReadCredential(dir, id string) (string, error) {
	if !provider.ValidID(id) {
		return "", Fail(CodeBadValue, "provider id %q is not valid", id)
	}
	key, err := ReadKeyFile(id, CredentialPath(dir, id))
	if err != nil {
		return "", Fail(CodeCredentialFile, "%v", err)
	}
	return key, nil
}

// ReadKeyFile reads a key from any file under the same rules as a credential
// file: mode without group or other access, one non-empty line.
func ReadKeyFile(id, path string) (string, error) {
	return provider.Provider{ID: id, CredentialFile: path}.Credential()
}
