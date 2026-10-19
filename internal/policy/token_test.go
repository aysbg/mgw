package policy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func testToken() string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 33)) }

func testAuthority() *Authority {
	return &Authority{
		Token: testToken(), RunID: "run-1", Attempt: 1, ExpiresAt: epoch.Add(time.Hour),
		Providers:    map[string][]string{"openai": {"gpt-6-astra"}, "anthropic": {"claude-opus-5-5"}},
		Budget:       Budget{MaxTokens: 50000},
		Concurrency:  2,
		PolicySHA256: "p", RegistrySHA256: "r",
	}
}

func TestAuthenticate(t *testing.T) {
	a := testAuthority()
	if err := a.Authenticate(testToken(), epoch); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		tok string
		now time.Time
	}{
		"wrong":     {"nope", epoch},
		"empty":     {"", epoch},
		"expired":   {testToken(), epoch.Add(2 * time.Hour)},
		"at expiry": {testToken(), a.ExpiresAt},
	} {
		if err := a.Authenticate(tc.tok, tc.now); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestCheckModel(t *testing.T) {
	a := testAuthority()
	if err := a.CheckModel("openai", "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckModel("openai", "claude-opus-5-5"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("got %v", err)
	}
	if err := a.CheckModel("gemini", "x"); !errors.Is(err, ErrProviderNotAllowed) {
		t.Fatalf("got %v", err)
	}
	if Code(ErrModelNotAllowed) != "model_not_allowed" || Code(errors.New("x")) != "" {
		t.Fatal("bad codes")
	}
}

func TestParseAuthority(t *testing.T) {
	ok := `{"token":"` + testToken() + `","run_id":"r","attempt":1,"expires_at":"2026-10-19T13:00:00Z",
	"providers":{"openai":["m"]},"budget":{"max_tokens":10},"concurrency":1,"policy_sha256":"a","registry_sha256":"b"}`
	if _, err := ParseAuthority([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"short token":   strings.Replace(ok, testToken(), "abc", 1),
		"unknown field": strings.Replace(ok, `"attempt":1`, `"attempt":1,"role":"admin"`, 1),
		"no providers":  strings.Replace(ok, `{"openai":["m"]}`, `{}`, 1),
		"zero budget":   strings.Replace(ok, `"max_tokens":10`, `"max_tokens":0`, 1),
		"zero conc":     strings.Replace(ok, `"concurrency":1`, `"concurrency":0`, 1),
		"bad expiry":    strings.Replace(ok, `2026-10-19T13:00:00Z`, `tomorrow`, 1),
	}
	for name, in := range bad {
		if _, err := ParseAuthority([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestReadBodyAndParse(t *testing.T) {
	if _, err := ReadBody(strings.NewReader("x"), MaxBodyBytes+1); !errors.Is(err, ErrBodyTooLarge) || !errors.Is(err, ErrMalformed) {
		t.Fatalf("declared oversize: %v", err)
	}
	big := strings.NewReader(strings.Repeat("a", MaxBodyBytes+1))
	if _, err := ReadBody(big, -1); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("streamed oversize: %v", err)
	}
	exact, err := ReadBody(strings.NewReader(strings.Repeat("a", MaxBodyBytes)), -1)
	if err != nil || len(exact) != MaxBodyBytes {
		t.Fatalf("exact cap: %v", err)
	}

	meta, err := ParseRequest([]byte(`{"model":"gpt-6-astra","max_tokens":100,"max_completion_tokens":300,"messages":[]}`))
	if err != nil || meta.Model != "gpt-6-astra" || meta.MaxOutputTokens != 300 {
		t.Fatalf("got %+v, %v", meta, err)
	}
	for _, in := range []string{``, `[]`, `{}`, `{"model":""}`, `{"model":5}`, `{"model":"m","max_tokens":"x"}`, `not json`} {
		if _, err := ParseRequest([]byte(in)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: got %v", in, err)
		}
	}
}
