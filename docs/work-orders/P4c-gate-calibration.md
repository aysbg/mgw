# P4c — gate calibration: top probability, margin, and the ambiguous band

Small, focused order. One rule change plus one new rule, both with captured
fixtures. Do not touch anything else.

Read `internal/decision/client.go`, `internal/decision/gate_test.go` and
`docs/fixtures/systemone-full-real.json` first.

## Why

Measured on one live eight-question response:

| question | choice | top prob | runner-up | margin | reported `confidence` |
|---|---|---|---|---|---|
| change_type | migration | 0.8758 | 0.0463 | 0.8295 | 0.7323 |
| size | M | 0.6607 | 0.2456 | 0.4151 | 0.2584 |

`confidence` is a provider-calibrated score, not the top probability, and its
scale differs per question. Gating a 0.70 floor on it makes `size` impossible to
ever pass, so every real dispatch falls back as `low_confidence`. The layer is
inert.

## Rule 1 — choices gate on top probability and margin

- Accept a choice when `topProbability >= ConfidenceFloor` **and**
  `topProbability - runnerUpProbability >= Margin`.
- Reject as `low_confidence` when the top probability is below the floor.
- Reject as `low_margin` when the margin is below the requirement.
- Keep the reported `confidence` in the Result as telemetry only. It never
  decides. Rename nothing; document in the struct comment that it is reported,
  not gating.

With the numbers above and the defaults (floor 0.70, margin 0.15): both choices
pass. A 0.52/0.48 near-tie still fails on the margin; a 0.55 top with a 0.10
runner-up fails on the floor.

## Rule 2 — the ambiguous band on flags (CTO decision, option B)

For a `noul` answer, with the same floor (0.70):

| probability | outcome |
|---|---|
| `>= floor` | flag set |
| `< 0.40` | flag absent, answer accepted |
| `0.40 <= p < floor` | **the whole classification falls back** with reason `low_confidence_flag` |

The four complexity inputs are then zero and the caller's own complexity is used
unchanged. This applies to each of the five risk flags and to `scout`.

Add `ReasonLowConfidenceFlag = "low_confidence_flag"` to the reason constants and
document it. It is a fallback reason like the others: exit 0, never an error.

Rationale to record in the code comment: below the floor the flag is absent, and
absence of a risk flag is the permissive direction — a schema change at 0.55
would route to a cheap seat. Falling back to the caller's own complexity is more
accurate than either forcing the flag on or silently dropping it.

## Tests

Extend the existing suite; do not rewrite it.

1. `docs/fixtures/systemone-full-real.json` through the gate: type `migration`,
   size `M`, `schema_risk` set, every other flag absent, `scout` false, no
   fallback. This is the end-to-end calibration test.
2. Top probability below the floor → `low_confidence`.
3. Margin below the requirement at an acceptable top probability → `low_margin`.
4. A risk flag at 0.55 → `low_confidence_flag` and all four inputs zero.
5. A risk flag at 0.39 → accepted, flag absent.
6. A risk flag at 0.70 exactly → accepted, flag set (boundary).
7. `scout` at 0.55 → `low_confidence_flag`, same rule.
8. The reported `confidence` value does not change any outcome: two responses
   with identical probabilities but different reported confidence produce the
   same decision.

## Constraints

- Only `internal/decision/client.go` and its tests.
- Standard library only. No dependency.
- Do not change the timeout, the shadow log, the CLI or the config.
- Do not commit or push.
- Run `make check` and paste the tail, plus one live `decide --json` on the
  migration state showing `fallback=false` with `size` populated.

## Acceptance

```
make check
MGW_CONFIG_DIR=<cfg> ./dist/mgw decide --state-file <migration state> --json
```
