# LOUPE — the retention / compact / reclaim trio (PRs #819 / #822 / #829)

**Scope (a SURFACE, not a window).** Three PRs merged 2026-09-02, all of them
destructive:

| PR | merge | what it added |
|---|---|---|
| #819 | `a9258b4` | `Store.PageStats` + `Store.Compact` (VACUUM + `wal_checkpoint(TRUNCATE)`), `Scanner.ScanInFlight`, `POST /api/database/compact`, the Diagnostics database panel |
| #822 | `a1a89d7` | `ReapOrphanDeviceRegistrations` / `ReapStaleDeviceRegistrations` / `ReapPlaybackHistory` / `RetentionCounts`, `RetentionConfig`, the daily `retentionSweeper` |
| #829 | `c080f98` | the Diagnostics retention panel + `RetentionCountsAvailable` |

Production files: `internal/manifest/compact.go` (195), `internal/manifest/retention.go`
(168), `cmd/bridge/retention_sweeper.go` (148), `internal/admin/handlers_database.go`
(67), plus hunks in `internal/manifest/{store,scanner}.go`,
`internal/admin/{admin,handlers_diagnostics,handlers_logs}.go`,
`internal/config/config.go`, `cmd/bridge/main.go`, and the console's
`app.js` / `diagnostics.html`. Tests: 21 across four files.

