package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// BlendLabel states the price blend. It changes the ranking, so the CLI
	// prints it and every snapshot records it.
	BlendLabel = "90/10 input/output"
	// PopulationAgentCapable names the model population of a snapshot.
	PopulationAgentCapable = "agent_capable"
	// MaxCatalogBytes caps a catalog download.
	MaxCatalogBytes = 32 << 20
	// StaleAfter is the age after which a snapshot is reported as stale.
	StaleAfter = 30 * 24 * time.Hour

	// MinAgentContext is the minimum context size for a coding-agent seat.
	MinAgentContext = 100000
	// maxMicro bounds one decoded price: 1,000,000 USD per 1M tokens. It keeps
	// the blend arithmetic far from int64 overflow.
	maxMicro = 1_000_000 * 1_000_000
)

// ErrDigestMismatch is returned when a download does not match the pinned digest.
var ErrDigestMismatch = errors.New("catalog digest does not match the pinned digest")

// Model is the subset of one catalog entry the gateway needs. Prices are
// integer micro-USD per 1M tokens.
type Model struct {
	ID       string
	Provider string
	// HasCost is true only when the catalog carries both input and output prices.
	HasCost          bool
	InputMicro       int64
	OutputMicro      int64
	CacheReadMicro   int64
	ToolCall         bool
	Reasoning        bool
	ReasoningOptions json.RawMessage
	ContextLimit     int64
	OutputModalities []string
	OpenWeights      bool
}

// Provider is one catalog provider and its models, keyed by model id.
type Provider struct {
	ID     string
	Models map[string]Model
}

// Catalog is a decoded model catalog.
type Catalog struct {
	Providers map[string]Provider
}

type rawModel struct {
	ID               string          `json:"id"`
	ToolCall         bool            `json:"tool_call"`
	Reasoning        bool            `json:"reasoning"`
	ReasoningOptions json.RawMessage `json:"reasoning_options"`
	OpenWeights      bool            `json:"open_weights"`
	Cost             *struct {
		Input     json.Number `json:"input"`
		Output    json.Number `json:"output"`
		CacheRead json.Number `json:"cache_read"`
	} `json:"cost"`
	Limit *struct {
		Context json.Number `json:"context"`
	} `json:"limit"`
	Modalities *struct {
		Output []string `json:"output"`
	} `json:"modalities"`
}

type rawProvider struct {
	Models map[string]rawModel `json:"models"`
}

// Decode parses a models.dev style catalog: an object of providers, each with
// a models object. Numbers are decoded as json.Number and converted without
// float arithmetic. Unknown fields are ignored; the subset is deliberate.
func Decode(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]rawProvider
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("catalog: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("catalog: decode: trailing data after document")
	}
	c := &Catalog{Providers: make(map[string]Provider, len(raw))}
	for pid, rp := range raw {
		p := Provider{ID: pid, Models: make(map[string]Model, len(rp.Models))}
		for key, rm := range rp.Models {
			m, err := convert(pid, key, rm)
			if err != nil {
				return nil, fmt.Errorf("catalog: %s/%s: %w", pid, key, err)
			}
			if _, dup := p.Models[m.ID]; dup {
				return nil, fmt.Errorf("catalog: %s/%s: duplicate model id %q", pid, key, m.ID)
			}
			p.Models[m.ID] = m
		}
		c.Providers[pid] = p
	}
	return c, nil
}

