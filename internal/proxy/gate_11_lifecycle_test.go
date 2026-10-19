package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aysbg/mgw/internal/audit"
	"github.com/aysbg/mgw/internal/ledger"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
)

type readyFile struct {
	URL       string `json:"url"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

func newBareServer(t *testing.T, readyPath, addr string) *Server {
	t.Helper()
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger"), 1000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	aw, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = aw.Close() })
	srv, err := New(Config{
		Providers: nil,
		Authority: &policy.Authority{
			Token: newToken(), RunID: testRunID, ExpiresAt: time.Now().Add(time.Hour),
			Providers: map[string][]string{"openai": {"m"}}, Budget: policy.Budget{MaxTokens: 1000},
			Concurrency: 1, PolicySHA256: "p", RegistrySHA256: "r",
		},
		Ledger: led, Audit: aw, ReadyFile: readyPath, Addr: addr, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestGate11_ReadyFileAfterBindSameListener: the ready file does not exist
// before the bind, is complete the moment Listen returns, names a port that is
// already accepting, and the same listener serves the first request.
func TestGate11_ReadyFileAfterBindSameListener(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready.json")
	srv := newBareServer(t, ready, "")
	if _, err := os.Stat(ready); !os.IsNotExist(err) {
		t.Fatalf("ready file exists before the listener is bound: %v", err)
	}
	if err := srv.Listen(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ready)
	if err != nil {
		t.Fatalf("no ready file after Listen: %v", err)
	}
	var rf readyFile
	if err := json.Unmarshal(data, &rf); err != nil {
		t.Fatal(err)
	}
	if rf.URL != srv.URL() || rf.PID != os.Getpid() {
		t.Fatalf("ready file %+v, url %q", rf, srv.URL())
	}
	if _, err := time.Parse(time.RFC3339Nano, rf.StartedAt); err != nil {
		t.Fatalf("started_at: %v", err)
	}
	if !strings.HasPrefix(rf.URL, "http://127.0.0.1:") {
		t.Fatalf("not loopback: %s", rf.URL)
	}
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(ready), ".ready-*")); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}

	// Serve has not started. The port named in the file is already bound, so a
	// connection made now is held in the listener's backlog.
	addr := strings.TrimPrefix(rf.URL, "http://")
	conn, err := dialTCP(addr)
	if err != nil {
		t.Fatalf("ready file names a port that is not accepting: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /nothing HTTP/1.1\r\nHost: "+addr+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Binding the same address again must fail: the listener is still held.
	if second, err := listenTCP(addr); err == nil {
		_ = second.Close()
		t.Fatal("the port was free after Listen: the listener was not kept")
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the queued first request was not served by the same listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ready); !os.IsNotExist(err) {
		t.Fatal("ready file left behind after shutdown")
	}
}

// TestGate11_ListenFailureWritesNoReadyFile: a failed bind leaves no ready file.
func TestGate11_ListenFailureWritesNoReadyFile(t *testing.T) {
	busy, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	ready := filepath.Join(t.TempDir(), "ready.json")
	srv := newBareServer(t, ready, busy.Addr().String())
	if err := srv.Listen(context.Background()); err == nil {
		t.Fatal("expected bind failure")
	}
	if _, err := os.Stat(ready); !os.IsNotExist(err) {
		t.Fatal("ready file written although the bind failed")
	}
}

func TestGate11_NonLoopbackAddrRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.168.1.5:80", ":0"} {
		dir := t.TempDir()
		led, _ := ledger.Open(filepath.Join(dir, "l"), 10, time.Second)
		aw, _ := audit.Open(filepath.Join(dir, "a"))
		_, err := New(Config{
			Authority: &policy.Authority{Concurrency: 1}, Ledger: led, Audit: aw, Addr: addr, CleanupTimeout: time.Second,
		})
		_ = aw.Close()
		if err == nil {
			t.Errorf("%q accepted", addr)
		}
	}
}

// serveProcess is a running `mgw serve` subprocess.
type serveProcess struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	mu     sync.Mutex
	url    string
	waited chan error
}

func buildMGW(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mgw")
	cmd := command("go", "build", "-o", bin, "github.com/aysbg/mgw/cmd/mgw")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// startServe writes a runtime file for one upstream and starts the binary.
func startServe(t *testing.T, bin, upstreamURL string, cleanupMS int) (*serveProcess, ledgerView) {
	t.Helper()
	dir := t.TempDir()
	token := newToken()
	write := func(name string, v any) string {
		data, _ := json.Marshal(v)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cred := filepath.Join(dir, "cred")
	if err := os.WriteFile(cred, []byte(testCredential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("authority.json", map[string]any{
		"token": token, "run_id": testRunID, "attempt": 3, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		"providers": map[string][]string{"openai": {"gpt-6-astra"}}, "budget": map[string]any{"max_tokens": 50000},
		"concurrency": 2, "policy_sha256": "p", "registry_sha256": "r",
	})
	runtime := write("runtime.json", map[string]any{
		"authority_file": "authority.json", "ledger_dir": "ledger", "audit_file": "audit.jsonl", "ready_file": "ready.json",
		"cleanup_timeout_ms": cleanupMS,
		"providers": []provider.Provider{{
			ID: "openai", Dialect: provider.DialectOpenAI, BaseURL: upstreamURL, CredentialFile: "cred",
			Timeouts: provider.Timeouts{ConnectMS: 2000, TTFBMS: 30000, IdleMS: 30000, OverallMS: 60000},
		}},
	})
	p := &serveProcess{stderr: &bytes.Buffer{}, waited: make(chan error, 1)}
	p.cmd = command(bin, "serve", "--runtime", runtime)
	pipe, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			p.mu.Lock()
			p.stderr.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
			lines <- sc.Text()
		}
		close(lines)
		p.waited <- p.cmd.Wait()
	}()
	select {
	case line := <-lines:
		var s struct {
			Serving struct {
				URL string `json:"url"`
				PID int    `json:"pid"`
			} `json:"serving"`
		}
		if err := json.Unmarshal([]byte(line), &s); err != nil || s.Serving.URL == "" || s.Serving.PID != p.cmd.Process.Pid {
			t.Fatalf("first stderr line is not the serving line: %q (%v)", line, err)
		}
		p.url = s.Serving.URL
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not print the serving line")
	}
	// The serving line comes after the ready file.
	data, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	if err != nil {
		t.Fatalf("serving line printed before the ready file existed: %v", err)
	}
	var rf readyFile
	if err := json.Unmarshal(data, &rf); err != nil || rf.URL != p.url || rf.PID != p.cmd.Process.Pid {
		t.Fatalf("ready file %q: %v", data, err)
	}
	return p, ledgerView{dir: dir, token: token}
}

type ledgerView struct{ dir, token string }

func (v ledgerView) totals(t *testing.T) ledger.Totals {
	t.Helper()
	l, err := ledger.Open(filepath.Join(v.dir, "ledger"), 50000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tot, err := l.Totals(testRunID)
	if err != nil {
		t.Fatal(err)
	}
	return tot
}

func (v ledgerView) auditLines(t *testing.T) []audit.Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(v.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (p *serveProcess) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-p.waited:
		return err
	case <-time.After(within):
		t.Fatalf("process still running after %v", within)
		return nil
	}
}

// TestGate11_SIGTERM: SIGTERM lets an in-flight request finish and settle, then
// the process exits 0 within the cleanup timeout.
func TestGate11_SIGTERMSettlesInFlight(t *testing.T) {
	bin := buildMGW(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-release
		okHandler(w, r)
	}))
	defer up.Close()
	const cleanupMS = 3000
	p, view := startServe(t, bin, up.URL, cleanupMS)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		req, _ := newTestRequest(http.MethodPost, p.url+chatPath, strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+view.token)
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode, body: string(b)}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached upstream through the first listener")
	}
	if tot := view.totals(t); tot.Reserved != 100 {
		t.Fatalf("request in flight should hold a reservation: %+v", tot)
	}

	start := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := p.wait(t, 10*time.Second); err != nil {
		t.Fatalf("exit status: %v\n%s", err, p.stderr.String())
	}
	elapsed := time.Since(start)
	if elapsed > cleanupMS*time.Millisecond {
		t.Fatalf("exit took %v, cleanup timeout is %dms", elapsed, cleanupMS)
	}
	r := <-got
	if r.err != nil || r.status != 200 || r.body != usageJSON {
		t.Fatalf("in-flight request: %+v", r)
	}
	if tot := view.totals(t); tot.Settled != 18 || tot.Reserved != 0 {
		t.Fatalf("not settled: %+v", tot)
	}
	recs := view.auditLines(t)
	if len(recs) != 1 || recs[0].UsageStatus != audit.UsageMeasured || recs[0].StatusCode != 200 {
		t.Fatalf("audit: %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(view.dir, "ready.json")); !os.IsNotExist(err) {
		t.Fatal("ready file left behind")
	}
}

// TestGate11_SIGTERMForcesStuckRequest: a request that will not finish is cut
// off at the cleanup timeout, still settled (charge kept as uncertain) and
// audited, and the process still exits 0 inside the timeout.
func TestGate11_SIGTERMForcesStuckRequest(t *testing.T) {
	bin := buildMGW(t)
	entered := make(chan struct{})
	stuck := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-stuck:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	defer close(stuck)
	const cleanupMS = 2000
	p, view := startServe(t, bin, up.URL, cleanupMS)

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		req, _ := newTestRequest(http.MethodPost, p.url+chatPath, strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+view.token)
		if resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("request never reached upstream")
	}
	start := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(t, 10*time.Second); err != nil {
		t.Fatalf("exit status: %v\n%s", err, p.stderr.String())
	}
	if elapsed := time.Since(start); elapsed > cleanupMS*time.Millisecond {
		t.Fatalf("exit took %v, cleanup timeout is %dms", elapsed, cleanupMS)
	}
	<-clientDone
	if tot := view.totals(t); tot.Reserved != 0 || tot.Uncertain != 100 {
		t.Fatalf("stuck request must keep an uncertain charge: %+v", tot)
	}
	recs := view.auditLines(t)
	if len(recs) != 1 || recs[0].UsageStatus != audit.UsageUnknown || recs[0].UpstreamAttempts != 1 {
		t.Fatalf("audit: %+v", recs)
	}
}

func TestGate11_ServeFlags(t *testing.T) {
	bin := buildMGW(t)
	if out, err := command(bin, "serve", "--version").CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "mgw ") {
		t.Fatalf("--version: %v %q", err, out)
	}
	err := command(bin, "serve").Run()
	var ee *exec.ExitError
	if !asExit(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("missing --runtime must exit 2, got %v", err)
	}
	err = command(bin, "serve", "--runtime", filepath.Join(t.TempDir(), "absent.json")).Run()
	if !asExit(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bad runtime file must exit 1, got %v", err)
	}
}
