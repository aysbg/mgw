package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Fallback reason codes. Every rejected answer carries exactly one of these,
// stable for logs and reports.
const (
	ReasonMissingAnswer = "missing_answer" // a requested answer was absent or mistyped
	ReasonBadLabel      = "bad_label"      // a label the caller did not offer
	ReasonLowConfidence = "low_confidence" // choice confidence below the floor
	ReasonLowMargin     = "low_margin"     // top probability not far enough above the runner-up
	ReasonTimeout       = "timeout"        // the call hit its hard deadline
	ReasonTransport     = "transport"      // the call failed at the transport level
	ReasonBadResponse   = "bad_response"   // the reply could not be decoded as systemone JSON
)

// Backend names understood by Config.
const (
	BackendOllama = "ollama"
	BackendJev    = "jev"
)

// Config is one decision backend setup. Zero fields take the defaults below.
// Config carries no strategy and no seat: it can only ask a backend to fill
// the four complexity inputs.
type Config struct {
	Backend         string        // "ollama" | "jev"
	BaseURL         string        // e.g. https://ollama.com
	Model           string        // e.g. clef-flash: the model asked to answer
	KeyFile         string        // 0600, one line; empty means no key
	Timeout         time.Duration // default 2s, hard deadline on the whole call
	ConfidenceFloor float64       // default 0.70
	Margin          float64       // default 0.15
}

// Result is the outcome of one classification. Type, Size, RiskFlags and
// Scout are the four router complexity inputs and are set only when the gate
// accepts the answer; on Fallback they are zero and the caller's own
// complexity is used unchanged. Result carries no seat and no strategy. The
// model field names the model that answered, which is not a routing seat and
// carries no strategy.
type Result struct {
	Type           string   `json:"type"`
	Size           string   `json:"size"`
	RiskFlags      []string `json:"risk_flags"`
	Scout          bool     `json:"scout"`
	Confidence     float64  `json:"confidence"`
	Backend        string   `json:"backend"`
	DecisionModel  string   `json:"decision_model"`
	LatencyMS      int64    `json:"latency_ms"`
	Fallback       bool     `json:"fallback"`
	FallbackReason string   `json:"fallback_reason"`
}

// Question kinds on the systemone wire.
const (
	kindChoice = "choice"
	kindNoul   = "noul"
)

// Question names on the systemone wire.
const (
	qChangeType      = "change_type"
	qSize            = "size"
	qSchemaRisk      = "schema_risk"
	qBillingRisk     = "billing_risk"
	qAuthRisk        = "auth_risk"
	qPrivacyRisk     = "privacy_risk"
	qConcurrencyRisk = "concurrency_risk"
	qNeedsScout      = "needs_scout"
)

// criterion is one offered label with the description shown to the backend.
type criterion struct {
	Label       string
	Description string
}

// changeTypeCriteria are the seven change types in offer order.
var changeTypeCriteria = []criterion{
	{"docs", "documentation only"},
	{"test_only", "tests only"},
	{"scaffold", "new project or module skeleton"},
	{"bugfix", "repair existing behaviour"},
	{"feature", "new behaviour"},
	{"refactor", "restructure without behaviour change"},
	{"migration", "schema or data migration"},
}

// sizeCriteria are the offered size labels in offer order.
var sizeCriteria = []criterion{
	{"S", "one file or a few lines"},
	{"M", "several files, one concern"},
	{"L", "many files or several concerns"},
}

// canonicalChangeTypes and canonicalSizes are the labels the gate asserts
// against after parsing, whatever order they were offered in.
var (
	canonicalChangeTypes = labels(changeTypeCriteria)
	canonicalSizes       = labels(sizeCriteria)
)

// canonicalFlags are the five known risk flag names. They are derived from
// the canonical question set so the gate cannot drift from it.
var canonicalFlags = func() []string {
	var out []string
	for _, q := range canonicalQuestions {
		if q.Body.Type == kindNoul && q.Name != qNeedsScout {
			out = append(out, riskFlag(q.Name))
		}
	}
	return out
}()

