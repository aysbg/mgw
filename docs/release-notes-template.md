# mgw <version>

commit <sha>, built <timestamp>, Go <version>

## Artifacts

All artifacts are **UNSIGNED**. Signature keys are a CTO decision.

| Artifact | Content |
|---|---|
| `mgw-<version>-linux-amd64` | Static Linux binary |
| `mgw-<version>-darwin-arm64` | Static macOS ARM64 binary |
| `mgw-<version>-darwin-amd64` | Static macOS AMD64 binary |
| `mgw-<version>-windows-amd64.exe` | Static Windows AMD64 binary |
| `SHA256SUMS` | SHA-256 digests, sorted by artifact filename |

## Verification

1. Download the artifacts and `SHA256SUMS` into the same directory.
2. Run `sha256sum -c SHA256SUMS` in that directory.
3. Run `./mgw-<version>-linux-amd64 version --json` and compare version and commit with this release.

Signatures are not yet published; digests are the current integrity check.
Digests do not prove publisher identity. Obtain `SHA256SUMS` from the trusted release source.
The build timestamp is the commit timestamp, so repeated builds use the same metadata.

## Known open items

- Artifacts are unsigned; signature keys need a CTO decision.
- macOS and Windows binaries are cross-compiled. The hub smoke-tests them, not CI on those platforms.
- The first-install catalog digest uses trust-on-first-use (MGW-016).
