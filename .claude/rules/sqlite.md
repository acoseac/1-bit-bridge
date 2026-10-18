---
paths:
  - "internal/manifest/store.go"
  - "internal/manifest/compact*.go"
  - "internal/manifest/retention*.go"
  - "internal/manifest/sqlhelpers*.go"
  - "internal/manifest/analysis_failure*.go"
  - "internal/backup/vacuum*.go"
  - "internal/dsn/**"
  - "internal/sqlitetest/**"
  - "cmd/bridge/retention_sweeper*.go"
  - "internal/admin/handlers_database*.go"
  - "internal/transcode/projection*.go"
  - "internal/admin/handlers_diagnostics*.go"
  - "internal/admin/settings_matrix_doc_test.go"
---
# SQLite predicates, compaction and retention

How a query reaches its index, FTS5 deletes, `VACUUM` and the WAL, the compaction's figures, and the retention windows.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## SQLite predicates and their indexes

- **A predicate that wants a PARTIAL index must contain the index's WHERE
  expression, verbatim.** SQLite admits one only when a query term MATCHES
  that expression; it does not reason that `analysis_fail_count >= 3`
  implies `!= 0`. v46 created `idx_tracks_analysis_fail … WHERE
  analysis_fail_count != 0` under a comment asserting "every predicate
  leads with `analysis_fail_count != 0` so the planner can use it" — true
  of `analysisFailureRecordedSQL`, false of the suppressed twin, which
  opened on the threshold. `SuppressedAnalysisPaths` uses that predicate
  as its ENTIRE where clause, so the miss was a full scan of `tracks` on
  the hourly sweep and on every `bridge analyze`. Measured, sqlite 3.54.0
  over 5,000 rows: `SCAN tracks USING INDEX …` for the recorded form,
  **`SCAN tracks`** for the suppressed one as shipped, and `SEARCH tracks
  USING INDEX … (analysis_fail_count>?)` once prefixed — a BETTER plan
  than the recorded predicate gets. **Assert on the PLAN, never a
  duration** (a timing test measures the host), and run the control on a
  SIBLING predicate FIRST, or a fixture with no index at all passes the
  real assertion for the wrong reason. Keep a threshold INLINED where a
  derived variant renumbers placeholders by replacing the first `?`. (#965)

- **Never key an FTS5 delete on a non-rowid column.** `tracks_fts` is a
  standalone FTS5 table. An equality on `path` is a full scan
  (`SCAN tracks_fts VIRTUAL TABLE INDEX 0:`); a `rowid` equality is a
  constant-cost lookup (`INDEX 0:=`). `CREATE INDEX` on the virtual table
  is rejected. `tracks_fts_ad` is what a prefix delete and
  `WipeFilesystemTracks` pay once per row, so
  `DELETE FROM tracks_fts WHERE path = old.path` made a 1,000-row delete
  take about 5 s and a 20,000-row delete about a minute (25,000-row
  store, sqlite 3.53.4). Migration v55 records the FTS rowid in
  `tracks_fts_row` (`path` PRIMARY KEY, `fts_rowid` UNIQUE, WITHOUT
  ROWID) inside the same trigger that inserts the FTS row
  (`last_insert_rowid()`). `tracks_fts_ad` and `tracks_fts_au` delete
  with `WHERE rowid = (SELECT fts_rowid FROM tracks_fts_row WHERE path =
  old.path)` and then delete the map row. The update trigger stays
  `AFTER UPDATE OF path, tags_json`. Its `WHEN` compares the path and the
  title, artist and album the insert writes; each `json_extract` sits
  inside `CASE WHEN json_valid`, because `json_extract` raises on
  malformed JSON. On the JSON this process stores the `CASE` value is
  the unguarded `COALESCE(json_extract(...), '')`. An MBID-only write
  leaves the FTS rowid where it is. A direct writer of `tracks_fts` —
  the v7 backfill, and the v55 rebuild — refills the map in the same
  transaction, before these triggers exist. A writer that inserts
  through `tracks` gets the map from the trigger. FTS5 `optimize` and
  `merge` are not called here. After the fill, one census requires
  `COUNT(tracks) = COUNT(tracks_fts) = COUNT(tracks_fts_row)`, every map
  path in `tracks`, and every track path in the map. Any miss rebuilds:
  empty `tracks_fts`, the v7 backfill, refill the map, check again. A
  second miss rolls the transaction back. The rebuild logs the counts
  and no paths. The whole step is one `BEGIN IMMEDIATE` that also stamps
  `user_version`, so a crash leaves version 54 and the old triggers.
  (`TestFTSDeleteAddressesTheRowMap`,
  `TestFTSCountsStayEqualAndMatchStaysRight`, the v54 reopen tests.)

## Database compaction and retention

The compact / reap / reclaim trio (#819 / #822 / #829) landed in one day and
had **no section here** — its three rules were scattered across *Scanner* and
*Config*, which is how the two most destructive `DELETE` statements in the tree
came to be filed under "Config, settings and process lifecycle". Everything
below is from the LOUPE run that swept it (#859 / #860 / #861). **Every
operator-facing number this feature reports was measuring something other than
what it claimed**, and none of it had a failing test.

- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before.** In WAL
  mode the vacuum's output lands in the WAL, so without the post-checkpoint the
  file does not shrink by a byte and peak disk RISES. Assert on the FILE
  shrinking, not on `freelist_count`, which reads 0 under the broken form.
- **A retention window must have a CEILING, and `beforeNS <= 0` is the wrong
  half of the guard.** The cutoff is `now.AddDate(0,0,-days).UnixNano()`, and
  `UnixNano` is undefined outside 1678–2262; past ~127,455 days it wraps, and
  **145,092 values in [1, 400000] land POSITIVE and greater than now**, which
  makes `DELETE … WHERE started_at < ?` match every row. `999999` — the
  canonical "effectively infinite" placeholder — is one of them. The existing
  `<= 0` no-op catches the harmless negative wrap and passes the dangerous
  positive one. `config.MaxRetentionDays` refuses (never clamps, like the floor
  beside it) and `manifest.ErrCutoffNotInThePast` is the belt. **`ErrNoLiveTokens`
  does not help here** — it guards only the ORPHAN reap, so the registration
  window is the half with no second line of defence.
- **A window past ~56 years is a deliberate no-op, not a bug.** It reaches
  before 1970, so the cutoff is negative and `<= 0` returns "delete nothing" —
  the honest answer for a window longer than the bridge has existed. The
  property to assert is "no accepted value lands at or after NOW", swept over
  the whole accepted range.
- **`freelist_count` is a FLOOR on what a compaction returns, never an
  estimate.** It counts only WHOLLY free pages; VACUUM also repacks intra-page
  fragmentation, which is what SCATTERED deletion produces — and scattered
  deletion is what every reaping path here does. Measured on a 72.5 MB store
  with every second row deleted: `freelist_count = 0` while VACUUM returned
  **36,233,216 bytes, half the file**. The panel said "nothing to reclaim".
  Never render a zero there as an answer to "should I compact"; it answers a
  different, much narrower question. A fixture guard in `compact_test.go`
  (`if pre.FreelistCount == 0 { t.Fatal(…) }`) shows this was hit during
  development and worked around in the test rather than recognised.
- **The compaction's before/after figures are FOOTPRINTS — main file plus
  `-wal`.** An arbitrary share of the database lives in the WAL until a
  checkpoint folds it back, and one open reader is enough to keep it there.
  Measuring the main file alone reported `reclaimedBytes: -2330624` ("the
  compaction added 2.3 MB") and made the headroom guard demand **8,192 bytes**
  free for a vacuum needing ~4.6 MB.
- **A compaction CAN legitimately raise peak disk, so the clamp is truth, not
  tidying.** When the post-checkpoint is busy the WAL is not truncated and the
  vacuum's output sits on top of the original — measured `before=132,823,160 →
  after=133,638,920`. Nothing has been reclaimed *yet*;
  `CompactResult.ReclaimedBytes()` reports 0 and `CheckpointBusy` carries the
  rest. One definition of that subtraction, beside the measurement.
- **A context cancellation during the post-VACUUM checkpoint is NOT a failed
  compaction.** The vacuum already committed; returning an error tells the
  operator the button failed after it did the work. It maps to `CheckpointBusy`.
  The decision is the pure `checkpointOutcome` helper because the branch is
  otherwise reachable only by winning a race, and a decision that cannot be
  driven is a decision nothing pins.
- **The free-space probe takes a DIRECTORY.** `transcode.AvailableDiskSpaceNearest`
  advances only on `os.IsNotExist`, so an existing FILE goes straight through to
  the platform probe. POSIX `statfs` accepts one; Windows `GetDiskFreeSpaceExW`
  opens `lpDirectoryName` with `FILE_DIRECTORY_FILE` and returns
  `ERROR_DIRECTORY` — so `POST /api/database/compact` 500'd on **every Windows
  install**, always. Assert the CONTRACT (what gets handed to the probe is a
  directory), not the symptom, so it fails on every platform.
- **The scan guard is a check-then-act GUARD, not mutual exclusion — say
  which.** It runs one direction only: a scan starting between the caller's
  `ScanInFlight()` check and `Store.mu` still serialises behind the whole
  vacuum, and two concurrent Compacts become two vacuums. A flag the scanner had
  to honour was declined for the reason `bridge restore` narrows its window
  rather than locking — the consequence is a stall, not corruption, and the flag
  would let a wedged vacuum silence the scanner.
- **`GET /api/diagnostics` is a database reader on a 5-second poll**, and its
  docblock denied it for four days with the body's own "Three PRAGMAs" and "Two
  COUNTs and a MIN" comments directly beneath — while `app.js` carried a second
  copy of the same false claim as the JUSTIFICATION for the interval. There is
  no index on `playback_history.started_at`, so the `MIN` turns a covering-index
  count into a full SCAN: **1.0 ms at 18k rows, 9.0 ms at 90k, 39.6 ms at 500k**
  (the three PRAGMAs are 7–12 µs and genuinely free). That block sits behind
  `databaseStatsTTL`, invalidated after a compaction.
- **A TTL that takes a REQUEST context must not cache what a cancelled request
  produced.** Both diagnostics reads take `r.Context()`, so a browser navigating
  away mid-poll — or an aborted bug-report download — fails them and yields an
  "unavailable" snapshot, which the cache then answers to every healthy request
  for the next fifteen seconds. Skip the write on `ctx.Err() != nil` and ONLY
  then: a genuine failure still caches, because repeating a doomed full table
  scan every 5 s helps nobody. The distinction is whether the failure was about
  the DATABASE or about the REQUEST. This one was introduced by the very batch
  that fixed the confident-wrong-answer class, one layer down.
- **Every availability flag needs a test in the FALSE direction.**
  `RetentionCountsAvailable` was only ever asserted true, so hoisting it out of
  its `err == nil` branch left every test green while the field's entire reason
  for existing evaporated. Its twin on the page-stats block did not exist at all
  — a failed PRAGMA rendered as a confident "0 B".
- **A config field with no `settingsPatch` field is invisible to the matrix
  guard.** `settings_matrix_doc_test.go` is bidirectional, but its universe is
  `reflect.TypeOf(settingsPatch{})` — never `config.Config`. `retention.*` sat
  outside that loop while the Diagnostics panel told operators to "set a window
  in Settings", and there was no such control anywhere. **When you add a config
  field an operator is meant to choose, add the patch field in the same PR** —
  or the prose that names Settings is a promise nothing keeps.
- **`Enrich`-style scan guards must strip COMMENTS before scanning JS**, for the
  reason the CSS guards already record: the code beside a fixed string explains
  the defect BY QUOTING IT, so an unstripped scan finds the commentary and
  reports the bug as still present. `stripJSNoise` is the wrong tool when the
  string literals ARE the subject — it blanks those too.

## Scanner, deletion passes, and manifest writes

- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before.** In WAL
  mode the vacuum's own output lands in the WAL, so without the post-checkpoint
  the file does not shrink by a byte and peak disk RISES. Measured: 5,623,808 →
  5,623,808 with the WAL grown to 2.8 MB, then 2,572,288 once checkpointed. A
  review proposed the pre-vacuum checkpoint alone, which would have shipped a
  button that reports success and reclaims nothing — so assert on the FILE
  shrinking, not on `freelist_count`, which reads 0 under the broken form.
