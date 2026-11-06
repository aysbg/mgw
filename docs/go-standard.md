# Go standard

Normative for this repository. A change that violates a rule here is rejected in
review even when the build is green, because not every rule is machine-checkable.

## Toolchain

- Go 1.27.3, pinned with the `go 1.27.3` directive in `go.mod` and in CI.
  Install from the official tarball into `/usr/local/go`; never a distribution
  package with an unknown patch level. Do not add a `toolchain` line: with the
  go directive already at that patch level, `go mod tidy` removes it and
  `make tidy` then fails.
- Bumping the toolchain is its own PR with a changelog line and a full
  `make check`.

## Layout

`cmd/` holds entry points only. Everything else lives under `internal/`, so no
external API is promised by accident. Exported identifiers carry a doc comment.

## Dependencies

- Standard library first. The default answer to a new dependency is no.
- Approved non-stdlib: `golang.org/x/sys`, for file locking only.
- Anything else needs a written justification in the PR, zero transitive
  dependencies outside `golang.org/x/`, and a clean `govulncheck` report.
- No HTTP framework, router, ORM, CLI framework, or logging library beyond
  `log/slog`.
- `go.mod` and `go.sum` are committed. `make tidy` fails on any diff.

## Errors

- Wrap with `%w`; branch with `errors.Is` and `errors.As`.
- Policy rejections are sentinel errors with a stable machine code:
  `malformed`, `unauthorized`, `model_not_allowed`, `provider_not_allowed`,
  `budget_exhausted`, `no_eligible_seat`, `upstream_unavailable`, `commit_lost`.
- No `panic` outside `main` and `init`. A recovered panic before response commit
  returns 500 with `internal_panic`. After commit, the handler terminates the
  response without writing another status line and records
  `internal_panic_after_commit`.
- Upstream error bodies are never logged verbatim. Record the status and a
  hashed correlation id.

## Context and timeouts

- Handlers use `r.Context()`. No `context.Background()` in a handler.
- Settlement and audit use `context.WithoutCancel`, and that context is itself
  bounded by an explicit cleanup timeout. If settlement cannot finish, the
  charge is preserved as `usage: unknown` with `settle_timeout`; it is never
  dropped.
- Every upstream call carries connect, time-to-first-byte, idle-stream and
  overall deadlines. A zero deadline is a defect.
- Client disconnect cancels the upstream request; the handler still settles and
  still writes the audit record.

## Concurrency

- Every mutable structure has one owner goroutine fed by a channel, or is
  immutable after load.
- The ledger is cross-process. An in-process mutex is never sufficient: file
  lock plus atomic rename, tested with two processes against one ledger.
- Per-provider concurrency uses a bounded semaphore sized by policy. Unbounded
  goroutine-per-event is forbidden.
- Every goroutine has a proven termination path. Leak tests run in CI.
- `-race` is mandatory.

## Security

- Listener bound to `127.0.0.1` only. Reject unexpected `Host` and any browser
  `Origin`.
- Token comparison uses `crypto/subtle`.
- Tokens, keys, prompts, completions and headers never appear in logs, audit
  records or error text.
- Body size cap and per-token concurrency cap are enforced before any upstream
  call.
- Secrets are read from a 0600 file outside every lane grant and never exported
  into a child environment.
- `gosec` and `govulncheck` must be clean for a release build.

## Logging and audit

`log/slog` JSON on stderr. Field names come from an allowlist in code; adding a
field is a code change with a review. Audit records are append-only JSON Lines
with a stable schema version.

## HTTP transport

Go's defaults are wrong for a policy gateway. All four are disabled explicitly
and each has a negative test:

| Default | Required |
|---|---|
| Automatic decompression | `DisableCompression: true`; `Accept-Encoding` passed through untouched |
| Environment proxy | `Proxy: nil`, never `ProxyFromEnvironment` |
| Redirect following | `CheckRedirect` returns `http.ErrUseLastResponse` |
| Implicit transport retry | Retries only through the gateway's pre-commit path; every upstream attempt audited |

Also: hop-by-hop headers stripped, `Host` rewritten, requests with both
`Transfer-Encoding` and `Content-Length` rejected.

## Testing

- Table-driven; `httptest` fake upstreams per dialect.
- Golden files for byte-exact relay, including fragmented SSE and a slow reader.
- Fuzz targets: SSE frame parser, usage parsers, registry decode, candidate
  lookup.
- Injected clock; no sleeps in unit tests.
- Every gate in `docs/gates.md` maps to `GateNN_Name` in
  `internal/<pkg>/gate_NN_name_test.go`. Coverage percentage is not a gate.
- Test evidence is command output, not a claim.

## Build and release

- `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=true`, `-ldflags "-s -w -X
  main.version=… -X main.commit=…"`.
- Reproducible: two builds of one commit produce identical bytes.
- v1 targets `linux/amd64`. macOS and Windows follow.
- Release: binary per target, `SHA256SUMS`, signature published out of band.
- A static binary still needs a CA bundle on the host; `mgw doctor` checks it.

## Review rules versus automated checks

| Rule | Enforced by |
|---|---|
| Formatting, vet, lint | `make check` |
| Race, fuzz crashes, leaks | `make check` |
| Vulnerabilities, static security findings | `govulncheck`, `gosec` |
| New dependency and transitive policy | Review, with a vuln report |
| One-owner-per-structure | Review plus leak tests; `-race` does not prove ownership |
| Audit field allowlist | Review plus a rejection test |
| No content in logs | Review plus a redaction test |
| Commit and admission correctness | Named gate tests |
