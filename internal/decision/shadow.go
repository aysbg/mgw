package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShadowFile is the append-only decision log under the install directory.
const ShadowFile = "decisions.jsonl"

// ShadowRecord is the allowlisted shadow log schema. It never stores full state.
type ShadowRecord struct {
	TS           string `json:"ts"`
	StateSHA256  string `json:"state_sha256"`
	StateExcerpt string `json:"state_excerpt"`
	Result
	PolicyVersion string `json:"policy_version"`
}

// AppendShadow appends one record with a bounded excerpt, never the API key.
// Existing files with group or other access are rejected, never chmodded.
func (cfg Config) AppendShadow(path, state string, result Result, now time.Time) error {
	key, err := cfg.loadKey()
	if err != nil {
		return err
	}
	redact := func(s string) string {
		if key != "" {
			return strings.ReplaceAll(s, key, "[redacted]")
		}
		return s
	}
	// The model name is upstream-controlled. Sanitize every string field, not
	// just the excerpt, so an upstream cannot echo a credential into this log.
	result.Type, result.Size = redact(result.Type), redact(result.Size)
	result.Backend, result.DecisionModel = redact(result.Backend), redact(result.DecisionModel)
	result.FallbackReason = redact(result.FallbackReason)
	flags := make([]string, len(result.RiskFlags))
	for i, flag := range result.RiskFlags {
		flags[i] = redact(flag)
	}
	result.RiskFlags = flags
	excerpt := []rune(strings.Join(strings.Fields(redact(state)), " "))
	if len(excerpt) > 300 {
		excerpt = excerpt[:300]
	}
	sum := sha256.Sum256([]byte(state))
	record := ShadowRecord{
		TS: now.UTC().Format(time.RFC3339Nano), StateSHA256: hex.EncodeToString(sum[:]),
		StateExcerpt: string(excerpt), Result: result, PolicyVersion: "decision-1.0",
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("decision_shadow_encode: %w", err)
	}
	f, err := openShadow(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("decision_shadow_write: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("decision_shadow_close: %w", err)
	}
	return nil
}

func checkShadowFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("decision_shadow_file: shadow log must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&^0o600 != 0 {
		return errors.New("decision_shadow_permissions: shadow log must be mode 0600")
	}
	return nil
}

func openShadow(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("decision_shadow_directory: %w", err)
	}
	before, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("decision_shadow_stat: %w", err)
	}
	if err == nil {
		if err := checkShadowFile(before); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- operator-supplied install path
	if err != nil {
		return nil, fmt.Errorf("decision_shadow_open: %w", err)
	}
	info, err := f.Stat()
	if err == nil {
		err = checkShadowFile(info)
	}
	if err == nil && before != nil && !os.SameFile(before, info) {
		err = errors.New("decision_shadow_file: shadow log changed while opening")
	}
	// Recheck the path after opening to reject symlinks introduced in between.
	if err == nil {
		var after os.FileInfo
		after, err = os.Lstat(path)
		if err == nil {
			err = checkShadowFile(after)
		}
		if err == nil && !os.SameFile(info, after) {
			err = errors.New("decision_shadow_file: shadow log changed while opening")
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("decision_shadow_file: %w", err)
	}
	return f, nil
}
