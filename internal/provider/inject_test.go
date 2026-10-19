package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const injected = `"stream_options":{"include_usage":true}`

func TestInjectStreamUsageRewritesOnlyWhenRulesAllow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // empty: the body must come back byte for byte
	}{
		{"stream true, no options",
			`{"model":"m","stream":true,"messages":[]}`,
			`{"model":"m","stream":true,"messages":[],` + injected + `}`},
		{"whitespace around members is preserved",
			"{\n  \"model\": \"m\",\n  \"stream\": true\n}\n",
			"{\n  \"model\": \"m\",\n  \"stream\": true," + injected + "\n}\n"},
		{"stream is the only member",
			`{"stream":true}`,
			`{"stream":true,` + injected + `}`},
		{"nested stream_options does not count as top-level",
			`{"stream":true,"extra":{"stream_options":{"include_usage":false}}}`,
			`{"stream":true,"extra":{"stream_options":{"include_usage":false}},` + injected + `}`},
		{"escaped key text is not a match",
			`{"stream":true,"note":"\"stream_options\""}`,
			`{"stream":true,"note":"\"stream_options\"",` + injected + `}`},

		{"caller include_usage false is preserved", `{"stream":true,"stream_options":{"include_usage":false}}`, ""},
		{"caller include_usage true is preserved", `{"stream":true,"stream_options":{"include_usage":true}}`, ""},
		{"caller stream_options with other fields", `{"stream":true,"stream_options":{"x":1}}`, ""},
		{"caller stream_options null", `{"stream":true,"stream_options":null}`, ""},
		{"caller stream_options precedes stream", `{"stream_options":{"include_usage":false},"stream":true}`, ""},
		{"stream false", `{"stream":false,"model":"m"}`, ""},
		{"stream absent", `{"model":"m"}`, ""},
		{"stream is a string", `{"stream":"true"}`, ""},
		{"stream is null", `{"stream":null}`, ""},
		{"empty object", `{}`, ""},
		{"duplicate stream key", `{"stream":true,"stream":false}`, ""},
		{"duplicate stream_options key", `{"stream":true,"stream_options":{},"stream_options":{}}`, ""},
		{"array body", `[{"stream":true}]`, ""},
		{"string body", `"stream"`, ""},
		{"not JSON", `stream=true`, ""},
		{"truncated JSON", `{"stream":true,`, ""},
		{"trailing data", `{"stream":true} {"x":1}`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			got, did := InjectStreamUsage(in)
			if tc.want == "" {
				if did || !bytes.Equal(got, []byte(tc.in)) {
					t.Fatalf("body must pass through unchanged: did=%v got=%q", did, got)
				}
				return
			}
			if !did || string(got) != tc.want {
				t.Fatalf("did=%v\n got %q\nwant %q", did, got, tc.want)
			}
			if string(in) != tc.in {
				t.Fatal("the input slice was modified")
			}
			if !json.Valid(got) {
				t.Fatalf("rewritten body is not valid JSON: %q", got)
			}
			if n := strings.Count(string(got), `"stream_options"`); n != strings.Count(tc.in, `"stream_options"`)+1 {
				t.Fatalf("stream_options injected %d times", n-strings.Count(tc.in, `"stream_options"`))
			}
		})
	}
}

func TestInjectStreamUsageIsIdempotent(t *testing.T) {
	once, did := InjectStreamUsage([]byte(`{"stream":true}`))
	if !did {
		t.Fatal("first pass must inject")
	}
	twice, did := InjectStreamUsage(once)
	if did || !bytes.Equal(once, twice) {
		t.Fatalf("second pass changed the body: %q", twice)
	}
}

func FuzzInjectStreamUsage(f *testing.F) {
	for _, s := range []string{`{"stream":true}`, `{"stream":true,"stream_options":{}}`, `[]`, ``, `{"a":{"stream":true}}`, "{\"stream\":true\n}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		orig := bytes.Clone(in)
		out, did := InjectStreamUsage(in)
		if !bytes.Equal(in, orig) {
			t.Fatal("input modified")
		}
		if !did {
			if !bytes.Equal(out, orig) {
				t.Fatal("unchanged body was altered")
			}
			return
		}
		if !json.Valid(out) {
			t.Fatalf("invalid JSON produced from %q", orig)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("not an object: %v", err)
		}
		if string(m["stream_options"]) != `{"include_usage":true}` {
			t.Fatalf("stream_options = %s", m["stream_options"])
		}
		if len(out) != len(orig)+len(`,`+injected) {
			t.Fatalf("more than the one field changed: %d -> %d bytes", len(orig), len(out))
		}
	})
}
