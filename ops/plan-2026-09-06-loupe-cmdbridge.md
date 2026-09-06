
---

## Phase 5–7 — what shipped

| PR | Theme | State |
|---|---|---|
| [#852](https://github.com/acoseac/1-bit-bridge/pull/852) | F1 + F2 — the two gates #781 removed | 3 review rounds |
| [#853](https://github.com/acoseac/1-bit-bridge/pull/853) | F3 — CLI config resolution + a package-wide guard | 1 round |
| [#854](https://github.com/acoseac/1-bit-bridge/pull/854) | F4 + F5 — fail closed in the artwork GC and `LiveHost` | 1 round |
| [#855](https://github.com/acoseac/1-bit-bridge/pull/855) | F7 + F8 — the stale operator promise, dead keepalives | 1 round |

Docs (F6 + the new `### The CLI and the serve wiring` section + the engineering
log record) went direct to `main` per the repo's docs-only convention.

### Bot coverage — stated rather than implied

**CodeRabbit reviewed #852 only.** #853 / #854 / #855 received Gemini and
SonarCloud but no CodeRabbit pass, which is the rolling-budget shape
`docs/LoupeReviewCycle.md` warns about for a multi-PR run. Those three are
Gemini + SonarCloud + CodeQL reviewed, not four-bot reviewed. Re-request the day
after if a second opinion on them is wanted.

### Findings taken from review

- **CodeRabbit, #852** — the disabled-gate test used fixed sleeps, so a loaded
  runner could reach the assertion before either trigger fired and pass against
  code that never consulted the gate. Now synchronises on the predicate actually
  being called. The sharpest comment of the run.
- **CodeRabbit, #852** — the optimize gate sat after `ResolveChecked` and the
  recursive walk, so a refused request still paid for a full `WalkDir`. Moved
  ahead of both; `TestUpscaleGateRunsBeforePathResolution` pins the ORDER via a
  nonexistent path (gate-first answers 503, resolve-first 404 — both "refused",
  which is why the other tests could not have caught it).
- **SonarCloud, #852** — 10 params / cognitive complexity 20, then 19. Fixed
  properly rather than appeased: dependencies became an `analysisSweeper` struct
  and the pass became `(*analysisSweeper).sweep`, which is the shape
  `fingerprintSweeper` already had. Then `new_duplicated_lines_density` at 10%
  → one parameterised `gateFixture`.
- **Gemini, #852** — the nil predicate contradicted its own docblock. Taken as
  **fail closed**, not as dropping the check.
- **Gemini, #853** — the `config.Load` guard stripped comments but kept string
  contents, so an error message containing the call spelling would cry wolf.
- **Gemini, #855** — drive `actUninstall` instead of scanning `menu.go`. Strictly
  better, and it buys an assertion a text scan cannot make: the config file is
  still on disk afterwards.

### Declined, with evidence

- **Gemini, #852 — move the test goroutine's join into `t.Cleanup`** to avoid a
  `t.TempDir()` race. It cannot race: the body already cancels and joins with a
  5 s bound *before returning*, and `t.Cleanup` runs after the body.
- **Gemini, #854 — memoise the folded UDN lookup in a package-level `sync.Map`.**
  Benchmarked: 297.9 ns / 304 B at one upstream, 410.6 ns / 912 B at five,
  994.6 ns / 3216 B at twenty — sub-microsecond once per request on a path that
  proxies multi-megabyte FLACs. Against that, a package-level memo outlives
  `runServe`'s in-process re-entry from the launcher menu and has no
  invalidation path when a cache entry ages out.

### Two process failures worth recording

1. **A negative control ran before the commit**, and its `git checkout --`
   restore took the fix with the mutation. `docs/LoupeReviewCycle.md` says commit
   first, in bold, for exactly this. It is easy to skip when the edit feels small.
2. **A source-scanning guard fired on its own docblock — twice.** Both the
   `config.Load` sweep and the uninstall-prompt guard. The eventual answers
   differ and both are worth knowing: strip comments *and* string-literal
   contents when a scan is genuinely the right tool, and prefer driving the real
   function when it isn't.

## Still open — a recorded follow-up batch, not a silent gap

F9–F16 in the triage above are confirmed and unshipped, deferred because they
overlap `main.go` with F1 or are below the bar for this batch's blast radius.
The highest-value three:

- **F9** `runSmartPlaylistRegenerator` freezes `analysisEnabled` at boot while
  the admin handler reads it live, so the same cache flips content depending on
  which trigger last ran. Found independently by two reviewers.
- **F10** the UPnP ingest loop is a third `scanIntervalSec` consumer with a fixed
  ticker and no rearm — both halves of the cadence rule, while the settings
  matrix classes the field `live`.
- **F11** four un-joined background writers (`integrity.VariantWatcher` passes
  `context.Background()` and writes the store), which falsifies CLAUDE.md's
  "Shutdown joins every background writer".

Plus, from the PRISM pass and not yet triaged in depth: `bridge enrichment retry`
accepting a positional path that `fs.Parse` silently drops, turning an intended
subtree reset into a whole-library one; `variants move --dry-run` calling
`os.MkdirAll`; and `duplicatesCmd` ignoring `--tier` / `--nested-only` under
`--json`.
