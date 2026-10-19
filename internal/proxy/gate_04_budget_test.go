package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
)

// Gate 4 at the proxy: the ledger primitives and the two-process test live in
// internal/ledger. These tests prove the contract the proxy keeps with them:
// reserve before dispatch, settle or keep an uncertain charge after, never
// reserve for a request that policy refuses, and never over-admit.

const gate04Wait = 10 * time.Second

// waitFor blocks the test goroutine until ch closes. Call it from the test
// goroutine only: it uses t.Fatalf.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(gate04Wait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestGate04_ReservesBeforeDispatchAndShowsItInFlight: by the time the upstream
// holds the request, the ledger already carries the reservation (the declared
// max_tokens of 100). Release turns it into the measured total.
func TestGate04_ReservesBeforeDispatchAndShowsItInFlight(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			once.Do(func() { close(arrived) })
			select {
			case <-release:
			case <-time.After(gate04Wait):
			}
			okHandler(w, nil)
		}
	})
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, err := newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(chatBody))
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := h.client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()

	waitFor(t, arrived, "the request to reach the upstream")
	if tot := h.totals(); tot.Reserved != 100 || tot.Settled != 0 || tot.Uncertain != 0 {
		t.Fatalf("reservation must exist while the request is in flight: %+v", tot)
	}
	close(release)
	r := <-done
	if r.err != nil || r.status != http.StatusOK {
		t.Fatalf("request: status %d, err %v", r.status, r.err)
	}
	h.idle() // the caller has its response; settlement may still be running
	if tot := h.totals(); tot.Reserved != 0 || tot.Settled != 18 || tot.Uncertain != 0 {
		t.Fatalf("after settle: %+v", tot)
	}
}

// TestGate04_SettleReleasesReservationAndAddsMeasuredTotal: the fake upstream
// reports 11 in and 7 out; the ledger ends with exactly 18 settled and nothing
// reserved, not the reservation of 100.
func TestGate04_SettleReleasesReservationAndAddsMeasuredTotal(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rec := h.lastRecord()
	if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 11 || i64v(rec.OutputTokens) != 7 {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Settled != 18 || tot.Reserved != 0 || tot.Uncertain != 0 {
		t.Fatalf("ledger: %+v", tot)
	}
}

// TestGate04_FailureAfterTransmissionKeepsUncertainCharge: the request reached
// the upstream and the connection died before any response. The reservation is
// released as an uncertain charge of the same size, never dropped.
func TestGate04_FailureAfterTransmissionKeepsUncertainCharge(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) { killConn(t, w) }
	})
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusBadGateway || errCode(t, body) != audit.FailCommitLost {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests", n)
	}
	if rec := h.lastRecord(); rec.UsageStatus != audit.UsageUnknown || rec.FailureCode != audit.FailCommitLost {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Uncertain != 100 || tot.Reserved != 0 || tot.Settled != 0 {
		t.Fatalf("uncertain charge not preserved: %+v", tot)
	}
}

// TestGate04_PolicyRejectionNeverReserves: every refusal that policy makes
// leaves the ledger at zero in every column and makes no upstream call.
func TestGate04_PolicyRejectionNeverReserves(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		hdr  map[string]string
		code string
	}{
		{"model not allowed", chatPath, `{"model":"gpt-4o-mini","max_tokens":100}`, nil, "model_not_allowed"},
		{"provider not allowed", "/p/extra/v1/chat/completions", chatBody, nil, "provider_not_allowed"},
		{"bad token", chatPath, chatBody, map[string]string{"Authorization": "Bearer nope"}, "unauthorized"},
		{"malformed body", chatPath, `not json`, nil, "malformed"},
		{"body over cap", chatPath, `{"model":"gpt-6-astra","pad":"` + strings.Repeat("a", policy.MaxBodyBytes) + `"}`, nil, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			resp, body := h.do(tc.path, tc.body, tc.hdr)
			if resp.StatusCode < 400 || errCode(t, body) != tc.code {
				t.Fatalf("got %d %s, want code %s", resp.StatusCode, body, tc.code)
			}
			if n := h.upstream.count(); n != 0 {
				t.Fatalf("upstream called %d times", n)
			}
			h.idle()
			if tot := h.totals(); tot != (ledger.Totals{}) {
				t.Fatalf("a rejected request touched the ledger: %+v", tot)
			}
		})
	}
}

