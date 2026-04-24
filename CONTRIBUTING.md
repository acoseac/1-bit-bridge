# Contributing to 1-bit-bridge

## Branch / PR workflow

- Work on a feature branch; never commit directly to `main`.
- Open a PR against `main`. Merges use the default merge strategy.
- Keep PRs small and focused — one feature or one fix per PR.

## Mirror-PR rule (wire-protocol changes)

`1-bit-bridge` and the iOS app [`1-bit`](https://github.com/acoseac/1-bit) share a wire protocol defined in [`PROTOCOL.md`](PROTOCOL.md). Any PR that touches `PROTOCOL.md`, the manifest JSON shape, or any request/response schema MUST satisfy one of the following:

1. **Link a companion PR** on the `1-bit` repo (branch: `feat/bridge-protocol-vNN` or similar) that updates:
   - `com.acoseac.dsdplayer/docs/BridgeProtocol.md` (verbatim mirror of this repo's `PROTOCOL.md`)
   - `com.acoseac.dsdplayer/Tests/com_acoseac_dsdplayerTests/Fixtures/Bridge/` (golden fixtures)
   - Any `BridgeSourceClient.swift` decoder changes the new shape requires
2. **OR explicitly justify** in the PR body: `Backward-compatible protocol change, no iOS change required` — and ensure the change is actually additive and older iOS clients keep working.

Every iOS-repo PR that lands a new `protocolVersion` MUST tick the PR template checkbox: *"Does this change consume a new protocol version? If yes, link the matching 1-bit-bridge PR."*

This rule is enforced by review convention, not CI. If you find yourself wanting to skip it "just this once", write it up in the PR body so the next reader can see why.

## Pre-push checklist

There is no PR-check workflow today — local-green is the gate. Every PR author runs these commands locally before pushing and pastes the result into the PR body:

```sh
make fmt
make vet
make test          # race-enabled; runs on cmd/bridge and any package with _test.go
make build-all     # cross-compile sanity check for darwin/linux/windows × amd64/arm64
```

If any step fails, fix it on the branch before requesting review. A green paste-in substitutes for CI.

## Go style

- Prefer stdlib. Dependencies are admitted one at a time, each justified in the PR that adds them.
- Every new package starts with a `doc.go` containing a one-paragraph package doc.
- Every new exported function / type gets a godoc comment.

## Golden fixtures

If your PR changes the manifest JSON shape, regenerate fixtures via `make fixtures` (to be added) and copy them into the iOS repo as part of the companion PR.
