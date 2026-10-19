package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureKey is the API key every test plants in a 0600 key file. It is a
// fake, and no output may ever contain it.
const fixtureKey = "test-key-3f81c2-never-print"

// testConfig returns a Config aimed at a fake systemone server built from
// the handler, with the standard test thresholds.
func testConfig(t *testing.T, handler http.HandlerFunc) Config {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return Config{
		Backend:         BackendOllama,
		BaseURL:         srv.URL,
		Model:           "clef-flash",
		Timeout:         2 * time.Second,
		ConfidenceFloor: 0.70,
		Margin:          0.15,
	}
}

// writeKeyFile writes a key to a 0600 file and returns its path.
func writeKeyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "decision.key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requestWire is the systemone request as the backend sees it.
type requestWire struct {
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Questions map[string]questionWire `json:"questions"`
}

// questionWire is one question in requestWire.
type questionWire struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// systemoneSpy records what a fake systemone server saw.
type systemoneSpy struct {
	mu   sync.Mutex
	auth string
	body []byte
}

// record stores one observed request.
func (s *systemoneSpy) record(auth string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = auth
	s.body = body
}

// seenAuth returns the last observed Authorization header.
func (s *systemoneSpy) seenAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

// seenBody returns the last observed raw request body.
func (s *systemoneSpy) seenBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.body)
}

