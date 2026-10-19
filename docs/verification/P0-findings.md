# P0 verification findings

Tracked as MGW-001 and MGW-002 in `docs/issues/REGISTER.md`, which is the
canonical ledger. This file holds the evidence.

Verification by the hub against the merged P0 tree, 2026-10-19. The lane's own
report was not taken as evidence; every claim below was reproduced here.

## What passed

- `make check` green: lint clean, `-race` tests, active fuzzing at 1.1 M
  executions, `govulncheck` clean, `gosec` clean, reproducible build.
- Six gates implemented and honest in `docs/gates.md`: G2, G3, G5, G11, G12,
  G15. Each is behavioural, not a field assertion: the redirect target is never
  contacted, the environment proxy never receives a connection, a gzip body is
  relayed byte-exact, a hidden transport retry is counted at the upstream.
- Cross-process ledger proves no overspend: four OS processes, 2400 tokens
  requested against a 1000 budget, exactly 100 grants.
- Real provider call through the proxy: HTTP 200, measured usage, commit state
  committed, reservation settled from 32 to 0 and 49 tokens billed.

## Real dispatch through the proxy

`opencode run --model mgw/glm-5.3-flash` with a custom OpenAI-compatible
provider pointing at the gateway returned `OK`. So a coding agent completes a
dispatch through the gateway with no provider key in its configuration.

## Two defects found

### D1 — streaming usage is unknown for the primary client (MGW-001)

Audit record from that successful opencode dispatch:

```json
{"model":"glm-5.3-flash","status_code":200,"usage_status":"unknown",
 "input_tokens":null,"output_tokens":null,"failure_code":"client_disconnect"}
```

The non-streaming curl call in the same session measured usage correctly
(17 in / 32 out). opencode streams, and a streaming response carries usage only
when the request asks for it, so the gateway has nothing to read.

Consequence: the headline benefit — authoritative token accounting — does not
hold for the dominant traffic pattern. The budget also degrades: the ledger
carries `uncertain: 34000` because each reservation is preserved as an uncertain
charge.

Options, to be decided in P1:

1. Add `stream_options: {"include_usage": true}` to OpenAI-dialect streaming
   requests. This is a payload edit, which the passthrough rule forbids, but it
   is additive and provider-standard.
2. Leave it unknown and label it. Honest, but the accounting claim weakens.
3. Ask the client to request usage. opencode offers no config switch for this
   that we have verified.

### D2 — a completed stream is recorded as a client disconnect (MGW-002)

Both opencode requests ended `failure_code: client_disconnect` while the client
received a complete answer and printed `OK`. After the final event the client
closes the connection, and the relay's next write fails; the relay treats that as
a disconnection.

Consequence: every successful agent run carries a failure code in the audit and
would read as incomplete to `result`. The audit must distinguish:

- client left before the stream finished — a real disconnect
- client closed after the upstream stream was fully relayed — a normal end

The relay knows the difference: it has seen the upstream EOF and the completion
signal. `client_disconnect` must only be recorded when the upstream stream did
not finish.

## Carried into P1

- D1 and D2, each with a gate test.
- Streaming usage needs a dialect-aware scanner test with a real SSE fixture.
- The ledger must not leave an uncertain charge when the stream completed.
