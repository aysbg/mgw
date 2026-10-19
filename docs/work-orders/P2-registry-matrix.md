# P2 — catalog, registry v2 and the decision table

Work order for one lane. Read `docs/go-standard.md`, `README.md` and
`docs/work-orders/P0-proxy-spike.md` first. P0 owns `internal/proxy`,
`internal/ledger`, `internal/audit`, `internal/provider`. This lane owns
`internal/catalog`, `internal/policy` and the CLI. Do not edit P0's packages.

## Goal

Ingest the live model catalog, merge it with a curated capability file, emit
registry v2, and generate the full decision table — every role × type × size ×
risk × strategy cell, as an **ordered candidate list**. Plus the CLI to print,
explain and write it.

## Non-goals

- No onboarding wizard, no provider probing, no decision models. Those are P3/P4.
- No runtime routing. P1/P5 consume this output.
- No network calls during generation except the catalog fetch, which is a
  separate explicit command with a pinned digest.

## Canonical hashing — exact rules

The harness pins a registry with `sha256` over canonical JSON. Python computes:

```python
json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
```

The Go encoder must reproduce that byte for byte:

- keys sorted by codepoint (UTF-8 byte order is equivalent for valid UTF-8)
- no spaces after `:` or `,`
- no HTML escaping (`SetEscapeHTML(false)`)
- UTF-8 output, no `\uXXXX` for non-ASCII
- integers only. **No floats anywhere in a hashed payload.** Prices are integers
  in micro-USD per 1M tokens. A `float64` round-trip is a defect.
- decode numbers as `json.Number`, never `interface{}` default

Parity fixtures. These are computed with the Python implementation above and
must be asserted in `internal/parity/gate_13_parity_test.go`:

| Fixture | Payload | Expected sha256 |
|---|---|---|
| f1 | `{"a":1,"b":["x","y"],"c":{"d":2}}` | `5fe78e4b0975502d88e204a83ca71ba77590deec6a6788f6d5b884ed0fd96861` |
| f2 | `{"models":[{"id":"ollama-cloud/glm-5.3","tier":"good","role_rank":{"build":20}}],"providers":[{"id":"ollama-cloud","billing":"included_credits"}],"version":"2.0"}` | `cc6a06fa1b43224e54dda5df40bb083613cab88425fed61be213adba4f06743f` |
| f3 | `{"unicode":"café ✓","n":-7,"big":9007199254740993,"bool":true,"null":null}` | `a2cc49f3df2d6f4a8a4482e5a5a50f568b96655ce88638b2dddeb7f85e7681ca` |

f3 exists to catch a `float64` decode: `9007199254740993` loses its last digit
if decoded as a float.

## 1. `internal/catalog/catalog.go`

- `Fetch(ctx, url) ([]byte, string, error)` returns bytes and sha256.
- `Decode` into the subset we need: per provider, per model — `id`, `cost.input`,
  `cost.output`, `cost.cache_read`, `tool_call`, `reasoning`,
  `reasoning_options`, `limit.context`, `modalities.output`, `open_weights`.
- `AgentCapable(m)` — true when `tool_call`, `reasoning`,
  `limit.context >= 100000`, and output modalities are exactly `["text"]`.
- `ProviderMedian(provider)` — median blended price over **agent-capable models
  only**. Blended = `(9*input + 1*output)/10`, integer arithmetic. State the 90/10
  ratio in the CLI header; it changes the ranking.
- Snapshot artifact: `{"captured_at","source_url","source_sha256","blend",
  "population","providers":[{id,median_micro_usd,n}],"models":[...]}`.
  `mgw pricing refresh` writes it; `mgw pricing show` prints it;
  `mgw doctor` warns when `captured_at` is older than 30 days.

## 2. `internal/catalog/capabilities.json`

Curated data. Seed exactly this (ids match the harness registry; prices are not
here — they come from the catalog):

| id | provider | family | tier | roles_allowed | efforts | prohibited |
|---|---|---|---|---|---|---|
| anthropic/claude-opus-5-5 | anthropic | anthropic | top | build,plan,review,advise,explore | low,medium,high,xhigh | — |
| openai/gpt-6-astra | openai | openai | top | build,plan,review,advise,explore | low,medium,high,xhigh | low |
| openai/gpt-6.1-sol | openai | openai | very_good | all five | low,medium,high,xhigh | — |
| anthropic/claude-sonnet-5-5 | anthropic | anthropic | very_good | all five | low,medium,high,xhigh | — |
| ollama-cloud/kimi-k3 | ollama-cloud | moonshot | very_good | plan,review,advise | low,high | — |
| ollama-cloud/glm-5.3 | ollama-cloud | glm | good | all five | low,high | — |
| anthropic/claude-haiku-5-5 | anthropic | anthropic | good | all five | low,medium,high,xhigh | — |
| ollama-cloud/glm-5.3-flash | ollama-cloud | glm | cheap | all five | low,high | — |
| openai/gpt-6-luna | openai | openai | cheap | all five | low,medium,high,xhigh | — |
| ollama-cloud/deepseek-v4.1-flash:1017 | ollama-cloud | deepseek | cheap | all five | low,medium,high,xhigh | — |

