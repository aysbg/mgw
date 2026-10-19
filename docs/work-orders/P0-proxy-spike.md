# P0 — proxy spike

Work order for one lane. Read `docs/go-standard.md` and `README.md` first.

## Goal

A working loopback passthrough proxy for one OpenAI-dialect provider and one
Anthropic-dialect provider, with run-scoped token authority, cross-process budget
reservation, explicit commit tracking, byte-exact SSE relay, and a
metadata-only audit record. Everything else stays out.

## Non-goals

- No onboarding, no matrix, no catalog, no decision models, no fidelity probes.
- No harness integration. The supervisor contract is written here; wiring is P5.
- No payload translation, no model-id rewriting, no retries across a commit.
- No `httputil.ReverseProxy` unless the architecture note in step 7 justifies it.

## Deliverables

### 1. `internal/provider/config.go`

Runtime provider definitions, loaded from JSON. No environment variables.

```go
type Dialect string // "openai" | "anthropic" | "openrouter" | "ollama"

type Provider struct {
    ID           string   `json:"id"`
    Dialect      Dialect  `json:"dialect"`
    BaseURL      string   `json:"base_url"`      // scheme+host, no path
    CredentialFile string `json:"credential_file"` // 0600, one line
    AllowedModels []string `json:"allowed_models"`
    Timeouts     Timeouts `json:"timeouts"`
}

type Timeouts struct {
    ConnectMS   int `json:"connect_ms"`
    TTFBMS      int `json:"ttfb_ms"`
    IdleMS      int `json:"idle_ms"`
    OverallMS   int `json:"overall_ms"`
}
```

Reject at load: empty ID, unknown dialect, non-loopback or non-https BaseURL
except an explicit `http://127.0.0.1` test base, empty credential file, zero
timeouts, duplicate ID.

### 2. `internal/policy/token.go`

The authority record is a 0600 JSON file written by whoever starts the gateway.

```json
{
  "token": "<opaque, 32+ bytes, base64url>",
  "run_id": "uuid",
  "attempt": 1,
  "expires_at": "RFC3339",
  "providers": {"openai": ["gpt-6-astra"], "anthropic": ["claude-opus-5-5"]},
  "budget": {"max_tokens": 50000},
  "concurrency": 2,
  "policy_sha256": "...",
  "registry_sha256": "..."
}
```

Rules:

- Compare the presented bearer token with `crypto/subtle` against the record.
- Deny when expired, when the provider is absent, or when the model is not in
  that provider's list. Deny before any upstream call.
- The model comes from the request body (`model` field), read from a bounded
  prefix (first 64 KiB). A body larger than the cap is rejected, not buffered.
- The authorization decision never reads a caller-supplied role, model list or
  provider header.

### 3. `internal/ledger/ledger.go`

Cross-process. In-process mutex alone is a defect.

- State file `ledger.json` guarded by `flock` on `ledger.lock`, updated by atomic
  rename (`write temp in same dir, fsync, rename`).
- `Reserve(id, runID, maxTokens) error` fails with `budget_exhausted` when the
  run's settled plus reserved total would exceed the token budget.
- `Settle(id, runID, usedTokens int64, measured bool) error` releases the
  reservation and adds usage. `measured=false` records the charge as uncertain
  and must not silently drop it.
- All ledger work happens under an explicit cleanup timeout, detached from the
  request context. On timeout return an error the caller records as
  `settle_timeout` with `usage: unknown`.
- Two processes using one ledger must not overspend. Test it with a subprocess.

### 4. `internal/audit/audit.go`

One JSON object per line, append-only, 0600. Field names from an allowlist
constant. Unknown fields cannot be added ad hoc: the writer takes a struct, not a
map.

Required fields: `schema`, `ts`, `request_id`, `run_id`, `attempt`, `provider`,
`model`, `method`, `path`, `upstream_attempts` (count), `commit_state`,
`status_code`, `duration_ms`, `usage_status` (`measured` | `unknown`),
`input_tokens`, `output_tokens`, `cache_read_tokens`, `failure_code`,
`policy_sha256`, `registry_sha256`.

Forbidden: prompt text, completion text, request or response headers, credential
values, raw upstream error bodies.

### 5. `internal/proxy/server.go` and `internal/proxy/relay.go`

- `net/http.Server` bound to `127.0.0.1` only. Bind once. Keep the listener.
  After binding, write the ready file atomically: `{"url","pid","started_at"}`.
- Routes: `/p/{provider}/{native path...}`. Anything else: 404. No `CONNECT`,
  no absolute-form targets, no path traversal after cleaning.
