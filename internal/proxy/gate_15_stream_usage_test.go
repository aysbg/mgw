package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/provider"
)

// MGW-001: the gateway adds stream_options.include_usage to streaming requests
// so the provider reports usage in the final chunk. Every assertion about the
// injected field reads the body the FAKE UPSTREAM received; nothing here looks
// at gateway-internal state.

const (
	streamChatBody = `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	// streamChatBodyInjected is streamChatBody plus the one sanctioned field.
	streamChatBodyInjected = `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":true}}`
	streamMsgBody          = `{"model":"claude-opus-5-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello"}]}`

	openAIStream = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	openAIStreamNoUsage = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// sseUpstream answers every request with the given SSE text, flushed in two parts.
func sseUpstream(stream string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		mid := len(stream) / 2
		_, _ = io.WriteString(w, stream[:mid])
		_ = rc.Flush()
		_, _ = io.WriteString(w, stream[mid:])
	}
}

// oneProvider replaces the harness providers with a single one that points at
// the fake upstream. The ID must be one the token authorizes.
func oneProvider(id string, dialect provider.Dialect, includeStreamUsage *bool) func(*harnessOpts) {
	return func(o *harnessOpts) {
		o.providers = func(upstreamURL, credFile string) []provider.Provider {
			return []provider.Provider{{
				ID: id, Dialect: dialect, BaseURL: upstreamURL, CredentialFile: credFile,
				Timeouts: o.timeouts, IncludeStreamUsage: includeStreamUsage,
			}}
		}
	}
}