`role_rank` per role, 0-100, higher is better. This is curated judgement, not a
derivation:

| id | build | plan | review | advise | explore |
|---|---|---|---|---|---|
| claude-opus-5-5 | 90 | 95 | 95 | 96 | 0 |
| gpt-6-astra | 95 | 95 | 92 | 94 | 0 |
| gpt-6.1-sol | 80 | 82 | 80 | 60 | 0 |
| claude-sonnet-5-5 | 78 | 80 | 85 | 65 | 0 |
| kimi-k3 | 0 | 75 | 72 | 70 | 0 |
| glm-5.3 | 60 | 58 | 40 | 28 | 20 |
| claude-haiku-5-5 | 40 | 38 | 45 | 30 | 25 |
| glm-5.3-flash | 25 | 20 | 0 | 0 | 30 |
| gpt-6-luna | 30 | 25 | 0 | 0 | 28 |
| deepseek-v4.1-flash:1017 | 28 | 22 | 0 | 0 | 35 |

A rank of 0 means the seat is not eligible for that role; `roles_allowed` is the
authority, and a rank of 0 on an allowed role is a validation error.

The `:1017` suffix is part of the model id, not decoration. The catalog lists
`deepseek-v4.1-flash` and `deepseek-v4.1-flash:1017` as separate entries, so an
un-suffixed id silently routes to the older release. 1017 is the current
release: the model card reports Terminal-Bench 4.0 at 42.3 against 31.2,
ProgramBench 29.1 against 20.3, DeepSWE v1.1 at 77.0 against 74.2, and Apex
Bench v1.1 at 72.1 against 64.6, with shorter outputs. Record those numbers in
the entry's `evidence` string.

## 3. `internal/policy/registry.go`

```go
type Registry struct {
    Version        string            `json:"version"`          // "2.0"
    PolicyVersion  string            `json:"policy_version"`    // "strategy-1.0"
    Providers      []ProviderPolicy  `json:"providers"`
    Models         []Model           `json:"models"`
    StrategyTargets map[string]map[string]string `json:"strategy_targets"`
    ProviderPolicy providerPolicyIn  `json:"provider_policy"`
    TieRules       TieRules          `json:"tie_rules"`
    DecisionTable  map[string][]string `json:"decision_table"`
}
```

Validation, each with a named error:

- unknown tier; rank 0 on an allowed role; rank > 0 on a disallowed role
- `roles_allowed` missing a role that appears in the strategy grid
- a model absent from the catalog (`unrated`), which must be listed and never
  routed
- a provider with no billing mode, or a deny entry that leaves a role with no
  eligible seat at the balanced target
- any float in the payload: reject outright

Hash: `RegistryHash(r)` over the canonical encoder.

## 4. Decision table generation

Grid: `role` (5) × `type` (docs, test_only, scaffold, bugfix, feature, refactor,
migration) × `size` (S, M, L) × `risk` (false, true) × `strategy` (cheapest,
balanced, best). 630 cells. Key format:
`<role>|<type>|<size>|<risk>|<strategy>`.

Target tiers:

| role | cheapest | balanced | best |
|---|---|---|---|
| build | cheap | good | best available |
| plan | good | good | top |
| review | good | very_good | top |
| advise | good | very_good | top |
| explore | cheap | cheap | cheap |

`best available` means the highest `role_rank` among seats allowed for the role,
regardless of tier.

`cheapest_min_rank`, pinned data alongside the strategy targets:

| role | min rank |
|---|---|
| build | 26 |
| plan | 50 |
| review | 35 |
| advise | 26 |
| explore | 0 |

This is what makes the worked example hold: `glm-5.3-flash` has build rank 25 and
is excluded from a cheap build, while `deepseek-v4.1-flash:1017` at 28 survives; both
clear the explore floor, so price decides and `glm-5.3-flash` wins there.

