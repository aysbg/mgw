package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
)

// killConn drops the TCP connection without writing a response.
func killConn(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func i64v(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// TestGate05_PreConnectionFailureRetriesOnce: a dial error before any byte is
// written upstream is retried exactly once.
func TestGate05_PreConnectionFailureRetriesOnce(t *testing.T) {
	t.Run("recovers on the second dial", func(t *testing.T) {
		var dials atomic.Int32
		h := newHarness(t, func(o *harnessOpts) {
			o.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return nil, errors.New("connection refused (injected)")
				}
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}
		})
		resp, body := h.do(chatPath, chatBody, nil)
		if resp.StatusCode != 200 || string(body) != usageJSON {
			t.Fatalf("got %d %q", resp.StatusCode, body)
		}
		if dials.Load() != 2 || h.upstream.count() != 1 {
			t.Fatalf("dials=%d upstream=%d", dials.Load(), h.upstream.count())
		}
		rec := h.lastRecord()
		if rec.UpstreamAttempts != 2 || rec.CommitState != audit.CommitCommitted || rec.StatusCode != 200 ||
			rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 11 || i64v(rec.OutputTokens) != 7 || rec.FailureCode != "" {
			t.Fatalf("audit: %+v", rec)
		}
		if tot := h.totals(); tot.Settled != 18 || tot.Reserved != 0 || tot.Uncertain != 0 {
			t.Fatalf("ledger: %+v", tot)
		}
	})
	t.Run("fails after one retry", func(t *testing.T) {
		var dials atomic.Int32
		h := newHarness(t, func(o *harnessOpts) {
			o.dial = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("connection refused (injected)")
			}
		})
		resp, body := h.do(chatPath, chatBody, nil)
		if resp.StatusCode != 502 || errCode(t, body) != "upstream_unavailable" {
			t.Fatalf("got %d %s", resp.StatusCode, body)
		}
		if dials.Load() != 2 {
			t.Fatalf("dials=%d, want exactly 2 (one retry)", dials.Load())
		}
		rec := h.lastRecord()
		// Nothing was written upstream, so the cost is known to be zero.
		if rec.UpstreamAttempts != 2 || rec.CommitState != audit.CommitNone || rec.FailureCode != "upstream_unavailable" ||
			rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 0 || i64v(rec.OutputTokens) != 0 {
			t.Fatalf("audit: %+v", rec)
		}
		if tot := h.totals(); tot.Spent() != 0 {
			t.Fatalf("ledger: %+v", tot)
		}
	})
}

// TestGate05_RetryAfterBeforeCommit: an explicit 429/503 with Retry-After
// received before commit is retried once; nothing else is.
func TestGate05_RetryAfterBeforeCommit(t *testing.T) {
	t.Run("429 with Retry-After retries and honours the wait", func(t *testing.T) {
		var calls atomic.Int32
		h := newHarness(t, func(o *harnessOpts) {
			o.handler = func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":"slow down"}`)
					return
				}
				okHandler(w, r)
			}
		})
		resp, body := h.do(chatPath, chatBody, nil)
		if resp.StatusCode != 200 || string(body) != usageJSON {
			t.Fatalf("got %d %q", resp.StatusCode, body)
		}
		h.idle()
		if len(h.sleeps) != 1 || h.sleeps[0] != 2*time.Second {
			t.Fatalf("Retry-After not honoured: %v", h.sleeps)
		}
		first, second := h.upstream.request(0), h.upstream.request(1)
		if string(first.Body) != chatBody || string(second.Body) != chatBody {
			t.Fatal("retry did not send the same request")
		}
		rec := h.lastRecord()
		if rec.UpstreamAttempts != 2 || rec.CommitState != audit.CommitCommitted || rec.StatusCode != 200 || rec.UsageStatus != audit.UsageMeasured {
			t.Fatalf("audit: %+v", rec)
		}
	})
	t.Run("503 with Retry-After date retries", func(t *testing.T) {
		var calls atomic.Int32
		h := newHarness(t, func(o *harnessOpts) {
			o.handler = func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				okHandler(w, r)
			}
		})
		if resp, _ := h.do(chatPath, chatBody, nil); resp.StatusCode != 200 || calls.Load() != 2 {
			t.Fatalf("status %d calls %d", resp.StatusCode, calls.Load())
		}
	})
	t.Run("only one retry", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOpts) {
			o.handler = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		})
		resp, _ := h.do(chatPath, chatBody, nil)
		if resp.StatusCode != 429 || h.upstream.count() != 2 {
			t.Fatalf("status %d upstream %d", resp.StatusCode, h.upstream.count())
		}
		rec := h.lastRecord()
		if rec.UpstreamAttempts != 2 || rec.CommitState != audit.CommitCommitted || rec.StatusCode != 429 {
			t.Fatalf("audit: %+v", rec)
		}
		// An explicit 429 means nothing was consumed.
		if rec.UsageStatus != audit.UsageMeasured || i64v(rec.OutputTokens) != 0 {
			t.Fatalf("usage: %+v", rec)
		}
	})
	t.Run("not retried", func(t *testing.T) {
		for name, tc := range map[string]struct {
			status int
			retry  string
		}{
			"429 without Retry-After": {429, ""},
			"503 without Retry-After": {503, ""},
			"400":                     {400, "0"},
			"401":                     {401, "0"},
			"500 with Retry-After":    {500, "0"},
			"unparsable Retry-After":  {429, "soon"},
			"wait beyond deadline":    {429, "3600"},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, func(o *harnessOpts) {
					o.handler = func(w http.ResponseWriter, _ *http.Request) {
						if tc.retry != "" {
							w.Header().Set("Retry-After", tc.retry)
						}
						w.WriteHeader(tc.status)
					}
				})
				resp, _ := h.do(chatPath, chatBody, nil)
				if resp.StatusCode != tc.status || h.upstream.count() != 1 || len(h.sleeps) != 0 {
					t.Fatalf("status %d upstream %d sleeps %v", resp.StatusCode, h.upstream.count(), h.sleeps)
				}
				if rec := h.lastRecord(); rec.UpstreamAttempts != 1 || rec.StatusCode != tc.status {
					t.Fatalf("audit: %+v", rec)
				}
			})
		}
	})
}

