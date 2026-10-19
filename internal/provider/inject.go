package provider

import (
	"bytes"
	"encoding/json"
	"io"
)

// streamUsageField is the single field the gateway may add to a request body.
const streamUsageField = `"stream_options":{"include_usage":true}`

// InjectStreamUsage adds "stream_options":{"include_usage":true} to a streaming
// request body so the provider reports token usage in the final stream chunk.
// It is the one sanctioned payload exception and is deliberately narrow.
//
// The body is rewritten only when it is a JSON object, its "stream" member is
// the literal true, and it has no "stream_options" member at all. A caller's
// stream_options, including include_usage:false or null, is never touched. In
// every other case, including any body that does not parse as a single JSON
// object and any body with a repeated "stream" or "stream_options" key, the
// input is returned unchanged with false.
//
// The new member is inserted by byte splice directly after the last member, so
// every other byte of the body is preserved.
func InjectStreamUsage(body []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return body, false
	}
	var (
		streamTrue    bool
		streamSeen    bool
		optionsSeen   bool
		members       int
		lastValueEnds int64
	)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return body, false
		}
		key, ok := tok.(string)
		if !ok {
			return body, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return body, false
		}
		members++
		lastValueEnds = dec.InputOffset()
		switch key {
		case "stream":
			if streamSeen {
				return body, false // ambiguous: parsers disagree on which one wins
			}
			streamSeen = true
			streamTrue = string(raw) == "true"
		case "stream_options":
			if optionsSeen {
				return body, false
			}
			optionsSeen = true
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return body, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return body, false // trailing data after the object
	}
	if !streamTrue || optionsSeen || members == 0 {
		return body, false
	}

	out := make([]byte, 0, len(body)+len(streamUsageField)+1)
	out = append(out, body[:lastValueEnds]...)
	out = append(out, ',')
	out = append(out, streamUsageField...)
	out = append(out, body[lastValueEnds:]...)
	return out, true
}
