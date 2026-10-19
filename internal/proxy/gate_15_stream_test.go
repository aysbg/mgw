package proxy

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
)

const streamWait = 10 * time.Second

func postStream(t *testing.T, h *harness, path, body string) *http.Response {
	t.Helper()
	req, err := newTestRequest(http.MethodPost, h.url+path, ioReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestGate15_FragmentedSSEByteExact: frames split across writes (including in
// the middle of a UTF-8 character) arrive byte-exact, and every fragment is
// flushed to the caller before upstream sends the next one.
func TestGate15_FragmentedSSEByteExact(t *testing.T) {
	golden, err := os.ReadFile("testdata/gate15_fragmented.sse")
	if err != nil {
		t.Fatal(err)
	}
	var bounds []int // end offset of each fragment
	for off, i := 0, 0; off < len(golden); i++ {
		off += []int{5, 1, 19, 2, 40, 3, 1, 64}[i%8]
		if off > len(golden) {
			off = len(golden)
		}
		bounds = append(bounds, off)
	}
	ack := make(chan struct{}, 1)
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			prev := 0
			for i, end := range bounds {
				if _, err := w.Write(golden[prev:end]); err != nil {
					t.Errorf("upstream write: %v", err)
					return
				}
				_ = rc.Flush()
				prev = end
				if i == len(bounds)-1 {
					return
				}
				select {
				case <-ack: // the caller has already received this fragment
				case <-time.After(streamWait):
					t.Errorf("fragment %d (ending at byte %d) was not flushed to the caller", i, end)
					return
				}
			}
		}
	})
	resp := postStream(t, h, msgPath, msgBody)
	defer func() { _ = resp.Body.Close() }()
	var got []byte
	buf := make([]byte, 4096)
	next := 0
	for {
		n, err := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		for next < len(bounds)-1 && len(got) >= bounds[next] {
			ack <- struct{}{}
			next++
			if len(got) < bounds[next] {
				break
			}
			// More than one fragment already arrived: ack the earlier ones too.
			select {
			case ack <- struct{}{}:
			default:
			}
		}
		if err != nil {
			if err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			break
		}
	}
	if !bytes.Equal(got, golden) {
		t.Fatalf("relay is not byte-exact: got %d bytes, want %d", len(got), len(golden))
	}
	rec := h.lastRecord()
	if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 25 || i64v(rec.OutputTokens) != 42 || rec.FailureCode != "" {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Settled != 67 || tot.Reserved != 0 {
		t.Fatalf("ledger: %+v", tot)
	}
}

// patternStream writes n bytes of deterministic SSE-like content in uneven
// chunk sizes and returns the SHA-256 of everything written.
func patternBytes(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "data: {\"i\":%d,\"pad\":\"%s\"}\n\n", i, strings.Repeat("x", 50+i%900))
	}
	return b.Bytes()[:n]
}

// TestGate15_LongStreamByteExact: more than 1 MiB relays byte-exact in bounded
// chunks, with a usage frame at the end still parsed.
func TestGate15_LongStreamByteExact(t *testing.T) {
	const size = 3 << 20
	body := append(patternBytes(size), []byte("\ndata: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n")...)
	want := sha256.Sum256(body)
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			for i, off := 0, 0; off < len(body); i++ {
				end := min(off+[]int{1000, 7919, 33, 20000}[i%4], len(body))
				if _, err := w.Write(body[off:end]); err != nil {
					return
				}
				if i%3 == 0 {
					_ = rc.Flush()
				}
				off = end
			}
		}
	})
	resp := postStream(t, h, chatPath, chatBody)
	defer func() { _ = resp.Body.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != len(body) || fmt.Sprintf("%x", hash.Sum(nil)) != fmt.Sprintf("%x", want) {
		t.Fatalf("not byte-exact: %d bytes, want %d", n, len(body))
	}
	rec := h.lastRecord()
	if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 10 || i64v(rec.OutputTokens) != 20 {
		t.Fatalf("audit: %+v", rec)
	}
}

