package config

import (
	"bytes"
	"encoding/json"
)

// Decision is the optional decision CLI configuration. It has no routing role.
type Decision struct {
	Enabled         bool    `json:"enabled"`
	Backend         string  `json:"backend"`
	BaseURL         string  `json:"base_url"`
	Model           string  `json:"model"`
	KeyFile         string  `json:"key_file"`
	TimeoutMS       int     `json:"timeout_ms"`
	ConfidenceFloor float64 `json:"confidence_floor"`
	ChoiceFloor     float64 `json:"choice_floor"`
	Margin          float64 `json:"margin"`
}

// DefaultDecision returns the disabled setup written by init without questions.
func DefaultDecision() *Decision {
	return &Decision{
		Backend: "ollama", BaseURL: "https://ollama.com", Model: "clef-flash",
		KeyFile: "credentials/decision.key", TimeoutMS: 2000,
		ConfidenceFloor: 0.70, ChoiceFloor: 0.55, Margin: 0.15,
	}
}

// UnmarshalJSON defaults omitted fields but preserves explicit invalid zeros.
func (d *Decision) UnmarshalJSON(data []byte) error {
	type plain Decision
	out := plain(*DefaultDecision())
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*d = Decision(out)
	return nil
}

// Validate checks the decision block, including a disabled block when present.
func (d *Decision) Validate() error {
	if d == nil {
		return nil
	}
	if d.Backend != "ollama" && d.Backend != "jev" {
		return Fail(CodeBadValue, "decision backend must be ollama or jev")
	}
	if d.TimeoutMS < 100 || d.TimeoutMS > 10000 {
		return Fail(CodeBadValue, "decision timeout_ms must be 100–10000")
	}
	if !(d.ConfidenceFloor > 0 && d.ConfidenceFloor < 1) {
		return Fail(CodeBadValue, "decision confidence_floor must be in (0,1)")
	}
	if !(d.ChoiceFloor > 0 && d.ChoiceFloor < 1) {
		return Fail(CodeBadValue, "decision choice_floor must be in (0,1)")
	}
	if !(d.Margin > 0 && d.Margin < 1) {
		return Fail(CodeBadValue, "decision margin must be in (0,1)")
	}
	if d.ConfidenceFloor+d.Margin > 1 {
		return Fail(CodeBadValue, "decision confidence_floor+margin must be <= 1")
	}
	return nil
}
