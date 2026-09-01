# Plan — improvement batch, 2026-09-01

Eleven items from a forward-looking survey at `6135b8f`, plus one item measured and
**dropped**. This is a gap-closing batch, not a bug batch: the last five audits came
back essentially clean, so nothing here is a live defect. Every item is either a
capability the bridge nearly has, a resource that grows without a bound, or a safety
net that does less than it appears to.

**Audience:** whoever implements this, across accounts and machines. **Scope:** three
items touch `/v1` (one of them a Mirror-PR pair with the iOS repo); the rest are
bridge-internal, admin, CI or docs. `ProtocolVersion` stays 1 throughout.

**Placement.** Kept in `ops/` per the repo rule, alongside `plan-web-upload.md`. It is
written **fix-shaped, not attack-shaped**: item C1 concerns a network-facing predicate
that is broader than its threat model, and this document describes the narrowing rather
than what the current breadth permits, so the file is safe to track in a public repo.
The traced rationale lives in the private CodeQL alert.

---

## What the research changed before this was written

Two things are worth reading before the plan itself, because both reverse a premise
from the survey that produced it.

**A fresh install replays all 41 migrations in 15–24 ms** (measured, three runs, this
Mac). The "squash the ladder into a baseline schema" item is therefore **dropped, and
should not be revived on performance grounds**. It would buy ~15 ms once per install in
exchange for a second, independently-maintained definition of the schema — precisely the
"two copies that can be wrong together" class this codebase has been bitten by
repeatedly (the 1BSP fixture, the hand-written PROTOCOL mirror, the `stringOf` alias
list). If a baseline is ever wanted, the motivation has to be something other than
speed, and the baseline must be *generated from* the ladder with a test that diffs
`sqlite_master` between the two, or it is not worth having.

**The Windows CI leg has not failed in the last 60 runs** across all branches — 21
consecutive successes on `main` alone, back to 2026-08-30. `gate.yml`'s own comment sets
the promotion bar at "several consecutive clean Windows runs across different PRs". That
bar is met and then some, so A2 below is a two-line change backed by evidence rather
than a project.

---

## What two external reviews changed — and what they got wrong

Both reviews were traced against the code before anything here moved. Between them,
**four suggestions improved the plan, two reversed a fix I had written, and three were
false positives**. The verification is recorded because two of the false positives are
confidently argued and will be raised again.

**Accepted, and the plan below is rewritten accordingly**

- **A3 — fuzz by matrix, not rotation.** Rotating `day-of-year % 32` gives each target 5
  minutes a month. `go test -list 'Fuzz.*' ./...` does enumerate fuzz targets (verified),
  so a generated matrix runs all 32 nightly at 5 minutes each, wall-clock 5 minutes, with
  no target count to maintain. The repo is public, so runner minutes are free.
- **C1 — warn before enforcing.** Some control points bind their outgoing SUBSCRIBE
  socket to one interface while requesting callbacks on another. I cannot verify that
  claim about specific hardware, which is exactly why observing first is right: it costs
  one release and converts a compatibility guess into evidence. It also matches how the
  Windows gate promotion in A2 is being justified.
- **C2 — size the burst, not just the rate.** Correct, and concrete: the limiter is
  already `rate.NewLimiter(rate, burst)`, so this is a sizing instruction rather than a
  change of shape.
- **D1 — assert the query plan.** The premise is wrong (below) but the test is worth
  having anyway, and cheap.

**Accepted concern, reversed fix — this one would have shipped a button that does nothing**

- **B2 — `wal_checkpoint(TRUNCATE)` goes AFTER the `VACUUM`, not before.** The review said
  before, reasoning that the WAL should be folded in "before the vacuum reads it".
  Measured on a seeded 8,000-track store with 7,000 deleted:

  | stage | `page_count` | `freelist` | `.db` | `-wal` |
  |---|---|---|---|---|
  | after deletes | 1373 | 701 | 5,623,808 | 226,632 |
  | after `VACUUM`, no checkpoint | 628 | 0 | **5,623,808** | **2,813,992** |
  | after `wal_checkpoint(TRUNCATE)` | 628 | 0 | **2,572,288** | **0** |

  In WAL mode the VACUUM's own output lands in the WAL, so without a **post**-VACUUM
  checkpoint the main file does not shrink by a single byte and peak disk *rises* by
  2.6 MB. A pre-VACUUM checkpoint is harmless hygiene; the post one is what reclaims.