func convert(provider, key string, rm rawModel) (Model, error) {
	m := Model{
		ID: key, Provider: provider, ToolCall: rm.ToolCall, Reasoning: rm.Reasoning,
		ReasoningOptions: rm.ReasoningOptions, OpenWeights: rm.OpenWeights,
	}
	if rm.ID != "" {
		m.ID = rm.ID
	}
	if rm.Cost != nil {
		var err error
		if rm.Cost.Input != "" {
			if m.InputMicro, err = ParseMicro(rm.Cost.Input); err != nil {
				return Model{}, fmt.Errorf("cost.input: %w", err)
			}
		}
		if rm.Cost.Output != "" {
			if m.OutputMicro, err = ParseMicro(rm.Cost.Output); err != nil {
				return Model{}, fmt.Errorf("cost.output: %w", err)
			}
		}
		m.HasCost = rm.Cost.Input != "" && rm.Cost.Output != ""
		if rm.Cost.CacheRead != "" {
			if m.CacheReadMicro, err = ParseMicro(rm.Cost.CacheRead); err != nil {
				return Model{}, fmt.Errorf("cost.cache_read: %w", err)
			}
		}
	}
	if rm.Limit != nil && rm.Limit.Context != "" {
		n, err := strconv.ParseInt(rm.Limit.Context.String(), 10, 64)
		if err != nil || n < 0 {
			return Model{}, fmt.Errorf("limit.context: %q is not a non-negative integer", rm.Limit.Context)
		}
		m.ContextLimit = n
	}
	if rm.Modalities != nil {
		m.OutputModalities = slices.Clone(rm.Modalities.Output)
	}
	return m, nil
}

