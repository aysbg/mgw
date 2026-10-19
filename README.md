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
internal/proxy   server, auth, route selection, relay, commit tracking
internal/provider dialect handlers, usage parsers, error mapping
internal/policy  registry load, candidate lists, hard guards
internal/ledger  cross-process reserve and settle
internal/catalog models.dev ingest, capability merge, pricing snapshot
internal/decision optional Clef and Jev adapter
internal/audit   metadata-only audit records
docs/gates.md    gate to test mapping
```

## Security invariants

- Provider keys never enter a lane. The lane holds a run-scoped token only.
- Loopback only; unexpected `Host` and browser `Origin` rejected.
- Prompts, completions, headers and credentials never reach a log or audit record.
- The gateway loads only the credential its authorized route needs.

## Licence

Private. Licence pending.