**Declined, with evidence**

- **B1 — a `TEMP` table instead of `json_each(?)`.** Two reasons. A `TEMP` table is
  **per connection**, and `internal/manifest` deliberately runs reads on
  `database/sql`'s pool with no `SetMaxOpenConns`; demonstrated directly — connection B
  cannot see connection A's temp table (`no such table: live_tokens`) while A still can.
  A `CREATE TEMP` / `DELETE` pair would therefore need pinning to a `*sql.Conn` or a `Tx`,
  which is strictly more machinery than the thing it replaces. And the cardinality is not
  a cliff: the live install has **10 tokens**, one per paired device, so the bound blob is
  a few hundred bytes — the PR #495 precedent uses `json_each` for MBID sets orders of
  magnitude larger. Worth recording for the future: if this ever became a large set, the
  answer is a **`Tx`-scoped** temp table, not a naive one.
- **A4 — "Node 22+ may let you drop the DOM stub".** Measured under Node 26: **4 of 8
  player modules throw at import**, every one on `window` (and then `location`). Node does
  not provide `window` and never will. A stub is required — but it is small and it
  converges; see A4 below for the measured shape.

**False positives**

- **B1 — "deleting a `device_registrations` row will trigger an FK violation."** There is
  no foreign key. `playback_history` declares none, and nothing anywhere `REFERENCES
  device_registrations`. The `LEFT JOIN` in `history.go` exists precisely because an
  unattributed history row is a supported state, and its comment says so.
- **D1 — "unparsed user input passed to FTS5 `MATCH` will crash SQLite."** Sanitisation
  already exists and is the documented design: `splitFTSTokens` strips every rune that is
  not a Unicode letter or digit, `buildFTSMatchExpr` double-quotes each surviving token
  and prefix-expands only at ≥3 runes. Operators, quotes and `NEAR` cannot survive it.
  This is the classic failure mode for review of this repo — reporting a deliberate,
  commented decision as a bug.
- **D1 — "the join may trick the planner into a catastrophic full scan."** Measured with
  `EXPLAIN QUERY PLAN` on a 2,000-track store; the naive join already produces the optimal
  plan, because `tracks.path` is the PRIMARY KEY and the `MATCH` constraint forces the
  virtual table to be the outer loop:

  ```
  SCAN f VIRTUAL TABLE INDEX 32:M4
  SEARCH t USING INDEX sqlite_autoindex_tracks_1 (path=?)
  ```

  An `EXISTS` rewrite produces the same plan. The premise is wrong; the suggested test is
  kept anyway, now with measured strings to assert.

**Partially accepted**

- **B2 — bound the compaction.** Yes, bound it. But "so API clients receive an immediate
  503 rather than timing out" does not follow: a deadline on `Compact` does not change
  what other clients experience, since they block on SQLite's lock regardless. The
  deadline bounds the operation; the scan-guard and the free-space guard are what protect
  the bridge.
- **Sequencing — same package is not same file.** B1, B2 and D1 all touch
  `internal/manifest`, but in different files (a reaper, a compaction helper, `search.go`),
  which git merges cleanly. The real conflict surfaces are narrower and worth naming:
  **`cmd/bridge/main.go`'s wiring block** (B1's sweeper and D2's poller both land there)
  and **`internal/config/config.go`** (B1 alone). Land B1 before D2 for the first; nothing
  else needs sequencing.

---

## The batch

Ten PRs. Most are disjoint and go **parallel off `main`** rather than stacked — stacked
PRs in this repo get no CI at all until they are retargeted (`gate.yml`/`gofmt.yml`/
`codeql.yml` are all `pull_request: branches: [main]`), and nothing here needs the
stacking.

| PR | Theme | Touches | Size |
|---|---|---|---|
| A1 | Dependency queue | `.github/workflows`, `go.mod` | S |
| A2 | Promote the Windows/macOS gate | `.github/workflows/gate.yml` | XS |
| A3 | Nightly fuzz + upload fuzz target | new workflow, `internal/upload` | M |
| A4 | Console smoke test | `internal/admin` (tests only) | M |
| B1 | Retention: registrations + history | `internal/manifest`, `internal/config`, `cmd/bridge` | M |
| B2 | Database compaction | `internal/manifest`, `internal/admin` | M |
| C1 | Narrow the GENA callback predicate (2 steps, a release apart) | `internal/dlna` | S |
| C2 | Per-route rate-limit classes | `internal/api` | M |
| D1 | `GET /v1/search` (**Mirror-PR**) | `internal/manifest`, `internal/api`, `PROTOCOL.md` | M |
| D2 | `manualDescriptionURL` support | `internal/upnp`, `cmd/bridge` | M |
| E1 | CLAUDE.md reconcile + split | docs | M |

