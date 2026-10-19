# P1 — provider plane: stream usage, disconnect truth, dialects, budget gate

Work order for one lane. Read `docs/go-standard.md`, `README.md`,
`docs/issues/REGISTER.md` and `docs/work-orders/P0-proxy-spike.md` first. P0 owns
`internal/proxy`, `internal/provider`, `internal/ledger`, `internal/audit` and
this lane extends them. P2 owns `internal/catalog`, `internal/policy/table.go`
and `internal/cli` — do not edit those.

Two blockers are in scope and must close here: MGW-001 and MGW-002.

## Task 1 — MGW-001: stream usage, the sanctioned payload exception

CTO decision 2026-10-19, recorded in the plan at section 3.3. Implement exactly
that shape, no wider:

1. Add `include_stream_usage *bool` to the provider config, `json:"include_stream_usage"`.
   When nil, the default depends on dialect: true for `openai`, `ollama`,
   `openrouter`; false for `anthropic`.
2. When the flag resolves true, the request body is a JSON object with
   `"stream": true`, and `stream_options` is absent, add
   `"stream_options":{"include_usage":true}`.
3. Preserve a caller-supplied `stream_options` untouched, including
   `include_usage: false`. Never overwrite.
4. Touch nothing else in the body. Non-JSON bodies, non-object bodies, bodies
   over the existing cap, and `stream` absent or false pass through unchanged.
5. The rewritten body must still stream: do not buffer the whole body to rewrite
   it. Read up to the existing body cap, rewrite if needed, and send the
   `Content-Length` of the rewritten body. If the body exceeds the cap the
   request is rejected as today.
6. Record `stream_usage_injected` (bool) in the audit record. Add it to the
   allowlist and to `Record`, keeping the existing test that the allowlist and
   the struct agree.
7. The usage scanner must read usage from the final SSE chunk where dialects
   report it: OpenAI-dialect `usage` on a chunk with empty `choices`, Anthropic
   `message_delta` with `usage.output_tokens` plus the earlier
   `message_start` `usage.input_tokens`. When usage is found, `usage_status` is
   `measured` and the ledger settles the measured total.
8. A provider that answers 400 to the injected field: record the failure, do not
   silently retry without the field. The fix, if needed, is the provider's
   `include_stream_usage: false` in config.

Tests, named and behavioural, with a fake SSE upstream:

- injection happens once and exactly when the rules allow it;
- a caller-supplied `stream_options` is preserved byte for byte;
- `include_stream_usage: false` disables it;
- anthropic dialect never injects;
- the injected field reaches the upstream (assert on the upstream's received
  body, not on the gateway's internal state);
- usage parsed from an SSE stream settles the ledger as measured;
- a stream with no usage chunk records `usage: unknown` and preserves the
  uncertain charge.

## Task 2 — MGW-002: a completed stream is not a disconnect

Right now a client that closes after the last event records
`failure_code: client_disconnect` on a successful run. Fix the distinction:

- The relay knows when the upstream body reached EOF. Track it.
- Upstream EOF reached, then a client write fails → the request succeeded. No
  failure code; usage stays as parsed; the audit record says committed.
- Client write fails before upstream EOF → `client_disconnect`, uncertain charge
  preserved, reservation released as uncertain.
- Client disconnect before response headers → `client_disconnect`, no commit.

Tests: both outcomes against a fake upstream, asserting the audit record and the
ledger state. The existing G15 client-disconnect test must be extended, not
replaced, and must now assert the success case explicitly.

## Task 3 — dialects

Add `openrouter` and `ollama` to the provider dialect set. All four are
passthrough; the dialect decides two things only:

| dialect | auth header | include_stream_usage default |
|---|---|---|
| openai | `Authorization: Bearer` | true |
| openrouter | `Authorization: Bearer` | true |
| ollama | `Authorization: Bearer` | true |
| anthropic | `x-api-key` | false |

Reject an unknown dialect at config load. Keep `base_url` handling as it is.

## Task 4 — Gate 4: budget reservation through the proxy

`internal/ledger` has the primitives and a two-process test. Add
`internal/proxy/gate_04_budget_test.go` proving the proxy-level contract:

- a request reserves before dispatch and the reservation is visible in the
  ledger while in flight;
- a request that settles releases the reservation and adds the measured total;
- a request that fails after transmission keeps an uncertain charge;
- a request rejected by policy never reserves;
- two concurrent requests against a budget that fits only one: exactly one is
  admitted, the other fails `budget_exhausted` and makes zero upstream calls.

Mark G4 `implemented` in `docs/gates.md` when the file passes.

## Constraints

- Standard library only. No dependency.
- Do not edit `internal/catalog`, `internal/policy/table.go`,
  `internal/cli/matrix.go`, `internal/cli/pricing.go`, `docs/go-standard.md`,
  `Makefile`, `.github/`, or `go.mod`.
- Do not commit or push.
- Every new behaviour gets a test named after the behaviour. Gate tests keep the
  `GateNN_Name` convention.
- Run `make check` at the end and paste its tail, plus the new test names.

## Acceptance

```
make check
go test -race ./internal/proxy ./internal/provider ./internal/ledger -run 'Gate'
```

Evidence to return: the `make check` tail, the gate test names, the upstream-body
assertion showing `stream_options` arrived, and the audit record from a
successful streaming run showing `stream_usage_injected` and a measured usage.
