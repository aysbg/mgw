# P4a — decision client and gate

Work order for one lane. Part A of P4; part B (CLI, shadow log, config, doctor)
is a separate order and must not be started here.

Read `docs/go-standard.md` and `README.md` first. Read only the files named here.

## What this is

A client for the typed-decision (`systemone`) interface plus the gate that
decides whether an answer may be used. The gate is the point: the client is
plumbing.

It fills four complexity inputs — `type`, `size`, `risk_flags`, `scout`. It never
selects a seat, never sees the strategy, and never authorises anything.

## `internal/decision/client.go`

```go
type Config struct {
    Backend         string        // "ollama" | "jev"
    BaseURL         string        // e.g. https://ollama.com
    Model           string        // e.g. clef-flash
    KeyFile         string        // 0600, one line
    Timeout         time.Duration // default 2s
    ConfidenceFloor float64       // default 0.70
    Margin          float64       // default 0.15
}

type Result struct {
    Type            string   // one of the seven change types
    Size            string   // S, M, L
    RiskFlags       []string // subset of schema,billing,auth,privacy,concurrency
    Scout           bool
    Confidence      float64
    Backend         string
    DecisionModel   string   // the MODEL THAT ANSWERED, e.g. clef-flash
    LatencyMS       int64
    Fallback        bool
    FallbackReason  string
}
```

`DecisionModel` is the answering model, which is not a routing seat and carries
no strategy. The struct must contain no field named or meaning `seat`, `strategy`
or a routed model id; a test asserts the JSON keys.

`Classify(ctx, state string) (Result, error)`:

- POST `{BaseURL}/v1/systemone` with `{"model":…,"state":…,"questions":{…}}`.
- The questions are fixed: one choice for `change_type` with the seven criteria
  (docs, test_only, scaffold, bugfix, feature, refactor, migration), one choice
  for `size` (S, M, L), five `noul` questions (schema_risk, billing_risk,
  auth_risk, privacy_risk, concurrency_risk) and one `noul` `needs_scout`.
- Hard timeout on the whole call. A timeout is a **fallback**, not an error to the
  caller.
- The key is read from `KeyFile` and never logged or returned.

## The gate

Applied to the parsed response, in this order. Every failure sets
`Fallback=true` and a machine reason from
`missing_answer | bad_label | low_confidence | low_margin | timeout | transport`.

1. Every requested answer present and of the expected type.
2. A choice answer must be one of the declared criteria. Never accept a label the
   caller did not offer.
3. Choice accepted only when `confidence >= floor` **and**
   `topProbability - runnerUpProbability >= margin`.
4. A `noul` flag is true only when its probability `>= floor`; below that it is
   absent, never guessed.
5. `Scout` follows the same rule as a flag.
6. After parsing, assert deterministically: type in the seven, size in S/M/L,
   risk flags a subset of the five known names. A failure here is `bad_label`
   and the whole result falls back.
7. On any fallback the caller's own complexity is used unchanged. The client
   returns the fallback Result; it never returns a half-applied answer.

## Tests (`internal/decision`)

Fake `systemone` server, no network, no model:

1. High-confidence answer → accepted, all four inputs set.
2. Confidence below floor → `low_confidence`.
3. Top and runner-up within the margin → `low_margin`.
4. Choice outside the declared criteria → `bad_label`.
5. Missing answer → `missing_answer`.
6. Wrong answer type for a question → `missing_answer`.
7. Server sleeps past the timeout → `timeout`, not an error.
8. Server closes the connection → `transport`.
9. Flag probability below floor → flag absent, answer still accepted.
10. **Order permutation**: the same state with `change_type` criteria in three
    different orders must give the same label at high confidence, or a
    `low_margin` fallback. Never average, never silently pick.
11. **Hostile response fed directly to the gate**: a crafted answer claiming
    `change_type: "deploy"` with probability 1.0, and one claiming
    `size: "XL"`, must both be rejected as `bad_label`.
12. The key value never appears in any output, log line or error string —
    assert by searching the captured output for the fixture key.
13. The `Result` JSON has no `seat`, `strategy` or routed-model field.

## Constraints

- Standard library only. No dependency.
- Do not edit `internal/proxy`, `internal/ledger`, `internal/policy`,
  `internal/catalog`, `internal/cli`, `docs/go-standard.md`, `Makefile`,
  `.github/`, or `go.mod`.
- Do not add the CLI, the shadow log, the config block or the doctor line.
- Do not commit or push.
- Run `make check` at the end and paste its tail plus the new test names.
