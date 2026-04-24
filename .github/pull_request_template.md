<!-- Keep this short. Delete sections that don't apply. -->

## Summary

<!-- 1–3 sentences: what changes, why, and the user-visible impact. -->

## Wire-protocol coupling

<!-- Delete this section if your PR doesn't touch PROTOCOL.md, any /v1/* response shape, the X-Bridge-Protocol header, or the manifest JSON.
     Otherwise — CONTRIBUTING.md's "Mirror-PR rule" applies: link the paired 1-bit iOS PR, OR explicitly state this is backward-compatible and older iOS clients still work. -->

- [ ] This change doesn't touch the wire protocol.
- [ ] **OR:** Paired iOS PR: acoseac/1-bit#NNN
- [ ] **OR:** Backward-compatible additive change; existing iOS clients continue to work.

## Pre-push checklist

Paste the clean output of each command in your branch.

```sh
make fmt
make vet
make test
make build-all
```

<!-- Paste output here. If a step failed, fix it before requesting review. -->

## Test plan

<!-- What did you verify manually? What scenario did you care most about not regressing? -->

- [ ]

---

🤖 Remove this line if the PR is written entirely by you. Leave it if Claude Code / another agent wrote a substantial portion.