**Order.** A1 and A2 first (A2 makes every later PR carry a blocking Windows signal).
C2 before D1, so search ships with a bucket on day one. B1 before D2, because both add
wiring to `cmd/bridge/main.go` and that is the one real conflict surface in the batch. E1
last, because it records what the batch did. Everything else is genuinely parallel —
B1/B2/D1 all touch `internal/manifest` but in different files, which git merges cleanly.

---

## A1 — clear the dependency queue

Nine open Dependabot PRs, oldest 11 days.

**The three `github/codeql-action/*` PRs (#776/#777/#778) each fail `analyze (go)`, and
that is expected, not a regression.** `codeql.yml` pins `init`, `autobuild` and `analyze`
to a single SHA with a comment saying they MUST stay in lockstep; bumping one alone
splits the version and the analyze step refuses. Close all three and open **one combined
PR** bumping all three to the v4.37.8 SHA — the #691 precedent.

The remaining six (`tailscale` 1.102.3, `x/crypto`, `x/net`, `x/image`, `x/mod`,
`docker/setup-buildx-action`) are ordinary. Check `x/net` 0.58.0 against the `quic-go`
0.61 pin and confirm `go mod tidy` stays clean before merging.

**Done when:** the queue is empty and `make fmt vet test build-all` is green on `main`.

---

## A2 — promote the cross-platform leg into the gate

Two lines: add `cross-platform` to the `gate` job's `needs:` and delete
`continue-on-error: true`.

Promote **both** matrix legs. They are one job, `needs:` cannot select a leg, and macOS
has been green alongside Windows. If macOS later proves flakier, split the matrix then.

Keep the 60-minute timeout, the per-package 30m `-timeout`, and the Defender exclusion —
all three are load-bearing and documented in place. Record the promotion evidence in the
job's own comment (date, run count, the fact that zero failures appeared in 60 audited
runs) so the decision is auditable later.

**Risk, stated plainly:** a flaky Windows test now blocks merges. The known flake
(`TestWatcherShutdownDrainsInflightScan`) was rewritten to be deterministic in #761, and
every Windows failure this project has seen was a real portability defect in a test. The
correct response to a new one is to fix the test; the escape hatch is reverting two
lines.

**Done when:** a PR's checks show `gate` waiting on `test (windows-latest)`.

---

## A3 — make the fuzz targets actually fuzz

32 targets exist and none of them fuzz. Without `-fuzz` they run their seed corpora as
ordinary tests, which is worth having but is not fuzzing, and only **one** target
(`FuzzSACDVirtualPathRoundTrip`) has a committed corpus — so nothing accumulates between
runs.

**New workflow** `.github/workflows/fuzz.yml`: nightly `schedule` plus
`workflow_dispatch`. Go permits one `-fuzz` per invocation, so **fan out rather than
rotate**: a setup job enumerates targets with `go test -list 'Fuzz.*' ./...` (verified to
work — filter its trailing `ok <pkg>` lines and emit `{package, target}` pairs as JSON),
and a `strategy.matrix` built with `fromJson` runs every one of them in parallel at
`-fuzztime 5m -fuzzminimizetime 1s`.

Every target gets five real minutes **every night** rather than five minutes a month,
wall clock stays at five minutes, and there is no target count to keep in step with the
tree. The repo is public, so the runner minutes are free.

**`-fuzzminimizetime` is not optional.** CLAUDE.md carries the measurement: on
`FuzzFoldForMatch`, `-fuzztime 60s` alone executes 19,003 inputs and then sits at 0/sec
for 43 seconds, while adding `-fuzzminimizetime 1s` executes 1,302,362 in half the wall
clock — and the run says `PASS` either way. A nightly job without it would report success
having done ~1.5% of the work.

**On a crash:** fail that matrix leg and upload `testdata/fuzz/` as an artifact. Do not
auto-commit corpus entries (noise); a crasher is worth a human-reviewed PR. Use
`fail-fast: false` so one bad target does not cancel the other 31 — the whole point of the
fan-out is a complete nightly picture.