// TestGate04_ConcurrentRequestsOnABudgetThatFitsOneAdmitExactlyOne: budget 100,
// two requests with caps of 1000, both allowed to run at once. One is admitted and
// held at the upstream; the other fails budget_exhausted without any upstream
// call. The loser is read before the winner is released, so the two were
// genuinely concurrent.
func TestGate04_ConcurrentRequestsOnABudgetThatFitsOneAdmitExactlyOne(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, func(o *harnessOpts) {
		o.budget = 100
		o.concurrency = 2
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			once.Do(func() { close(arrived) })
			select {
			case <-release:
			case <-time.After(gate04Wait):
			}
			okHandler(w, nil)
		}
	})
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			req, err := newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(strings.Replace(chatBody, `"max_tokens":100`, `"max_tokens":1000`, 1)))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+h.token)
			resp, err := h.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			results <- result{status: resp.StatusCode, body: b}
		}()
	}

	var loser result
	select {
	case loser = <-results: // the only one that can finish while the upstream is held
	case <-time.After(gate04Wait):
		t.Fatal("neither request finished: the loser should be refused at once")
	}
	if loser.err != nil || loser.status != http.StatusForbidden || errCode(t, loser.body) != audit.FailBudgetExhausted {
		t.Fatalf("loser: status %d, err %v, body %s", loser.status, loser.err, loser.body)
	}
	waitFor(t, arrived, "the admitted request to reach the upstream")
	if tot := h.totals(); tot.Reserved != 100 {
		t.Fatalf("exactly one reservation must be held: %+v", tot)
	}
	close(release)
	var winner result
	select {
	case winner = <-results:
	case <-time.After(gate04Wait):
		t.Fatal("the admitted request never finished")
	}
	if winner.err != nil || winner.status != http.StatusOK {
		t.Fatalf("winner: status %d, err %v", winner.status, winner.err)
	}
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests, want exactly 1", n)
	}
	var admitted, refused int
	for _, rec := range h.records() {
		switch {
		case rec.FailureCode == audit.FailBudgetExhausted:
			refused++
			if rec.UpstreamAttempts != 0 {
				t.Fatalf("the refused request reached the upstream: %+v", rec)
			}
		case rec.FailureCode == "" && rec.StatusCode == http.StatusOK:
			admitted++
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted %d, refused %d, want 1 and 1", admitted, refused)
	}
	if tot := h.totals(); tot.Settled != 18 || tot.Reserved != 0 || tot.Uncertain != 0 {
		t.Fatalf("ledger: %+v", tot)
	}
}

func TestGate04_ReservationCappedByRemainingBudget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		budget         int64
		cap            int64
		priorUsage     int64
		reserved       int64
		defaultReserve int64
		ceiling        int64
	}{
		{name: "large output cap", budget: 100000, cap: 1000000, reserved: 2000},
		{name: "remaining below ceiling", budget: 1000, cap: 1000000, reserved: 1000},
		{name: "default capped", budget: 100, cap: 0, reserved: 100},
		{name: "fallback capped by default ceiling", budget: 100000, cap: 0, reserved: 2000},
		{name: "default fits", budget: 100000, cap: 0, defaultReserve: 1000, reserved: 1000},
		{name: "configured ceiling", budget: 100000, cap: 1000000, ceiling: 8000, reserved: 8000},
		{name: "default above ceiling", budget: 100000, cap: 0, defaultReserve: 10000, ceiling: 2000, reserved: 2000},
		{name: "negative cap uses bounded default", budget: 100000, cap: -1, defaultReserve: 10000, ceiling: 2000, reserved: 2000},
		{name: "measured usage exceeds ceiling", budget: 100000, cap: 1000000, ceiling: 100, reserved: 100},
		{name: "settled usage counts", budget: 1000, cap: 1000000, priorUsage: 400, reserved: 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var releaseOnce sync.Once
			releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
			h := newHarness(t, func(o *harnessOpts) {
				o.budget = tc.budget
				o.defaultReserve, o.maxReservation = tc.defaultReserve, tc.ceiling
				o.handler = func(w http.ResponseWriter, _ *http.Request) {
					once.Do(func() { close(arrived) })
					select {
					case <-release:
					case <-time.After(gate04Wait):
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":200,"completion_tokens":300}}`)
				}
			})
			defer releaseRequest()
			if tc.priorUsage > 0 {
				if err := h.led.Reserve("prior", testRunID, tc.priorUsage); err != nil {
					t.Fatal(err)
				}
				if err := h.led.Settle("prior", testRunID, tc.priorUsage, true); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				status int
				err    error
			}
			done := make(chan result, 1)
			body := fmt.Sprintf(`{"model":"gpt-6-astra","max_tokens":%d}`, tc.cap)
			go func() {
				req, err := newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(body))
				if err != nil {
					done <- result{err: err}
					return
				}
				req.Header.Set("Authorization", "Bearer "+h.token)
				resp, err := h.client.Do(req)
				if err != nil {
					done <- result{err: err}
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				done <- result{status: resp.StatusCode}
			}()
			select {
			case <-arrived:
			case r := <-done:
				t.Fatalf("request not admitted: status %d, err %v", r.status, r.err)
			case <-time.After(gate04Wait):
				t.Fatal("request did not reach upstream")
			}
			if tot := h.totals(); tot != (ledger.Totals{Reserved: tc.reserved, Settled: tc.priorUsage}) {
				t.Fatalf("in-flight totals: %+v", tot)
			}
			releaseRequest()
			select {
			case r := <-done:
				if r.err != nil || r.status != http.StatusOK {
					t.Fatalf("response: status %d, err %v", r.status, r.err)
				}
			case <-time.After(gate04Wait):
				t.Fatal("response did not finish")
			}
			h.idle()
			if tot := h.totals(); tot != (ledger.Totals{Settled: tc.priorUsage + 500}) {
				t.Fatalf("settled totals: %+v", tot)
			}
		})
	}
}

