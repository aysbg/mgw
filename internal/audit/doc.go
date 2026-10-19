// Package audit writes metadata-only audit records: one JSON object per line,
// append-only, mode 0600. The writer takes a struct, never a map, so a field
// cannot be added ad hoc. Prompts, completions, headers, credentials and raw
// upstream error bodies have no field and so cannot be recorded.
package audit
