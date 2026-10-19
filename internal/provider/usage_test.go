package provider

import (
	"strings"
	"testing"
)

func ptr(n int64) *int64 { return &n }

func eq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestParseUsage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Usage
	}{
		{"openai chat json", `{"id":"x","usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`,
			Usage{ptr(11), ptr(7), ptr(4)}},
		{"openai chat pretty json", "{\n \"usage\": {\n  \"prompt_tokens\": 3,\n  \"completion_tokens\": 2\n }\n}\n",
			Usage{ptr(3), ptr(2), nil}},
		{"openai sse final chunk", "data: {\"choices\":[]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n",
			Usage{ptr(5), ptr(9), nil}},
		{"openai responses completed", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n",
			Usage{ptr(8), ptr(2), ptr(1)}},
		{"anthropic json", `{"usage":{"input_tokens":20,"output_tokens":30,"cache_read_input_tokens":6}}`,
			Usage{ptr(20), ptr(30), ptr(6)}},
		{"anthropic stream cumulative", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1,\"cache_read_input_tokens\":3}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n",
			Usage{ptr(25), ptr(42), ptr(3)}},
		{"ollama native", `{"done":true,"prompt_eval_count":12,"eval_count":34}`, Usage{ptr(12), ptr(34), nil}},
		{"crlf sse", "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\r\n\r\n", Usage{ptr(1), ptr(2), nil}},
		{"no usage", `{"error":{"message":"nope"}}`, Usage{}},
		{"negative ignored", `{"usage":{"prompt_tokens":-1,"completion_tokens":-5}}`, Usage{}},
		{"garbage", "not json at all", Usage{}},
		{"empty", "", Usage{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUsage([]byte(tc.in))
			if !eq(got.InputTokens, tc.want.InputTokens) || !eq(got.OutputTokens, tc.want.OutputTokens) || !eq(got.CacheReadTokens, tc.want.CacheReadTokens) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestUsageScannerSplitAcrossWrites(t *testing.T) {
	in := "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n"
	var s UsageScanner
	for i := 0; i < len(in); i++ {
		_, _ = s.Write([]byte{in[i]})
	}
	got := s.Usage()
	if !eq(got.InputTokens, ptr(5)) || !eq(got.OutputTokens, ptr(9)) {
		t.Fatalf("got %+v", got)
	}
}

func TestUsageScannerBounded(t *testing.T) {
	var s UsageScanner
	chunk := []byte(strings.Repeat("x", 1<<16))
	for i := 0; i < 64; i++ { // 4 MiB with no newline
		_, _ = s.Write(chunk)
	}
	if len(s.line) > maxUsageLine || len(s.body) > maxUsageBody {
		t.Fatalf("unbounded: line=%d body=%d", len(s.line), len(s.body))
	}
	if s.Usage().Measured() {
		t.Fatal("expected no usage")
	}
}

func FuzzParseUsage(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		"data: {\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\ndata: [DONE]\n\n",
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n",
		`{"prompt_eval_count":3,"eval_count":4}`,
		`{"usage":{"prompt_tokens":-3,"completion_tokens":9223372036854775807}}`,
		"data:\n\n\x00\xff",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		u := ParseUsage(data)
		for _, p := range []*int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens} {
			if p != nil && *p < 0 {
				t.Fatalf("negative token count %d", *p)
			}
		}
	})
}