**Nothing has touched these files since they merged** (`git log a9258b4..HEAD --
<files>` returns only #829 itself), so the tree under review is the tree that
shipped. **The trio is LIVE on `bridge.ars.md`** (`v0.1.9-142-g19b00dc`), with
both retention knobs at their default 0.

## Phase 0 — the structural check

This repo has no `.claude/rules/`, so the structural question is whether the
surface has a `### ` section under `## Things that have bitten before`.

**It does not.** Its three rules are scattered across two other sections:

- `wal_checkpoint(TRUNCATE)` after VACUUM → **### Scanner, deletion passes, and manifest writes**
- the fail-closed empty-token-set guard → **### Config, settings and process lifecycle**
- `playbackHistoryDays` refuses 1–89 → **### Config, settings and process lifecycle**

All three claims are **still true** — verified against the source. But the
placement is itself a finding: the two most destructive `DELETE` statements in
the tree have their rules filed under *Config, settings and process lifecycle*,
away from the section that opens by declaring deletion passes "the highest-stakes
area in the codebase". A session about to change `internal/manifest/retention.go`
reads the Scanner section and finds nothing about it.

`ops/engineering-log.md` carries the record for #819/#822 under
*"## 2026-09-01 improvement batch (PRs #816–#827)"*. **#829 is not in the log at
all** — it merged after that entry was written and nothing added it.

## Findings

Severity, mechanism, trigger, consequence, evidence, fix shape, test.

### F1 — HIGH — a large `retention.*Days` value deletes the ENTIRE table

`cmd/bridge/retention_sweeper.go:98,110` computes the cutoff as
`now.AddDate(0, 0, -days).UnixNano()`. Go documents `UnixNano` as **undefined
outside 1678–2262**, and `internal/config/config.go:2440-2455` puts **no ceiling**
on either knob — it refuses negatives, and (for `playbackHistoryDays`) 1–89.

Past ~349 years the value wraps, and it wraps *chaotically*: some day counts land
negative (caught by the reaps' `beforeNS <= 0` guard, a harmless no-op) and some
land as a **huge positive number greater than `now`**, at which point
`DELETE FROM playback_history WHERE started_at < ?` matches **every row**.

Measured (`now = 2026-09-06T21:00Z`):

| `days` | resulting date | cutoff ns | what the reap does |
|---|---|---|---|
| 99,999 | 1752-11-22 | −6.85e18 | no-op (the `<= 0` guard catches it) |
| 100,000 | 1752-11-21 | −6.85e18 | no-op |
| **127,455** | 1677-09-20 | **+9.22e18** | **deletes every row** |
| **200,000** | 1479-02-06 | +2.96e18 | **deletes every row** |
| **365,000** | 1027-05-07 | +7.15e18 | **deletes every row** |
| **999,999** | −0712-10-10 | +7.62e18 | **deletes every row** |
| **1,000,000** | −0712-10-09 | +7.62e18 | **deletes every row** |
| 2,147,483,647 | −5877584-02-27 | −3.45e18 | no-op |

**The trigger is not exotic.** `999999` is the canonical "effectively infinite"
placeholder — exactly what an operator hedging *"set it very high so it never
actually deletes anything"* types. They get the opposite: the whole table, gone,
permanently, on the next daily tick, and the log line reports it as success —
`retention: reaped playback history past the window rows=18432 days=999999`.
For `deviceRegistrationDays` the same input wipes every registration, which
un-attributes all history and forces every paired device to re-register.

This is the class CLAUDE.md already names under **### Config, settings and
process lifecycle** — *"Cadence ceilings are unit-appropriate and ports admit 0"*,
born from an overflow that PANICKED `bridge serve` at startup. That rule was
applied to the cadence fields and not to these two.

- **Fix**: a `MaxRetentionDays` ceiling in `config.Validate`, **refusing** rather
  than clamping — the same shape as the existing 90-day floor, whose docblock
  already argues that *"a value that quietly does something other than what it
  says is worse than a refusal"*. 36,500 (100 years) is comfortably inside the
  representable range and far past any real retention policy.
- **Belt**: the reaps must also refuse a cutoff that is not in the past. The
  config layer is the right place for the message, but `internal/manifest` is a
  library with other callers and a store method that can delete a whole table
  when handed a future timestamp is a loaded gun regardless of who validates.
- **Test**: table-driven over the measured day counts above, asserting
  `Validate` refuses; plus a store-level test that a future cutoff reaps zero
  rows. Negative control: remove the ceiling → the `999999` row goes green-to-red.
- **Wire**: none.

### F2 — HIGH — the Compact button fails on every Windows install

`internal/manifest/compact.go:144` calls `freeBytes(s.path)` — the **database
file** path. `cmd/bridge/main.go:3909` wires that to
`transcode.AvailableDiskSpaceNearest`, whose contract is a **directory**: the
parameter is named `dir`, its doc says *"free space for `dir`"*, and its whole
ancestor walk is directory-shaped. Every other caller in the tree passes a
directory (`handlers_space.go:93`, `handlers_library_browse.go:801`,
`upload/space.go:11`, `auto_optimize.go:73`).

The wrapper `os.Stat`s first; the database file **exists**, so it does not walk
up and hands the file path straight to the platform probe.

- POSIX `syscall.Statfs` accepts any existing path, file included — so macOS and
  the Linux VPS work, which is why this shipped.
- Windows `GetDiskFreeSpaceExW` takes `lpDirectoryName`. `kernelbase` opens the
  path with `FILE_DIRECTORY_FILE`; a regular file returns
  `STATUS_NOT_A_DIRECTORY` → Win32 **267 `ERROR_DIRECTORY`**,
  *"The directory name is invalid."*

Result on the Windows home-pc bridge: `POST /api/database/compact` returns 500
with `manifest: probe free space: GetDiskFreeSpaceEx "…\bridge.db": The
directory name is invalid.` — **always**. The button never works on Windows.

`internal/manifest/compact_test.go` never exercises the real probe: line 64
passes `nil`, lines 109/117/123 pass stubs. So the blocking Windows CI leg has
never called `AvailableDiskSpace` with a file path.

Evidence: Gemini consult (`gemini-3.8-flash`, 2026-09-06) separating documented
(`lpDirectoryName` is "a directory on the disk") from inferred (the
`FILE_DIRECTORY_FILE` open), and confirming the behaviour is uniform across
Windows versions, filesystems, `\\?\` extended paths and UNC.

- **Fix**: pass `filepath.Dir(s.path)`. Fix the CALLER, not the shared helper —
  the helper has one contract and five correct callers; giving it a second
  behaviour on Windows only would make the two platforms diverge. Correct
  `Deps.DBFreeBytes`'s doc from "the given path" to "the given directory".
- **Test**: assert the contract, not the symptom, so it fails on **every**
  platform rather than only on the Windows leg —
  `TestCompactProbesADirectoryNotTheDatabaseFile` captures what `Compact` hands
  the probe and `os.Stat(...).IsDir()`s it. Negative control: revert to
  `s.path` → red everywhere.
- **Wire**: none.

### F3 — HIGH — the knob the console tells operators to set does not exist

`internal/admin/templates/diagnostics.html:78` tells the operator, in the panel
whose whole purpose is to make retention a decision:

> *…everything else is kept unless you **set a window in Settings**.*

There is no such control. Verified on four surfaces:

- `settingsPatch` (`internal/admin/handlers_api.go:1974`, 28 fields) has no
  retention field.
- `internal/admin/settings_apply.go` has no retention entry.
- `internal/admin/templates/settings.html` has no retention control.
- `ops/settings-apply-semantics.md` has no retention row — its only "retention"
  is `backupKeep`.

**An env override does exist** — and this is a claim I got wrong first time
round and had corrected by measurement. Grepping for a literal `BRIDGE_RETENTION`
finds nothing because the overrides are *derived reflectively* from the `Config`
struct (`internal/config/env.go:230`). Probed against the real function:
`BRIDGE_RETENTION_PLAYBACK_HISTORY_DAYS=365` and
`BRIDGE_RETENTION_DEVICE_REGISTRATION_DAYS=400` both apply. `Save()` also
round-trips the block. So the knobs are *settable* — by hand-edited YAML or an
env var — just not by the route the console names, and `EnvOverrideDocs()` has
no production caller, so the derived names are printed to an operator nowhere.
Outside the Go source the knobs are named only in `CLAUDE.md`,
`ops/engineering-log.md` and the batch plan — all internal. Nothing in `docs/`,
no README mention, no sample `bridge.yaml`.

Two consequences follow:

1. **The promise is false.** CLAUDE.md's own rule from the last LOUPE run —
   *"An operator-facing promise is a contract with an expiry date"* — is the same
   class, and the sibling rule *"A settings control that renders but isn't in the
   PATCH allowlist saves nothing while the page still says 'Saved.'"* is its
   mirror image. `internal/manifest/retention.go:120` also cites *"the settings
   hint"* as a place the Forgotten-Favourites consequence is stated. There is no
   settings hint.
2. **The live-config machinery is unexercisable.** `retention_sweeper.go:47`
   reads `r.cfg()` per pass, documented as *"a settings change takes effect on
   the next tick rather than at the next restart"*. `cfgHolder` only ever moves
   through `RuntimeConfig.Update`, which is the settings PATCH — and there is no
   config-file watcher (`grep` for one returns nothing). With no PATCH field, the
   value **cannot** change without a restart. The comment describes a capability
   nothing can reach.

`settings_matrix_doc_test.go` cannot catch this, and the precise reason matters:
the test IS bidirectional, but its universe is `reflect.TypeOf(settingsPatch{})`
— never `config.Config`. So the guarantee it gives is
**`settingsPatch` ⇄ doc ⇄ handler**, a closed loop a config field can sit
entirely outside of. `retention.*` is the first field to do so. Its sibling
`TestEveryConfigLeafIsEnvSettable` does walk the real `Config`, but only asks
"is it env-settable", never "is it reachable or documented".

- **Fix**: add the two fields to `settingsPatch` + `settings_apply.go` + the
  Settings template + `ops/settings-apply-semantics.md` (class **live**: read per
  pass, no rearm needed — a ticker period is not involved). That makes the
  panel's sentence true, makes the live-read real, and puts the fields under the
  existing matrix guard. The 90-day floor and the new F1 ceiling are enforced by
  `Validate`, which the PATCH path already runs.
- **Test**: the matrix guard covers it once the patch fields exist; add a handler
  test that a PATCH lands live (no restart banner) and that an out-of-range value
  is refused with the config error rather than silently accepted.
- **Wire**: none. Admin-only.

### F1a — the same overflow, with NO live-token backstop, on the registrations table

Worth calling out separately because the guard that people will assume covers it
does not. `ErrNoLiveTokens` protects only the **orphan** reap. The
`ReapStaleDeviceRegistrations` branch (`retention_sweeper.go:97-105`) is a plain
`last_seen_at < cutoff` DELETE with no token set involved at all, so an
overflowed `deviceRegistrationDays` empties `device_registrations` **including
rows bound to live, working tokens**.

Driven end-to-end through the real `retentionSweeper.sweep()` against a real
`manifest.Store`:

```
days=365       cutoff=1757185792534211000    historyRows=1  registrations=1
days=365000    cutoff=7146215091970680232    historyRows=0  registrations=0
days=999999    cutoff=7622533713116351080    historyRows=0  registrations=0
days=1000000   cutoff=7622447313133224080    historyRows=0  registrations=0
```

Both tables emptied — including a playback event recorded one second earlier and
a registration bound to a live token. Sweeping `days ∈ [1, 400000]`, **145,092
values wipe the table**, in two bands: `127455..213504` and `340959..400000`.

The store's `if beforeNS <= 0 { return 0, nil }` guard is **inverted with respect
to this**: it catches the negative wrap, which was already harmless because
`started_at` is positive, and passes the positive wrap straight through.

### F5 — MEDIUM — the test the PR says it fixed still pins nothing

`cmd/bridge/retention_sweeper_test.go:77`,
`TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead`.

PR #822's body tells this story well: the original asserted that registrations
*survived* a token-read failure, that went green against a sweeper that skipped
nothing (the store's own `ErrNoLiveTokens` saves the rows either way), it was
found by negative-controlling, and the fix was a `countingReaper` asserting the
reap is never **attempted**.

The upgrade is real. **The fixture still cannot distinguish the guard it names
from the one beside it.** `liveToken` returns `(nil, error)` — and a `nil` slice
satisfies `len(ids) == 0` as well as `err != nil`. Mutating away the fail-closed
`case err != nil:` skip entirely:

```
###### MUTATION: delete the 'err != nil' fail-closed skip ######
ok  github.com/acoseac/1-bit-bridge/cmd/bridge  0.719s      <-- SURVIVES
```

The only fixture that separates them is a **partial read** —
`return []string{"tok-a"}, errors.New(...)` — which fails against the mutation
and passes against the real code. This is CLAUDE.md's own rule, one level in:
*"A fixture must be a value the transformation would actually change."* The
test's comment — *"(Verified — the first version of this test did exactly
that.)"* — records a verification that stopped one step short.

- **Fix**: change the fixture to a partial read and keep the `nil` case in its
  sibling test, where it belongs.
- **Test**: this IS the test. Negative control: the mutation above must go red.

### F6 — MEDIUM — half the configurable feature is untested

`DeviceRegistrationDays` is never set to a non-zero value anywhere in the tree.
Deleting the whole stale-registration branch from `sweep` leaves the **entire
`cmd/bridge` suite** green:

```
###### whole stale-reg reap DELETED, entire cmd/bridge suite ######
ok  github.com/acoseac/1-bit-bridge/cmd/bridge  8.805s
```

`internal/manifest/retention_test.go:175` tests the *store* method; nothing tests
that the sweeper wires the config field to it. For contrast, the two mutations
the suite does catch: removing the `len(ids)==0` guard fails
`TestSweepSkipsTheOrphanReapOnAnEmptyTokenSet`, and `days > 0` → `days >= 0`
fails two more. Per F1a this is the branch with no live-token backstop, which
makes it the worse half to leave unpinned.

- **Fix**: extend `TestSweepHonoursTheWindowsAndDefaultsToOff` to cover the
  registration window too.

### F4 — MEDIUM — two comments justify a 5-second poll with "no database", which is no longer true

`internal/admin/handlers_diagnostics.go:83-88`:

> *Every field reads either an atomic counter or a sliding-window quantile
> snapshot, so this returns in well under a millisecond … and needs no TTL
> cache. **It touches no database.***

`internal/admin/static/app.js:5468`, justifying `DIAGNOSTICS_POLL_MS = 5000`:

> *the numbers here are cheap (atomic counters and sliding-window quantiles,
> **no database**), so this can poll…*

Both were true before #819/#829. The body now runs three PRAGMAs, plus
`SELECT COUNT(*), COALESCE(MIN(started_at),0) FROM playback_history` and
`SELECT COUNT(*) FROM device_registrations` — and the docblock's own inline
comments ("Three PRAGMAs, microseconds", "Two COUNTs and a MIN") sit directly
below the sentence denying it.

`playback_history` has indexes on `(device_token, id DESC)` and `(path)` — **none
on `started_at`** — so `MIN(started_at)` forces a full table scan. Measured
against the real driver with the real schema:

| rows | `COUNT(*) + MIN(started_at)` | `COUNT(*)` alone | 3 PRAGMAs |
|---|---|---|---|
| 18,000 (≈1 yr, heavy listener) | **1.02 ms** | 55 µs | 7 µs |
| 90,000 (≈5 yr) | **9.02 ms** | 748 µs | 8 µs |
| 500,000 | **39.5 ms** | 5.5 ms | 12 µs |

`EXPLAIN QUERY PLAN` → `SCAN playback_history`. The PRAGMAs are genuinely free;
the `MIN` is the whole cost.

**Being honest about the size of this.** At realistic sizes — the config docblock
itself says ~18 k rows a year — 1 ms every 5 s is a 0.02 % duty cycle. This is
not a performance emergency and I am not going to dress it up as one. The
load-bearing half of the finding is the **two false comments**, and specifically
that one of them is the stated *justification for the design choice*: a future
session reading "no database, so this can poll" will add the next database read
to the same handler on the same reasoning, which is precisely how the second one
got here. CLAUDE.md's rule — *"Expensive snapshots go behind a TTL +
singleflight and never on a fast tick"* — is the guardrail that was silently
crossed.

- **Fix**: correct both comments (that is the defect), and put the DB-backed
  block behind the short TTL + singleflight this server already uses for the
  composition and coverage snapshots (`libMetaCache`, `catalog.go`) — cheap
  insurance that makes the corrected comment stay true as the table grows. No
  migration: a `started_at` index would tax every history insert to speed a
  telemetry read.
- **Test**: a counting fake asserting the second call inside the TTL does not
  re-query, and that the numbers move again after it expires.
- **Wire**: none.

### F9 — HIGH — the panel says "nothing to reclaim" about a database a VACUUM would halve

`PageStats.ReclaimedBytes` is `PageSize * FreelistCount`, documented as
*"what a Compact would return"*. `freelist_count` counts only **wholly free**
pages; VACUUM also repacks **intra-page fragmentation**, and scattered row
deletion — what every reaping path the panel's own prose enumerates actually
does — produces exactly that and no free pages.

Reproduced independently on a 72,474,624-byte store:

| deletion pattern | `freelist_count` | panel says reclaimable | VACUUM actually returned |
|---|---|---|---|
| **every 2nd row** | **0** | **0 → "nothing to reclaim"** | **36,233,216 (50.0 % of the file)** |
| 9 of every 10 | 15,536 | 63,635,456 | 65,220,608 |
| the first 90 % (contiguous) | 15,920 | 65,208,320 | 65,216,512 |

On the real `tracks` schema the scattered case is milder but not better: floor
81,920 against 5,398,800 actually reclaimed — a **66× understatement**, which
the panel would render as "1 %". Both shapes are the same defect.

`app.js:5524` renders the literal string `"nothing to reclaim"`, which an
operator correctly reads as *"don't press the button"*. That is the exact
inverse of the rationale in `handlers_diagnostics.go` (*"an operator cannot
sensibly decide whether to compact a file whose size they have never seen"*),
and the same confident-wrong-answer class that `RetentionCountsAvailable` was
added six lines below to prevent.

**The repo already brushed against this.** `compact_test.go:57-59` carries a
fixture guard — `if pre.FreelistCount == 0 { t.Fatal("fixture produced no free
pages; the test would prove nothing") }` — so freelist-reads-zero-on-real-
deletions was hit during development and worked around in the fixture rather
than recognised as a product-level under-report.

- **Fix**: rename to `FreePageBytes`, document it as a **floor**, render
  "at least X", and never render a zero as an answer to "should I compact".
- **Test**: assert the floor never *overstates* a real VACUUM, across a
  scattered and a contiguous fixture.

### F10 — MEDIUM/HIGH — the before/after figures ignore the WAL, so a compaction can report GROWTH

`before` is `os.Stat` of the main file alone, taken *before* the pre-VACUUM
checkpoint; `after` is taken once the mandatory post-checkpoint has truncated
the WAL to zero. The two measure different things. Measured against a store
whose WAL could not be checkpointed (one open reader is enough):

```
pre : .db=4,096       -wal=222,088,632
post: .db=2,334,720   -wal=0
admin JSON: {"beforeBytes":4096,"afterBytes":2334720,"reclaimedBytes":-2330624}
```

The console reported that the compaction **added 2.3 MB**. The same number feeds
the headroom guard whose entire job is preventing an ENOSPC mid-VACUUM: it
demanded **8,192 bytes** free for a vacuum that needed ~4.6 MB.

- **Fix**: a `dbFootprint` helper summing the main file and `-wal`, used for
  both sides and for the headroom multiplier.
- This also makes PRISM's "clamp `reclaimedBytes` to non-negative" nit more than
  a nit — the negative is reachable — so the clamp ships as a belt.

### F11 — MEDIUM — a cancelled context after a successful VACUUM is reported as failure

If the context dies during the mandatory post-checkpoint, `Compact` returns
`post-VACUUM checkpoint: context canceled` and the handler 500s — *after* the
vacuum has committed. That is the mirror image of the failure `compact.go`'s
header exists to prevent ("a button that reports success and reclaims nothing"),
and nothing distinguishes it from a genuine failure. It should be reported as
`CheckpointBusy`, which is precisely what that field already means.

Note the related mechanism, measured and left alone: `r.Context()` does **not**
cancel on a browser abort today, but only because the handler never touches
`r.Body` — Go starts its disconnect-detector goroutine on a body read. One line
of body decoding would arm it. `BaseContext` means SIGTERM and `POST
/api/restart` already cancel. A cancelled VACUUM is itself safe:
`integrity_check: ok`, rows intact, writes work, retry works, no stray temp files.

### F12 — LOW/MEDIUM — the scan guard is check-then-act, and reads as if it were exclusion

`Compact`'s docblock states the requirement as sufficient (*"Callers MUST refuse
to call it while a library scan is in flight"*). It is a one-directional guard:
nothing refuses or defers a **scan** during a compaction, and a scan starting in
the gap — the 6-hour timer, an fsnotify subtree scan, an upload commit, a second
tab's `POST /api/scan` — blocks on `Store.mu` for the vacuum's whole duration,
which is the outcome the guard exists to prevent. Two concurrent Compacts become
two sequential vacuums; `btn.dataset.busy` is per-document and cannot see another
tab.

CLAUDE.md already has the rule: *"A write gate on a second process is a GUARD,
not mutual exclusion — say which."*

- **Fix**: say which. A compaction-in-flight flag the scanner must honour is
  declined for the same reason `bridge restore` narrows its window rather than
  taking a lock — the consequence is a stall, not corruption, and the flag would
  give a wedged vacuum the power to silence the scanner.

### F13 — MEDIUM — `PageStats` failure has no availability flag while `RetentionCounts` does

`RetentionCounts` failing sets `RetentionCountsAvailable = false` and the panel
says "unavailable", with an explicit rationale. `PageStats` failing in the block
immediately above has no such flag: the sizes stay 0 and the panel renders
**"0 B"** and **"nothing to reclaim"** with the same confidence as a reading.
The identical argument applies verbatim; the flag landed on one block because
that is the one a bot happened to review.

### F14 — MEDIUM — the bug-report bundle pays the full DB cost and prints none of it

`apiLogBundle` now calls `diagnosticsSnapshot(ctx)`, so it does three PRAGMAs,
two COUNTs and a MIN it did not do before #819 — and prints **none** of
`DatabaseBytes`, `DatabaseFreePageBytes`, `PlaybackHistoryRows`,
`DeviceRegistrationRows`, `OldestPlaybackStartedAt`. Every new field is computed
and discarded.

The split's stated purpose is *"so the bug-report bundle embeds the SAME numbers
the page shows … two assemblies of them is how the bundle and the page come to
disagree"*. They now disagree by construction, on five fields. The cost is
irrelevant (one-shot download); the missing output is the finding — "how big is
your database" and "how many history rows" are among the most useful things to
know when triaging a stranger's bridge from a pasted text file.

### Test-quality findings folded into the PRs above

- `TestPageStatsIsInternallyConsistent` recomputes `PageSize*FreelistCount` from
  `PageStats`' own output — a tautology. Only
  `TestCompactReclaimsFreePagesAndShrinksTheFile` catches a swapped PRAGMA.
- `TestDatabaseCompactReclaimsAndReports` asserts
  `ReclaimedBytes == Before-After`, which the handler computes from those same
  two fields. It holds against a `Compact` that never vacuums, against removing
  the mandatory post-checkpoint, and against F10's negative figure. Its name
  over-claims: the store is fresh, nothing is deleted, nothing is reclaimed.
- `TestDiagnosticsCarriesDatabaseSize` checks only that the floor's **key**
  exists; the field has no `omitempty`, so it is always present whatever it holds.
- **`RetentionCountsAvailable` is only ever asserted `true`.** Hoisting it out of
  its `err == nil` branch leaves every test green while the field's entire reason
  for existing evaporates and the JS branch becomes dead code.
- The **507** `insufficient-disk-space` mapping had never executed:
  `newTestServer` leaves `Deps.DBFreeBytes` nil, so `Compact`'s headroom check is
  skipped in every admin test. `CheckpointBusy` is never asserted to reach the
  wire either.

## Rejected, with the reason — so the next run does not re-raise them

- **PRISM: "`sweep` does not abort on context cancellation" (MEDIUM).**
  *Partly accepted, downgraded.* Real but tiny: on shutdown `scanCtx` cancels,
  the three reaps each take `Store.mu`, get `context.Canceled` from
  `ExecContext`, and log three `Warn` lines about a failure that is a clean
  exit. The window is the few milliseconds a day a sweep is actually running, and
  `bgWriters` already waits for it, so nothing is corrupted — but a `Warn` an
  operator would investigate on every unlucky restart is worth two lines. Taken
  as a ride-along on PR 1, not as a finding.
- **PRISM: "fall back to `time.Now` if `r.now` is nil" (quick win).** *Rejected.*
  `retentionSweeper` is a struct literal with one production construction site
  (`cmd/bridge/main.go:2974`) and five test sites, all of which set `now`. A nil
  clock would panic on the first tick of the first test run — loud, immediate,
  unshippable. The repo's idiom for a defaulted clock is a constructor that sets
  it (`OpenStore` does exactly this for `Store.now`); there is no constructor
  here, and adding a nil-check inside the hot path instead is defensive noise
  against a caller that cannot reach production.
- **PRISM: clamp `reclaimedBytes` to non-negative (quick win).** *Accepted as a
  ride-along on PR 2, not as a finding.* `AfterBytes > BeforeBytes` is not
  reachable in any way I could construct, and the browser already renders a
  non-positive value as "Nothing to reclaim." (`Number(x) || 0` then `> 0`), so
  there is no user-visible bug — but a negative `reclaimedBytes` on the wire
  would be a dishonest number and the fix is `max(0, …)`, which this repo
  already uses (`projection.go`).
- **Shutdown wiring.** *Refuted.* `bgWriters.Wait()` is at `main.go:2158`,
  written **inline** in the defer registered at `:2156` — exactly the shape
  CLAUDE.md requires — grace-bounded by a 5 s timer and registered after
  `defer manifestStore.Close()` so LIFO drains writers first. `scanCtx` derives
  from the `signal.NotifyContext`. The sweeper's `Add(1)`/`defer Done()` are
  correct.
- **`retention.*` has no env override.** *Refuted — this was my own claim, and
  measurement corrected it.* See F3.
- **`Save()` may not round-trip the retention block.** *Refuted* — probed; both
  fields write and read back.
- **A fixed `time.NewTicker(24 h)` violates the "cadence provider needs a
  rearm" rule.** *Refuted.* The dividing line in this repo is whether a **config
  field** controls the cadence, not ticker-vs-timer. Five loops use
  `runSweepLoop`; five use a plain const ticker (`runArtworkCacheSweeper`, the
  upload and trash sweepers, duplicates, and a second hardcoded 24 h ticker in
  `tailscale.go:216`). `retentionSweepInterval` is a compile-time const with no
  config field, so it is on the const side and idiomatic.
- **Retention should join `cadenceRearms` / `postScanNudges`.** *Refuted.*
  `cadenceRearms` is fired only by `ScanIntervalSec`, `BackupIntervalHours` and
  `UpdateCheckIntervalHours`; `runRetentionSweeper` takes no interval provider,
  so a rearm would have nothing to re-read. The `postScanNudges` exclusion is
  deliberate and documented in the file's header.
- **`time.After` timer leak on launcher-menu re-entry.** *Refuted* —
  `runRetentionSweeper` already uses `NewTimer` + `defer Stop`, the PR #290
  convention, and says so.
- **The fail-closed orphan guard can be defeated by an auth-store read
  failure.** *Refuted for the total-wipe case.* Every realistic empty path —
  ENOENT in `reloadIfStale`, a zero-byte or `null` `tokens.json` — arrives as
  `(empty, nil)` and is caught by the `len(ids) == 0` branch. See F7 for the
  residual narrow case that remains.
- **The Jobs page has a dead "Retention" row.** *Refuted* — `jobs.html:219` is
  the Backups card's `backupKeep`, unrelated to this sweeper.
- **`MarkScanInFlightForTests` is an exported test seam in a production file.**
  *Refuted as a defect.* One balanced caller pair
  (`handlers_database_test.go:50-51`); the name is deliberately unusable-looking
  and the docblock says production must not call it. Worth knowing that an
  unbalanced call drives `activeScans` negative and would make `ScanInFlight`
  read false *during a real scan* — but nothing does that, and a guard for it
  would cost more than it protects.

## Recorded, not fixed

- **F7 — LOW — expired tokens keep their registrations alive.**
  `auth.Store.List()` does not filter on `ExpiresAt`; expiry is enforced only in
  `Validate()`. Revoked tokens are hard-deleted so they are structurally absent,
  but an **expired** one stays in the live set and its registration is never seen
  as an orphan. By the sweeper's own rationale — *"a row bound to a revoked
  token can never be used again; that is garbage, not policy"* — an expired
  token's row is equally garbage. Left alone deliberately: filtering `List()`
  would change the semantics of a function with many other callers, and doing it
  in the closure instead splits the definition of "live token" across two
  places. It belongs in a decision about token expiry, not in this batch.
- **F8 — LOW — a bridge restarted inside 5 minutes never sweeps at all.**
  `retentionSweepSettleDelay` gates the *first* pass, so an install that never
  stays up 5 minutes runs the sweep zero times rather than once. `runServe` is
  re-entered in-process by the launcher menu, and each re-entry starts a fresh
  settle timer. Benign in steady state; recorded because "retention never runs"
  is otherwise a mystifying operator report.
- **`ReapPlaybackHistory`'s DELETE is a full table scan** — no index on
  `started_at` (see F4's measurement) — held under `Store.mu`. Daily, and only
  when the knob is on; ~40 ms at 500 k rows. Not worth an index that would tax
  every history insert.
- **`EnvOverrideDocs()` has no production caller.** The derived env names are
  printed to an operator nowhere. Pre-existing, not from this trio, but it is
  why "just use the env var" does not rescue F3's discoverability.

## Phase 4 — plan review, and the dispositions

An adversarial pass over my own findings, before any code.

**Accepted as written**: F1, F1a, F2, F5, F6.

**Corrected before adoption:**

- **F3's "no env override" was wrong**, and measurement corrected it —
  `BRIDGE_RETENTION_*` derives reflectively and applies. The finding survives
  (the console names a route that does not exist) but the framing changes from
  *"unsettable"* to *"settable by two routes the operator is told nothing
  about"*, which is a weaker and more accurate claim.
- **F3's "the matrix guard runs one direction only" was imprecise.** It is
  bidirectional; its *universe* is `settingsPatch`, not `config.Config`. Same
  conclusion, correct mechanism.
- **F4 was overstated as a performance finding.** Measured, it is 1 ms every 5 s
  at realistic sizes. Reframed around the two false comments, with the TTL as
  insurance rather than as a fix for a stall that is not happening.
- **F1's fix is complete at the config layer, and I checked rather than
  assumed.** `Load` runs `applyEnvOverrides()` → `resolvePaths` →
  `NormalizeAndValidate()` (`config.go:2142-2152`), and the admin settings PATCH
  runs `NormalizeAndValidate()` too (`handlers_api.go:2633`). So a ceiling in
  `Validate` gates YAML, env **and** (once F3 lands) the console. The
  store-level belt is still worth adding — `internal/manifest` is a library and
  a method that empties a table when handed a future timestamp is a loaded gun
  regardless of who validates upstream — but it is defence in depth, not the
  gate.
- **F2's `filepath.Dir` is safe on a relative path**, which I checked because
  the consult raised it: a relative `bridge.db` yields `"."`, and since a
  relative DB path resolves from the CWD, `"."` *is* its directory — the right
  volume on both platforms.

**Rejected**: see the section above.

**The one judgement call worth naming.** F3's fix builds a new Settings control,
which brushes against "product direction". I am treating it as **completing a
decision already shipped**, not making a new one: `config.go`'s docblock, #822's
PR body and #829's PR body all frame these knobs as operator-facing (*"an
operator who looks and decides 'off' has made a decision rather than inherited
one"*), and the console already tells the operator to use Settings. The cheaper
alternative — leave the control unbuilt and correct the sentence to name
`bridge.yaml` — is a legitimate call and is one line of HTML; it is flagged here
so it can be taken instead.

## Phase 5 — execution: three file-disjoint PRs off `main`

One PR per script run. A/B/C touch no file in common, so they go in parallel
rather than stacked.

| PR | findings | files |
|---|---|---|
| **A — the reaps are unsafe at the edges** | F1, F1a, F5, F6, + the ctx ride-along | `internal/config/config.go`, `internal/manifest/retention.go`, `cmd/bridge/retention_sweeper.go`, and the three test files |
| **B — the compact probe wants a directory** | F2, + the `max(0,…)` ride-along | `internal/manifest/compact.go`, `internal/manifest/compact_test.go`, `internal/admin/handlers_database.go`, one doc line in `internal/admin/admin.go` |
| **C — make the console tell the truth** | F3, F4 | `internal/admin/handlers_api.go`, `settings_apply.go`, `handlers_diagnostics.go`, `templates/settings.html`, `templates/diagnostics.html`, `static/app.js`, `ops/settings-apply-semantics.md` |

Every PR: a transactional count-asserted apply script in the scratchpad, dry-run
first; commit **before** the negative control; each control's mutation
count-asserted and `grep`-verified in the source; the red set predicted by name
and reconciled; `-count=1` throughout.

**Wire-protocol impact: none.** No `/v1` shape, no DTO, no `PROTOCOL.md` section,
no `ProtocolVersion` move, and therefore **no iOS Mirror-PR**. Everything here is
admin-console, config and store-internal.


## Phase 5 execution log — what the controls actually taught

Recorded because three of them changed the code, not just confirmed it.

**PR A — `fix/retention-reap-input-bounds` (#859).** Five controls, all
predicted by name and reconciled. One correction: the first form of the
fail-closed control (`case false:`) **failed to build** — `err` lost its only
use — and a control whose build fails proves nothing. Replaced with the
realistic weakened guard `case err != nil && len(ids) == 0`, which is the wrong
implementation a reader would actually write, and which yields the decisive
counterfactual: the new fixture FAILS under it, the shipped fixture PASSES.

Writing `TestNoAcceptedWindowProducesAFutureCutoff` also corrected my own
proposed constant: a 100-year window reaches back past 1970, so its cutoff is a
NEGATIVE UnixNano and the reaps' `<= 0` no-op turns it into "delete nothing".
That is the right behaviour, but "the cutoff is positive" was the wrong property
to assert. The test now sweeps the whole accepted range for the property that
matters — no accepted value may land at or after `now`.

**PR B — `fix/compaction-honest-numbers`.** Nine controls, and three of them
found tests of mine that pinned nothing:

- **b3 (make the floor overstate) passed.** The mutation set
  `FreelistCount = PageCount`, which on this fixture still came out *below* the
  footprint delta — so it never produced the overstatement the assertion tests
  for. The mutation was not a value the transformation would change, one level
  up from the rule this batch is about. Replaced by pinning the half that
  actually shipped the wrong answer: the **rendering**, in
  `TestTheConsoleNeverCallsTheFreePageFloorAnEstimate`.
- **b2 (revert to the WAL-blind `before`) passed**, because the fixture never
  reached a state where the WAL held a meaningful share of the database. A
  second connection parked in a read transaction is what gets there; the test
  now `t.Fatal`s if the WAL is empty rather than asserting into thin air.
- **b8 (make a cancelled context a failure again) passed**, because there was no
  test at all — the branch is reachable only by winning a race. It is now a pure
  `checkpointOutcome` helper driven across six cases.

**And one control falsified an assertion I had just written.** The held-WAL
fixture showed that *"a compaction can never report growth"* is **false**: with
the reader still parked, the post-checkpoint answers busy, the WAL is not
truncated, and the vacuum's output sits in it on top of the original —

```
main=1,671,168  wal=131,151,992  before=132,823,160  after=133,638,920  busy=true
```

Peak disk really has risen, exactly as `compact.go`'s own header says it does
without a post-checkpoint. So the clamp PRISM asked for as a tidy-up is not
tidying: zero is the only truthful rendering of that state, and `CheckpointBusy`
carries the rest. The subtraction moved to `CompactResult.ReclaimedBytes()` so
one definition sits beside the measurement that justifies it.

**A process failure worth recording.** The JS-guard test was written *after* PR
B's commit and then destroyed by the next control's `git checkout -- internal/`
— the exact hazard LOUPE names ("commit BEFORE the control, always"), hit while
following the procedure that names it. It cost nothing only because the apply
script, not the tree, was the source of truth. That is the second half of the
same rule and it is what made the loss recoverable.

**The JS guard needed comment-stripping, for the repo's recorded reason.** Its
first run reported the bug as still present — it had found the *explanatory
comment beside the fix*, which quotes the offending string. `stripJSNoise` is
deliberately not used here: it blanks string literals too, and the literals are
the whole subject of the scan.

**And a wedged gate that was a disk problem.** A `make check` stalled with
`internal/manifest` at **0.0 % CPU** after 11 minutes; the stack dump put the
wedge in an unrelated test's `OpenStore` -> `migrate`. `df -h` said **97 %
full**, with a **27 GB** Go build cache — and my own new fixture was writing a
131 MB WAL on top. The repo already records this exact shape ("when unrelated
tests start failing, check `df -h /` before reading the failures"), and reading
the failure first would have sent me hunting a phantom deadlock in the
compaction code. Cache cleaned, fixture cut to an 11 MB WAL — which is all the
assertion ever needed.

## Bot coverage — stated rather than implied

`gemini-code-assist` posted its **daily quota limit** on the first PR of this
batch, so **no PR in this run received a Gemini review**. CodeRabbit reviewed
#859 (two Minor items, both valid, taken in the fix round); SonarCloud's quality
gate and CodeQL passed. Re-request Gemini tomorrow rather than reading its
silence as approval.


## Outcome

| PR | title | merged |
|---|---|---|
| [#859](https://github.com/acoseac/1-bit-bridge/pull/859) | a ceiling on the two windows, and the two tests that pinned nothing | `54d4d99` |
| [#860](https://github.com/acoseac/1-bit-bridge/pull/860) | the compaction reported numbers it could not support | `1adba6d` |
| [#861](https://github.com/acoseac/1-bit-bridge/pull/861) | the retention control the console has been telling operators to use | `d33dab3` |

All three merged with every CI leg green — including the **Windows** leg, which
is where PR #860's probe fix would have shown up and where nothing had ever
driven the real free-space probe before.

**SonarCloud's quality gate failed twice on #861**, both times on new-code
duplication (3.1 % then 3.0 % against a 3 % threshold) and both times entirely
inside the new test file. Two rounds of real de-duplication fixed it — a shared
`postCompact` helper for the four copies of the compact-request block, and a
shared `jsFunctionBody` for the two copies of the app.js window extraction. Both
are improvements I would defend without the gate; the second is what took it
green.

**The same rule bit twice.** The `jsFunctionBody` refactor was written, tested,
and then destroyed by the next control's `git checkout -- internal/` — the
second time in this run, after the JS-guard test went the same way in PR B. The
second occurrence had a compounding cause worth naming: the commit was issued
from a **backgrounded** shell with a heredoc, so `git commit -F -` read an empty
stdin and failed silently, and `git status` then looked clean because the
controls had already reverted everything. Writing the message to a FILE and
using `git commit -F <file>` — which the tooling notes already recommend for a
different reason (backticks in prose) — removes both halves at once.

## Still open, deliberately

- **F7** — expired-but-not-revoked tokens keep their registrations alive.
  Belongs in a decision about token expiry, not this batch.
- **F8** — a bridge restarted inside 5 minutes never sweeps at all.
- **`ReapPlaybackHistory`'s DELETE is a full table scan** (no `started_at`
  index), held under `Store.mu`. Daily, and only when the knob is on.
- **`EnvOverrideDocs()` has no production caller**, so the derived env names are
  printed to an operator nowhere. Pre-existing, not from this trio.
- **Nothing is deployed.** The verification checklist is at
  `~/Desktop/to-do/2026-09-06-loupe-retention-verify.md`.
