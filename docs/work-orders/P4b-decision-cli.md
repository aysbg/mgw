# P4b — decision CLI, shadow log, config, doctor

Work order for one lane. Part B of P4. Part A (client and gate) is merged;
extend it, do not redesign it.

Read `docs/go-standard.md`, `README.md`, `docs/work-orders/P4a-decision-gate.md`
and `internal/decision/client.go` first.

## Task 1 — `mgw decide`

```
mgw decide --state-file <path> [--backend ollama|jev] [--json] [--shadow]
```

- Reads the state from the file (never from argv).
- Builds a Config from the `decision` block in `config.json`, with flags
  overriding backend and model.
- Prints, plain: type, size, risk flags, scout, confidence, backend, the
  decision model, latency, and on a fallback the reason.
- `--json` prints the `decision.Result` as JSON.
- `--shadow` appends one record and prints only a single line
  `shadow: recorded`.
- Exit codes: 0 on a decided or fallback result (a fallback is not an error),
  2 on a usage error, 3 when the decision layer is disabled or unconfigured.

## Task 2 — shadow log

- Path `~/.config/mgw/decisions.jsonl`, mode 0600, append-only, one JSON object
  per line:

```json
{"ts":"…","state_sha256":"…","state_excerpt":"…","type":"…","size":"…",
 "risk_flags":[],"scout":false,"confidence":0.83,"backend":"ollama",
 "decision_model":"clef-flash","latency_ms":420,"fallback":false,
 "fallback_reason":"","policy_version":"decision-1.0"}
```

- `state_excerpt` is at most 300 characters, taken from the start, with newlines
  collapsed. The full state is never stored.
- The file is created 0600 even if the directory already exists with other
  modes; a pre-existing file with looser permissions is an error, not a silent
  chmod.
- The API key must never appear in the record; a test asserts by searching the
  file for the fixture key.

## Task 3 — config block

Add to `config.json` (and to the type that reads it):

```json
"decision":{"enabled":false,"backend":"ollama","base_url":"https://ollama.com",
            "model":"clef-flash","key_file":"credentials/decision.key",
            "timeout_ms":2000,"confidence_floor":0.70,"margin":0.15}
```

- Absent block means disabled, and `mgw decide` exits 3 with a named reason.
- Validation on load: backend in {ollama, jev}; timeout 100–10000 ms; floor and
  margin in (0,1); floor+margin <= 1 or the config is rejected with that reason.
- `mgw init` does not ask about it. `mgw decide` and `doctor` are the only
  readers.

## Task 4 — doctor line

`mgw doctor` reports the decision layer:

- `ok decision disabled` when the block is absent or `enabled:false`;
- `ok decision clef-flash via ollama` when enabled and the key file is readable;
- `fail decision key file missing: <path>` when enabled without a key;
- `warn decision shadow log is world-readable` when the log exists with looser
  permissions than 0600.

## Tests

1. `decide --json` with a fake systemone server returns the gate's result and
   exit 0, including the fallback case.
2. `decide --shadow` writes exactly one line, mode 0600, with the bounded
   excerpt and no key value.
3. A pre-existing 0644 decisions file makes `--shadow` fail with a named reason.
4. Disabled block → exit 3 and no network call.
5. Config validation: bad backend, out-of-range timeout, floor+margin > 1 each
   rejected with the reason.
6. Doctor: the four states above.
7. Gate 10 is not touched: this phase does not add assurance logic.

## Constraints

- Standard library only. No dependency.
- Do not edit `internal/proxy`, `internal/ledger`, `internal/policy`,
  `internal/catalog`, `internal/decision/client.go` (except to export what the
  CLI needs), `docs/go-standard.md`, `Makefile`, `.github/`, `go.mod`.
- Do not call the decision layer from any routing path.
- Do not commit or push.
- Run `make check` at the end and paste its tail plus the new test names.

## Acceptance

```
make check
./dist/mgw decide --state-file <fixture> --shadow
ls -l ~/.config/mgw/decisions.jsonl
./dist/mgw doctor
```

Plus one live classification through `clef-flash` on ollama-cloud with its
latency and confidence, if a key is configured.
