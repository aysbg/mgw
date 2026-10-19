package proxy

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
)

// TestGate02_TokenAuthority: each denial returns its named failure and makes
// zero upstream calls.
func TestGate02_TokenAuthority(t *testing.T) {
	oversize := `{"model":"gpt-6-astra","pad":"` + strings.Repeat("a", policy.MaxBodyBytes) + `"}`
	cases := []struct {
		name   string
		path   string
		body   string
		hdr    map[string]string
		mod    func(*harnessOpts)
		status int
		code   string
	}{
		{"wrong token", chatPath, chatBody, map[string]string{"Authorization": "Bearer not-the-token"}, nil, 401, "unauthorized"},
		{"no token", chatPath, chatBody, map[string]string{"Authorization": ""}, nil, 401, "unauthorized"},
		{"expired token", chatPath, chatBody, nil, func(o *harnessOpts) { o.expires = time.Now().Add(-time.Second) }, 401, "unauthorized"},
		{"provider not in record", "/p/extra/v1/chat/completions", chatBody, nil, nil, 403, "provider_not_allowed"},
		{"model not allowed", chatPath, `{"model":"gpt-4o-mini","messages":[]}`, nil, nil, 403, "model_not_allowed"},
		{"other provider's model", chatPath, msgBody, nil, nil, 403, "model_not_allowed"},
		{"caller-supplied role and lists are ignored", chatPath, `{"model":"gpt-4o-mini"}`,
			map[string]string{"X-Role": "admin", "X-Allowed-Models": "*", "X-Mgw-Provider": "openai", "X-Models": "gpt-4o-mini"}, nil, 403, "model_not_allowed"},
		{"body over cap", chatPath, oversize, nil, nil, 413, "malformed"},
		{"body is not json", chatPath, `not json`, nil, nil, 400, "malformed"},
		{"no model field", chatPath, `{"messages":[]}`, nil, nil, 400, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			resp, body := h.do(tc.path, tc.body, tc.hdr)
			if resp.StatusCode != tc.status || errCode(t, body) != tc.code {
				t.Fatalf("got %d %s, want %d %s", resp.StatusCode, body, tc.status, tc.code)
			}
			if n := h.upstream.count(); n != 0 {
				t.Fatalf("upstream was called %d times", n)
			}
			rec := h.lastRecord()
			if rec.FailureCode != tc.code || rec.UpstreamAttempts != 0 || rec.CommitState != audit.CommitNone {
				t.Fatalf("audit: %+v", rec)
			}
			if tot := h.totals(); tot.Reserved != 0 || tot.Settled != 0 || tot.Uncertain != 0 {
				t.Fatalf("ledger touched by a denied request: %+v", tot)
			}
		})
	}
}

func TestGate02_BudgetExhausted(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) { o.budget = 99 })
	// An output cap above the budget is allowed; only a fully used budget denies.
	if err := h.led.Reserve("held", testRunID, 99); err != nil {
		t.Fatal(err)
	}
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusForbidden || errCode(t, body) != audit.FailBudgetExhausted {
		t.Fatalf("got %d %s, want 403 budget_exhausted", resp.StatusCode, body)
	}
	if h.upstream.count() != 0 {
		t.Fatal("budget-exhausted request reached upstream")
	}
	if rec := h.lastRecord(); rec.FailureCode != audit.FailBudgetExhausted || rec.UpstreamAttempts != 0 || rec.CommitState != audit.CommitNone {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot != (ledger.Totals{Reserved: 99}) {
		t.Fatalf("denied request changed ledger: %+v", tot)
	}
}

// TestGate02_ConcurrentCap: the third in-flight request on a 2-slot token is
// refused before any upstream call.
func TestGate02_ConcurrentCap(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	h := newHarness(t, func(o *harnessOpts) {
		o.concurrency = 2
		o.handler = func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
			okHandler(w, r)
		}
	})
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := h.do(chatPath, chatBody, nil)
			statuses <- resp.StatusCode
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream was not reached by the first two requests")
		}
	}
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests || errCode(t, body) != "concurrency_exceeded" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if n := h.upstream.count(); n != 2 {
		t.Fatalf("upstream saw %d requests, want exactly the 2 admitted ones", n)
	}
	close(release)
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("admitted request got %d", s)
		}
	}
	// The slot is free again once the admitted requests finish.
	h.idle()
	if resp, _ := h.do(chatPath, chatBody, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("slot not released: %d", resp.StatusCode)
	}
	var denied int
	for _, r := range h.records() {
		if r.FailureCode == "concurrency_exceeded" {
			denied++
			if r.UpstreamAttempts != 0 {
				t.Fatalf("denied request reached upstream: %+v", r)
			}
		}
	}
	if denied != 1 {
		t.Fatalf("want one concurrency_exceeded audit record, got %d", denied)
	}
}