func withStreamHandler(stream string, mod func(*harnessOpts)) func(*harnessOpts) {
	return func(o *harnessOpts) {
		o.handler = sseUpstream(stream)
		if mod != nil {
			mod(o)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// received returns the single request the fake upstream saw.
func received(t *testing.T, h *harness) upstreamRequest {
	t.Helper()
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests, want exactly 1", n)
	}
	return h.upstream.request(0)
}

func TestGate15_StreamUsageInjectedOnceIntoTheUpstreamBody(t *testing.T) {
	h := newHarness(t, withStreamHandler(openAIStream, nil))
	resp := postStream(t, h, chatPath, streamChatBody)
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, err %v", resp.StatusCode, err)
	}
	if string(got) != openAIStream {
		t.Fatalf("relay is not byte-exact: %q", got)
	}

	up := received(t, h)
	if string(up.Body) != streamChatBodyInjected {
		t.Fatalf("upstream body:\n got %s\nwant %s", up.Body, streamChatBodyInjected)
	}
	if n := bytes.Count(up.Body, []byte(`"stream_options"`)); n != 1 {
		t.Fatalf("upstream body carries stream_options %d times", n)
	}
	var parsed struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(up.Body, &parsed); err != nil || !parsed.StreamOptions.IncludeUsage {
		t.Fatalf("upstream did not receive stream_options.include_usage=true: %s (%v)", up.Body, err)
	}
	if cl := up.Header.Get("Content-Length"); cl != strconv.Itoa(len(up.Body)) {
		t.Fatalf("Content-Length %q does not match the rewritten body of %d bytes", cl, len(up.Body))
	}
	if rec := h.lastRecord(); !rec.StreamUsageInjected {
		t.Fatalf("audit does not record the injection: %+v", rec)
	}
}

func TestGate15_StreamUsageContentLengthMatchesRewrittenBodyAtTheCap(t *testing.T) {
	// A body just under the cap grows by the injected field and must still be
	// sent whole, with its own length, not the caller's.
	pad := strings.Repeat("x", 60<<10)
	body := `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"` + pad + `"}]}`
	h := newHarness(t, withStreamHandler(openAIStream, nil))
	resp := postStream(t, h, chatPath, body)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	up := received(t, h)
	if !strings.HasSuffix(string(up.Body), `,"stream_options":{"include_usage":true}}`) || len(up.Body) != len(body)+len(`,"stream_options":{"include_usage":true}`) {
		t.Fatalf("upstream body is %d bytes, caller sent %d", len(up.Body), len(body))
	}
	if cl := up.Header.Get("Content-Length"); cl != strconv.Itoa(len(up.Body)) {
		t.Fatalf("Content-Length %q, body %d bytes", cl, len(up.Body))
	}
}

func TestGate15_CallerStreamOptionsReachUpstreamUntouched(t *testing.T) {
	for name, body := range map[string]string{
		"include_usage false": `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"stream_options":{"include_usage":false},"messages":[]}`,
		"include_usage true":  `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"stream_options":{"include_usage":true},"messages":[]}`,
		"other options only":  `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"messages":[],"stream_options":{"x":1,  "y": [1, 2]}}`,
		"stream_options null": `{"model":"gpt-6-astra","max_tokens":100,"stream":true,"stream_options":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withStreamHandler(openAIStream, nil))
			resp := postStream(t, h, chatPath, body)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if up := received(t, h); string(up.Body) != body {
				t.Fatalf("caller body was changed:\n got %s\nwant %s", up.Body, body)
			}
			if rec := h.lastRecord(); rec.StreamUsageInjected {
				t.Fatalf("audit claims an injection that did not happen: %+v", rec)
			}
		})
	}
}

func TestGate15_StreamUsageNotInjectedWhenProviderFlagIsFalse(t *testing.T) {
	h := newHarness(t, withStreamHandler(openAIStream, oneProvider("openai", provider.DialectOpenAI, boolPtr(false))))
	resp := postStream(t, h, chatPath, streamChatBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if up := received(t, h); string(up.Body) != streamChatBody {
		t.Fatalf("include_stream_usage:false still changed the body: %s", up.Body)
	}
	if rec := h.lastRecord(); rec.StreamUsageInjected {
		t.Fatalf("audit: %+v", rec)
	}
}

func TestGate15_StreamUsageInjectedForOpenRouterAndOllamaDialects(t *testing.T) {
	for _, d := range []provider.Dialect{provider.DialectOpenAI, provider.DialectOpenRouter, provider.DialectOllama} {
		t.Run(string(d), func(t *testing.T) {
			h := newHarness(t, withStreamHandler(openAIStream, oneProvider("openai", d, nil)))
			resp := postStream(t, h, chatPath, streamChatBody)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			up := received(t, h)
			if string(up.Body) != streamChatBodyInjected {
				t.Fatalf("upstream body: %s", up.Body)
			}
			if a := up.Header.Get("Authorization"); a != "Bearer "+testCredential {
				t.Fatalf("Authorization = %q", a)
			}
			if up.Header.Get("X-Api-Key") != "" {
				t.Fatal("a bearer dialect sent x-api-key")
			}
		})
	}
}

func TestGate15_AnthropicDialectNeverInjectsStreamUsage(t *testing.T) {
	h := newHarness(t, withStreamHandler(anthropicStream, nil))
	resp := postStream(t, h, msgPath, streamMsgBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	up := received(t, h)
	if string(up.Body) != streamMsgBody || bytes.Contains(up.Body, []byte("stream_options")) {
		t.Fatalf("anthropic body was changed: %s", up.Body)
	}
	if up.Header.Get("X-Api-Key") != testCredential || up.Header.Get("Authorization") != "" {
		t.Fatalf("anthropic credential headers: %v", up.Header)
	}
	if rec := h.lastRecord(); rec.StreamUsageInjected {
		t.Fatalf("audit: %+v", rec)
	}
}

func TestGate15_NonStreamingAndNonJSONBodiesPassThroughUnchanged(t *testing.T) {
	for name, body := range map[string]string{
		"stream absent": chatBody,
		"stream false":  `{"model":"gpt-6-astra","max_tokens":100,"stream":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			resp, _ := h.do(chatPath, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if up := received(t, h); string(up.Body) != body {
				t.Fatalf("body changed: %s", up.Body)
			}
			if rec := h.lastRecord(); rec.StreamUsageInjected {
				t.Fatalf("audit: %+v", rec)
			}
		})
	}
	t.Run("not a JSON object is rejected as before and never forwarded", func(t *testing.T) {
		for _, body := range []string{`stream=true`, `[{"stream":true}]`} {
			h := newHarness(t, nil)
			resp, _ := h.do(chatPath, body, nil)
			if resp.StatusCode != http.StatusBadRequest || h.upstream.count() != 0 {
				t.Fatalf("body %q: status %d, upstream calls %d", body, resp.StatusCode, h.upstream.count())
			}
		}
	})
	t.Run("a body over the cap is rejected as before and never forwarded", func(t *testing.T) {
		h := newHarness(t, nil)
		big := `{"model":"gpt-6-astra","stream":true,"p":"` + strings.Repeat("x", 70<<10) + `"}`
		resp, body := h.do(chatPath, big, nil)
		if resp.StatusCode != http.StatusRequestEntityTooLarge || h.upstream.count() != 0 {
			t.Fatalf("status %d (%s), upstream calls %d", resp.StatusCode, body, h.upstream.count())
		}
	})
}

func TestGate15_StreamUsageMeasuredFromOpenAIFinalChunkSettlesLedger(t *testing.T) {
	h := newHarness(t, withStreamHandler(openAIStream, nil))
	resp := postStream(t, h, chatPath, streamChatBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	rec := h.lastRecord()
	if line, err := json.Marshal(rec); err == nil {
		t.Logf("audit record of a successful streaming run: %s", line)
	}
	if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 12 || i64v(rec.OutputTokens) != 5 {
		t.Fatalf("audit usage: %+v", rec)
	}
	if !rec.StreamUsageInjected || rec.FailureCode != "" || rec.CommitState != audit.CommitCommitted || rec.StatusCode != 200 {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Settled != 17 || tot.Uncertain != 0 || tot.Reserved != 0 {
		t.Fatalf("ledger must settle the measured total of 17: %+v", tot)
	}
}

func TestGate15_StreamUsageMeasuredFromAnthropicMessageDeltaSettlesLedger(t *testing.T) {
	h := newHarness(t, withStreamHandler(anthropicStream, nil))
	resp := postStream(t, h, msgPath, streamMsgBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	rec := h.lastRecord()
	if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 25 || i64v(rec.OutputTokens) != 42 || rec.StreamUsageInjected || rec.FailureCode != "" {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Settled != 67 || tot.Uncertain != 0 || tot.Reserved != 0 {
		t.Fatalf("ledger: %+v", tot)
	}
}

func TestGate15_StreamWithoutUsageChunkRecordsUnknownAndKeepsTheUncertainCharge(t *testing.T) {
	h := newHarness(t, withStreamHandler(openAIStreamNoUsage, nil))
	resp := postStream(t, h, chatPath, streamChatBody)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != openAIStreamNoUsage {
		t.Fatalf("relay: %q", got)
	}
	rec := h.lastRecord()
	if rec.UsageStatus != audit.UsageUnknown || rec.InputTokens != nil || rec.OutputTokens != nil {
		t.Fatalf("a stream with no usage chunk must record usage unknown: %+v", rec)
	}
	if tot := h.totals(); tot.Uncertain != 100 || tot.Settled != 0 || tot.Reserved != 0 {
		t.Fatalf("the reservation must stay as an uncertain charge: %+v", tot)
	}
}

func TestGate15_ProviderRejectingInjectedFieldIsRecordedAndNotRetriedWithoutIt(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown parameter: stream_options"}}`)
		}
	})
	resp := postStream(t, h, chatPath, streamChatBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: the upstream 400 must reach the caller", resp.StatusCode)
	}
	up := received(t, h) // exactly one call: no silent retry without the field
	if string(up.Body) != streamChatBodyInjected {
		t.Fatalf("upstream body: %s", up.Body)
	}
	rec := h.lastRecord()
	if rec.StatusCode != http.StatusBadRequest || !rec.StreamUsageInjected || rec.UpstreamAttempts != 1 {
		t.Fatalf("the failure and the injection must both be on the record: %+v", rec)
	}
}
