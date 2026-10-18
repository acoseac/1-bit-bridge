---
name: gemini-consult
description: "Consult Gemini on a non-obvious decision instead of guessing: Go concurrency subtleties, tsnet and Tailscale internals, behaviour that differs between darwin, linux and windows, framework upgrade gotchas. Covers the direct consult.py route, framing the question, when to skip, and applying the answer. Use it before shipping anything you would otherwise caveat with \"I think this is right but I'm not 100% sure\"."
---

# External consultation (Gemini)

Moved verbatim from CLAUDE.md on 2026-10-18. Paths are relative to the repository root.

## External consultation (Gemini)

When you hit a non-obvious decision — Go concurrency subtleties, tsnet integration nuances, framework version-specific behaviors, algorithm tradeoffs that aren't covered in this CLAUDE.md or visible in the code — **consult rather than guess**. Two routes, and prefer the first: **`python3 ~/dev/gemini-review/consult.py --question-file q.md --context <file>`** sends one focused question directly (key at `~/dev/gemini.api`, header-only, never logged) and comes back in seconds, so a question no longer has to interrupt the work; **or** formulate the question and share it with the user, who routes it through Gemini and relays the response back. The direct route is what makes "don't stop, resolve it" achievable — see [docs/LoupeReviewCycle.md](../../../docs/LoupeReviewCycle.md) § "Consulting mid-run". The cost of one extra round-trip is small; the cost of shipping wrong is bot reviews, follow-up PRs, regressions visible to operators running the bridge.

The pattern that works:
1. Diagnose the problem in your own words first — don't outsource the thinking.
2. Write a question that includes context: what you tried, what you observed, what your current hypothesis is, what alternatives you're considering.
3. Share the question with the user, wait for the response.
4. Apply the fix grounded in the response. Cite the consultation in the commit message so future sessions can trace the rationale.

Lean toward consulting whenever you'd otherwise ship code with a "I think this is right but I'm not 100% sure" caveat. Worth consulting on: subtle Go concurrency questions (lock ordering, goroutine lifecycle), tsnet / Tailscale internals not covered in their docs, cross-platform behavior that differs between darwin / linux / windows, framework upgrade gotchas. Pattern proven on the iOS side (1-bit) — see CLAUDE.md `## External consultation (Gemini)` entry there for examples that prevented multiple SwiftUI / iOS-internal regressions.

**Skip when:** the answer is verifiable by reading the code, running a test (the project's `make test` is fast), or checking the upstream library's source. Don't consult on things you could resolve in a minute by reading the source.
