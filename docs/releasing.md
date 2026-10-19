# Release builds

Run `make check`, then run `make release` from a clean Git worktree.
The release target rejects tracked changes and untracked files.
Do not bypass this check to publish uncommitted source.

The target builds four binaries and `SHA256SUMS` in `dist/release/`.
It replaces an older release directory only after all builds and the Linux
reproducibility check succeed. It disables CGO and toolchain downloads.
The embedded version comes from `git describe --tags --always --dirty`.
The build time uses the commit timestamp, not the current clock time.
Use the same Go toolchain to reproduce the bytes.

Copy `docs/release-notes-template.md` for each release. Artifacts are **UNSIGNED**.
Do not add signatures until the CTO selects the keys.

## Doctor and mixed installs

`mgw doctor` prints the running binary's version, commit and build time.
`mgw init` and `mgw matrix --write` record the writer's build metadata in
`registry.json.build.json`, next to the registry and its `.sha256` pin.
The build pin includes the registry digest. It does not change the registry schema.
For a custom registry path, the sidecar uses that path plus `.build.json`.

Doctor compares the binary commit with this build pin for the selected registry.
A mismatch produces a warning, not a failure. An older registry can remain valid.
A missing, invalid or stale build pin also produces a warning on a release build.
Local builds without commit metadata cannot compare commits.
The build pin is diagnostic metadata, not a signature or proof of publisher identity.