// ParseMicro converts a decimal USD amount per 1M tokens, such as "0.15", into
// integer micro-USD without any float arithmetic. Digits beyond the sixth
// decimal round half up. Exponent forms, signs and bare dots are rejected.
func ParseMicro(n json.Number) (int64, error) {
	s := n.String()
	if s == "" || len(s) > 40 {
		return 0, fmt.Errorf("invalid price %q", s)
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(whole) > 12 || !allDigits(whole) || (hasDot && (frac == "" || !allDigits(frac))) {
		return 0, fmt.Errorf("invalid price %q", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid price %q: %w", s, err)
	}
	micro := w * 1_000_000
	if w > maxMicro/1_000_000 {
		return 0, fmt.Errorf("price %q out of range", s)
	}
	if hasDot {
		padded := frac
		if len(padded) < 7 {
			padded += strings.Repeat("0", 7-len(padded))
		}
		f, err := strconv.ParseInt(padded[:6], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid price %q: %w", s, err)
		}
		micro += f
		if padded[6] >= '5' {
			micro++
		}
	}
	if micro > maxMicro {
		return 0, fmt.Errorf("price %q out of range", s)
	}
	return micro, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Lookup finds a model by its full id, "<provider>/<model id>". The provider id
// never contains a slash; the model id may.
func (c *Catalog) Lookup(fullID string) (Model, bool) {
	pid, mid, ok := strings.Cut(fullID, "/")
	if !ok {
		return Model{}, false
	}
	m, ok := c.Providers[pid].Models[mid]
	return m, ok
}

// AgentCapable reports whether a model can serve as a coding-agent seat: tool
// calls, reasoning, a context of at least 100000 tokens, and text-only output.
func AgentCapable(m Model) bool {
	return m.ToolCall && m.Reasoning && m.ContextLimit >= MinAgentContext &&
		len(m.OutputModalities) == 1 && m.OutputModalities[0] == "text"
}

// Blended is the blended price in micro-USD per 1M tokens at 90% input and 10%
// output: (9*input + 1*output)/10, integer division.
func Blended(m Model) int64 {
	return (9*m.InputMicro + m.OutputMicro) / 10
}

// ProviderMedian is the median blended price over the provider's agent-capable
// models that carry a price. It returns the median and the population size. For
// an even population it is the floor of the mean of the two middle prices.
func ProviderMedian(p Provider) (median int64, n int) {
	prices := make([]int64, 0, len(p.Models))
	for _, m := range p.Models {
		if AgentCapable(m) && m.HasCost {
			prices = append(prices, Blended(m))
		}
	}
	slices.Sort(prices)
	n = len(prices)
	switch {
	case n == 0:
		return 0, 0
	case n%2 == 1:
		return prices[n/2], n
	default:
		return (prices[n/2-1] + prices[n/2]) / 2, n
	}
}

// ProviderStat is one provider's median in a snapshot.
type ProviderStat struct {
	ID             string `json:"id"`
	MedianMicroUSD int64  `json:"median_micro_usd"`
	N              int    `json:"n"`
}

// SnapshotModel is one agent-capable model in a snapshot.
type SnapshotModel struct {
	ID                string `json:"id"`
	InputMicroUSD     int64  `json:"input_micro_usd"`
	OutputMicroUSD    int64  `json:"output_micro_usd"`
	CacheReadMicroUSD int64  `json:"cache_read_micro_usd"`
	BlendedMicroUSD   int64  `json:"blended_micro_usd"`
	Priced            bool   `json:"priced"`
}

// Snapshot is the pricing artifact written by `mgw pricing refresh`. It holds
// agent-capable models only; a model absent from it is unrated.
type Snapshot struct {
	CapturedAt   string          `json:"captured_at"`
	SourceURL    string          `json:"source_url"`
	SourceSHA256 string          `json:"source_sha256"`
	Blend        string          `json:"blend"`
	Population   string          `json:"population"`
	Providers    []ProviderStat  `json:"providers"`
	Models       []SnapshotModel `json:"models"`
}

// NewSnapshot builds a deterministic snapshot of c.
func NewSnapshot(c *Catalog, sourceURL, sourceSHA256 string, now time.Time) Snapshot {
	s := Snapshot{
		CapturedAt: now.UTC().Format(time.RFC3339), SourceURL: sourceURL, SourceSHA256: sourceSHA256,
		Blend: BlendLabel, Population: PopulationAgentCapable,
		Providers: []ProviderStat{}, Models: []SnapshotModel{},
	}
	for pid, p := range c.Providers {
		med, n := ProviderMedian(p)
		s.Providers = append(s.Providers, ProviderStat{ID: pid, MedianMicroUSD: med, N: n})
		for _, m := range p.Models {
			if !AgentCapable(m) {
				continue
			}
			sm := SnapshotModel{ID: pid + "/" + m.ID, Priced: m.HasCost}
			if m.HasCost {
				sm.InputMicroUSD, sm.OutputMicroUSD = m.InputMicro, m.OutputMicro
				sm.CacheReadMicroUSD, sm.BlendedMicroUSD = m.CacheReadMicro, Blended(m)
			}
			s.Models = append(s.Models, sm)
		}
	}
	slices.SortFunc(s.Providers, func(a, b ProviderStat) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(s.Models, func(a, b SnapshotModel) int { return strings.Compare(a.ID, b.ID) })
	return s
}

// ReadSnapshot parses and validates a snapshot. A fractional number anywhere in
// it is an error, because every price field is an integer.
func ReadSnapshot(data []byte) (Snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Snapshot{}, errors.New("snapshot: decode: trailing data after document")
	}
	if _, err := time.Parse(time.RFC3339, s.CapturedAt); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: captured_at: %w", err)
	}
	return s, nil
}

// Lookup finds a model by its full id in the snapshot.
func (s Snapshot) Lookup(fullID string) (SnapshotModel, bool) {
	i := slices.IndexFunc(s.Models, func(m SnapshotModel) bool { return m.ID == fullID })
	if i < 0 {
		return SnapshotModel{}, false
	}
	return s.Models[i], true
}

// Stale reports whether the snapshot is older than StaleAfter at now.
func (s Snapshot) Stale(now time.Time) (bool, error) {
	t, err := time.Parse(time.RFC3339, s.CapturedAt)
	if err != nil {
		return false, fmt.Errorf("snapshot: captured_at: %w", err)
	}
	return now.Sub(t) > StaleAfter, nil
}

// Digest returns the lowercase hex sha256 of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VerifyDigest compares a computed digest with the pinned one. An empty pin
// never matches.
func VerifyDigest(got, want string) error {
	want = strings.TrimSpace(want)
	if want == "" || !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: got %s", ErrDigestMismatch, got)
	}
	return nil
}

// Fetch downloads the catalog and returns the body with its sha256. It follows
// no redirect, sends no proxy or compression negotiation, caps the body at
// MaxCatalogBytes, and never puts a response body into an error.
func Fetch(ctx context.Context, url string) ([]byte, string, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, "", fmt.Errorf("catalog: unsupported url scheme in %q", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("catalog: request: %w", err)
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DisableCompression:    true,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("catalog: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("catalog: fetch: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("catalog: read body: %w", err)
	}
	if len(body) > MaxCatalogBytes {
		return nil, "", fmt.Errorf("catalog: body exceeds %d bytes", MaxCatalogBytes)
	}
	return body, Digest(body), nil
}
