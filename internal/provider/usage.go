package provider

import (
	"bytes"
	"encoding/json"
)

// Bounds on the memory a UsageScanner may hold for one response.
const (
	maxUsageLine = 64 << 10 // longest single line inspected
	maxUsageBody = 1 << 20  // longest whole body kept for a non-line-based parse
)

// Usage is the token usage a provider reported. A nil field was not reported.
type Usage struct {
	InputTokens     *int64
	OutputTokens    *int64
	CacheReadTokens *int64
}

// Measured reports whether the provider reported input or output tokens.
func (u Usage) Measured() bool { return u.InputTokens != nil || u.OutputTokens != nil }

// Total is input plus output tokens, counting unreported fields as zero.
func (u Usage) Total() int64 {
	var n int64
	if u.InputTokens != nil {
		n += *u.InputTokens
	}
	if u.OutputTokens != nil {
		n += *u.OutputTokens
	}
	return n
}

// usageObj covers the usage object of every supported dialect.
//
//	openai chat:      prompt_tokens, completion_tokens, prompt_tokens_details.cached_tokens
//	openai responses: input_tokens, output_tokens, input_tokens_details.cached_tokens
//	anthropic:        input_tokens, output_tokens, cache_read_input_tokens
type usageObj struct {
	PromptTokens         *int64 `json:"prompt_tokens"`
	CompletionTokens     *int64 `json:"completion_tokens"`
	InputTokens          *int64 `json:"input_tokens"`
	OutputTokens         *int64 `json:"output_tokens"`
	CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
	PromptDetails        *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// usageEnvelope covers where a usage object can sit in a body or stream event.
type usageEnvelope struct {
	Usage   *usageObj `json:"usage"`
	Message *struct { // anthropic message_start
		Usage *usageObj `json:"usage"`
	} `json:"message"`
	Response *struct { // openai responses: response.completed
		Usage *usageObj `json:"usage"`
	} `json:"response"`
	PromptEvalCount *int64 `json:"prompt_eval_count"` // ollama native
	EvalCount       *int64 `json:"eval_count"`        // ollama native
}

// UsageScanner extracts usage from a response as it streams past, in bounded
// memory. It understands SSE data lines, NDJSON lines and a single JSON body.
// Later reported values replace earlier ones field by field, which matches
// Anthropic's cumulative message_delta usage.
type UsageScanner struct {
	line     []byte
	overflow bool // current line exceeded maxUsageLine
	body     []byte
	bodyFull bool // body exceeded maxUsageBody
	usage    Usage
}

// Write feeds response bytes to the scanner. It never fails.
func (s *UsageScanner) Write(p []byte) (int, error) {
	n := len(p)
	if !s.bodyFull {
		if len(s.body)+len(p) > maxUsageBody {
			s.bodyFull = true
			s.body = nil
		} else {
			s.body = append(s.body, p...)
		}
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		s.endLine()
		p = p[i+1:]
	}
	return n, nil
}

func (s *UsageScanner) appendLine(p []byte) {
	if s.overflow {
		return
	}
	if len(s.line)+len(p) > maxUsageLine {
		s.overflow = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

func (s *UsageScanner) endLine() {
	if !s.overflow {
		s.inspectLine(s.line)
	}
	s.line = s.line[:0]
	s.overflow = false
}

func (s *UsageScanner) inspectLine(line []byte) {
	line = bytes.TrimSpace(line)
	if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		line = bytes.TrimSpace(rest)
	}
	if len(line) == 0 || line[0] != '{' {
		return
	}
	s.absorb(line)
}

func (s *UsageScanner) absorb(data []byte) bool {
	var env usageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return false
	}
	found := false
	for _, u := range []*usageObj{env.Usage, messageUsage(env), responseUsage(env)} {
		if u != nil {
			s.merge(u)
			found = true
		}
	}
	if env.PromptEvalCount != nil || env.EvalCount != nil {
		setIfValid(&s.usage.InputTokens, env.PromptEvalCount)
		setIfValid(&s.usage.OutputTokens, env.EvalCount)
		found = true
	}
	return found
}

func messageUsage(e usageEnvelope) *usageObj {
	if e.Message == nil {
		return nil
	}
	return e.Message.Usage
}

func responseUsage(e usageEnvelope) *usageObj {
	if e.Response == nil {
		return nil
	}
	return e.Response.Usage
}

func (s *UsageScanner) merge(u *usageObj) {
	setIfValid(&s.usage.InputTokens, first(u.PromptTokens, u.InputTokens))
	setIfValid(&s.usage.OutputTokens, first(u.CompletionTokens, u.OutputTokens))
	cached := u.CacheReadInputTokens
	if cached == nil && u.PromptDetails != nil {
		cached = u.PromptDetails.CachedTokens
	}
	if cached == nil && u.InputDetails != nil {
		cached = u.InputDetails.CachedTokens
	}
	setIfValid(&s.usage.CacheReadTokens, cached)
}

func first(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}

// setIfValid stores a non-negative reported value.
func setIfValid(dst **int64, v *int64) {
	if v != nil && *v >= 0 {
		n := *v
		*dst = &n
	}
}

// Usage returns what the scanner found. Call it after the last Write.
func (s *UsageScanner) Usage() Usage {
	if len(s.line) > 0 && !s.overflow {
		s.inspectLine(s.line)
	}
	if !s.usage.Measured() && !s.bodyFull && len(s.body) > 0 {
		s.absorb(bytes.TrimSpace(s.body))
	}
	return s.usage
}

// ParseUsage extracts usage from a complete response body: one JSON object, an
// SSE stream, or NDJSON.
func ParseUsage(data []byte) Usage {
	var s UsageScanner
	_, _ = s.Write(data)
	return s.Usage()
}
