package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func loadFixture(t testing.TB) (*Catalog, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/models.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode fixture: %v", err)
	}
	return c, raw
}

func TestParseMicro(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"5", 5000000}, {"0.15", 150000}, {"2.5", 2500000}, {"0.000001", 1},
		{"0.0000005", 1}, {"0.0000004", 0}, {"0.1234567", 123457}, {"12.5", 12500000},
	}
	for _, c := range ok {
		got, err := ParseMicro(json.Number(c.in))
		if err != nil || got != c.want {
			t.Errorf("ParseMicro(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "-1", "-0.5", "1e3", "1E-7", "abc", ".5", "5.", "1.2.3", "99999999999999999999", "0x10", "+1"} {
		if _, err := ParseMicro(json.Number(in)); err == nil {
			t.Errorf("ParseMicro(%q): expected error", in)
		}
	}
}

func TestDecodeFixture(t *testing.T) {
	c, _ := loadFixture(t)
	m, ok := c.Lookup("ollama-cloud/deepseek-v4.1-flash:1017")
	if !ok {
		t.Fatal("suffixed id not found")
	}
	if m.InputMicro != 150000 || m.OutputMicro != 600000 || !m.HasCost || m.ContextLimit != 128000 {
		t.Fatalf("bad decode: %+v", m)
	}
	old, ok := c.Lookup("ollama-cloud/deepseek-v4.1-flash")
	if !ok || old.OutputMicro != 550000 {
		t.Fatalf("un-suffixed release must be a separate entry: %+v %v", old, ok)
	}
	opus, _ := c.Lookup("anthropic/claude-opus-5-5")
	if opus.CacheReadMicro != 500000 || len(opus.ReasoningOptions) == 0 {
		t.Fatalf("cache_read / reasoning_options not decoded: %+v", opus)
	}
	if free, _ := c.Lookup("ollama-cloud/free-preview"); free.HasCost {
		t.Fatal("model without cost must not be marked priced")
	}
	if _, ok := c.Lookup("ollama-cloud/nope"); ok {
		t.Fatal("unexpected hit")
	}
	if _, ok := c.Lookup("noslash"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestAgentCapable(t *testing.T) {
	base := Model{ToolCall: true, Reasoning: true, ContextLimit: 100000, OutputModalities: []string{"text"}}
	if !AgentCapable(base) {
		t.Fatal("base must be capable (context bound is inclusive)")
	}
	cases := map[string]func(*Model){
		"no tool_call":  func(m *Model) { m.ToolCall = false },
		"no reasoning":  func(m *Model) { m.Reasoning = false },
		"small context": func(m *Model) { m.ContextLimit = 99999 },
		"image output":  func(m *Model) { m.OutputModalities = []string{"text", "image"} },
		"no modalities": func(m *Model) { m.OutputModalities = nil },
		"audio only":    func(m *Model) { m.OutputModalities = []string{"audio"} },
	}
	for name, mut := range cases {
		m := base
		m.OutputModalities = slices.Clone(base.OutputModalities)
		mut(&m)
		if AgentCapable(m) {
			t.Errorf("%s: must not be agent capable", name)
		}
	}
}

func TestBlended(t *testing.T) {
	if got := Blended(Model{InputMicro: 150000, OutputMicro: 600000}); got != 195000 {
		t.Fatalf("blended = %d, want 195000", got)
	}
	// Integer arithmetic floors.
	if got := Blended(Model{InputMicro: 1, OutputMicro: 0}); got != 0 {
		t.Fatalf("blended = %d, want 0", got)
	}
	// The 90/10 ratio changes the ranking: a 50/50 blend would order these the other way.
	a := Blended(Model{InputMicro: 1000000, OutputMicro: 10000000}) // 1.9M
	b := Blended(Model{InputMicro: 2000000, OutputMicro: 2000000})  // 2.0M
	if a >= b {
		t.Fatalf("90/10 ranking wrong: %d vs %d", a, b)
	}
}

func TestMedianAgentCapableOnly(t *testing.T) {
	c, _ := loadFixture(t)
	med, n := ProviderMedian(c.Providers["ollama-cloud"])
	// embed-small (not agent capable) and free-preview (unpriced) are excluded.
	// Remaining: 185000 190000 195000 1700000 4200000.
	if n != 5 || med != 195000 {
		t.Fatalf("median = %d n = %d, want 195000 5", med, n)
	}
	med, n = ProviderMedian(c.Providers["openai"])
	if n != 3 || med != 2800000 {
		t.Fatalf("openai median = %d n = %d", med, n)
	}
	if med, n = ProviderMedian(Provider{}); med != 0 || n != 0 {
		t.Fatalf("empty provider = %d %d", med, n)
	}
}

func TestMedianEvenCountFloors(t *testing.T) {
	mk := func(in int64) Model {
		return Model{ToolCall: true, Reasoning: true, ContextLimit: 200000, OutputModalities: []string{"text"}, HasCost: true, InputMicro: in}
	}
	p := Provider{Models: map[string]Model{"a": mk(10), "b": mk(21), "c": mk(100), "d": mk(1000)}}
	// blended of an input-only model is 9*in/10: 9, 18, 90, 900 -> (18+90)/2 = 54
	if med, n := ProviderMedian(p); med != 54 || n != 4 {
		t.Fatalf("median = %d n = %d, want 54 4", med, n)
	}
}

func TestSnapshot(t *testing.T) {
	c, raw := loadFixture(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := NewSnapshot(c, "https://example.test/api.json", Digest(raw), now)
	if s.CapturedAt != "2026-10-19T12:00:00Z" || s.Blend != BlendLabel || s.Population != PopulationAgentCapable {
		t.Fatalf("header: %+v", s)
	}
	if s.SourceSHA256 != Digest(raw) {
		t.Fatal("digest not recorded")
	}
	ids := map[string]SnapshotModel{}
	for _, m := range s.Models {
		ids[m.ID] = m
	}
	if _, ok := ids["openai/gpt-image-9"]; ok {
		t.Fatal("non-agent-capable model in snapshot")
	}
	if m := ids["ollama-cloud/deepseek-v4.1-flash:1017"]; m.BlendedMicroUSD != 195000 || !m.Priced {
		t.Fatalf("bad model entry: %+v", m)
	}
	if m := ids["ollama-cloud/free-preview"]; m.Priced || m.BlendedMicroUSD != 0 {
		t.Fatalf("unpriced entry: %+v", m)
	}
	var got []string
	for _, p := range s.Providers {
		got = append(got, p.ID)
	}
	if !slices.IsSorted(got) || len(got) != 3 {
		t.Fatalf("providers: %v", got)
	}
	if !slices.IsSortedFunc(s.Models, func(a, b SnapshotModel) int { return strings.Compare(a.ID, b.ID) }) {
		t.Fatal("models not sorted")
	}
	// Deterministic and round-trips.
	b1, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(NewSnapshot(c, "https://example.test/api.json", Digest(raw), now))
	if string(b1) != string(b2) {
		t.Fatal("snapshot not deterministic")
	}
	back, err := ReadSnapshot(b1)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if back.Providers[0] != s.Providers[0] || len(back.Models) != len(s.Models) {
		t.Fatal("round trip changed the snapshot")
	}
	// A float anywhere is refused.
	if _, err := ReadSnapshot([]byte(`{"captured_at":"2026-10-19T12:00:00Z","models":[{"id":"a/b","blended_micro_usd":1.5}]}`)); err == nil {
		t.Fatal("float price accepted")
	}
	if _, err := ReadSnapshot([]byte(`{"captured_at":"yesterday"}`)); err == nil {
		t.Fatal("bad captured_at accepted")
	}
	if _, ok := s.Lookup("ollama-cloud/glm-5.3"); !ok {
		t.Fatal("snapshot lookup failed")
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	fresh := Snapshot{CapturedAt: now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)}
	old := Snapshot{CapturedAt: now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)}
	if stale, err := fresh.Stale(now); err != nil || stale {
		t.Fatalf("fresh: %v %v", stale, err)
	}
	if stale, err := old.Stale(now); err != nil || !stale {
		t.Fatalf("old: %v %v", stale, err)
	}
	if _, err := (Snapshot{CapturedAt: "x"}).Stale(now); err == nil {
		t.Fatal("bad time accepted")
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, in := range []string{
		``, `[]`, `{"p":{"models":{"m":{"cost":{"input":-1}}}}}`, `{"p":{"models":{"m":{"cost":{"input":1e3}}}}}`,
		`{"p":{"models":{"m":{"limit":{"context":1.5}}}}}`, `{"p":{"models":{"m":{"tool_call":"yes"}}}}`, `{} {}`,
	} {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("%.40q: expected error", in)
		}
	}
}

func TestCapabilitiesEmbedded(t *testing.T) {
	caps, err := LoadCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	// 10 curated seats plus 6 OpenRouter mirrors of the frontier seats. The mirrors
	// exist so a host whose only API keys are Ollama Cloud and OpenRouter can still
	// route the OpenAI and Anthropic families; they carry the same family and ranks.
	if len(caps.Models) != 16 {
		t.Fatalf("models = %d", len(caps.Models))
	}
	for _, id := range []string{"openrouter/openai/gpt-6.1-sol", "openrouter/anthropic/claude-sonnet-5.5"} {
		var ok bool
		for _, m := range caps.Models {
			if m.ID == id && m.Provider == "openrouter" {
				ok = true
			}
		}
		if !ok {
			t.Errorf("expected OpenRouter mirror %s", id)
		}
	}
	var found bool
	for _, m := range caps.Models {
		if m.ID == "ollama-cloud/deepseek-v4.1-flash:1017" {
			found = true
			for _, want := range []string{"42.3", "31.2", "29.1", "20.3", "77.0", "74.2", "72.1", "64.6", "Terminal-Bench", "ProgramBench", "DeepSWE", "Apex"} {
				if !strings.Contains(m.Evidence, want) {
					t.Errorf("evidence missing %q", want)
				}
			}
		}
		if strings.HasSuffix(m.ID, "deepseek-v4.1-flash") {
			t.Error("un-suffixed deepseek id present")
		}
	}
	if !found {
		t.Fatal("deepseek :1017 missing")
	}
	if _, err := ParseCapabilities([]byte(`{"models":[{"id":"a/b","bogus":1}]}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := ParseCapabilities([]byte(`{"models":[{"id":"a/b","fixture_price":1.5}]}`)); err == nil {
		t.Fatal("float accepted")
	}
}

func TestFetch(t *testing.T) {
	body := []byte(`{"p":{"models":{}}}`)
	var redirected bool
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("Accept-Encoding must not be set by the client: %q", r.Header.Get("Accept-Encoding"))
		}
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) { redirected = true })
	mux.HandleFunc("/500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom secret-body", 500) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, MaxCatalogBytes+10))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()

	got, sum, err := Fetch(ctx, srv.URL+"/ok")
	if err != nil || string(got) != string(body) || sum != Digest(body) {
		t.Fatalf("Fetch: %q %s %v", got, sum, err)
	}
	if _, _, err := Fetch(ctx, srv.URL+"/redir"); err == nil {
		t.Fatal("redirect must not be followed")
	}
	_ = redirected
	_, _, err = Fetch(ctx, srv.URL+"/500")
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("500: %v (body must not leak)", err)
	}
	if _, _, err := Fetch(ctx, srv.URL+"/big"); err == nil {
		t.Fatal("oversize body accepted")
	}
	if _, _, err := Fetch(ctx, "file:///etc/passwd"); err == nil {
		t.Fatal("non-http scheme accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := Fetch(cctx, srv.URL+"/ok"); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestVerifyDigest(t *testing.T) {
	d := Digest([]byte("x"))
	if err := VerifyDigest(d, strings.ToUpper(d)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDigest(d, Digest([]byte("y"))); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v", err)
	}
	if err := VerifyDigest(d, ""); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("empty pin accepted: %v", err)
	}
}

func FuzzDecodeCatalog(f *testing.F) {
	_, raw := loadFixture(f)
	f.Add(raw)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"p":{"models":{"m":{"cost":{"input":0.15,"output":1e2},"limit":{"context":1.5}}}}}`))
	f.Add([]byte(`{"p":{"id":"q","models":{"m":{"modalities":{"output":["text"]},"tool_call":true,"reasoning":true,"limit":{"context":100000},"cost":{"input":1,"output":2}}}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Decode(data)
		if err != nil {
			return
		}
		s := NewSnapshot(c, "u", Digest(data), time.Unix(0, 0).UTC())
		for _, m := range s.Models {
			if m.BlendedMicroUSD < 0 {
				t.Fatalf("negative price: %+v", m)
			}
		}
		for _, p := range s.Providers {
			if p.MedianMicroUSD < 0 || p.N < 0 {
				t.Fatalf("bad provider stat: %+v", p)
			}
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	})
}
