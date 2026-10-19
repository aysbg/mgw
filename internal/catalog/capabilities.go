package catalog

import (
	"bytes"
	_ "embed" // capabilities.json is embedded.
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

//go:embed capabilities.json
var capabilitiesJSON []byte

// CapProvider is a curated provider and its billing mode.
type CapProvider struct {
	ID      string `json:"id"`
	Billing string `json:"billing"`
}

// Capability is one curated model seat. RoleRank holds a rank per role; 0 marks
// the role ineligible. FixturePrice is the blended price at 90/10 in micro-USD
// per 1M tokens, a fixed input for the Gate 8 fixture; the live catalog supplies
// prices in normal operation.
type Capability struct {
	ID           string         `json:"id"`
	Provider     string         `json:"provider"`
	Family       string         `json:"family"`
	Tier         string         `json:"tier"`
	RoleRank     map[string]int `json:"role_rank"`
	RolesAllowed []string       `json:"roles_allowed"`
	Efforts      []string       `json:"efforts"`
	Prohibited   []string       `json:"prohibited"`
	Evidence     string         `json:"evidence"`
	FixturePrice int64          `json:"fixture_price"`
}

// Capabilities is the curated capability file.
type Capabilities struct {
	Version   string        `json:"version"`
	Providers []CapProvider `json:"providers"`
	Models    []Capability  `json:"models"`
}

// LoadCapabilities parses the embedded capability file.
func LoadCapabilities() (*Capabilities, error) {
	return ParseCapabilities(capabilitiesJSON)
}

// ParseCapabilities parses a capability file strictly: unknown fields and
// fractional numbers are errors.
func ParseCapabilities(data []byte) (*Capabilities, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Capabilities
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("capabilities: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("capabilities: decode: trailing data after document")
	}
	return &c, nil
}