// questionBody is one offered question on the wire.
type questionBody struct {
	Type         string       `json:"type"`
	Instructions string       `json:"instructions"`
	Criteria     criteriaJSON `json:"criteria,omitempty"`
}

// question is one offered question: its wire name and its body.
type question struct {
	Name string
	Body questionBody
}

// canonicalQuestions is the fixed question set in wire order: two choices
// with their declared criteria, five noul risk questions and one noul scout
// question. The set never varies per call, so the backend is never offered a
// label the caller did not declare.
var canonicalQuestions = questionsJSON{
	{qChangeType, questionBody{
		Type:         kindChoice,
		Instructions: "Which change type is this task?",
		Criteria:     criteriaJSON(changeTypeCriteria),
	}},
	{qSize, questionBody{
		Type:         kindChoice,
		Instructions: "How large is the change?",
		Criteria:     criteriaJSON(sizeCriteria),
	}},
	{qSchemaRisk, questionBody{
		Type:         kindNoul,
		Instructions: "Does this change touch a persisted schema or wire format?",
	}},
	{qBillingRisk, questionBody{
		Type:         kindNoul,
		Instructions: "Does this change touch billing, entitlements or money?",
	}},
	{qAuthRisk, questionBody{
		Type:         kindNoul,
		Instructions: "Does this change touch authentication, authorization or secrets?",
	}},
	{qPrivacyRisk, questionBody{
		Type:         kindNoul,
		Instructions: "Does this change touch personal data handling or logging of content?",
	}},
	{qConcurrencyRisk, questionBody{
		Type:         kindNoul,
		Instructions: "Does this change touch locking, queues, workers or ordering?",
	}},
	{qNeedsScout, questionBody{
		Type:         kindNoul,
		Instructions: "Is this a small reconnaissance task whose output is a report rather than a change?",
	}},
}

// requestBody is one systemone request.
type requestBody struct {
	Model     string        `json:"model"`
	State     string        `json:"state"`
	Questions questionsJSON `json:"questions"`
}

// responseBody is one systemone reply. Answers are keyed by question name.
type responseBody struct {
	Model   string                 `json:"model"`
	Answers map[string]*answerBody `json:"answers"`
}

// answerBody is one per-question answer. A choice declares its label and a
// probability object keyed by label; a noul declares one probability. Map
// lookups make the gate independent of wire order. Unknown reply fields,
// such as usage, are ignored.
type answerBody struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Probability   float64            `json:"probability"`
	Confidence    float64            `json:"confidence"`
}