// answersHandler replies to any systemone request with the given answers,
// echoing the requested model.
func (s *systemoneSpy) answersHandler(t *testing.T, answers map[string]*answerBody) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake systemone: read request: %v", err)
			return
		}
		var req requestWire
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("fake systemone: decode request: %v", err)
		}
		s.record(r.Header.Get("Authorization"), body)
		out, err := json.Marshal(responseBody{Model: req.Model, Answers: answers})
		if err != nil {
			t.Errorf("fake systemone: encode reply: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}
}

// acceptedAnswers is a high-confidence fixture answer set: change type
// bugfix, size M, auth and concurrency flags above the floor, everything
// else below it.
func acceptedAnswers() map[string]*answerBody {
	return map[string]*answerBody{
		qChangeType: {
			Type:       kindChoice,
			Choice:     "bugfix",
			Confidence: 0.95,
			Probabilities: []probPair{
				{"bugfix", 0.93}, {"feature", 0.03}, {"docs", 0.01},
				{"test_only", 0.01}, {"scaffold", 0.01}, {"refactor", 0.005},
				{"migration", 0.005},
			},
		},
		qSize: {
			Type:       kindChoice,
			Choice:     "M",
			Confidence: 0.90,
			Probabilities: []probPair{
				{"M", 0.88}, {"S", 0.07}, {"L", 0.05},
			},
		},
		qSchemaRisk:      {Type: kindNoul, Probability: 0.02, Confidence: 0.90},
		qBillingRisk:     {Type: kindNoul, Probability: 0.01, Confidence: 0.90},
		qAuthRisk:        {Type: kindNoul, Probability: 0.92, Confidence: 0.90},
		qPrivacyRisk:     {Type: kindNoul, Probability: 0.03, Confidence: 0.90},
		qConcurrencyRisk: {Type: kindNoul, Probability: 0.85, Confidence: 0.90},
		qNeedsScout:      {Type: kindNoul, Probability: 0.05, Confidence: 0.90},
	}
}

// Test 1: a high-confidence answer is accepted and sets all four inputs.
func TestClassifyAcceptedSetsAllFourInputs(t *testing.T) {
	const state = "fix the retry loop in the uploader"
	spy := &systemoneSpy{}
	cfg := testConfig(t, spy.answersHandler(t, acceptedAnswers()))
	cfg.KeyFile = writeKeyFile(t, fixtureKey)

	res, err := cfg.Classify(context.Background(), state)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Fallback {
		t.Fatalf("high-confidence answer must be accepted, got fallback %q", res.FallbackReason)
	}
	if res.Type != "bugfix" || res.Size != "M" {
		t.Errorf("type/size = %q/%q; want bugfix/M", res.Type, res.Size)
	}
	if want := []string{"auth", "concurrency"}; !slices.Equal(res.RiskFlags, want) {
		t.Errorf("risk flags = %v; want %v", res.RiskFlags, want)
	}
	if res.Scout {
		t.Error("needs_scout at 0.05 is below the floor; scout must be false")
	}
	if res.Confidence != 0.95 {
		t.Errorf("confidence = %v; want 0.95", res.Confidence)
	}
	if res.Backend != BackendOllama || res.DecisionModel != "clef-flash" {
		t.Errorf("backend/model = %q/%q; want ollama/clef-flash", res.Backend, res.DecisionModel)
	}
	if res.LatencyMS < 0 {
		t.Errorf("latency = %d; want >= 0", res.LatencyMS)
	}

	// The request carried the fixed question set, the state, and the key.
	var req requestWire
	if err := json.Unmarshal([]byte(spy.seenBody()), &req); err != nil {
		t.Fatalf("decode observed request: %v", err)
	}
	if req.Model != "clef-flash" || req.State != state {
		t.Errorf("model/state = %q/%q; want clef-flash/%q", req.Model, req.State, state)
	}
	if len(req.Questions) != 8 {
		t.Fatalf("questions = %d; want 8", len(req.Questions))
	}
	ct := req.Questions[qChangeType]
	if ct.Type != kindChoice || len(ct.Criteria) != 7 ||
		ct.Criteria["migration"] != "schema or data migration" ||
		ct.Instructions != "Which change type is this task?" {
		t.Errorf("change_type question = %+v", ct)
	}
	sz := req.Questions[qSize]
	if sz.Type != kindChoice || len(sz.Criteria) != 3 || sz.Criteria["L"] != "many files or several concerns" {
		t.Errorf("size question = %+v", sz)
	}
	for _, name := range []string{qSchemaRisk, qBillingRisk, qAuthRisk, qPrivacyRisk, qConcurrencyRisk, qNeedsScout} {
		if q := req.Questions[name]; q.Type != kindNoul || q.Instructions == "" {
			t.Errorf("%s question = %+v; want a noul with instructions", name, q)
		}
	}
	if auth := spy.seenAuth(); auth != "Bearer "+fixtureKey {
		t.Errorf("Authorization = %q; want the fixture key", auth)
	}

	// The criteria are offered in the documented order, so the backend never
	// sees a set the caller did not declare, in any order it likes.
	raw := spy.seenBody()
	last := -1
	for _, pair := range []string{
		`"docs":"documentation only"`,
		`"test_only":"tests only"`,
		`"scaffold":"new project or module skeleton"`,
		`"bugfix":"repair existing behaviour"`,
		`"feature":"new behaviour"`,
		`"refactor":"restructure without behaviour change"`,
		`"migration":"schema or data migration"`,
	} {
		i := strings.Index(raw, pair)
		if i < 0 {
			t.Fatalf("request missing criterion %s", pair)
		}
		if i <= last {
			t.Errorf("criteria out of offer order at %s", pair)
		}
		last = i
	}
}

// Test 2: confidence below the floor is a low_confidence fallback.
func TestClassifyLowConfidenceFallsBack(t *testing.T) {
	answers := acceptedAnswers()
	answers[qChangeType].Confidence = 0.50 // below the 0.70 floor
	cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, answers))

	res, err := cfg.Classify(context.Background(), "state")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !res.Fallback || res.FallbackReason != ReasonLowConfidence {
		t.Fatalf("result = %+v; want low_confidence fallback", res)
	}
	if res.Type != "" || res.Size != "" || res.RiskFlags != nil || res.Scout {
		t.Errorf("a rejected answer must not be half-applied: %+v", res)
	}
}

// Test 3: top and runner-up within the margin is a low_margin fallback.
func TestClassifyLowMarginFallsBack(t *testing.T) {
	answers := acceptedAnswers()
	answers[qChangeType].Confidence = 0.95 // at the floor
	answers[qChangeType].Probabilities = []probPair{
		{"bugfix", 0.52}, {"feature", 0.48}, // margin 0.04 < 0.15
	}
	cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, answers))

	res, err := cfg.Classify(context.Background(), "state")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !res.Fallback || res.FallbackReason != ReasonLowMargin {
		t.Fatalf("result = %+v; want low_margin fallback", res)
	}
	if res.Type != "" {
		t.Errorf("a near tie must never be silently picked, got type %q", res.Type)
	}
}

// Test 5: a missing answer is a missing_answer fallback.
func TestClassifyMissingAnswerFallsBack(t *testing.T) {
	for _, name := range []string{qChangeType, qSize, qNeedsScout} {
		t.Run(name, func(t *testing.T) {
			answers := acceptedAnswers()
			delete(answers, name)
			cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, answers))

			res, err := cfg.Classify(context.Background(), "state")
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if !res.Fallback || res.FallbackReason != ReasonMissingAnswer {
				t.Fatalf("result = %+v; want missing_answer fallback", res)
			}
			if res.Type != "" || res.Size != "" || res.RiskFlags != nil || res.Scout {
				t.Errorf("a rejected answer must not be half-applied: %+v", res)
			}
		})
	}
}