- Reject an unexpected `Host` header and any request carrying `Origin`.
- Reject a request with both `Transfer-Encoding` and `Content-Length`.
- Transport per provider, all four defaults disabled:
  `Proxy: nil`, `DisableCompression: true`, `CheckRedirect` returns
  `http.ErrUseLastResponse`, and no transport-level retry (do not reuse a
  connection after an ambiguous write; open a fresh one).
- Pass `Accept-Encoding` through untouched. Strip hop-by-hop headers
  (`Connection` and anything it names, `Keep-Alive`, `Proxy-*`, `TE`,
  `Trailer`, `Transfer-Encoding`, `Upgrade`). Rewrite `Host` to the upstream.
- Replace `Authorization` (and `x-api-key` for the Anthropic dialect) with the
  provider credential. Never forward the local token upstream.
- Stream the response: read fixed-size chunks, write, flush after each write.
  Never buffer the whole body. Do not exceed a bounded buffer per response.
- Commit tracking is explicit: the flag flips on the status line, the first
  header write, the first flush, or a forwarded 1xx. Branch exactly as:

```
committed?  yes -> propagate, never restart
            no  -> retryable non-acceptance established?
                     yes -> retry once, same request, original deadline
                     no  -> fail, keep the uncertain charge
```

- Retryable non-acceptance means: dial error, connection reset before any bytes
  were written upstream, or an explicit 429/503 with `Retry-After` received
  before commit. Honour `Retry-After` and the overall deadline. Never retry a
  4xx other than 429.
- Client disconnect: cancel upstream, still settle, still write the audit record.
- Panic: before commit, 500 plus `internal_panic`. After commit, terminate the
  response without a second status line and record `internal_panic_after_commit`.

### 6. `cmd/mgw serve`

Flags: `--runtime <file>` (required), `--version`. Prints one JSON line on stderr
after the ready file is written: `{"serving":{"url":...,"pid":...}}`. Exits 0 on
SIGTERM after settling in-flight requests within the cleanup timeout.

### 7. `docs/architecture/proxy-relay.md`

Record the decision: `httputil.ReverseProxy` with `ModifyResponse` and a custom
`FlushInterval`, or a hand-written relay. State which was chosen, why, and what
the other option would have cost in commit tracking. Cite the tests that prove
the choice. This is a decision record, not an essay: half a page.

## Tests — gates

Name each test `GateNN_Name` in `internal/<pkg>/gate_NN_name_test.go`, then mark
the gate `implemented` in `docs/gates.md`.

- **Gate 2 — token authority.** Wrong token, expired token, provider not in the
  record, model not allowed for the provider, body over the cap, concurrent-cap
  breach. Each returns the named failure and makes zero upstream calls.
- **Gate 3 — upstream discipline.** Request to an unlisted provider 404s; a
  redirect from upstream is not followed; `HTTP_PROXY` set in the environment
  does not take effect; the local token never appears in the upstream request;
  hop-by-hop headers are stripped; `Host` is rewritten.
- **Gate 5 — commit boundary.** Pre-connection failure retries once; a 429 with
  `Retry-After` before commit retries; an ambiguous failure after transmission
  before commit does not retry and records `usage: unknown`; a post-commit
  stream failure propagates and is never restarted. Audit fields complete in all
  four cases.
- **Gate 11 — lifecycle.** The ready file appears only after the listener is
  bound; the same listener serves the first request; SIGTERM settles and exits
  within the cleanup timeout.
- **Gate 12 — transport hardening.** Four negative tests, one per default:
  decompression (`Content-Encoding: gzip` response is relayed byte-exact, not
  inflated), environment proxy, redirect, implicit retry.
- **Gate 15 — stream proof.** A fragmented SSE stream (frames split across
  writes), a long stream (>1 MiB), and a slow reader under bounded memory all
  relay byte-exact; each frame is flushed promptly; a client disconnect cancels
  upstream and the handler returns.

Also: a `FuzzParseUsage` target for the usage parser and a `FuzzRoutePath` target
for the path-prefix router, both with seed corpora.

## Acceptance commands

```
make check
go test -race ./internal/... -run 'Gate'
go test -run '^$' -fuzz '^FuzzRoutePath$' -fuzztime 20s ./internal/proxy
```

Evidence to return in the completion manifest: the `make check` tail, the gate
test names with pass status, the architecture note path, and the two-process
ledger test output.

## Constraints

- Standard library only. `golang.org/x/sys` is allowed for flock. Nothing else.
- No new dependency without stopping and reporting.
- Do not touch `docs/go-standard.md`, `Makefile`, `.github/`, or `go.mod`
  except to add `golang.org/x/sys`.
- Do not commit. The hub reviews the diff and commits.
