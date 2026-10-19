# P4d — per-question degradation (option B')

Supersedes the whole-result fallback in P4c. Small change, three rules, one
required question and two best-effort ones.

Read `internal/decision/client.go`, `internal/decision/gate_test.go` and
`docs/fixtures/systemone-full-real.json` first.

## Why

Three live states measured: `change_type` is reliable (top 0.85–0.94, margin
0.79–0.93). `size` is not — on realistic tasks it returns M or L at 0.477–0.487
with a margin of ~0.03. And one ambiguous flag (schema at 0.607) discarded a
confident 0.854 type call. Discarding good signal to report one uncertain
dimension is the wrong trade.

## Semantics

### `type` is required

If `change_type` cannot be accepted (missing, mistyped, unoffered label, below
the choice floor, or below the margin), the whole classification is a fallback
with the existing reasons. The caller then supplies its own complexity, as today.

### `size` is best-effort

If `size` fails its floor or margin, the classification is **not** a fallback:

- `Size` is left empty and a new field `SizeFallback` is `true`;
- `Fallback` stays `false` and `FallbackReason` stays empty;
- the type and the flags are kept.

The caller fills size from its own complexity input. This is the normal path on
non-trivial work, not an error, and must not be logged as one.

### Flags are tri-state and fail-safe

For each of the five risk flags and for `scout`, with the 0.70 floor:

| probability | outcome |
|---|---|
| `>= 0.70` | flag set |
| `< 0.40` | flag absent |
| `0.40 <= p < 0.70` | flag **set** AND recorded in a new `AmbiguousFlags` list |

An ambiguous flag is treated as risk for routing, because absent is the
permissive direction. It is never a reason to discard the classification.

`scout` follows the same rule: an ambiguous `scout` sets `Scout = true` and adds
`"scout"` to `AmbiguousFlags`.

### Result shape

Add:

```go
AmbiguousFlags []string // flags in the 0.40..0.70 band, treated as set
SizeFallback   bool     // size rejected; caller supplies it
```

`ReasonLowConfidenceFlag` is no longer produced. Remove it from the constants and
from the config documentation if it is documented there.

## Tests

Replace the ambiguity-band tests from P4c, keep everything else.

1. Captured eight-question fixture: type `migration`, size `M`, `schema_risk` set,
   no ambiguous flags, `SizeFallback` false, no fallback.
2. Type accepted, size at top 0.48 / margin 0.03 → no fallback, `Size` empty,
   `SizeFallback` true, type preserved.
3. Type below the floor → fallback `low_confidence`, all inputs zero.
4. `schema_risk` at 0.607 → flag set, `AmbiguousFlags` contains `schema`, no
   fallback, type preserved.
5. `schema_risk` at 0.39 → flag absent, no ambiguity.
6. `schema_risk` at 0.70 exactly → flag set, not ambiguous.
7. `schema_risk` at 0.40 exactly → flag set and ambiguous.
8. `scout` at 0.55 → `Scout` true, `AmbiguousFlags` contains `scout`.
9. Reported `confidence` still changes nothing: identical probabilities with
   different reported confidence produce identical decisions.
10. A hostile label is still `bad_label` and still prevents the whole result.

## CLI

- `mgw decide` plain output gains `ambiguous_flags:` and `size_fallback:` lines.
- JSON already carries them through the Result.
- `fallback_reason` is empty on a size-only degradation; do not print a reason
  for a normal degraded size.

## Constraints

- Only `internal/decision/client.go`, its tests, `internal/cli/decide.go` and
  `internal/cli/decide_test.go`. Nothing else.
- Standard library only. No dependency.
- Do not commit or push.
- `make check` must be green.

## Acceptance

```
make check
MGW_CONFIG_DIR=<cfg> ./dist/mgw decide --state-file <migration state> --json
```

The live run must show `fallback:false`, `type:migration`, `risk_flags:[schema]`
and either a size or `size_fallback:true` — both are acceptable outcomes, and
you must paste what actually happened.
