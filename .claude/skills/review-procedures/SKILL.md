---
name: review-procedures
description: "The bridge's named review procedures: LOUPE (the recent-work review cycle) and the DeepSeek code-review sweep. Use it when the user asks for either by name, e.g. \"run LOUPE on last week\", \"LOUPE the last N commits\", \"LOUPE the enrich package\", \"LOUPE since v0.1.9\", \"run the DeepSeek sweep\"."
---

# Review procedures: LOUPE and the DeepSeek sweep

Moved verbatim from CLAUDE.md on 2026-10-18. Paths are relative to the repository root. Each procedure's full write-up is the doc it names; read that doc in full before starting. The PR-time bot-review rules stay in CLAUDE.md.

## LOUPE — the recent-work review cycle (user-invoked by name)

Full procedure: **[docs/LoupeReviewCycle.md](../../../docs/LoupeReviewCycle.md)** — the
bridge-side twin of the iOS repo's `docs/LoupeReviewCycle.md`. When the user says
*"run LOUPE on last week"* / *"LOUPE the last N commits"* / *"LOUPE the enrich
package"* / *"LOUPE since v0.1.9"*, that is a request for the whole loop: scope
the window (or the package) → review it from two directions (PRISM batches +
targeted read-only agents) → triage every finding against the code and the
invariants in `## Things that have bitten before` → one written plan with its
rejections → plan review → ship as file-disjoint PRs, **one per script run**,
each negative-controlled red-first (`-count=1`, never a cached PASS) → bot sweep
across all FOUR bots (Gemini, CodeRabbit, SonarCloud, **CodeQL**) with
evidence-backed declines → merge → the `make fmt vet test build-all` gate → a
dated CLAUDE.md entry → a deploy-and-journal field loop, where
`journalctl -u 1-bit-bridge` outranks anything this file claims.

**It runs end to end without stopping for approval**, and a *question* is not a
stopping condition — measure it (module source under `$(go env GOMODCACHE)`, a
three-line probe, `-gcflags=-m`, `go test -race`, a fuzz target), then consult
Gemini over the API, then decide and record the decision. Escalate only for
product direction, a production deploy, a wire change that commits the iOS side
(the Mirror-PR contract above), or a refusal condition.

It is the third named procedure beside PRISM (bug review) and VISTA (design
briefs), and it uses PRISM as one phase. **The Gemini API key file is selected by
`GEMINI_API_KEY_FILE`**; the key is sent as an
`x-goog-api-key` header, and must never reach a URL, a log, or a commit; the
three harnesses (`consult.py`, `relay.py`, `prism.py`) live at
`<GEMINI_REVIEW_DIR>`, outside both repos, and work from here unchanged.

## External code review (DeepSeek sweep)

The repeatable external-LLM (DeepSeek v4-pro) review process spans BOTH repos — full procedure is documented on the iOS side at `1-bit/docs/DeepSeekReviewProcess.md`. Harness at `<DEEPSEEK_REVIEW_DIR>` (`plan.py` collects last-week's changed files incl. `**/*.go`; user runs `run_all.py`; agent triages `responses/*.md`). **Value is in the triage** — the 2026-06 baseline ran ~70% false-positive, so verify EVERY finding against the actual code + the invariants in `## Things that have bitten before` before acting (zero false fixes shipped that way). Go-specific FP classes are pre-encoded in `review.py` + `<DEEPSEEK_REVIEW_DIR>/known_fp.md` (append confirmed FPs there so re-runs stay quiet): `&local` in an atomic ≠ use-after-free (escape analysis heap-promotes); defers run during panic unwind (no "permanent deadlock"); builder/`With*` setters are construction-time not racy; partial-file chunk views can't see paired Close/shutdown elsewhere; check multi-return signatures before "swallowed error". Real fixes ship via the Multi-PR batch workflow below (themed, parallel-off-main when disjoint, `make fmt vet test build-all` before push, fast-forward-never-force on bot-review commits). `run_all.py bridge` limits a run to this repo.
