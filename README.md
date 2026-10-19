# mgw — model gateway

A loopback HTTP gateway that holds provider credentials and gives coding agents
bounded inference authority. Separate from the Python executor harness, which
talks to it only over HTTP and two JSON artifacts.

Status: project foundation. No gateway behaviour is implemented yet; every
subcommand except `version` exits 3.

## Why Go

Distribution to client hosts, byte-exact streaming relay, and a single static
artifact. Not throughput. See `docs/go-standard.md`.

## Build

```
make tools     # govulncheck, gosec, golangci-lint
make check     # format, vet, lint, tidy, race tests, fuzz, vuln, sec, build, repro, gates
make build     # dist/mgw
./dist/mgw version --json
```

Go 1.27.2 is pinned in `go.mod`. Install it from the official tarball, not a
distribution package.

## Layout

```
cmd/mgw          entry point: flags, subcommand dispatch, exit codes
internal/cli     init, providers, strategy, matrix, pricing, doctor
internal/config  install layout: config.json, credential files, engagement profiles
internal/proxy   server, auth, route selection, relay, commit tracking
internal/provider dialect handlers, usage parsers, error mapping
internal/policy  registry load, candidate lists, hard guards
internal/ledger  cross-process reserve and settle
internal/catalog models.dev ingest, capability merge, pricing snapshot
internal/decision optional Clef and Jev adapter
internal/audit   metadata-only audit records
docs/gates.md    gate to test mapping
```

## Provider base URLs

A provider `base_url` can carry an API path prefix. The gateway preserves it
verbatim: `https://openrouter.ai/api` plus `/v1/chat/completions` becomes
`https://openrouter.ai/api/v1/chat/completions`. Host-root bases also work.
Use HTTPS, with no trailing slash, query string, fragment, userinfo, `..` path
segment, or double slash in the path. Only `http://127.0.0.1[:port]` is allowed
for HTTP tests. See [runtime configuration](docs/runtime.md#provider-base-urls).

## Decision layer

`mgw decide` asks a typed-decision model (Clef via Ollama Cloud, or Jev) for the
complexity inputs the router takes. What it supplies reliably, measured on live
responses:

| input | reliability | when it degrades |
|---|---|---|
| `type` | high (top probability 0.85–0.94, margin 0.79–0.93) | a rejected type falls the whole classification back to the caller |
| risk flags | high, and fail-safe | a flag between 0.40 and 0.70 is **set** and recorded in `ambiguous_flags`, never treated as absent |
| `size` | best-effort | a hedged size (M vs L, margin ~0.03) leaves `size` empty and sets `size_fallback`; the caller supplies it |
| `scout` | same rule as a flag | |

A degraded size is not a failure: it is the normal outcome on non-trivial work,
and the run record says so. The reported `confidence` field is telemetry and
never decides anything.

## Security invariants

- Provider keys never enter a lane. The lane holds a run-scoped token only.
- Loopback only; unexpected `Host` and browser `Origin` rejected.
- Prompts, completions, headers and credentials never reach a log or audit record.
- The gateway loads only the credential its authorized route needs.

## Licence

Private. Licence pending.