Balanced applies the existing complexity ladder: `level = TYPE_TIERS[type] +
(size == L)`, `tier = max(floor, min(top, level))` with
`TYPE_TIERS = {docs:cheap, test_only:cheap, scaffold:good, bugfix:good,
feature:very_good, refactor:very_good, migration:very_good}` and
`floors = {build:cheap, plan:good, review:very_good, advise:top, explore:cheap}`.
Note that `balanced` targets in the table above override the floor where they
differ only when the floor is unreachable; the generated cell records the target
and the achieved tier.

Resolution order inside one cell:

1. candidates at the target tier, allowed for the role, with a non-zero rank,
   ordered by a key that depends on the strategy:

   - `cheapest`: first drop candidates below `cheapest_min_rank[role]`, then
     order by provider preference, then blended price asc, then `role_rank` desc,
     then model id asc. The minimum is what stops "cheapest" from picking a seat
     that cannot do the job; the price key is what makes it cheapest once the
     floor is cleared. Preference first is deliberate: the owner of a prepaid
     subscription asked to spend it before cash.
   - `balanced` and `best`: `role_rank` desc first, then provider preference,
     then blended price asc, then model id asc. Preference never outranks
     capability; it decides between equally capable seats.

   Price is a tie-break, never the primary key. The CTO's worked example is the
   acceptance fixture for this rule; if a change to the ranks or prices breaks
   it, the fixture is right and the change is wrong.
2. if empty, nearest tier with a candidate, preferring the higher tier on a tie
3. at most 4 candidates per cell
4. empty cell is allowed in the table, but `mgw matrix --check` must name every
   empty cell and exit non-zero when `review` or `advise` has no candidate under
   `balanced`

`risk=true` forces the top tier for build and plan, unchanged from the current
router, and records the effort rung for each candidate where the seat supports a
risk rung (xhigh clamped to the seat's highest declared rung, at least high).

The table is **ordered candidates, never authorization**. Write that in the
package doc comment, and name the runtime guards that P5 will apply: writer
family exclusion, risk requirement, deny list, role eligibility, tool grade,
budget, pin match.

## 5. CLI

```
mgw matrix                      print the table for the configured strategy
mgw matrix --strategy best      print one strategy
mgw matrix --all                three strategies side by side with blended price
mgw matrix --explain <cell>     why the candidates won: rank, tier, price, preference
mgw matrix --write              write the pinned registry to the output path
mgw matrix --check              name empty cells and hard failures
mgw pricing refresh             fetch, verify digest, write snapshot
mgw pricing show                print the snapshot with its captured_at
mgw doctor                      catalog staleness, missing CA bundle, unpinned registry
```

Output is plain text, stable and diffable: no colour, no tables with merged
cells. Column widths computed from content.

## 6. Tests

- **Gate 8 — matrix fidelity.** A fixture catalog plus the seed capabilities must
  produce an exact expected table for the Ollama-only install:

  | role | cheapest | balanced | best |
  |---|---|---|---|
  | build | deepseek-v4.1-flash:1017 | glm-5.3 | glm-5.3 |
  | plan | glm-5.3 | glm-5.3 | kimi-k3 |
  | review | glm-5.3 | kimi-k3 | kimi-k3 |
  | advise | glm-5.3 | kimi-k3 | kimi-k3 |
  | explore | glm-5.3-flash | deepseek-v4.1-flash:1017 | deepseek-v4.1-flash:1017 |

  Asserted for the default cell (`feature`, `M`, no risk). Also assert one
  multi-provider fixture where `cheapest` keeps the work on the preferred
  provider while `balanced` reaches for the higher-ranked seat elsewhere.
- **Gate 13 — parity.** The three fixtures above, plus a full registry hash
  round-trip.
- **Gate 7 — routing guards, static half.** A seat with rank 0 on a disallowed
  role is a validation error; a deny list that empties a balanced cell is
  reported by `--check`.
- Fuzz targets: `FuzzDecodeCatalog`, `FuzzTableLookup` (hostile cell keys).

## Acceptance commands

```
make check
go test -race ./internal/catalog/... ./internal/policy/... ./internal/parity/...
go test -run '^$' -fuzz '^FuzzDecodeCatalog$' -fuzztime 20s ./internal/catalog
```

Evidence: `make check` tail, gate test names with pass status, the generated
table for the Ollama-only fixture, and the three parity hashes.

## Constraints

- Standard library only. No dependency may be added.
- Do not edit `internal/proxy`, `internal/ledger`, `internal/audit`,
  `internal/provider`, `docs/go-standard.md`, `Makefile`, `.github/`, or `go.mod`.
- Do not commit or push.
- The catalog fetch is not part of `make check`; tests use a fixture file under
  `internal/catalog/testdata/`.
