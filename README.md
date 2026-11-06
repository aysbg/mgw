# mgw — model gateway

A loopback HTTP gateway that holds provider credentials and gives coding agents
bounded inference authority. Separate from the Python executor harness, which
talks to it only over HTTP and two JSON artifacts.

Status: implemented. `mgw serve`, `authority`, `init`, `providers`, `strategy`, `matrix`, `pricing`, `catalog`,
`doctor`, `decide` and `version` work. `mgw exec` exits 3 on purpose: a later lane owns it.

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

Go 1.27.3 is pinned in `go.mod`. Install it from the official tarball, not a
distribution package.

## Layout

```
cmd/mgw          entry point: flags, subcommand dispatch, exit codes
internal/cli     init, providers, strategy, matrix, pricing, catalog, doctor, decide, authority, serve
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

## Catalog and pricing provenance

`mgw catalog diff` compares the curated seat library (`internal/catalog/capabilities.json`) with the upstream catalog
and reports three kinds of difference: `new_upstream` (the catalog carries a model the library does not),
`not_upstream` (the library carries a seat the catalog no longer lists) and `provider_missing` (a library provider is
absent upstream). It is read-only: it assigns no tier, rank or price, and it writes no file. Flags: `--catalog-url`,
`--min-context` (floor 100000, below which the flag is a usage error), `--json`, and `--quiet` (kept silent when there
is nothing to report; it cannot be combined with `--json`). Detection is upstream and shared; seating a model stays a
per-repository judgement.

The pricing snapshot records the catalog digest it was built from as `source_sha256`. `mgw pricing refresh --sha256
<digest>` writes `pricing.json` and its adjacent `pricing.json.sha256`; `mgw init` does the same and records whether an
operator supplied a pin. That approval flag is `pinned` inside the snapshot.

`mgw doctor` checks the snapshot in this order: a missing snapshot warns, an unreadable or undecodable one fails, a
missing pin warns, a pin that does not match the file **fails** naming both digests, an unapproved digest warns with
the first-use text, then the unchanged 30-day staleness warning applies, and otherwise the check reports ok.

The pin detects a change to one file made outside `mgw`: a hand-edited price, an older binary that rewrites the
snapshot but not its pin, a partially restored or synced file, or a file that is corrupt yet still decodes. It does not
resist someone who can write the config directory, because that person can edit the config, the credentials and the
binary too, and it is checked by `doctor` only.

Known limit, tracked as MGW-034: a repeated `mgw init` and `mgw providers add` refetch the catalog without a pin and
drop the snapshot to `pinned=false`. `doctor` reports that downgrade; the paths are not prevented from doing it.

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

## Deployment scope

This gateway exists for **client hosts and consulting engagements**, not as the
default path for our own repository work. Internal model-to-role assignment lives
in opencode's own configuration, which is more direct than proxying it.

Use gateway mode when these properties are the point: a lane that holds a
run-scoped token and never a provider key, request-level usage measurement, a
per-engagement profile carrying strategy and provider policy, and one API key set
serving every family an engagement may use.

Using direct mode forgoes authoritative per-request accounting and in-lane
credential isolation. Neither is a requirement for internal work today; the mode
is per dispatch, so it is not all-or-nothing.

**The economic reason is stronger than the architectural one.** A subscription
(Claude Max, a ChatGPT or Codex plan) is a flat fee with a usage window, so the
marginal cost of a request inside that window is about zero. This gateway
requires provider API keys and cannot use subscription credentials, by design and
for legal reasons. Routing internal work through it would therefore replace
already-paid capacity with metered per-token billing — slower economics for no
benefit our own repository needs.

The same fact bounds the product: it serves engagements that hold **API keys or
pay-per-token accounts**. A client whose access is subscription-only keeps using
their own agent tooling's authentication; the gateway is not in that path, and
the `cheapest` strategy ranks prepaid credits among API-key providers only — it
does not and cannot model a flat-fee subscription.

## Security invariants

- Provider keys never enter a lane. The lane holds a run-scoped token only.
- Loopback only; unexpected `Host` and browser `Origin` rejected.
- Prompts, completions, headers and credentials never reach a log or audit record.
- The gateway loads only the credential its authorized route needs.

## Licence

Private. Licence pending.
