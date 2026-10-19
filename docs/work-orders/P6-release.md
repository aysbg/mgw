# P6 — release builds, tuning, and what stays open

Last phase. Two small pieces of product work and one honest gap.

## Task 1 — reservation ceiling default (MGW-031)

`mgw` currently defaults `max_reservation_tokens` to 8000. One real gateway-mode
run measured completions at 70–807 output tokens, so 8000 is roughly 10x larger
than anything observed; 6 aborts in that run held 56000 tokens of budget.

Change the default to **2000** and record why in the runtime documentation:
about 2.5x the largest observed completion, which keeps roughly 30 aborted
streams inside a 100k run budget instead of 12. Keep it configurable; do not
remove the setting.

Tests: the default is 2000 when the runtime file omits the field; an explicit
value still wins; validation still rejects zero and negatives.

## Task 2 — release artifacts

`make release` produces, in `dist/release/`:

| artifact | content |
|---|---|
| `mgw-<version>-linux-amd64` | static binary, `CGO_ENABLED=0`, trimmed |
| `mgw-<version>-darwin-arm64` | cross-compiled, static |
| `mgw-<version>-darwin-amd64` | cross-compiled, static |
| `mgw-<version>-windows-amd64.exe` | cross-compiled, static |
| `SHA256SUMS` | one line per artifact, `sha256sum -c` compatible |

Rules:

- version from `git describe --tags --always --dirty`, embedded with ldflags
  together with the commit and build time;
- every artifact built with `-trimpath` and `-buildvcs=true`;
- the Linux artifact must be byte-identical to a second build of the same commit
  (reproducibility, as `make check` already does for `dist/mgw`);
- `make release` fails if `git status --porcelain` is not empty, because a dirty
  tree cannot produce a reproducible artifact;
- `SHA256SUMS` is generated in a stable order (sorted by filename) so two runs
  compare cleanly.

Do not add signing. Signature keys are a CTO decision and the release notes must
say the artifacts are **unsigned** and how to verify them today (digest against
this file).

## Task 3 — release notes template

`docs/release-notes-template.md`, short:

```markdown
# mgw <version>

commit <sha>, built <timestamp>, Go <version>

## Artifacts
<the table, plus the SHA256SUMS line>

## Verification
1. `sha256sum -c SHA256SUMS`
2. `./mgw-<version>-linux-amd64 version --json` and compare version and commit.

Signatures are not yet published; digests are the current integrity check.
```

## Task 4 — `mgw doctor` on a release artifact

Add one check: when running from a release build, `doctor` reports the binary's
own version, commit and build time, and warns when the commit does not match the
`registry.json` pin recorded in the same directory (a mixed install). Do not fail
on a mismatch — warn, because a registry can legitimately outlive a binary.

## Acceptance

```
make check
make release
ls -l dist/release
cd dist/release && sha256sum -c SHA256SUMS
file dist/release/mgw-<version>-windows-amd64.exe   # must say PE32+
file dist/release/mgw-<version>-linux-amd64         # must say ELF 64-bit, statically linked
```

Evidence: the `ls`, the `sha256sum -c` output, both `file` results, and
`mgw version --json` from the Linux artifact.

## Constraints

- Standard library only; the release target uses `go build` cross-compilation, no
  toolchain download.
- Do not change `internal/proxy` beyond the Task 1 default, `internal/policy`,
  `internal/catalog`, `internal/decision`, or `docs/go-standard.md`.
- Do not commit or push.

## Known open items to state in the release notes, not to fix here

- artifacts are unsigned (CTO key decision);
- macOS and Windows binaries are cross-compiled and **smoke-tested by the hub**,
  not by CI on those platforms;
- the first-install catalog digest uses trust-on-first-use (MGW-016).
