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
	"sort"
	"strings"

	"github.com/aysbg/mgw/internal/catalog"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

// Where an effective value came from, in precedence order.
const (
	SourceFlag    = "dispatch flag"
	SourceProfile = "engagement profile"
	SourceInstall = "install default"
	// SourceBuiltin labels a value no config supplied.
	SourceBuiltin = "built-in default"
)

// ProfilePolicy is the provider policy an engagement profile may override. A
// nil list is "not set": the install value stays. An empty list is a real,
// empty override.
type ProfilePolicy struct {
	Order []string `json:"order"`
	Deny  []string `json:"deny"`
}

// Profile is one engagement profile: overrides for one piece of work.
type Profile struct {
	Schema         string            `json:"schema,omitempty"`
	Strategy       string            `json:"strategy,omitempty"`
	ProviderPolicy *ProfilePolicy    `json:"provider_policy,omitempty"`
	DataClass      string            `json:"data_class,omitempty"`
	Pins           map[string]string `json:"pins,omitempty"`
}

// ProfileSchema is the optional schema tag of a profile.
const ProfileSchema = "mgw.profile/1"

// ProfilePath resolves which profile file applies. An explicit value is a path
// when it contains a separator or ends in .json, otherwise a name under
// profiles/. With no explicit value, the install's profile name applies; the
// default profile may be absent, any other named profile must exist.
func ProfilePath(dir string, cfg *Config, explicit string) (string, error) {
	named := func(name string) (string, error) {
		if !provider.ValidID(name) {
			return "", Fail(CodeBadValue, "profile name %q is not valid", name)
		}
		return filepath.Join(dir, ProfileDir, name+".json"), nil
	}
	if explicit != "" {
		if strings.ContainsAny(explicit, `/\`) || strings.HasSuffix(explicit, ".json") {
			return explicit, nil
		}
		return named(explicit)
	}
	if cfg == nil || cfg.Profile == "" {
		return "", nil
	}
	p, err := named(cfg.Profile)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if cfg.Profile == DefaultProfile {
			return "", nil
		}
		return "", Fail(CodeMissingProfile, "profile %q has no file at %s", cfg.Profile, p)
	}
	return p, nil
}

// LoadProfile reads and validates a profile file.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied profile path
	if errors.Is(err, os.ErrNotExist) {
		return nil, Fail(CodeMissingProfile, "no profile at %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, Fail(CodeBadValue, "profile %s: %v", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, Fail(CodeBadValue, "profile %s: trailing data after document", path)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return &p, nil
}

// CheckPinShape checks the role and the "<provider>/<model>" form of a pin.
func CheckPinShape(role, model string) error {
	if !slices.Contains(policy.Roles, role) {
		return Fail(CodeUnknownRole, "unknown role %q (want %s)", role, strings.Join(policy.Roles, ", "))
	}
	prov, name, ok := strings.Cut(model, "/")
	if !ok || prov == "" || name == "" {
		return Fail(CodeBadValue, "pin %s=%q: want <provider>/<model>", role, model)
	}
	return nil
}

// Validate checks a profile on its own: vocabulary, pin shapes and the
// order/deny conflict. Provider and model names are checked against an install
// by CheckAgainst.
func (p *Profile) Validate() error {
	if p.Schema != "" && p.Schema != ProfileSchema {
		return Fail(CodeBadValue, "schema %q, want %q", p.Schema, ProfileSchema)
	}
	if p.Strategy != "" {
		if err := CheckStrategy(p.Strategy); err != nil {
			return err
		}
	}
	if p.DataClass != "" {
		if err := CheckDataClass(p.DataClass); err != nil {
			return err
		}
	}
	if p.ProviderPolicy != nil {
		if err := CheckPolicyConflict(p.ProviderPolicy.Order, p.ProviderPolicy.Deny); err != nil {
			return err
		}
	}
	for role, model := range p.Pins {
		if err := CheckPinShape(role, model); err != nil {
			return err
		}
	}
	return nil
}

// CheckAgainst checks the provider and model names of a profile against an
// install: order entries must be connected providers and deny entries must be
// known providers or models.
func (p *Profile) CheckAgainst(cfg *Config, caps *catalog.Capabilities) error {
	if p.ProviderPolicy == nil {
		return nil
	}
	for _, id := range p.ProviderPolicy.Order {
		if _, ok := cfg.Find(id); !ok {
			return Fail(CodeUnknownProvider, "profile order names %q, which is not connected", id)
		}
	}
	return CheckDeny(p.ProviderPolicy.Deny, caps)
}

// CheckDeny checks that each deny entry is a known provider id or a known
// full model id.
func CheckDeny(entries []string, caps *catalog.Capabilities) error {
	for _, e := range entries {
		known := slices.ContainsFunc(caps.Providers, func(p catalog.CapProvider) bool { return p.ID == e }) ||
			slices.ContainsFunc(caps.Models, func(m catalog.Capability) bool { return m.ID == e })
		if !known {
			return Fail(CodeUnknownProvider, "deny entry %q is not a known provider or model", e)
		}
	}
	return nil
}

// CheckPin applies admission to one pin: the role exists, the provider is
// connected, the model is curated for that provider, it is not denied, the role
// is allowed with a non-zero rank, and a build pin does not land on a provider
// whose tool grade is fail. A failure is named; nothing is substituted.
func CheckPin(role, model string, providers []Provider, caps *catalog.Capabilities, deny []string) error {
	if err := CheckPinShape(role, model); err != nil {
		return err
	}
	prov, _, _ := strings.Cut(model, "/")
	i := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == prov })
	if i < 0 {
		return Fail(CodeUnknownProvider, "pin %s=%s: provider %q is not connected", role, model, prov)
	}
	j := slices.IndexFunc(caps.Models, func(m catalog.Capability) bool { return m.ID == model })
	if j < 0 || caps.Models[j].Provider != prov {
		return Fail(CodeUnknownModel, "pin %s=%s: no such model for provider %q", role, model, prov)
	}
	m := caps.Models[j]
	switch {
	case slices.Contains(deny, prov) || slices.Contains(deny, model):
		return Fail(CodePinNotAdmitted, "pin %s=%s: the provider or model is denied", role, model)
	case m.RoleRank[role] == 0:
		return Fail(CodePinNotAdmitted, "pin %s=%s: rank for %s is 0", role, model, role)
	case !slices.Contains(m.RolesAllowed, role):
		return Fail(CodePinNotAdmitted, "pin %s=%s: role %s is not allowed for this model", role, model, role)
	case role == "build" && providers[i].ToolGrade == provider.GradeFail:
		return Fail(CodePinNotAdmitted, "pin %s=%s: provider %q has tool_grade fail", role, model, prov)
	}
	return nil
}

// Flags are the dispatch-flag overrides of one command. A nil pointer or nil
// slice is "not given".
type Flags struct {
	Strategy  *string
	DataClass *string
	Order     []string
	Deny      []string
	Pins      map[string]string
}

// Effective is the set of values a command runs with, and where each came from.
// Sources is keyed "strategy", "order", "deny", "data_class" and "pin.<role>".
type Effective struct {
	Strategy  string
	Order     []string
	Deny      []string
	DataClass string
	Pins      map[string]string
	Sources   map[string]string
}

// Resolve applies the precedence: dispatch flags, then the engagement profile,
// then the install default. install and profile may be nil.
func Resolve(install *Config, profile *Profile, flags Flags) Effective {
	e := Effective{Pins: map[string]string{}, Sources: map[string]string{}}

	pickString := func(key string, flag *string, prof string, inst string, def string) string {
		switch {
		case flag != nil:
			e.Sources[key] = SourceFlag
			return *flag
		case prof != "":
			e.Sources[key] = SourceProfile
			return prof
		case install != nil && inst != "":
			e.Sources[key] = SourceInstall
			return inst
		}
		e.Sources[key] = SourceBuiltin
		return def
	}
	pickList := func(key string, flag, prof, inst []string) []string {
		switch {
		case flag != nil:
			e.Sources[key] = SourceFlag
			return slices.Clone(flag)
		case prof != nil:
			e.Sources[key] = SourceProfile
			return slices.Clone(prof)
		case install != nil:
			e.Sources[key] = SourceInstall
			return slices.Clone(inst)
		}
		e.Sources[key] = SourceBuiltin
		return nil
	}

	var profStrategy, profClass string
	var profOrder, profDeny []string
	var instStrategy, instClass string
	var instOrder, instDeny []string
	if profile != nil {
		profStrategy, profClass = profile.Strategy, profile.DataClass
		if profile.ProviderPolicy != nil {
			profOrder, profDeny = profile.ProviderPolicy.Order, profile.ProviderPolicy.Deny
		}
	}
	if install != nil {
		instStrategy, instClass = install.Strategy, install.DataClass
		instOrder, instDeny = install.ProviderPolicy.Order, install.ProviderPolicy.Deny
	}
	e.Strategy = pickString("strategy", flags.Strategy, profStrategy, instStrategy, "balanced")
	e.DataClass = pickString("data_class", flags.DataClass, profClass, instClass, "internal")
	e.Order = pickList("order", flags.Order, profOrder, instOrder)
	e.Deny = pickList("deny", flags.Deny, profDeny, instDeny)

	// Pins merge per role; the highest source wins each role.
	if install != nil {
		for role, m := range install.Pins {
			e.Pins[role], e.Sources["pin."+role] = m, SourceInstall
		}
	}
	if profile != nil {
		for role, m := range profile.Pins {
			e.Pins[role], e.Sources["pin."+role] = m, SourceProfile
		}
	}
	for role, m := range flags.Pins {
		e.Pins[role], e.Sources["pin."+role] = m, SourceFlag
	}
	return e
}

// SourceLines renders each effective value with its source, one per line, with
// a stable order: strategy, order, deny, data_class, then pins by role.
func (e Effective) SourceLines() []string {
	list := func(v []string) string {
		if len(v) == 0 {
			return "-"
		}
		return strings.Join(v, ",")
	}
	lines := []string{
		fmt.Sprintf("strategy %s (%s)", e.Strategy, e.Sources["strategy"]),
		fmt.Sprintf("order %s (%s)", list(e.Order), e.Sources["order"]),
		fmt.Sprintf("deny %s (%s)", list(e.Deny), e.Sources["deny"]),
		fmt.Sprintf("data_class %s (%s)", e.DataClass, e.Sources["data_class"]),
	}
	roles := make([]string, 0, len(e.Pins))
	for r := range e.Pins {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		lines = append(lines, fmt.Sprintf("pin %s=%s (%s)", r, e.Pins[r], e.Sources["pin."+r]))
	}
	return lines
}