// TestGate05_AmbiguousFailureNotRetried: the request was transmitted, nothing
// was committed, the connection died. No retry, usage unknown, charge kept.
func TestGate05_AmbiguousFailureNotRetried(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) { killConn(t, w) }
	})
	resp, body := h.do(chatPath, chatBody, nil)
	if resp.StatusCode != 502 || errCode(t, body) != "commit_lost" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests; an ambiguous failure must not be retried", n)
	}
	rec := h.lastRecord()
	if rec.UpstreamAttempts != 1 || rec.CommitState != audit.CommitAmbiguous || rec.UsageStatus != audit.UsageUnknown ||
		rec.InputTokens != nil || rec.OutputTokens != nil || rec.FailureCode != "commit_lost" || rec.StatusCode != 502 {
		t.Fatalf("audit: %+v", rec)
	}
	// The reservation (declared max_tokens=100) is kept as an uncertain charge.
	if tot := h.totals(); tot.Uncertain != 100 || tot.Reserved != 0 || tot.Settled != 0 {
		t.Fatalf("uncertain charge not preserved: %+v", tot)
	}
}

// TestGate05_PostCommitFailurePropagates: once the response is committed, a
// stream failure reaches the caller and the request is never restarted.
func TestGate05_PostCommitFailurePropagates(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"partial\":true}\n\n")
			_ = http.NewResponseController(w).Flush()
			killConn(t, w)
		}
	})
	req, _ := newTestRequest(http.MethodPost, h.url+chatPath, ioReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, rerr := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != "data: {\"partial\":true}\n\n" {
		t.Fatalf("got %d %q", resp.StatusCode, got)
	}
	if rerr == nil {
		t.Fatal("the failed stream looked like a clean end to the caller")
	}
	if n := h.upstream.count(); n != 1 {
		t.Fatalf("upstream saw %d requests; a committed response must never be restarted", n)
	}
	rec := h.lastRecord()
	if rec.UpstreamAttempts != 1 || rec.CommitState != audit.CommitCommitted || rec.StatusCode != 200 ||
		rec.FailureCode != "stream_interrupted" || rec.UsageStatus != audit.UsageUnknown {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Uncertain != 100 || tot.Reserved != 0 {
		t.Fatalf("ledger: %+v", tot)
	}
}

// TestGate05_PanicHandling: before commit 500 internal_panic; after commit the
// response is terminated with no second status line.
func TestGate05_PanicHandling(t *testing.T) {
	t.Run("before commit", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOpts) {
			o.panicHook = func(stage string) {
				if stage == "before_commit" {
					panic("boom")
				}
			}
		})
		resp, body := h.do(chatPath, chatBody, nil)
		if resp.StatusCode != 500 || errCode(t, body) != "internal_panic" {
			t.Fatalf("got %d %s", resp.StatusCode, body)
		}
		if h.upstream.count() != 0 {
			t.Fatal("upstream called")
		}
		rec := h.lastRecord()
		if rec.FailureCode != "internal_panic" || rec.CommitState != audit.CommitNone {
			t.Fatalf("audit: %+v", rec)
		}
		if tot := h.totals(); tot.Reserved != 0 {
			t.Fatalf("reservation leaked: %+v", tot)
		}
	})
	t.Run("after commit", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOpts) {
			o.panicHook = func(stage string) {
				if stage == "after_commit" {
					panic("boom")
				}
			}
		})
		req, _ := newTestRequest(http.MethodPost, h.url+chatPath, ioReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := h.client.Do(req)
		if err == nil {
			_, rerr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if rerr == nil && resp.StatusCode == 500 {
				t.Fatal("a second status line was written after commit")
			}
		}
		rec := h.lastRecord()
		if rec.FailureCode != "internal_panic_after_commit" || rec.CommitState != audit.CommitCommitted {
			t.Fatalf("audit: %+v", rec)
		}
	})
}
