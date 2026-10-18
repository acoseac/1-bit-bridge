---
name: release-playbook
description: "Release day for the bridge: ops/release-playbook.md end to end (scope and freeze, the three sanity reviews, validating the candidate, the annotated tag and the artifact checks, the gated rollout, running it with agents) and docs/release-process.md for the per-release docs. Use it when the operator says a release is being cut, tagged or rolled out."
---

# Release day

Moved verbatim from CLAUDE.md on 2026-10-18. Paths are relative to the repository root. The two rules for any doc edit (user-facing docs live in the 1bitapp repo; public prose names the entity ars.md) stay in CLAUDE.md under "Documentation refresh on each release".

## Documentation refresh on each release

**Release day: follow [`ops/release-playbook.md`](../../../ops/release-playbook.md)**, the end-to-end procedure as run for v0.2.1:
- scope and freeze, the three read-only sanity reviews (network, data, operations including the upgrade itself), and one fresh fix agent per finding (two related findings may share one, in sequence);
- validating the candidate: the gate and a goreleaser snapshot, a Docker upgrade test from the previous release image, the candidate on the operator's bridge (and, with the operator's go-ahead, the cloud tenants), and the nightly fuzz;
- the annotated tag, the artifact checks (checksums, codesign, both image arches), and publishing the notes;
- the gated rollout of the release artifact: the operator's bridge, then the demo, then the tenants, each healthy before the next;
- how to run all of it with agents inside the usage limits.

The operator decides the scope, every production deploy, the tag and the publish.

**See `docs/release-process.md`** — which docs to update per release, which NOT to touch, the process, gotchas, after-the-tag steps.
