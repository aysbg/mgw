# P5a — `mgw authority`: emit the per-attempt runtime and token

Small order. One subcommand, one test file.

Read `README.md`, `internal/config/config.go`, `internal/proxy/runtime.go` and
`internal/policy/token.go` first.

## Why

The harness needs to start a gateway for one attempt. It must not duplicate the
gateway's knowledge of providers and credentials, so `mgw` emits both files from
its own config.

## Command

```
mgw authority --route <route.json> --out <dir> [--deadline-s 900]
```

`route.json` is what the harness already has:

```json
{"run_id":"uuid","attempt":1,"budget_max_tokens":200000,
 "primary":"ollama-cloud/glm-5.3#high",
 "chain":["ollama-cloud/glm-5.3#high","openai/gpt-6.1-sol#high"],
 "provider_order":["ollama-cloud","openai"]}
```

Output, in `--out`:

**`runtime.json`** — the file `mgw serve --runtime` reads:

- `authority_file` → `authority.json` in the same directory;
- `providers` — only the providers named in `primary` and `chain`, taken from
  `config.json`: id, dialect, base_url, `credential_file` resolved to the
  absolute credential path, `allowed_models` = the models the route names for
  that provider, `include_stream_usage` from config;
- `ledger_dir` → `ledger` under `--out`, created 0700;
- `audit_file` → `audit.jsonl`, `ready_file` → `ready.json`;
- `cleanup_timeout_ms` 5000, `default_reserve_tokens` 2000.

**`authority.json`** — 0600:

- `token` — 32 random bytes, base64url without padding;
- `run_id`, `attempt` from the route;
- `expires_at` = now + `--deadline-s` + 60 seconds, RFC3339 UTC;
- `providers` = provider → allowed model ids, from the route;
- `budget.max_tokens` = the route's `budget_max_tokens`;
- `concurrency` = 2;
- `policy_sha256` and `registry_sha256` = the route's values when present, else
  64 zeros. Do not invent hashes; pass through what the caller pinned.

Both files 0600. `--out` is created if absent.

One JSON line on stdout:

```json
{"out":"/abs/dir","runtime":"/abs/dir/runtime.json","authority":"/abs/dir/authority.json","expires_at":"…"}
```

## Refusals, named

- a provider in the route with no entry in `config.json` →
  `provider_not_configured`
- a provider whose credential file is missing →
  `credential_missing`
- a model in the route not among that provider's `allowed_models` in
  `config.json` → `model_not_allowed`
- `--out` not writable, or a file already present with looser than 0600 →
  `out_not_private`
- a route with no `primary` or no `run_id` → `route_malformed`

Exit 2 for each, with the name in the message. Never print a credential value.

## Tests

1. A two-provider route emits both files with only those providers, and the
   provider that appears only in `chain` is included.
2. Token: base64url, length >= 43, unique across two runs.
3. `expires_at` is between `now+deadline` and `now+deadline+120s`.
4. `budget.max_tokens` and the provider→models map match the route.
5. `credential_file` in runtime.json is absolute and exists.
6. Each refusal above, asserting the named code and that nothing was written.
7. No output line contains the credential value (search captured output).

## Constraints

- Standard library only. No dependency.
- Only `internal/cli/` (a new `authority.go`), a small addition to `cmd/mgw`, and
  tests. Do not edit `internal/proxy`, `internal/policy`, `internal/decision`,
  `internal/catalog`, `docs/go-standard.md`, `Makefile`, `.github/`, `go.mod`.
- Do not commit or push.
- `make check` green.

## Acceptance

```
make check
MGW_CONFIG_DIR=<cfg> ./dist/mgw authority --route <route.json> --out /tmp/att-1
ls -l /tmp/att-1
```