// TestGate15_SlowReaderBoundedMemory: while the caller stops reading, upstream
// is held back by back-pressure and gateway memory stays flat; after the
// caller resumes, every byte arrives exactly.
func TestGate15_SlowReaderBoundedMemory(t *testing.T) {
	const total = 32 << 20
	line := append(bytes.Repeat([]byte{'a'}, 1023), '\n')
	var written atomic.Int64
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, _ *http.Request) {
			chunk := bytes.Repeat(line, 32) // 32 KiB
			for written.Load() < total {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
			}
		}
	})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	resp := postStream(t, h, chatPath, chatBody)
	defer func() { _ = resp.Body.Close() }()
	hash := sha256.New()
	var got int64
	first := make([]byte, 64<<10)
	n, err := io.ReadFull(resp.Body, first)
	if err != nil {
		t.Fatal(err)
	}
	hash.Write(first[:n])
	got += int64(n)

	// The caller stops reading. Wait until upstream stops making progress.
	deadline := time.Now().Add(streamWait)
	var last int64 = -1
	stable := 0
	for stable < 5 {
		if time.Now().After(deadline) {
			t.Fatal("upstream never stalled: no back-pressure")
		}
		time.Sleep(40 * time.Millisecond) // real time: back-pressure is a property of the network path
		if cur := written.Load(); cur == last {
			stable++
		} else {
			last, stable = cur, 0
		}
	}
	if last >= total/2 {
		t.Fatalf("upstream wrote %d of %d bytes while the caller was not reading: the gateway is buffering", last, total)
	}
	t.Logf("upstream stalled after writing %d of %d bytes while the caller was not reading", last, int64(total))
	runtime.GC()
	var stalled runtime.MemStats
	runtime.ReadMemStats(&stalled)
	if grown := int64(stalled.HeapAlloc) - int64(before.HeapAlloc); grown > 32<<20 {
		t.Fatalf("heap grew by %d bytes while the caller was stalled", grown)
	}

	t.Logf("heap growth while stalled: %d bytes", int64(stalled.HeapAlloc)-int64(before.HeapAlloc))
	m, err := io.Copy(hash, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got += m

	want := sha256.New()
	chunk := bytes.Repeat(line, 32)
	for i := int64(0); i < got/int64(len(chunk)); i++ {
		want.Write(chunk)
	}
	if got != written.Load() || got%int64(len(chunk)) != 0 {
		t.Fatalf("received %d bytes, upstream wrote %d", got, written.Load())
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != fmt.Sprintf("%x", want.Sum(nil)) {
		t.Fatal("bytes differ after the slow reader resumed")
	}
}

// goroutineRunning reports whether any goroutine is inside a function whose
// name contains substr.
func goroutineRunning(substr string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Contains(string(buf), substr)
}

// TestGate15_ClientDisconnectCancelsUpstream: a caller that hangs up cancels
// the upstream request, the handler returns, the reservation is settled and
// the audit record is still written.
func TestGate15_ClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	h := newHarness(t, func(o *harnessOpts) {
		o.handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			frame := []byte("data: {\"delta\":\"tick\"}\n\n")
			for {
				select {
				case <-r.Context().Done():
					close(upstreamCancelled)
					return
				default:
				}
				if _, err := w.Write(frame); err != nil {
					<-r.Context().Done()
					close(upstreamCancelled)
					return
				}
				_ = rc.Flush()
			}
		}
	})
	resp := postStream(t, h, chatPath, chatBody)
	first := make([]byte, 16)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // hang up mid-stream

	select {
	case <-upstreamCancelled:
	case <-time.After(streamWait):
		t.Fatal("upstream request was not cancelled after the caller disconnected")
	}
	done := make(chan struct{})
	go func() { h.idle(); close(done) }()
	select {
	case <-done:
	case <-time.After(streamWait):
		t.Fatal("handler did not return after the caller disconnected")
	}
	deadline := time.Now().Add(streamWait)
	for goroutineRunning("proxy.(*Server).stream") || goroutineRunning("proxy.(*Server).handle") {
		if time.Now().After(deadline) {
			t.Fatal("handler goroutine leaked")
		}
		runtime.Gosched()
	}
	rec := h.lastRecord()
	if rec.FailureCode != audit.FailClientDisconnect || rec.CommitState != audit.CommitCommitted || rec.UsageStatus != audit.UsageUnknown {
		t.Fatalf("audit: %+v", rec)
	}
	if tot := h.totals(); tot.Reserved != 0 || tot.Uncertain != 100 {
		t.Fatalf("reservation not settled after disconnect: %+v", tot)
	}

	// MGW-002: the cases above and below separate a real disconnect from the
	// normal end of a stream that was fully relayed.
	t.Run("client write fails after upstream EOF is a success", func(t *testing.T) {
		reached := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		h := newHarness(t, func(o *harnessOpts) {
			// Content-Length makes net/http report EOF together with the last
			// bytes, so the relay holds the whole stream and the EOF before it
			// attempts the write that the reset client makes fail.
			o.handler = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(openAIStream)))
				_, _ = io.WriteString(w, openAIStream)
			}
			o.panicHook = func(stage string) {
				if stage == "after_commit" {
					once.Do(func() { close(reached) })
					<-release
				}
			}
		})
		conn, err := dialTCP(hostPort(h))
		if err != nil {
			t.Fatal(err)
		}
		req := "POST " + chatPath + " HTTP/1.1\r\nHost: " + hostPort(h) + "\r\nAuthorization: Bearer " + h.token +
			"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(streamChatBody)) + "\r\n\r\n" + streamChatBody
		if _, err := io.WriteString(conn, req); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reached:
		case <-time.After(streamWait):
			t.Fatal("the gateway never committed the response")
		}
		// Reset the connection (SO_LINGER 0) so the gateway's next write fails
		// at once, then let the relay proceed.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
		close(release)

		rec := h.lastRecord()
		if rec.FailureCode != "" {
			t.Fatalf("a stream the upstream finished must not carry a failure code: %+v", rec)
		}
		if rec.CommitState != audit.CommitCommitted || rec.StatusCode != http.StatusOK {
			t.Fatalf("audit must say committed: %+v", rec)
		}
		if rec.UsageStatus != audit.UsageMeasured || i64v(rec.InputTokens) != 12 || i64v(rec.OutputTokens) != 5 {
			t.Fatalf("usage must stay as parsed: %+v", rec)
		}
		if tot := h.totals(); tot.Settled != 17 || tot.Uncertain != 0 || tot.Reserved != 0 {
			t.Fatalf("ledger must settle the measured total with no uncertain charge: %+v", tot)
		}
	})

	t.Run("client disconnect before response headers commits nothing", func(t *testing.T) {
		received := make(chan struct{})
		var once sync.Once
		h := newHarness(t, func(o *harnessOpts) {
			o.handler = func(_ http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(received) })
				select { // never answers; ends when the gateway cancels the upstream call
				case <-r.Context().Done():
				case <-time.After(streamWait):
				}
			}
		})
		conn, err := dialTCP(hostPort(h))
		if err != nil {
			t.Fatal(err)
		}
		req := "POST " + chatPath + " HTTP/1.1\r\nHost: " + hostPort(h) + "\r\nAuthorization: Bearer " + h.token +
			"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(streamChatBody)) + "\r\n\r\n" + streamChatBody
		if _, err := io.WriteString(conn, req); err != nil {
			t.Fatal(err)
		}
		select {
		case <-received:
		case <-time.After(streamWait):
			t.Fatal("upstream never saw the request")
		}
		_ = conn.Close() // hang up before any response header exists

		rec := h.lastRecord()
		if rec.FailureCode != audit.FailClientDisconnect {
			t.Fatalf("failure_code = %q, want client_disconnect: %+v", rec.FailureCode, rec)
		}
		if rec.CommitState == audit.CommitCommitted || rec.StatusCode != 0 {
			t.Fatalf("no response was committed, the audit must not say so: %+v", rec)
		}
		if rec.UsageStatus != audit.UsageUnknown {
			t.Fatalf("usage: %+v", rec)
		}
		// The request reached the upstream, so the charge is kept as uncertain.
		if tot := h.totals(); tot.Reserved != 0 || tot.Uncertain != 100 || tot.Settled != 0 {
			t.Fatalf("ledger: %+v", tot)
		}
	})
}
