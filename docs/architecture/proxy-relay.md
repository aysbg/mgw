# Proxy relay: decision record

Status: accepted for P0.

## Decision

A hand-written relay (`internal/proxy/relay.go`) on `net/http.Client`, not
`httputil.ReverseProxy` with `ModifyResponse` and a custom `FlushInterval`.

## Why

The gateway must know, at every moment, whether the response is committed, and
it must branch on that: committed means propagate and never restart; not
committed means retry once only when non-acceptance is established.

- The relay owns the commit flag. `trackedWriter` flips it on a status line, a
  forwarded 1xx, the first body write or the first flush. The retry decision, the
  stream copy and the abort all read that one flag.
- With `ReverseProxy`, the commit happens inside `ServeHTTP`, after
  `ModifyResponse` returns. A retry before commit would need a retrying
  `RoundTripper` wrapped around the transport, and a 429 with `Retry-After` is a
  successful round trip, so `ModifyResponse` could not re-dispatch it. The
  commit flag would be inferred from a wrapped `ResponseWriter` in one place and
  from the transport wrapper in another.
- `ReverseProxy` reports a failed copy after commit by panicking with
  `http.ErrAbortHandler`. The Go standard forbids `panic` outside `main` and
  `init`. The relay aborts by hijacking and closing the connection, so the caller
  sees a failed stream and no second status line.
- `ReverseProxy` adds `X-Forwarded-For` and edits hop-by-hop headers by its own
  rules. The relay keeps an explicit header allowlist per direction.

The cost of the choice: about 200 lines of relay we own, including the
hop-by-hop table and the 1xx forwarding, and no free upgrade when `net/http`
changes `ReverseProxy`. `FlushInterval: -1` would have given per-write flushing
for free; here it is one `Flush` call after each `Write`.

## Consequences

- One fresh upstream connection per attempt (`DisableKeepAlives`, HTTP/2 off).
  This is how "no reuse after an ambiguous write" and "no implicit retry" hold.
  It costs a dial and TLS handshake per request, which does not matter for an
  inference gateway.
- `net/http` removes `Content-Length` when `Transfer-Encoding` is present, so a
  handler cannot see a request that carries both. The gateway therefore refuses
  every request with `Transfer-Encoding` (400 `malformed`). Callers send
  `Content-Length` bodies, capped at 64 KiB.
- Usage is read from the stream in bounded memory (`provider.UsageScanner`). When
  it finds none, usage is `unknown` and the reservation stays as an uncertain
  charge.

## Tests that prove it

| Claim | Test |
|---|---|
| Pre-connection failure retries once, then fails | `TestGate05_PreConnectionFailureRetriesOnce` |
| 429/503 with `Retry-After` retries once; nothing else does | `TestGate05_RetryAfterBeforeCommit` |
| Ambiguous failure after transmission is not retried; usage unknown | `TestGate05_AmbiguousFailureNotRetried` |
| Post-commit failure propagates and is never restarted | `TestGate05_PostCommitFailurePropagates` |
| Panic before and after commit | `TestGate05_PanicHandling` |
| Four transport defaults are off | `TestGate12_*` |
| Fragmented, long and slow-reader streams are byte-exact, flushed promptly, memory bounded | `TestGate15_*` |
| Client disconnect cancels upstream, settles, audits | `TestGate15_ClientDisconnectCancelsUpstream` |

## Supervisor contract (`mgw serve --runtime <file>`)

The runtime file is JSON, unknown fields rejected, relative paths resolved
against the file's directory: `authority_file` (0600 authority record),
`providers` (array of provider definitions), `ledger_dir`, `audit_file`,
`ready_file`, `cleanup_timeout_ms`, optional `default_reserve_tokens`
(reserved when a request declares no output cap; default 4096). After binding,
the gateway writes `ready_file` atomically, then prints
`{"serving":{"url":...,"pid":...}}` on stderr. SIGTERM or SIGINT stops accepting,
lets requests finish for half the cleanup timeout, cancels the rest, settles and
audits every request, removes the ready file and exits 0.
