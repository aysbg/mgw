package parity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aysbg/mgw/internal/parity"
)

// The expected digests were computed with the Python implementation:
//
//	json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
var fixtures = []struct {
	name, payload, sha string
}{
	{"f1", `{"a":1,"b":["x","y"],"c":{"d":2}}`,
		"5fe78e4b0975502d88e204a83ca71ba77590deec6a6788f6d5b884ed0fd96861"},
	{"f2", `{"models":[{"id":"ollama-cloud/glm-5.3","tier":"good","role_rank":{"build":20}}],"providers":[{"id":"ollama-cloud","billing":"included_credits"}],"version":"2.0"}`,
		"cc6a06fa1b43224e54dda5df40bb083613cab88425fed61be213adba4f06743f"},
	{"f3", `{"unicode":"café ✓","n":-7,"big":9007199254740993,"bool":true,"null":null}`,
		"a2cc49f3df2d6f4a8a4482e5a5a50f568b96655ce88638b2dddeb7f85e7681ca"},
}

// TestGate13_PythonParityFixtures asserts the three Python-computed digests.
func TestGate13_PythonParityFixtures(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			out, err := parity.CanonicalJSON([]byte(f.payload))
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if got := parity.SHA256Hex(out); got != f.sha {
				t.Fatalf("sha256 = %s, want %s\ncanonical: %s", got, f.sha, out)
			}
		})
	}
}

// TestGate13_BigIntegerSurvives guards against a float64 decode: the last digit
// of 9007199254740993 is lost when it goes through float64.
func TestGate13_BigIntegerSurvives(t *testing.T) {
	out, err := parity.CanonicalJSON([]byte(`{"big":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"big":9007199254740993}` {
		t.Fatalf("got %s", out)
	}
}

// TestGate13_FloatsRejected: no float in a hashed payload, in any spelling.
func TestGate13_FloatsRejected(t *testing.T) {
	for _, in := range []string{`{"a":1.5}`, `{"a":2.0}`, `{"a":1e3}`, `{"a":[1,{"b":-0.1}]}`, `[1E2]`} {
		if _, err := parity.CanonicalJSON([]byte(in)); !errors.Is(err, parity.ErrFloat) {
			t.Errorf("%s: err = %v, want ErrFloat", in, err)
		}
	}
	// A float-looking string is not a float.
	if _, err := parity.CanonicalJSON([]byte(`{"v":"2.0"}`)); err != nil {
		t.Fatalf("string 2.0 rejected: %v", err)
	}
	if _, err := parity.Canonical(struct {
		F float64 `json:"f"`
	}{F: 0.5}); !errors.Is(err, parity.ErrFloat) {
		t.Fatalf("float64 field: err = %v, want ErrFloat", err)
	}
}

// TestGate13_StringEscapingMatchesPython covers the cases where Go's own
// encoder differs from json.dumps(ensure_ascii=False).
func TestGate13_StringEscapingMatchesPython(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"k":"<a&b>"}`, `{"k":"<a&b>"}`},                                 // no HTML escaping
		{`{"k":"\u2028\u2029"}`, "{\"k\":\"\u2028\u2029\"}"},               // Go escapes these; Python does not
		{`{"k":"\u0001\u001f\u007f"}`, "{\"k\":\"\\u0001\\u001f\u007f\"}"}, // lowercase hex, DEL literal
		{`{"k":"\b\f\n\r\t\"\\/"}`, `{"k":"\b\f\n\r\t\"\\/"}`},
		{`{"é":1,"z":2,"a":3}`, `{"a":3,"z":2,"é":1}`}, // codepoint order
		{`{"n":-0}`, `{"n":0}`},                        // Python loads -0 as int 0
		{"{ \"a\" : [ 1 , 2 ] }", `{"a":[1,2]}`},
	}
	for _, c := range cases {
		out, err := parity.CanonicalJSON([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if string(out) != c.want {
			t.Errorf("%s: got %q want %q", c.in, out, c.want)
		}
	}
}

// TestGate13_StructAndJSONAgree: encoding a Go value and encoding its JSON text
// give identical bytes.
func TestGate13_StructAndJSONAgree(t *testing.T) {
	type rank struct {
		Build int `json:"build"`
	}
	type model struct {
		ID       string `json:"id"`
		Tier     string `json:"tier"`
		RoleRank rank   `json:"role_rank"`
	}
	v := map[string]any{
		"version":   "2.0",
		"providers": []map[string]string{{"id": "ollama-cloud", "billing": "included_credits"}},
		"models":    []model{{ID: "ollama-cloud/glm-5.3", Tier: "good", RoleRank: rank{Build: 20}}},
	}
	out, err := parity.Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	if got := parity.SHA256Hex(out); got != fixtures[1].sha {
		t.Fatalf("struct encoding sha = %s, want %s", got, fixtures[1].sha)
	}
}

func TestGate13_RejectsTrailingAndInvalid(t *testing.T) {
	for _, in := range []string{``, `{"a":1} {"b":2}`, `{"a":`, `[1,]`, strings.Repeat("[", 10000)} {
		if _, err := parity.CanonicalJSON([]byte(in)); err == nil {
			t.Errorf("%.20q: expected error", in)
		}
	}
}