**New target:** `FuzzValidateRelPath` in `internal/upload`. This is the newest untrusted
input surface in the tree — client-supplied relative paths, arriving over HTTP, that
decide where bytes land on the operator's disk — and it has no fuzz coverage against a
written policy that fuzzes exactly this class. Give it a **property assertion**, not just
a no-panic check, in the shape of `FuzzResolveContainment`: a successful
`ValidateRelPath` must return a path that is `path.Clean`-stable, slash-separated, has no
`..` segment, no dot-prefixed segment, and no NUL. Asymmetric, so only a real escape
fails it.

**Done when:** the nightly has run once green, and the new target fails under a
deliberately weakened `validateSegment`.

---

## A4 — a smoke pass over the admin console

`go test` was green through five upload-stack defects, four console defects in #798, and
two separate incidents where a misplaced `boot();` put the whole player module in the
temporal dead zone and the page rendered nothing. Every one was found by a human opening
a browser. Two cheap layers close most of that gap.

**Layer 1 (Go).** `TestEveryAdminPageRenders`: boot the existing `newTestServer`
harness, GET every entry in the `pages` map plus the server-rendered player routes,
assert 200, assert the body parses with `golang.org/x/net/html` (already a dependency),
and assert each page's own content marker is present. Then GET every read-only API route
and assert non-5xx. Enumerate from `pages` and the route table rather than a hand-written
list — a hand-written list is the forgot-the-list failure the test exists to catch.

**Layer 2 (node).** `TestEveryPlayerModuleLoads`: run each ES module under node and
assert it reaches the end of module evaluation without throwing. The precedent exists —
`upload_digest_test.go` already runs shipped client JS under node, including its `t.Skip`
when node is absent; GitHub runners ship node, so add a CI step echoing `node --version`
rather than assuming it ran.

Everything below was measured rather than guessed, because both reviews disagreed about
this layer and both were partly wrong.

- **A stub is required.** Under bare Node 26, **4 of the 8 player modules throw at
  import** — `audio.js`, `boot.js`, `nowplaying.js`, `views.js` — every one on `window`,
  then `location`. Node provides neither and never will, so "drop the stub on Node 22+" is
  not available.
- **The stub is small and it converges.** Iterating to green took five additions and
  lands at **~35 lines**: `window`, `document`, `location`, `sessionStorage`/`localStorage`,
  `Audio`, `EventSource`, plus `createElementNS` and `style.setProperty` on the element
  shape. It is not a growing tax; it is a fixed one.
- **Do not make it a permissive `Proxy`.** The obvious "answer every property" stub —
  which is what "don't play catch-up with inline stubs" argues for — **hangs**: a module
  loops while a truthy value keeps coming back, and the run never terminates. Values have
  to terminate (`querySelectorAll` → `[]`, `parentNode` → `null`, `contains` → `false`).
- **The harness needs a per-module watchdog** for exactly that reason. A stub-induced
  infinite loop must be reported as a failure, not hang CI.
- **Negative controls have to copy the whole directory.** The modules import each other
  relatively, so mutating one in isolation fails on a missing sibling rather than on the
  mutation. Copy `static/player/` to a temp dir, mutate there, run there.

Do not reach for jsdom — a network dependency in CI for a 35-line stub is a bad trade.

**Two honest limitations for the test's docblock.** It catches import-time errors, not
runtime ones — real runtime coverage needs a browser, and that is a deliberate follow-on
with its own cost. And it covers the **module tree only**: `app.js` is a classic script,
not an ES module, so the deleted-helper class there stays covered by the existing
`TestAppJSHasNoCallsToDeletedHelpers` guard rather than by this.

**Done when:** the Go layer fails against a page whose template errors, and the node layer
fails against `boot.js` with `boot();` hoisted above the `const` declarations — **verified
to reproduce**, giving `ReferenceError: Cannot access 'seed' before initialization`, which
is the exact incident this layer exists for.

---

## B1 — retention for the two unbounded tables

Neither `playback_history` nor `device_registrations` has ever had a `DELETE` anywhere in
the tree. Both were documented as future work when they landed in v1.7 and never got it.

### device_registrations — garbage collection, always on

A row is written for every distinct device token that presents a valid bearer. Nothing
removes one, including token revocation — so a registration bound to a revoked token
survives forever, and it can never be used again.

