# Issue register

Every defect, ambiguity and infrastructure gap found while building this gateway
is recorded here, with an id, a severity and where it landed. Nothing is dropped
because it was small, fixed quickly, or found by a reviewer instead of a test.

## Rules

1. **One entry per issue, append-only.** Fixing an issue changes its status; it
   never removes the row. The history is the point.
2. **Severity decides what happens next.**
   - `blocker` — must be closed, or explicitly waived by the CTO, before the next
     phase is dispatched. A blocker left open behind a "we'll get to it" is how a
     defect becomes a regression.
   - `major` — closed inside the owning phase, before that phase is merged.
   - `minor` — fixed opportunistically; may ride with the next phase.
3. **A design-level issue gets an Advisor consult before the fix lands**, and the
   ruling is appended to the plan (`artifacts/model-gateway/plan-v2.md`,
   section 17). A plain defect does not need one; it needs a test.
4. **Every closed issue names its proof**: a gate test, a commit, or a command
   whose output was reproduced by the hub.
5. **Work orders are checked against this register before dispatch.** An open
   blocker for the phase being dispatched stops the dispatch.

## Register

| id | date | phase | severity | status | issue | proof |
|---|---|---|---|---|---|---|
| MGW-001 | 2026-10-19 | P0→P1 | blocker | open | Streaming requests record `usage_status: unknown`, so token accounting and budget enforcement fail for the dominant client pattern. A non-streaming call measured 17/32 tokens; the same dispatch through opencode measured nothing and parked 34 000 tokens as an uncertain charge. | `docs/verification/P0-findings.md` D1 |
| MGW-002 | 2026-10-19 | P0→P1 | major | open | A completed stream is recorded as `failure_code: client_disconnect`. The client closes the connection after the final event, the relay's next write fails, and a successful run reads as incomplete. | `docs/verification/P0-findings.md` D2 |
| MGW-003 | 2026-10-19 | infra | blocker | open | The executor gateway cannot execute a Go toolchain. `allowed_paths.r` carries no EXECUTE bit and `allowed_paths.exec` accepts files only, so the compiler, its tools and the generated test binaries are all unexecutable. Confined Go lanes are impossible until an exec-tree grant exists. Workaround in use: unconfined OC2 on this host. | `enforcement.py:30-32`, `:202-206`; lane dispatched unconfined |
| MGW-004 | 2026-10-19 | P2 | major | fixed | The P2 work order defined `balanced` twice: a fixed target-tier row and the complexity ladder. The lane stopped rather than choose. The ladder is authoritative because it is what the harness already does. | commit `62734a0` |
| MGW-005 | 2026-10-19 | P2 | major | fixed | The capability seed allowed five roles for seats whose `role_rank` was 0 for some of them, contradicting the validation rule. `roles_allowed` is now exactly the set of roles with a non-zero rank. | commit `62734a0` |
| MGW-006 | 2026-10-19 | P2 | minor | fixed | Gate 8's expected table depended on prices that the work order never supplied. Fixture prices are now data in micro-USD per 1M tokens. | commit `62734a0` |
| MGW-007 | 2026-10-19 | P0 | major | fixed | `make tools` installed golangci-lint through the project's `install.sh`, whose embedded checksum table disagreed with the pinned release, failing CI. Replaced with the release tarball verified against its own `checksums.txt`. | CI run `39224558334` green; `Makefile` `tools` |
| MGW-008 | 2026-10-19 | P0 | major | fixed | Checksum verification matched the `.sbom.json` line as well as the tarball, so `sha256sum -c` printed a failure while the recipe continued. Exact-name match plus `set -e`; a corrupted checksum is proven to exit non-zero. | negative test in the P0 session; `Makefile` |
| MGW-009 | 2026-10-19 | P0 | minor | fixed | A recipe ended with a bare `golangci-lint --version`; CI sets `GOBIN` to a directory that is not on PATH, so the install succeeded and the step failed with 127. Absolute path now. | CI run green |
| MGW-010 | 2026-10-19 | P0 | minor | fixed | `go.mod` carried a `toolchain` line beside a patch-level `go` directive; `go mod tidy` removes it, and the tidy gate then failed on the tool's own edit. Pin with the `go` directive only. | `docs/go-standard.md`; `make check` tidy step |
| MGW-011 | 2026-10-19 | P1 | minor | open | `net/http` strips `Content-Length` when `Transfer-Encoding` is present, so "reject both" is unobservable. The relay refuses any request carrying `Transfer-Encoding`. Correct, but the plan wording should say so. | `docs/architecture/proxy-relay.md` |

## Open blockers

| id | blocks | resolution path |
|---|---|---|
| MGW-001 | the P1 accounting claim, and any budget enforcement measured on streaming traffic | Decide whether the gateway may add `stream_options.include_usage` to OpenAI-dialect streaming requests. The passthrough rule forbids payload edits, so this is a CTO decision, not an implementation detail. |
| MGW-003 | confined Go lanes, including P5 verification | Add an explicit exec-tree grant to the harness policy vocabulary, with tests, and point `GOCACHE` at a worktree directory that is also an exec tree. |
