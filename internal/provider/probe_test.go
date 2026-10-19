package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const probeKey = "sk-probe-SECRET-0123456789"

// fakeUpstream is a per-dialect fake. Each knob breaks one probe step.
type fakeUpstream struct {
	mu           sync.Mutex
	dialect      Dialect
	rejectKey    bool
	noCompletion bool
	noUsage      bool
	badTool      bool
	echoKeyInErr bool
	seen         []string // method + path
	streamOpts   bool     // a stream request carried stream_options.include_usage
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		got := r.Header.Get("Authorization")
		if f.dialect == DialectAnthropic {
			got = "Bearer " + r.Header.Get("X-Api-Key")
		}
		if f.rejectKey || got != "Bearer "+probeKey {
			w.WriteHeader(http.StatusUnauthorized)
			if f.echoKeyInErr {
				_, _ = io.WriteString(w, `{"error":"bad key `+got+`"}`)
			}
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		if f.noCompletion {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"`+got+`"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		switch {
		case req["stream"] == true:
			f.stream(w, req)
		case req["tools"] != nil:
			f.tool(w)
		default:
			if f.dialect == DialectAnthropic {
				_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
			}
		}
	})
}

func (f *fakeUpstream) stream(w http.ResponseWriter, req map[string]any) {
	if so, ok := req["stream_options"].(map[string]any); ok && so["include_usage"] == true {
		f.mu.Lock()
		f.streamOpts = true
		f.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if f.dialect == DialectAnthropic {
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n")
		return
	}
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
	if !f.noUsage && f.streamOpts {
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (f *fakeUpstream) tool(w http.ResponseWriter) {
	switch {
	case f.badTool:
		if f.dialect == DialectAnthropic {
			_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"I will not call it"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"no call","tool_calls":[{"type":"function","function":{"name":"record_result","arguments":"{not json"}}]}}]}`)
		}
	case f.dialect == DialectAnthropic:
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","name":"record_result","input":{"value":7}}]}`)
	default:
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"record_result","arguments":"{\"value\":7}"}}]}}]}`)
	}
}

func target(url string, d Dialect) ProbeTarget {
	return ProbeTarget{ID: "fake", Dialect: d, BaseURL: url, Key: probeKey, Model: "m", IncludeStreamUsage: true}
}

func TestProbePassesAllSteps(t *testing.T) {
	for _, d := range []Dialect{DialectOpenAI, DialectOllama, DialectOpenRouter, DialectAnthropic} {
		t.Run(string(d), func(t *testing.T) {
			f := &fakeUpstream{dialect: d}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			res := Probe(context.Background(), target(srv.URL, d))
			if !res.Auth.OK() || !res.Inference.OK() || !res.StreamUsage.OK() || !res.ToolCall.OK() || res.ToolGrade != GradePass {
				t.Fatalf("%+v", res)
			}
			wantPrefix := "/v1"
			if d == DialectOpenRouter {
				wantPrefix = "/api/v1"
			}
			if f.seen[0] != "GET "+wantPrefix+"/models" {
				t.Fatalf("first request %q", f.seen[0])
			}
			if d != DialectAnthropic && !f.streamOpts {
				t.Fatal("stream probe did not exercise stream_options injection")
			}
		})
	}
}

func TestProbePreservesBaseURLPrefix(t *testing.T) {
	f := &fakeUpstream{dialect: DialectOpenAI}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	res := Probe(context.Background(), target(srv.URL+"/%61pi", DialectOpenAI))
	if !res.Usable() || !res.StreamUsage.OK() || res.ToolGrade != GradePass {
		t.Fatalf("probe with a prefix failed: %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, got := range f.seen {
		want := "POST /api/v1/chat/completions"
		if i == 0 {
			want = "GET /api/v1/models"
		}
		if got != want {
			t.Errorf("request %d = %q, want %q", i, got, want)
		}
	}
}

func TestProbeFailureModesNeverLeakTheKey(t *testing.T) {
	cases := []struct {
		name      string
		f         *fakeUpstream
		auth      bool
		inference bool
		grade     string
	}{
		{"rejected key", &fakeUpstream{rejectKey: true, echoKeyInErr: true}, false, false, ""},
		{"no completion", &fakeUpstream{noCompletion: true}, true, false, ""},
		{"no stream usage", &fakeUpstream{noUsage: true}, true, true, GradePass},
		{"malformed tool call", &fakeUpstream{badTool: true}, true, true, GradeFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.f.dialect = DialectOpenAI
			srv := httptest.NewServer(c.f.handler())
			defer srv.Close()
			res := Probe(context.Background(), target(srv.URL, DialectOpenAI))
			if res.Auth.OK() != c.auth || res.Inference.OK() != c.inference || res.ToolGrade != c.grade {
				t.Fatalf("%+v", res)
			}
			blob, _ := json.Marshal(res)
			if strings.Contains(string(blob), probeKey) || strings.Contains(string(blob), "SECRET") {
				t.Fatalf("result carries the key: %s", blob)
			}
			if c.name == "no stream usage" && res.StreamUsage.OK() {
				t.Fatal("stream without usage passed")
			}
			if !c.auth && res.Inference.Result != ProbeSkipped {
				t.Fatalf("later step ran after auth failed: %+v", res)
			}
		})
	}
}

func TestProbeRefusesBadEndpointAndRedactsTransportErrors(t *testing.T) {
	res := Probe(context.Background(), target("http://example.com", DialectOpenAI))
	if res.Auth.OK() || !strings.Contains(res.Auth.Detail, "127.0.0.1") {
		t.Fatalf("plain http to a remote host must be refused: %+v", res)
	}
	// A closed port: the transport error text must not carry the key.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	res = Probe(context.Background(), target(url, DialectOpenAI))
	if res.Auth.OK() || strings.Contains(res.Auth.Detail, probeKey) {
		t.Fatalf("%+v", res)
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		t.Errorf("redirect was followed to %s", r.URL.Path)
	}))
	defer srv.Close()
	if res := Probe(context.Background(), target(srv.URL, DialectOpenAI)); res.Auth.OK() {
		t.Fatalf("a redirect must not authenticate: %+v", res)
	}
	if hits != 1 {
		t.Fatalf("hits = %d", hits)
	}
}
