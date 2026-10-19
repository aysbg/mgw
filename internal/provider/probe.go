package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Probe step outcomes and tool grades.
const (
	ProbeOK   = "ok"
	ProbeFail = "fail"
	// ProbeSkipped marks a step that did not run because an earlier one failed.
	ProbeSkipped = "skipped"

	GradePass = "pass"
	GradeFail = "fail"
)

// Probe deadlines. Every upstream call carries a connect, a time-to-first-byte
// and an overall deadline; a zero deadline is a defect.
const (
	probeConnect = 10 * time.Second
	probeTTFB    = 30 * time.Second
	probeStep    = 60 * time.Second
	// probeMaxBody bounds what one probe response may read.
	probeMaxBody = 1 << 20

	probeTool = "record_result"
)

// ProbeTarget is one provider to probe. Key is the credential value; it is used
// only in the request header and never appears in a result or an error.
type ProbeTarget struct {
	ID      string
	Dialect Dialect
	BaseURL string
	Key     string
	// Model is the upstream model name, without the "<provider>/" prefix.
	Model string
	// IncludeStreamUsage asks the stream probe to add stream_options, which
	// exercises the same injection path the gateway uses. Ignored for anthropic.
	IncludeStreamUsage bool
}

// ProbeStep is the outcome of one probe step. Detail is a short status text; it
// never carries a response body or a credential.
type ProbeStep struct {
	Result string
	Detail string
}

// OK reports whether the step passed.
func (s ProbeStep) OK() bool { return s.Result == ProbeOK }

// ProbeResult is what `mgw providers test` reports. ToolGrade is empty when the
// fidelity probe did not run.
type ProbeResult struct {
	Auth        ProbeStep
	Inference   ProbeStep
	StreamUsage ProbeStep
	ToolCall    ProbeStep
	ToolGrade   string
}

// Usable reports whether authentication and inference both passed.
func (r ProbeResult) Usable() bool { return r.Auth.OK() && r.Inference.OK() }

// ValidateEndpoint checks the identity and the base URL of a provider without a
// credential or timeouts, by running the runtime provider validation on a
// stand-in definition.
func ValidateEndpoint(id string, d Dialect, baseURL string) error {
	return Provider{
		ID: id, Dialect: d, BaseURL: baseURL, CredentialFile: "-",
		Timeouts: Timeouts{ConnectMS: 1, TTFBMS: 1, IdleMS: 1, OverallMS: 1},
	}.Validate()
}

// apiPrefix is the path prefix of the dialect's API.
func apiPrefix(d Dialect) string {
	if d == DialectOpenRouter {
		return "/api/v1"
	}
	return "/v1"
}