Reap on two predicates:

1. **Orphaned:** `token_id` is not in the auth store's live set. The auth store is a flat
   file, not SQLite, so the live set is passed in from `cmd/bridge` as a bound JSON array
   consumed by `json_each(?)` — the PR #495 shape, which avoids both placeholder-concat
   SQL and the bind ceiling. The set is one row per paired device (**10** on the live
   install), so the blob is a few hundred bytes. A `TEMP` table was proposed and declined:
   it is per-connection, the store runs reads on an unpinned pool, and it would need a
   `*sql.Conn` or `Tx` to be correct — see the review section above for the demonstration.
2. **Stale:** `last_seen_at` older than a configured age (default off).

**Fail closed.** If the auth store cannot be read, pass nothing and skip the reap
entirely. An empty live set means "delete every registration", which is the one outcome
this must never produce — the same asymmetry as the scanner's `errorSubtrees` sparing.

History rows LEFT JOIN registrations for source attribution, so reaping a registration
leaves its history rows unattributed rather than orphaned. That path already exists and
is commented in `history.go`; state it in the reaper's docblock so nobody adds a cascade —
and note there, explicitly, that **`playback_history` declares no foreign key** and
nothing anywhere `REFERENCES device_registrations`, so `PRAGMA foreign_keys = 1` has
nothing to enforce here. That was raised as a blocker in review and is not one; write it
down so it is not raised twice.

### playback_history — a knob, default off, with the consequence stated

One row per play, forever. At a heavy 50 plays/day that is ~18k rows and ~3 MB a year —
so the problem is not today's size, it is that there is no bound and no visibility.

Add `retention.playbackHistoryDays`, **default 0 = keep forever**.

**The floor is the interesting part.** Every smart-mix family is a `GROUP BY path`
aggregation over this table, and their windows run to 90 days (`HourWindow`,
`SessionWindow`, `DeepCutsCutoff`). A 30-day retention would silently gut the
time-of-day and session families. So `Validate` accepts `0` or `>= 90` and **rejects
1–89** rather than clamping — a value that silently does something other than what it
says is worse than a refusal.

**Forgotten Favourites degrades at any window, and this must be documented, not hidden.**
`PlayStatsForgotten` selects tracks whose most recent play predates a cutoff, with **no
lower bound at all** — a track played twenty times two years ago and never since is
exactly what it is built to surface. Any retention deletes that evidence. Put this in the
config field's docblock and in the settings hint next to the control, in one sentence, so
the operator chooses with it in front of them.

### Visibility is the half that pays for itself