// Test 6: an answer of the wrong type for its question is a missing_answer
// fallback, not an acceptance.
func TestClassifyWrongAnswerTypeFallsBack(t *testing.T) {
	cases := map[string]*answerBody{
		"noul for a choice": {Type: kindNoul, Probability: 0.90, Confidence: 0.90},
		"no type at all":    {Choice: "bugfix", Confidence: 0.90},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			answers := acceptedAnswers()
			answers[qChangeType] = bad
			cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, answers))

			res, err := cfg.Classify(context.Background(), "state")
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if !res.Fallback || res.FallbackReason != ReasonMissingAnswer {
				t.Fatalf("result = %+v; want missing_answer fallback", res)
			}
		})
	}
}

// Test 7: a server that sleeps past the deadline produces a timeout
// fallback, never an error and never a half-applied answer.
func TestClassifyTimeoutIsFallbackNotError(t *testing.T) {
	cfg := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
		// The fake upstream is slower than the deadline. The wait is bounded
		// because the server cannot see the client cancel while the request
		// body is unconsumed (server.go: no background read before EOF).
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	})
	cfg.Timeout = 50 * time.Millisecond

	res, err := cfg.Classify(context.Background(), "state")
	if err != nil {
		t.Fatalf("a timeout is a fallback, not an error: %v", err)
	}
	if !res.Fallback || res.FallbackReason != ReasonTimeout {
		t.Fatalf("result = %+v; want timeout fallback", res)
	}
	if res.Type != "" || res.Size != "" || res.RiskFlags != nil || res.Scout {
		t.Errorf("a rejected answer must not be half-applied: %+v", res)
	}
	if res.LatencyMS < 50 {
		t.Errorf("latency = %dms; want at least the 50ms deadline window", res.LatencyMS)
	}
}

// Test 8: a server that closes the connection produces a transport fallback.
func TestClassifyTransportErrorIsFallback(t *testing.T) {
	t.Run("aborted connection", func(t *testing.T) {
		cfg := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
			// Closes the connection without answering. ErrAbortHandler is
			// the supported way; the server recovers it quietly.
			panic(http.ErrAbortHandler)
		})
		res, err := cfg.Classify(context.Background(), "state")
		if err != nil {
			t.Fatalf("a transport failure is a fallback, not an error: %v", err)
		}
		if !res.Fallback || res.FallbackReason != ReasonTransport {
			t.Fatalf("result = %+v; want transport fallback", res)
		}
	})
	t.Run("refused connection", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		base := srv.URL
		srv.Close()
		cfg := Config{Backend: BackendOllama, BaseURL: base, Model: "clef-flash"}
		res, err := cfg.Classify(context.Background(), "state")
		if err != nil {
			t.Fatalf("a transport failure is a fallback, not an error: %v", err)
		}
		if !res.Fallback || res.FallbackReason != ReasonTransport {
			t.Fatalf("result = %+v; want transport fallback", res)
		}
	})
}

// Test 9: a flag probability below the floor leaves the flag absent while
// the answer stays accepted.
func TestClassifyFlagBelowFloorIsAbsentNotGuessed(t *testing.T) {
	answers := acceptedAnswers()
	answers[qAuthRisk].Probability = 0.55 // above zero, below the 0.70 floor
	cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, answers))

	res, err := cfg.Classify(context.Background(), "state")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Fallback {
		t.Fatalf("a flag below the floor is absent, not a fallback: %+v", res)
	}
	if want := []string{"concurrency"}; !slices.Equal(res.RiskFlags, want) {
		t.Errorf("risk flags = %v; want %v: the low flag must be absent, never guessed", res.RiskFlags, want)
	}
	if res.Type != "bugfix" {
		t.Errorf("type = %q; want bugfix", res.Type)
	}
}