func TestGate04_DisconnectKeepsBoundedReservation(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	h := newHarness(t, func(o *harnessOpts) {
		o.budget = 100000
		o.handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"delta\":\"tick\"}\n\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
			close(upstreamCancelled)
		}
	})
	const body = `{"model":"gpt-6-astra","max_tokens":1000000,"stream":true}`
	resp := postStream(t, h, chatPath, body)
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 16)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	if tot := h.totals(); tot != (ledger.Totals{Reserved: 2000}) {
		t.Fatalf("in-flight reservation must be bounded: %+v", tot)
	}
	_ = resp.Body.Close()
	waitFor(t, upstreamCancelled, "upstream cancellation after disconnect")
	done := make(chan struct{})
	go func() { h.idle(); close(done) }()
	waitFor(t, done, "disconnect settlement")
	if tot := h.totals(); tot != (ledger.Totals{Uncertain: 2000}) {
		t.Fatalf("disconnect must charge only the reservation taken: %+v", tot)
	}
	if rec := h.lastRecord(); rec.FailureCode != audit.FailClientDisconnect || rec.UsageStatus != audit.UsageUnknown {
		t.Fatalf("audit: %+v", rec)
	}
	h.upstream.handler.Store(http.HandlerFunc(okHandler))
	next, response := h.do(chatPath, body, nil)
	if next.StatusCode != http.StatusOK {
		t.Fatalf("next request must still succeed: %d %s", next.StatusCode, response)
	}
	h.idle()
	if tot := h.totals(); tot != (ledger.Totals{Uncertain: 2000, Settled: 18}) {
		t.Fatalf("ledger after next request: %+v", tot)
	}
}

func TestGate04_MeasuredOverBudgetResponseDelivered(t *testing.T) {
	const response = `{"id":"delivered","usage":{"prompt_tokens":200,"completion_tokens":300}}`
	h := newHarness(t, func(o *harnessOpts) {
		o.budget = 100
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}
	})
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusOK || string(body) != response {
		t.Fatalf("response: %d %s", resp.StatusCode, body)
	}
	rec := h.lastRecord()
	if rec.FailureCode != "" || rec.UsageStatus != audit.UsageMeasured || rec.CommitState != audit.CommitCommitted || i64v(rec.InputTokens)+i64v(rec.OutputTokens) != 500 {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot != (ledger.Totals{Settled: 500}) {
		t.Fatalf("measured truth not settled: %+v", tot)
	}
	resp, body = h.do(chatPath, chatBody, nil)
	if resp.StatusCode != http.StatusForbidden || errCode(t, body) != audit.FailBudgetExhausted {
		t.Fatalf("next request: %d %s", resp.StatusCode, body)
	}
	if h.upstream.count() != 1 || h.lastRecord().UpstreamAttempts != 0 {
		t.Fatal("exhausted request reached upstream")
	}
}

func TestGate04_ConcurrentRequestsUseRemainingBudget(t *testing.T) {
	const requests = 12
	arrived := make(chan struct{}, requests)
	release := make(chan struct{})
	h := newHarness(t, func(o *harnessOpts) {
		o.budget = 150
		o.concurrency = requests
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			arrived <- struct{}{}
			select {
			case <-release:
			case <-time.After(gate04Wait):
			}
			okHandler(w, nil)
		}
	})
	var once sync.Once
	releaseRequests := func() { once.Do(func() { close(release) }) }
	defer releaseRequests()
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, requests)
	start := make(chan struct{})
	for range requests {
		go func() {
			<-start
			req, err := newTestRequest(http.MethodPost, h.url+chatPath, strings.NewReader(chatBody))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+h.token)
			resp, err := h.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			results <- result{status: resp.StatusCode, body: body}
		}()
	}
	close(start)
	for range 2 {
		waitFor(t, arrived, "a request to use the remaining budget")
	}
	for range requests - 2 {
		select {
		case r := <-results:
			if r.err != nil || r.status != http.StatusForbidden || errCode(t, r.body) != audit.FailBudgetExhausted {
				t.Fatalf("refused request: status %d, err %v, body %s", r.status, r.err, r.body)
			}
		case <-time.After(gate04Wait):
			t.Fatal("exhausted request did not finish")
		}
	}
	if tot := h.totals(); tot != (ledger.Totals{Reserved: 150}) {
		t.Fatalf("expected reservations of 100 and 50: %+v", tot)
	}
	if h.upstream.count() != 2 {
		t.Fatalf("upstream saw %d requests, want 2", h.upstream.count())
	}
	releaseRequests()
	for range 2 {
		select {
		case r := <-results:
			if r.err != nil || r.status != http.StatusOK {
				t.Fatalf("admitted request: status %d, err %v", r.status, r.err)
			}
		case <-time.After(gate04Wait):
			t.Fatal("admitted request did not finish")
		}
	}
	h.idle()
	if tot := h.totals(); tot != (ledger.Totals{Settled: 36}) {
		t.Fatalf("settled totals: %+v", tot)
	}
}