Surface both row counts (and the history table's byte size) on the Diagnostics page.
An operator cannot sensibly choose a retention policy for a table whose size they have
never seen, and most will correctly choose "off".

**Where it runs:** one `runRetentionSweeper` in `cmd/bridge`, joined to `bgWriters`,
on a daily tick. Not post-scan — this is unrelated to scanning and should not fire on
every watcher event.

**Done when:** the orphan reap deletes nothing under an empty live-token set (negative
control), `Validate` rejects 45, and the counts render.

---

## B2 — reclaim database free space

Measured on the local bridge DB: `page_size` 4096, `page_count` 9410 (36.7 MiB),
**`freelist_count` 946 — 3.6 MiB, 10% of the file**, `auto_vacuum = NONE`. `VACUUM INTO`
exists only in the backup path; nothing ever compacts the live file, and the reaping
paths added since (duplicate suppression, the threshold reap, trash purge, B1 above) only
add to the freelist.

**Shape:**

- `Store.PageStats(ctx)` — three PRAGMAs, cheap. Surfaced on Diagnostics as
  "Database 36.7 MiB · 3.6 MiB reclaimable" — through `formatBytes`, which is binary
  (the #798 decision), not a second formatter.
- `Store.Compact(ctx)` — `VACUUM` under `s.mu`, refusing when a scan is in flight (reuse
  the `activeScans` predicate the duplicate-stamping pass already uses, not the public
  `IsScanning`, which does not cover subtree scans) and when free disk is under 2× the DB
  size, because `VACUUM` writes a full temporary copy.

  **`PRAGMA wal_checkpoint(TRUNCATE)` MUST run after the `VACUUM`.** In WAL mode the
  vacuum's own output lands in the WAL, so without it the main file does not shrink at all
  and peak disk *rises*. Measured — 5,623,808 → 5,623,808 bytes with the WAL growing to
  2.8 MB, then 2,572,288 with the WAL at 0 once checkpointed. A checkpoint *before* the
  vacuum is optional hygiene; the one after is the whole feature. The table is in the
  review section above.

  Give the whole operation a context deadline so a pathological run is bounded. Note what
  that deadline does and does not do: it bounds the compaction, it does **not** give
  concurrent API clients a fast 503 — they block on SQLite's lock either way. The
  scan-guard and the free-space guard are what protect the bridge; the deadline just stops
  a wedged vacuum from holding `s.mu` indefinitely.
- `POST /api/database/compact`, wired to a button on Diagnostics. **Operator-triggered,
  not automatic:** `VACUUM` takes an exclusive lock, and on a live bridge with a phone
  mid-sync that is a visible stall. Operator-in-the-loop matches how backups, GC and
  clear-all-variants already work.

**Rejected, do not revive without new information:** switching `auto_vacuum` to
`INCREMENTAL`. It cannot be changed on an existing database without a full `VACUUM`
anyway, so setting it for new databases alone creates two populations that behave
differently — strictly worse than one honest button.

**Deliberate follow-on, not this PR:** auto-compacting once after a mass reap (root
removal, trash purge) when the free fraction crosses a threshold. Worth doing, worth
doing separately.

**Done when:** a test asserts **all three** of `freelist_count` → 0, the `.db` file
actually shrinking, and the `-wal` file returning to 0 — the middle one is the assertion
that fails if the checkpoint is dropped or moved before the vacuum, and without it the
test passes against a compaction that reclaims nothing. Plus: Compact refuses during a
scan and on a full disk.

---

## C1 — narrow the GENA callback predicate

`callbackHostAllowed` accepts any callback IP that is loopback, RFC1918-private or
link-local, **or** that equals the SUBSCRIBE source IP. The last clause is the one that
matches the threat model: a renderer's event sink is, by definition, the device that
subscribed. The first three are broader than that and are what CodeQL alert #12 flags.

**Change:** require the callback IP to equal the SUBSCRIBE source IP. Drop the blanket
range acceptance. This keeps every legitimate subscriber working and closes the alert
properly rather than dismissing it again.

**Ship it in two steps, a release apart.** Step one: keep accepting, but log a Warn
whenever the callback IP differs from the source IP, with both addresses. Step two, once
the LAN bridges have run a release without that line appearing: enforce. The concern
raised in review — that some control points bind their outgoing SUBSCRIBE socket to one
interface while requesting callbacks on another — is a claim about specific hardware that
this project cannot verify from here, and observing costs one release. It is the same
posture A2's promotion is being justified on: promote on evidence, not on reasoning.

**A wording fix rides along.** The predicate's comment says "a renderer's event sink" —
but this is the bridge's own GENA SUBSCRIBE handler, so the subscriber is a **control
point** (BubbleUPnP, mconnect, Kazoo), not a renderer. The existing comment has it
backwards and the plan's first draft repeated it.

**No config knob.** A switch that re-opens a request-forgery path is worse than the path.

**Done when:** the table test covers same-IP allowed; a different private IP refused; a
loopback callback from a LAN source refused; hostname refused; `remoteAddr` without a
port; and IPv6 forms of each.

---

## C2 — per-route rate-limit classes

Rate limiting on `/v1` is wired to exactly one route. `POST /v1/upscale`,
`POST /v1/history/batch`, `PUT /v1/playlists/{id}`, `PUT /v1/favorites` and every other
authenticated route are unlimited per token.

**Shape:** add a `rateClass` field to the `route` struct and classify every entry, the
same discipline `kind` and `writeDeadline` already carry — a new route cannot silently
get no limit, because the classification test fails on a missing class. Reuse the
existing per-token `limiterMap`. `/v1/manifest` keeps its bespoke limiter and its
pagination exemption unchanged.

Classes: a write bucket for the mutating routes, a generous read bucket (or none) for
reads, `rateNone` for pairing and health, which have their own limiters and caches.

**The main risk is functional, not performance.** iOS surfaces 429 as a transport error
and does **not** retry — that is why the manifest limiter exempts paginated pulls. A
bucket sized too tight does not slow a sync down, it breaks it. Size every bucket against
the real client's burst behaviour, and say in each class's docblock what traffic shape it
was sized for.

**Size the burst, not just the rate.** The limiter is already
`rate.NewLimiter(rate, burst)`, so both knobs exist. A client that fires 50 requests in
two seconds and then goes quiet passes comfortably under `rate=5/s, burst=100` and fails
under `rate=10/s, burst=10`, even though the second is nominally the looser limit. Sustained
abuse is what `rate` is for; a normal sync is a burst, and the burst parameter is what has
to absorb it.

**Fail open on an empty token ID**, per the existing convention.

**Done when:** the classification test fails on an unclassified route, and a simulated
full iOS sync (manifest pull + artwork sweep + playlist push + history batch) trips
nothing.

---

## D1 — `GET /v1/search` (Mirror-PR pair)

The FTS5 index is fully built — `tracks_fts` plus INSERT/UPDATE/DELETE triggers, created
by migration 7 — and has three consumers: the admin player, admin library search, and the
DLNA ContentDirectory adapter. It is not on `/v1`, so the iOS app can only search what it
has already synced.

**The load-bearing correctness point:** `tracks_fts` is trigger-populated from `tracks`
and therefore contains **duplicate-suppressed rows**. Serving the raw FTS result on `/v1`
would put copies on the wire that `/v1/manifest` deliberately withholds, contradicting
every count beside it and violating the served-population rule. The new store method must
join back to `tracks` on `path` (the primary key, so an index lookup) with
`dupe_suppressed = 0`. **This is the assertion the PR lives or dies on: seed a suppressed
twin and require it absent, negative-controlled against the unjoined query.**

**The query plan is already optimal — measured, not assumed.** Review raised the join as a
planner hazard; on a 2,000-track store the naive form produces exactly the right plan,
because `tracks.path` is the PRIMARY KEY and the `MATCH` forces the virtual table to be the
outer loop:

```
SCAN f VIRTUAL TABLE INDEX 32:M4
SEARCH t USING INDEX sqlite_autoindex_tracks_1 (path=?)
```

An `EXISTS` rewrite plans identically, so pick the join for readability. **Assert the plan
anyway** with an `EXPLAIN QUERY PLAN` test against those two lines — it is three lines of
test and it pins a property a future rewrite could quietly lose. Confirm `ORDER BY rank`
survives the join in the same test.

**Sanitisation already exists — do not add a second layer.** `splitFTSTokens` strips every
rune that is not a Unicode letter or digit and `buildFTSMatchExpr` double-quotes each
surviving token, prefix-expanding only at ≥3 runes, so FTS5 operators cannot reach the
`MATCH`. Review flagged this as a crash risk; it is a commented, deliberate design. Reuse
the helpers unchanged.

**Wire shape.** A DTO under `internal/api/` — never `manifest.TrackHit` straight from a
handler. Return `{tracks: [{path, title, artist, album}], truncated}`; the client already
holds the manifest and `path` is the join key, so full `Track` objects are not needed and
would duplicate the manifest's schema on a second surface. Paths in the manifest's own
form (slashless), stated in PROTOCOL.md like every other path field. Tracks only in v1 —
a folder rollup exists in the store and can be added later, additively.

**Errors:** 400 `query_too_short` below 2 runes (count runes, not bytes — the admin
handler's own comment explains why); 503 `search_unavailable` when FTS5 is absent.

**Feature flag** `"search"` in `/v1/health.features`, gated on `SearchAvailable()` — a
runtime fact, not a config one, so a bridge whose FTS5 probe failed never advertises it.

**Rate class:** land C2 first and give search a bucket. It is the one route a client will
call per keystroke.

**Mirror-PR obligations:** `PROTOCOL.md` here and `docs/BridgeProtocol.md` in the iOS repo
byte-identical, plus the iOS `BridgeSourceClient` consumer, in the same PR pair.
`ProtocolVersion` stays 1 — an additive route plus an additive flag.

**Done when:** the suppressed-twin test is red against the unjoined query, and a
pre-search iOS build is unaffected.

---

## D2 — implement `manualDescriptionURL`

The field is configured, validated at load, and refused at runtime with "not yet
supported". It is the only escape hatch for a network that blocks SSDP multicast, and
today it looks supported and is not.

**The design falls out of one observation: all three surfaces read the same
`upnp.ServerCache`.** `ResolveControlURL` looks up the cache, `LiveHost` derives its
host:port from the cached control URL, and the online chip reads the cache. So a single
insertion point makes all three work, rather than three parallel implementations.

**Shape:** a manual-server poller that, on the discovery cadence, fetches each configured
URL with the existing `discovery.FetchDeviceDescription`, extracts the ContentDirectory
control URL with the existing `lookupContentDirectoryControlURL`, and `Upsert`s a
`ServerInfo` into the same cache.

**Key it under `upnpingest.StableServerKey(srv)`** — the `manual:<sha256(url)>` form —
**not the device's real UDN.** Routing rows, per-server telemetry, `LiveHost` and the
status chip all key on that string; PR #807 already had to carry both spellings for
exactly this reason. Keep the device's real UDN as a display-only field.

**The poller must refresh on the cache's TTL cadence**, because `EvictStale` will
otherwise reap manual entries — and that eviction is *correct* behaviour when the URL
stops answering, which is what makes an unreachable manual server show as offline for
free.

**The duplicate-device case needs a guard.** A device discovered by SSDP *and* configured
by manual URL produces two cache entries under two keys, which the ingest would walk twice
under two routing prefixes — duplicate rows for one upstream. Detect it (the fetched
description carries the real UDN; compare against the configured UDN set), warn, and skip
the manual entry.

Then delete the runtime refusal, the `ResolveControlURL` TODO, and the admin form's "not
yet supported" hint.

**`/v1/health` is affected but not changed:** manual servers will now appear in
`upnpUpstreamServers` with their `descriptionURL`, which is the shape PR #815 just added.
Additive, no bump, no iOS change.

**Done when:** a manual-URL-only server resolves, walks, plays back and reports online;
an unreachable one reports offline honestly; and the SSDP+manual duplicate is refused.

---

## E1 — reconcile and split CLAUDE.md

**The immediate fix:** the PR #316 entry says DLNA `Search` is unsupported and
`SearchCaps` is empty. Both are false — `handleSearch` exists and `searchCapsFields`
advertises `dc:title,upnp:artist,upnp:album`. That is the third stale claim of this kind,
after the WAV/AIFF extractor gap (which the file self-corrects) and the `deletedIds`
field name. The file's own guidance is the right lesson: *check the code before believing
a doc about it* — but a doc that needs that caveat on every read is doing less work than
it should.

**The structural fix.** At 3,095 lines every session pays for the whole file, and its two
kinds of content have different value. Split on one rule:

- **An entry that states an invariant stays.** "Don't do X because Y" is load-bearing on
  every read, and CLAUDE.md is what auto-loads.
- **An entry that narrates what a PR did moves** to `ops/decisions-archive.md`, with a
  one-line pointer left behind.

That is a judgement pass, not a mechanical one, and it is the largest item in this batch
— size it accordingly and do it last, when the batch's own entries are ready to be
written into whichever half they belong in.

**Optional guard, worth prototyping before committing to:** a test that extracts
backticked Go identifiers from CLAUDE.md and asserts each still exists in the tree. The
existence half is mechanical and would have caught the `deletedIds` case. It will be noisy
— prose names many things that are not identifiers — so it needs a strict shape match and
an explicit allowlist, and it is only worth keeping if the allowlist stays short.

---

## Not in this batch

- **Squashing the migration ladder.** Measured at 15–24 ms for a full fresh install; see
  the top of this document. Dropped on the evidence.
- **Cross-source duplicate suppression and UPnP import.** Blocked on a product decision
  (where availability sits in the `outranks` ranking), not on code. `df0c70a` records why.
- **A real browser in CI.** The right long-term answer for console coverage, and a
  deliberate follow-on to A4 with its own cost.
- **Multi-tenancy.** The single writer mutex and single SQLite file are the structural
  ceiling for the cloud posture; that decision constrains everything above it and does not
  belong inside a hardening batch.

---

## Deployment

Nothing here changes on-disk formats or the wire in a way that requires a re-pair or a
re-scan. Two items are operator-visible after deploy:

- **B1** adds config keys, both defaulting to off, and reaps orphaned device
  registrations on first run — expected, and the count is worth reading in the log line.
- **C1 step one changes no behaviour** — it only starts logging when a callback IP and a
  SUBSCRIBE source IP disagree. Read that line on the LAN bridges for a release; enforcing
  in step two is conditional on it staying silent.

Per the repo default, post-merge deploys target `bridge.ars.md` only; the local Mac and
`home-pc` are on explicit ask. Note the local Mac install is currently at `user_version`
30, several migrations behind `main`.
