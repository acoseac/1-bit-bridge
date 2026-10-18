---
name: multi-pr-batch
description: "Workflow for a bridge job that spans three or more PRs: stacked versus parallel PRs, one review wait, one combined fix pass per branch, merging bottom-up with retargeting, the follow-up PR, the end-of-session gate and one deploy at the end. Use it when planning or running such a batch."
---

# Multi-PR batch workflow

Moved verbatim from CLAUDE.md on 2026-10-18. Paths are relative to the repository root. The single-PR loop it extends is CLAUDE.md's "Development workflow".

## Multi-PR batch workflow

For any larger job spanning **3+ PRs**, use the **stack-and-batch** pattern instead of the default serial merge-after-each. Time-validated against the v1.2 improvements batch (PRs #76 / #81 / #82 / #83 / #84 / #85 — security + slog + CLI + fsnotify + Docker + post-merge follow-ups). The serial pattern would have spent ~30 min just on bot-review wait windows; the stacked pattern collapsed that to one ~6 min wait.

1. **Plan first.** Write the plan to the plan file before any code. Cross-PR invariants caught at plan time save hours of post-merge debugging — the dropped `db.SetMaxOpenConns(1)` from PR #76's first draft is the canonical example. The plan file is the highest-leverage artifact in the batch; every "Things that have bitten before" entry traces back to a deliberate plan-time decision.
2. **Stack PRs end-to-end.** Each branch bases off the prior one's tip (`git checkout -b feat/X feat/W`); each PR's `base` is the prior branch, NOT main. Open all PRs in one pass without waiting between them. Build PR-N+1 while bots review PR-N — overlap the work.
3. **One 6-minute wait** after the last PR opens. Bots (CodeRabbit / Gemini / Qodo) post within ~3 min; 6 min covers the slow-arrival tail. Don't poll; use ScheduleWakeup once.
4. **Address all comments in one combined pass per branch.** Don't merge anything yet. Bots see cross-PR context on a stack — their PR-N+1 comments may reference PR-N's invariants, and folding both into a single fix is cheaper than amending after merge. Reject bot suggestions that contradict deliberate in-code rationale (the PR-76 review's "cache transient MB errors" suggestion vs the PR #74 invariant is the canonical example — verify, don't blindly apply).
5. **Merge bottom-up in dependency order at the end.** GitHub auto-closes a stacked PR when its base branch is deleted, so as each ancestor merges, retarget the next via `git rebase --onto main <ancestor-tip>` and open a fresh PR (the previous one auto-closed). Plan ~2 min of rebase per child PR; `--reapply-cherry-picks` is unnecessary because already-applied commits are detected and skipped automatically.
6. **One combined follow-up PR** for any post-merge bot comments. Bots run a second review pass after the first round of fixes lands — that's the realistic floor (two rounds, not one). Batch the late-arriving items into a single follow-up PR rather than amending merged branches.
7. **CLAUDE.md-only updates direct to main.** A change to `CLAUDE.md` alone (the batch's closing entry, a corrected claim) is the one change that bypasses the feature-branch path, per step 1 of the Development workflow. A rule that describes a PR's code rides in that PR, and a change to any other doc, `AGENTS.md` included, goes through a PR. This step said "docs-only changes bypass the feature-branch path" until 2026-09-29, and `AGENTS.md` said new invariants went "committed direct to `main`", which review bots quoted back on PRs carrying a rule with its code (#1079, #1087).
8. **End-of-session quality gate.** `make fmt vet test build-all` on bridge, `xcodebuild build` on iOS, resolve any warnings before reporting done. The stacked workflow's main risk is cross-PR drift; the build matrix catches it cheaply at the end.
9. **Post-merge deploy** after the whole stack lands — see [Post-merge deployment](../../../CLAUDE.md#post-merge-deployment). For a stack, deploy ONCE at the end carrying every merged fix, not per-merge.

**Avoid small PRs.** 5 cohesive ~200-line PRs ship faster and review better than 15 micro-PRs — bots calibrate review priority to PR scope, and a coherent theme per PR makes the eventual squash-commit message useful as ship-history.

**When NOT to stack:** if PRs are genuinely independent (disjoint files, no semantic dependencies, no shared invariants), open them in parallel against main rather than stacking — review converges faster and there's no rebase cost. Stack only when PR-N+1 logically depends on PR-N's API surface or invariants.