// UnmarshalJSON normalizes the provider's noul field into Probability. The
// legacy probability field is read only when noul is absent, not when it is 0.
func (a *answerBody) UnmarshalJSON(data []byte) error {
	type plain answerBody
	var wire struct {
		plain
		Noul        json.RawMessage `json:"noul"`
		Probability json.RawMessage `json:"probability"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	probability := wire.Noul
	if len(probability) == 0 {
		probability = wire.Probability
	}
	if len(probability) != 0 {
		if err := json.Unmarshal(probability, &wire.plain.Probability); err != nil {
			return err
		}
	}
	*a = answerBody(wire.plain)
	return nil
}

// criteriaJSON marshals an ordered criteria list as a JSON object with the
// labels in offer order, matching the documented systemone shape.
type criteriaJSON []criterion

// MarshalJSON implements json.Marshaler.
func (c criteriaJSON) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, cr := range c {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(cr.Label)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(cr.Description)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// questionsJSON marshals the question set as a JSON object keyed by question
// name, in offer order.
type questionsJSON []question

// MarshalJSON implements json.Marshaler.
func (qs questionsJSON) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, q := range qs {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(q.Name)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(q.Body)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(body)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// labels returns just the labels of the criteria, in offer order.
func labels(criteria []criterion) []string {
	out := make([]string, len(criteria))
	for i, c := range criteria {
		out[i] = c.Label
	}
	return out
}

// riskFlag returns the Result.RiskFlags name for a risk question.
func riskFlag(question string) string {
	return strings.TrimSuffix(question, "_risk")
}

// maxResponseBytes caps the systemone reply before parsing.
const maxResponseBytes = 1 << 20

// upstream is the shared transport for systemone calls. The environment proxy
// is disabled so the key never transits a configured proxy, redirects are not
// followed, compression is off, and each stage carries its own deadline. The
// per-call context carries the overall deadline.
var upstream = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:    true,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Classify asks the configured systemone backend to classify one dispatch
// state into the four complexity inputs: type, size, risk flags and scout.
//
// The call carries a hard timeout. Every backend failure — timeout, transport
// error, malformed reply, or an answer the gate rejects — is reported as a
// fallback Result with a machine reason, never as an error and never as a
// half-applied answer: the caller's own complexity is used unchanged.
// Classify returns an error only for a bad configuration, such as an
// unreadable key file.
func (cfg Config) Classify(ctx context.Context, state string) (Result, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return Result{}, err
	}
	key, err := cfg.loadKey()
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(requestBody{Model: cfg.Model, State: state, Questions: canonicalQuestions})
	if err != nil {
		return Result{}, fmt.Errorf("decision: encode request: %w", err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint(), bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("decision: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return cfg.roundTrip(req, start), nil
}

// roundTrip performs one systemone call and turns every failure into a
// fallback Result. The key travels only in the Authorization header and never
// appears in any returned value.
func (cfg Config) roundTrip(req *http.Request, start time.Time) Result {
	latency := func() int64 { return time.Since(start).Milliseconds() }
	resp, err := upstream.Do(req)
	if err != nil {
		return cfg.failed(reasonFor(err), "", latency())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return cfg.failed(ReasonTransport, "", latency())
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return cfg.failed(reasonFor(err), "", latency())
	}
	var parsed responseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		return cfg.failed(ReasonBadResponse, "", latency())
	}
	model := parsed.Model
	if model == "" {
		model = cfg.Model
	}
	return applyGate(cfg, &parsed, canonicalChangeTypes, canonicalSizes, cfg.Backend, model, latency())
}

// applyGate applies the acceptance rules to a parsed systemone response, in
// the mandatory order. changeTypes and sizes are the offered criteria in
// offer order; the verdict must never depend on that order. Every failure
// returns a fallback Result with a machine reason, and the four complexity
// inputs are never half-applied.
func applyGate(cfg Config, resp *responseBody, changeTypes, sizes []string, backend, model string, latencyMS int64) Result {
	res := Result{Backend: backend, DecisionModel: model, LatencyMS: latencyMS}

	// Rule 1: every requested answer present and of the expected type.
	for _, q := range canonicalQuestions {
		a := resp.Answers[q.Name]
		if a == nil || a.Type != q.Body.Type {
			return fail(res, ReasonMissingAnswer)
		}
	}

	// Rule 2: never accept a label the caller did not offer — not as the
	// declared choice and not anywhere in the probabilities.
	ct, sz := resp.Answers[qChangeType], resp.Answers[qSize]
	if !labelsOffered(ct, changeTypes) || !labelsOffered(sz, sizes) {
		return fail(res, ReasonBadLabel)
	}

	// Rule 3: floor and margin on each choice, change_type first.
	for _, a := range []*answerBody{ct, sz} {
		if a.Confidence < cfg.ConfidenceFloor {
			return fail(res, ReasonLowConfidence)
		}
		top, runnerUp := choiceSupport(a)
		if top-runnerUp < cfg.Margin {
			return fail(res, ReasonLowMargin)
		}
	}

	// Rules 4 and 5: a noul counts only at or above the floor; below it the
	// flag is absent, never guessed. Probability is normalized from noul (or
	// the legacy probability field) during decoding.
	var flags []string
	scout := false
	for _, q := range canonicalQuestions {
		if q.Body.Type != kindNoul {
			continue
		}
		if resp.Answers[q.Name].Probability < cfg.ConfidenceFloor {
			continue
		}
		if q.Name == qNeedsScout {
			scout = true
		} else {
			flags = append(flags, riskFlag(q.Name))
		}
	}

	// Rule 6: deterministic validation of what is about to be returned.
	if !slices.Contains(canonicalChangeTypes, ct.Choice) || !slices.Contains(canonicalSizes, sz.Choice) {
		return fail(res, ReasonBadLabel)
	}
	for _, f := range flags {
		if !slices.Contains(canonicalFlags, f) {
			return fail(res, ReasonBadLabel)
		}
	}

	res.Type = ct.Choice
	res.Size = sz.Choice
	res.RiskFlags = flags
	res.Scout = scout
	res.Confidence = ct.Confidence
	return res
}

// choiceSupport returns the probability the answer assigns to its declared
// label and the best probability of any other label. The declared choice is
// looked up by label, never by position, and probabilities are never averaged.
func choiceSupport(a *answerBody) (top, runnerUp float64) {
	top = a.Probabilities[a.Choice]
	for label, p := range a.Probabilities {
		if label != a.Choice {
			runnerUp = math.Max(runnerUp, p)
		}
	}
	return top, runnerUp
}

// labelsOffered reports whether the declared choice and every probability
// label are among the offered criteria.
func labelsOffered(a *answerBody, offered []string) bool {
	if !slices.Contains(offered, a.Choice) {
		return false
	}
	for label := range a.Probabilities {
		if !slices.Contains(offered, label) {
			return false
		}
	}
	return true
}

// fail returns res as a fallback with the machine reason, dropping any
// complexity the gate may have gathered: the caller's own complexity is used
// unchanged.
func fail(res Result, reason string) Result {
	res.Type = ""
	res.Size = ""
	res.RiskFlags = nil
	res.Scout = false
	res.Confidence = 0
	res.Fallback = true
	res.FallbackReason = reason
	return res
}

// reasonFor maps a transport error to a machine reason. A deadline, ours or
// the caller's, is a timeout; everything else is transport.
func reasonFor(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	return ReasonTransport
}

// failed returns a fallback Result carrying telemetry only.
func (cfg Config) failed(reason, model string, latencyMS int64) Result {
	return Result{
		Backend:        cfg.Backend,
		DecisionModel:  model,
		LatencyMS:      latencyMS,
		Fallback:       true,
		FallbackReason: reason,
	}
}

// withDefaults returns cfg with zero fields set to their defaults.
func (cfg Config) withDefaults() Config {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.ConfidenceFloor <= 0 {
		cfg.ConfidenceFloor = 0.70
	}
	if cfg.Margin <= 0 {
		cfg.Margin = 0.15
	}
	return cfg
}

// validate rejects a Config the client cannot call.
func (cfg Config) validate() error {
	switch cfg.Backend {
	case BackendOllama, BackendJev:
	default:
		return fmt.Errorf("decision: backend %q is not %q or %q", cfg.Backend, BackendOllama, BackendJev)
	}
	if cfg.Model == "" {
		return errors.New("decision: model not set")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("decision: base URL %q is not an http(s) URL", cfg.BaseURL)
	}
	return nil
}

// endpoint returns the systemone URL for this backend.
func (cfg Config) endpoint() string {
	return strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/systemone"
}

// loadKey reads the API key: one line, mode 0600, operator-supplied path. The
// key is used only in the Authorization header and never appears in a log
// line, an error, or a Result.
func (cfg Config) loadKey() (string, error) {
	if cfg.KeyFile == "" {
		return "", nil
	}
	info, err := os.Stat(cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("decision: key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("decision: key file is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("decision: key file must be mode 0600")
	}
	data, err := os.ReadFile(cfg.KeyFile) // #nosec G304 -- operator-supplied key path
	if err != nil {
		return "", fmt.Errorf("decision: key file: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" || strings.ContainsAny(key, "\n\r") {
		return "", errors.New("decision: key file must hold exactly one line")
	}
	return key, nil
}