func probeClient() *http.Client {
	return &http.Client{
		Timeout: probeStep,
		Transport: &http.Transport{
			Proxy:                 nil,
			DisableCompression:    true,
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: probeConnect}).DialContext,
			TLSHandshakeTimeout:   probeConnect,
			ResponseHeaderTimeout: probeTTFB,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Probe runs the three-part provider test: authentication through the model
// list, one minimal non-streaming completion, and a fidelity probe made of a
// streaming request that asks for usage and a forced tool call. The tool call
// decides ToolGrade. Probe never returns an error: each failure is a step.
func Probe(ctx context.Context, t ProbeTarget) ProbeResult {
	skipped := ProbeStep{Result: ProbeSkipped}
	res := ProbeResult{Inference: skipped, StreamUsage: skipped, ToolCall: skipped}
	if err := ValidateEndpoint(t.ID, t.Dialect, t.BaseURL); err != nil {
		res.Auth = ProbeStep{Result: ProbeFail, Detail: t.redact(err.Error())}
		return res
	}
	if t.Key == "" || t.Model == "" {
		res.Auth = ProbeStep{Result: ProbeFail, Detail: "probe needs a key and a model"}
		return res
	}
	p := prober{t: t, c: probeClient()}

	res.Auth = p.auth(ctx)
	if !res.Auth.OK() {
		return res
	}
	res.Inference = p.inference(ctx)
	if !res.Inference.OK() {
		return res
	}
	res.StreamUsage = p.streamUsage(ctx)
	res.ToolCall = p.toolCall(ctx)
	if res.ToolCall.OK() {
		res.ToolGrade = GradePass
	} else {
		res.ToolGrade = GradeFail
	}
	return res
}

type prober struct {
	t ProbeTarget
	c *http.Client
}

func (t ProbeTarget) redact(s string) string {
	if t.Key == "" {
		return s
	}
	return strings.ReplaceAll(s, t.Key, "[redacted]")
}

// do sends one request and returns the status and a bounded body. The error
// text is redacted; the body is returned to the caller for parsing only.
func (p prober) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, probeStep)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.t.BaseURL+apiPrefix(p.t.Dialect)+path, rd)
	if err != nil {
		return 0, nil, errors.New(p.t.redact(err.Error()))
	}
	if p.t.Dialect == DialectAnthropic {
		req.Header.Set("X-Api-Key", p.t.Key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.t.Key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.c.Do(req)
	if err != nil {
		return 0, nil, errors.New(p.t.redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, probeMaxBody))
	if err != nil {
		return resp.StatusCode, nil, errors.New(p.t.redact(err.Error()))
	}
	return resp.StatusCode, data, nil
}

func (p prober) fail(format string, args ...any) ProbeStep {
	return ProbeStep{Result: ProbeFail, Detail: p.t.redact(fmt.Sprintf(format, args...))}
}

func (p prober) auth(ctx context.Context) ProbeStep {
	status, _, err := p.do(ctx, http.MethodGet, "/models", nil)
	switch {
	case err != nil:
		return p.fail("request failed: %v", err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return p.fail("key rejected (HTTP %d)", status)
	case status != http.StatusOK:
		return p.fail("model list returned HTTP %d", status)
	}
	return ProbeStep{Result: ProbeOK, Detail: "model list HTTP 200"}
}

func (p prober) messages(content string) []map[string]string {
	return []map[string]string{{"role": "user", "content": content}}
}

// base builds the request body shared by the completion probes.
func (p prober) base(content string, maxTokens int) map[string]any {
	m := map[string]any{"model": p.t.Model, "messages": p.messages(content)}
	if p.t.Dialect == DialectOpenAI {
		m["max_completion_tokens"] = maxTokens
	} else {
		m["max_tokens"] = maxTokens
	}
	return m
}

func (p prober) completionPath() string {
	if p.t.Dialect == DialectAnthropic {
		return "/messages"
	}
	return "/chat/completions"
}

func (p prober) inference(ctx context.Context) ProbeStep {
	status, body, err := p.do(ctx, http.MethodPost, p.completionPath(), p.base("Reply with the word OK.", 64))
	switch {
	case err != nil:
		return p.fail("request failed: %v", err)
	case status != http.StatusOK:
		return p.fail("completion returned HTTP %d", status)
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return p.fail("completion body is not a JSON object")
	}
	if _, ok := doc["choices"]; !ok {
		if _, ok := doc["content"]; !ok {
			return p.fail("completion body has neither choices nor content")
		}
	}
	return ProbeStep{Result: ProbeOK, Detail: "completion HTTP 200"}
}

func (p prober) streamUsage(ctx context.Context) ProbeStep {
	body := p.base("Reply with the word OK.", 64)
	body["stream"] = true
	if p.t.Dialect != DialectAnthropic && p.t.IncludeStreamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	status, data, err := p.do(ctx, http.MethodPost, p.completionPath(), body)
	switch {
	case err != nil:
		return p.fail("request failed: %v", err)
	case status != http.StatusOK:
		return p.fail("stream returned HTTP %d", status)
	}
	if streamHasUsage(data) {
		return ProbeStep{Result: ProbeOK, Detail: "stream reported token usage"}
	}
	return p.fail("stream carried no usage")
}

// streamHasUsage reports whether an SSE body carries a non-zero usage object:
// top-level usage (OpenAI style), message.usage or usage (Anthropic events).
func streamHasUsage(data []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), probeMaxBody)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev struct {
			Usage   map[string]json.RawMessage `json:"usage"`
			Message struct {
				Usage map[string]json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev) != nil {
			continue
		}
		if usageNonZero(ev.Usage) || usageNonZero(ev.Message.Usage) {
			return true
		}
	}
	return false
}

func usageNonZero(u map[string]json.RawMessage) bool {
	for _, k := range []string{"prompt_tokens", "completion_tokens", "input_tokens", "output_tokens", "total_tokens"} {
		var n int64
		if raw, ok := u[k]; ok && json.Unmarshal(raw, &n) == nil && n > 0 {
			return true
		}
	}
	return false
}

func (p prober) toolCall(ctx context.Context) ProbeStep {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "integer"}},
		"required":   []string{"value"},
	}
	body := p.base("Call "+probeTool+" with value 7.", 128)
	if p.t.Dialect == DialectAnthropic {
		body["tools"] = []map[string]any{{"name": probeTool, "description": "Record one integer.", "input_schema": schema}}
		body["tool_choice"] = map[string]any{"type": "tool", "name": probeTool}
	} else {
		body["tools"] = []map[string]any{{"type": "function", "function": map[string]any{
			"name": probeTool, "description": "Record one integer.", "parameters": schema}}}
		body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": probeTool}}
	}
	status, data, err := p.do(ctx, http.MethodPost, p.completionPath(), body)
	switch {
	case err != nil:
		return p.fail("request failed: %v", err)
	case status != http.StatusOK:
		return p.fail("tool call returned HTTP %d", status)
	}
	if wellFormedToolCall(p.t.Dialect, data) {
		return ProbeStep{Result: ProbeOK, Detail: "tool call well formed"}
	}
	return p.fail("tool call malformed or absent")
}

// wellFormedToolCall checks that the response calls the probe tool by name with
// arguments that parse as a JSON object.
func wellFormedToolCall(d Dialect, data []byte) bool {
	if d == DialectAnthropic {
		var doc struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &doc) != nil {
			return false
		}
		for _, c := range doc.Content {
			if c.Type == "tool_use" && c.Name == probeTool && isJSONObject(c.Input) {
				return true
			}
		}
		return false
	}
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &doc) != nil || len(doc.Choices) == 0 {
		return false
	}
	for _, tc := range doc.Choices[0].Message.ToolCalls {
		if tc.Function.Name == probeTool && isJSONObject(json.RawMessage(tc.Function.Arguments)) {
			return true
		}
	}
	return false
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}