// captureStderr returns everything fn writes to os.Stderr while it runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Test 12: the key value never appears in any output, log line or error
// string, even though the backend received it.
func TestKeyNeverAppearsInOutputOrError(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		spy := &systemoneSpy{}
		cfg := testConfig(t, spy.answersHandler(t, acceptedAnswers()))
		cfg.KeyFile = writeKeyFile(t, fixtureKey)

		var res Result
		var err error
		stderr := captureStderr(t, func() {
			res, err = cfg.Classify(context.Background(), "state")
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if got := spy.seenAuth(); got != "Bearer "+fixtureKey {
			t.Fatalf("Authorization = %q; want the fixture key, or the test proves nothing", got)
		}
		blob, mErr := json.Marshal(res)
		if mErr != nil {
			t.Fatal(mErr)
		}
		haystack := stderr + "\n" + fmt.Sprintf("%+v", res) + "\n" + string(blob)
		if strings.Contains(haystack, fixtureKey) {
			t.Fatalf("key leaked into output:\n%s", haystack)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		cfg := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
		})
		cfg.KeyFile = writeKeyFile(t, fixtureKey)
		cfg.Timeout = 100 * time.Millisecond

		var res Result
		var err error
		stderr := captureStderr(t, func() {
			res, err = cfg.Classify(context.Background(), "state")
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		haystack := stderr + "\n" + fmt.Sprintf("%+v", res) + "\n" + res.FallbackReason
		if strings.Contains(haystack, fixtureKey) {
			t.Fatalf("key leaked into output:\n%s", haystack)
		}
	})
	t.Run("transport", func(t *testing.T) {
		cfg := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
			panic(http.ErrAbortHandler)
		})
		cfg.KeyFile = writeKeyFile(t, fixtureKey)

		var res Result
		var err error
		stderr := captureStderr(t, func() {
			res, err = cfg.Classify(context.Background(), "state")
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		haystack := stderr + "\n" + fmt.Sprintf("%+v", res) + "\n" + res.FallbackReason
		if strings.Contains(haystack, fixtureKey) {
			t.Fatalf("key leaked into output:\n%s", haystack)
		}
	})
	t.Run("server error", func(t *testing.T) {
		cfg := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		cfg.KeyFile = writeKeyFile(t, fixtureKey)

		var res Result
		var err error
		stderr := captureStderr(t, func() {
			res, err = cfg.Classify(context.Background(), "state")
		})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if !res.Fallback || res.FallbackReason != ReasonTransport {
			t.Fatalf("result = %+v; want transport fallback", res)
		}
		haystack := stderr + "\n" + fmt.Sprintf("%+v", res) + "\n" + res.FallbackReason
		if strings.Contains(haystack, fixtureKey) {
			t.Fatalf("key leaked into output:\n%s", haystack)
		}
	})
	t.Run("bad key file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "decision.key")
		if err := os.WriteFile(path, []byte(fixtureKey+"\n"), 0o644); err != nil { // wrong mode on purpose
			t.Fatal(err)
		}
		cfg := testConfig(t, (&systemoneSpy{}).answersHandler(t, acceptedAnswers()))
		cfg.KeyFile = path

		var err error
		stderr := captureStderr(t, func() {
			_, err = cfg.Classify(context.Background(), "state")
		})
		if err == nil {
			t.Fatal("a key file with the wrong mode must be a configuration error")
		}
		if strings.Contains(stderr+"\n"+err.Error(), fixtureKey) {
			t.Fatalf("key leaked into error: %v", err)
		}
	})
}

// Test 13: the Result JSON has no seat, strategy or routed-model field.
func TestResultJSONHasNoSeatStrategyOrRoutedModel(t *testing.T) {
	res := Result{
		Type: "bugfix", Size: "M", RiskFlags: []string{"auth"}, Scout: false,
		Confidence: 0.9, Backend: BackendOllama, DecisionModel: "clef-flash",
		LatencyMS: 3, Fallback: false, FallbackReason: "",
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for key := range m {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "seat") || strings.Contains(lower, "strategy") {
			t.Errorf("Result JSON key %q names a seat or a strategy", key)
		}
		if key == "model" || key == "model_id" || key == "routed_model" {
			t.Errorf("Result JSON key %q carries a routed model id", key)
		}
	}
	// The answering model is required and is not a routing seat.
	if _, ok := m["decision_model"]; !ok {
		t.Error("Result JSON must carry decision_model, the model that answered")
	}
	// Exactly the documented keys, nothing else.
	got := make([]string, 0, len(m))
	for key := range m {
		got = append(got, key)
	}
	slices.Sort(got)
	want := []string{
		"backend", "confidence", "decision_model", "fallback", "fallback_reason",
		"latency_ms", "risk_flags", "scout", "size", "type",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Result JSON keys = %v; want %v", got, want)
	}
	// Field names too, in case the tags change.
	rt := reflect.TypeOf(Result{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(name, "seat") || strings.Contains(name, "strategy") {
			t.Errorf("Result field %s names a seat or a strategy", rt.Field(i).Name)
		}
	}
}
