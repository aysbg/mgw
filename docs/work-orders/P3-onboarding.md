# P3 — onboarding, provider probes, strategy CLI, engagement profile

Work order for one lane. Read `docs/go-standard.md`, `README.md`,
`docs/issues/REGISTER.md`, `docs/work-orders/P2-registry-matrix.md` first.

This is the phase that makes the gateway usable by someone who is not me.
Until now every run needs a hand-written runtime config. After this, a person
with four API keys types `mgw init` and gets a working install.

## Task 1 — config layout

One directory, `~/.config/mgw/` (override with `MGW_CONFIG_DIR`):

| file | mode | content |
|---|---|---|
| `config.json` | 0600 | schema, providers, strategy, provider_policy, data_class, profile name |
| `credentials/<provider>.key` | 0600 | one line, the API key |
| `registry.json` | 0644 | the pinned registry written by `mgw matrix --write` |
| `pricing.json` | 0644 | the pricing snapshot |
| `profiles/<name>.json` | 0600 | engagement profile overrides |

`config.json` shape:

```json
{"schema":"mgw.config/1",
 "providers":[{"id":"openai","dialect":"openai","base_url":"https://api.openai.com",
               "billing":"pay_per_token","include_stream_usage":true}],
 "strategy":"balanced",
 "provider_policy":{"order":["ollama-cloud","openai"],"deny":[]},
 "data_class":"internal",
 "profile":"default"}
```

Rules:

- The credential **value** never appears in `config.json`. Only the provider id
  and the file path derivation.
- File modes are enforced, not requested: write with 0600, and refuse to read a
  credential file that grants group or other access.
- `MGW_CONFIG_DIR` and `--config` override the directory. A missing config is a
  named failure, never a silent default.

## Task 2 — `mgw init`

Two modes, both first-class:

**Interactive** (stdin is a TTY): ask, in order — which providers to connect,
each key (hidden input), billing shape for each, strategy, provider order, deny
list, data class. Print what will be written before writing it.

**Non-interactive** (stdin is not a TTY, or any prompt flag given): every
question has a flag, and a missing required value is a named failure, never a
prompt.

```
mgw init --provider openai --key-file /path/key --billing pay_per_token \
         --provider ollama-cloud --key-file /path/ollama --billing included_credits \
         --strategy balanced --prefer ollama-cloud,openai --data-class internal \
         --yes
```

Behaviour:

- `--provider` may repeat. `--key-file` reads the key from a file; a key must
  never be accepted as an argv value.
- Validate each key with `mgw providers test` before writing anything. A failed
  validation writes nothing and exits non-zero naming the provider.
- Write `config.json`, the credential files, then generate the registry and the
  pricing snapshot from the live catalog, then print the matrix.
- Idempotent: re-running with the same flags rewrites the same files and does not
  duplicate providers. Re-running with an added provider keeps the existing ones.
- `--yes` skips the confirmation print. Without it, interactive mode asks and
  non-interactive mode prints the plan and exits 0 with "dry run; pass --yes".

## Task 3 — `mgw providers add|list|test`

- `list` prints id, dialect, billing, key file present or missing, and the last
  probe result if any.
- `add` is `init` for one provider, without touching strategy.
- `test <provider>` does three things and reports each:
  1. `GET /models` (or the dialect equivalent) with the key — proves
     authentication;
  2. one minimal non-streaming completion — proves inference;
  3. a **fidelity probe**: one streaming request asking for usage (so the
     injection path is exercised), and one tool-call request.
  Result is a `tool_grade`: `pass` when the tool call comes back well-formed,
  `fail` otherwise. Store it in `config.json` under the provider.
- A provider whose `tool_grade` is `fail` must be reported when `mgw matrix`
  would place it in `build`. Refuse to write a registry that puts a failing
  provider in `build` unless `--allow-ungraded-build` is passed.

Tests use a fake upstream; the real probe is exercised by the operator.

## Task 4 — `mgw strategy set|show`

- `set --strategy cheapest|balanced|best`, `--prefer a,b`, `--deny x,y`,
  `--data-class internal|customer`, `--pin role=provider/model` (repeatable).
- `show` prints the effective values and which source they came from: dispatch
  flag, engagement profile, or install default.
- Unknown strategy, unknown provider and unknown role are named failures.
- A pin that fails admission (role not allowed, rank zero, tool grade) is a
  named failure, not a silent substitution.

## Task 5 — engagement profile

A file the binary reads, one per engagement:

```
mgw --profile /path/engagement.json matrix --all
mgw --profile /path/engagement.json matrix --write
```

`engagement.json` carries `strategy`, `provider_policy`, `data_class`, and an
optional `pins` map. Precedence: dispatch flags > profile > install default.
Every command that uses a profile prints the source of each effective value.
A profile that names a denied provider and a preferred provider for the same id
is rejected.

## Task 6 — `mgw doctor`

Checks, each printed with pass, fail or warn, and a non-zero exit when any check
fails:

- config directory exists and every credential file is 0600
- CA bundle present for TLS verification
- registry pinned and hash matching its file
- pricing snapshot age; warn past 30 days
- catalog reachability, only with `--network`
- a provider whose `tool_grade` is `fail` would serve `build`
- the configured strategy is one of the three values

## Tests

- **Gate 9 — onboarding.** Drive `init` non-interactively into a temp
  `MGW_CONFIG_DIR` with a fake upstream: it writes the expected files with the
  expected modes, refuses a bad key naming the provider, is idempotent across
  two runs, and a subsequent `mgw matrix` works from what it wrote. Also assert a
  key is never written into `config.json`.
- Interactive prompts tested by feeding scripted stdin; assert the question order
  and that a hidden key input is not echoed.
- Strategy precedence: three sources, one winner, printed correctly.
- Profile rejection: conflicting order and deny entries.
- `doctor` fails on a 0644 credential file and on a registry hash mismatch.

## Constraints

- Standard library only. No dependency.
- Do not edit `internal/proxy`, `internal/ledger`, `internal/provider/config.go`
  (beyond what P1 already added), `internal/policy/table.go`,
  `docs/go-standard.md`, `Makefile`, `.github/`, or `go.mod`.
- Do not commit or push.
- No command may print a credential value, ever, including in an error.
- Run `make check` at the end and paste its tail plus the new test names.

## Acceptance

```
make check
MGW_CONFIG_DIR=$(mktemp -d) ./dist/mgw init --provider ollama-cloud --key-file <fixture> \
  --billing included_credits --strategy balanced --yes
MGW_CONFIG_DIR=<same> ./dist/mgw doctor
MGW_CONFIG_DIR=<same> ./dist/mgw matrix --all
```

Evidence: the three commands above with real output on a temp config dir, plus
the gate test names.
