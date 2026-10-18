---
paths:
  - "internal/smartplaylist/**"
  - "internal/smartplaylistgen/**"
  - "internal/manifest/smartplaylist*.go"
  - "cmd/bridge/smartplaylists*.go"
  - "internal/admin/handlers_smartplaylists*.go"
---
# Smart mixes

The smart-mix generator: the families and the harmonic engine (`internal/smartplaylist`), the regenerator and its inputs (`internal/smartplaylistgen`), the store queries and the `smart_playlist_served` table (`internal/manifest/smartplaylist*.go`), the serve-side regeneration loop (`cmd/bridge/smartplaylists.go`) and the console's regenerate handlers. `/v1/smart-playlists` serves the cache; its wire rules are in `wire.md`.

The record behind each rule is in `ops/engineering-log.md` (backlog B451, 2026-10-18).

## Smart mixes

- **The analyzed pool and the deep-cut pool are a week-seeded window of
  the whole keyed library, not a path prefix** (2026-10-18, backlog B451).
  `AnalyzedTrackFeatures` and `LovedArtistDeepCuts` order by
  `mix_week_hash(weekSeed, path)` — the same FNV-1a as
  `hashSeedPath`, seed bytes big-endian — and only then apply the cap
  (5000 keyed rows; 3 per artist, then 200). The seed is the UTC ISO
  week (`SeedFromISOWeek`), passed as `int64(weekSeed)` because
  `database/sql` will not bind a uint64 with the high bit set. The same
  seed is the same window; a different week moves it, and every keyed
  row and every eligible deep cut is reachable across seeds. **Don't
  order either pool by path or by artist before the cap, don't drop the
  per-artist or pool sizes to "fix" the prefix, and don't bind the seed
  as a uint64.** The two hashes are held to one answer by the same table
  of golden vectors in each package (seeds with the high bit set,
  non-ASCII paths), which the manifest test also runs through SQL
  `mix_week_hash`. **Don't change one hash, or one table, without the
  other.** Tests: `TestAnalyzedTrackFeatures_WeekSeedMovesTheWindow`,
  `TestLovedArtistDeepCuts_WeekSeedRotatesArtistsAndTracks`,
  `TestMixWeekHash_MatchesTheEngineGoldenVectors`,
  `TestHashSeedPath_MatchesTheManifestGoldenVectors`.
- **An unknown tempo is a mid-range penalty in the harmonic walk**
  (2026-10-18, backlog B451). `normalizedBPMDistance` used to return 0
  when either BPM was missing, so an untempoed track won every key tie.
  One missing tempo now costs half the known tempo, the same fraction
  Daily Mix discovery already charges (`unknownBPMPenaltyFraction`).
  Both missing stays 0: there is no known tempo to halve. **Don't treat
  a missing BPM as distance 0 against a track that has one.** Test:
  `TestSequenceHarmonic_UnknownBPMDoesNotWinAKeyTie`.
- **A heart the chain cannot place keeps a slot inside the Favorites
  cap** (2026-10-18, backlog B451). A heart with no key, or a mode
  `ToCamelot` rejects, is not a candidate for `sequenceHarmonic`. The
  chain used to be asked for the whole cap, so those hearts were
  appended into a list that was already full. The chain is capped at
  `cap − unkeyed` (or not run when that is 0) and the unkeyed hearts
  fill the reserved tail. When there are more unkeyed hearts than the
  cap, they fill the cap. Keyed hearts still take only what is left.
  **Don't append unkeyed hearts only into leftover room, and don't
  treat a bad mode as keyed.** Tests:
  `TestBuildFavorites_UnkeyedHeartsFitInsideTheCap`,
  `TestBuildFavorites_UnmappedKeyStillFitsInsideTheCap`.
- **Heavy Rotation reads a candidate window the diversity caps can fill,
  then caps** (2026-10-18, backlog B451). The query used to be limited
  to the display cap (50). One artist could fill those rows; the 4/4
  caps then kept fewer than the floor and the mix fell back to that
  uncapped prefix. The read is `heavyRotationCandidateLimit` (5000, the
  query's own ceiling). The fallback to the uncapped list stays for a
  pool that really is one artist. **Don't pass the display cap as that
  query's limit.** Test:
  `TestRegenerate_HeavyRotationCapsSeePastOneArtist`.
- **Finish Line's session average is the newest events in the lookback,
  returned chronological** (2026-10-18, backlog B451).
  `OrderedPlayEvents` takes the newest `LIMIT` rows (`started_at DESC,
  id DESC`) and hands them back `started_at ASC`, which is the order
  the session walk needs. The outer query does not name `id`: SQLite
  will not order by a column the subquery did not expose. **Don't cut
  the event query on the oldest rows.** Test:
  `TestOrderedPlayEvents_KeepsTheNewestWindow`.
- **A time-of-day hour is served only at `MinTimeOfDayHourPlays`**
  (2026-10-18, backlog B451). The floor is 2 in `DefaultOptions`; a
  zero value uses that same 2. The family still turns on from the
  busiest hour (`MinTimeOfDayPlays`, 15). The count is the hour's
  plays, judged before a path is resolved. An hour at the floor is
  served; an hour under it is not stored. **Don't store an hour the
  play count has not met, and don't raise this floor to the family
  gate.** Test: `TestBuildTimeOfDay_ASinglePlayHourIsNotServed`.
- **Discovery shelves skip what they showed on the 7 UTC days before the
  build's own, and the paths on the familiar shelves, unless that would
  hide the family** (2026-10-18, backlog B451). The shelves are Daily
  Mix, Auto Mix, deep cuts, Lift Off and Wind Down. When the cache is
  built, the paths they served are written to `smart_playlist_served`
  (v58: `served_at` UnixNano, `slug`, `path`) and rows older than the
  window are deleted in that same write. The window is whole UTC days,
  the zone of the week seed: a build excludes what was served in
  `[dayStart − 7 days, dayStart)` of its `NowNS`
  (`SmartPlaylistServedOnEarlierDays`), and writing a shelf replaces
  that shelf's rows for the build's own day
  (`ReplaceSmartPlaylistServedToday`), so the settle run 120 s after
  every start, "Regenerate now" and a per-family regenerate (which
  replaces only its own slug's rows) rebuild the day's shelves unchanged
  and leave one run's rows per shelf. **Don't let a same-day run exclude
  what that day already served.** Daily Mix records every track it
  showed; only its discovery slice is filtered, and emptying that slice
  does not relax the family (its minimum is the familiar set). The
  familiar shelves are the items Heavy Rotation, Favorites and Recently
  Played will actually contain — Heavy Rotation's input is the 5000-row
  candidate window, not the shelf. If the filter would leave a family
  under its minimum, that family is built without the filter and one
  Info line names it (`ExclusionRelaxed`). The engine stays pure: it
  reads `Inputs.ServedRecently` and does not log. **Don't lower the
  minimum, don't apply the filter to the familiar shelves or to Daily
  Mix's familiar slice, don't replace a slug's rows from an earlier day
  (prune those by age), and don't put the table on the wire.** Tests:
  `TestDailyDiscoverySkipsAPathServedThisWeek`,
  `TestDiscoverySkipsTheFamiliarShelves`,
  `TestDiscoveryKeepsAFamilyThatTheExclusionWouldHide`,
  `TestSmartPlaylistServed_RecordsAndPrunesPastSevenDays`,
  `TestSmartPlaylistServed_ExclusionStartsAtUTCMidnight`,
  `TestSmartPlaylistServed_ReplaceTouchesOnlyTheRunsUTCDay`,
  `TestRegenerate_SkipsAPathServedThisWeek`,
  `TestRegenerate_SameDayRunsBuildTheSameDiscoveryShelves`,
  `TestRegenerate_SameDayRunsLeaveOneRunOfServedRows`,
  `TestRegenerate_NextDayExcludesThePreviousDaysPicks`,
  `TestRegenerateFamily_ReplacesOnlyItsOwnSlugsRowsForToday`.
- **Daily Mix is first in the default homepage order** (2026-10-18,
  backlog B451). `Generate` stamps `Position` from the slice index, and
  the cache is read `ORDER BY position`. The app's hero is
  `families.first` after per-mix hides and an optional pin; it does not
  rerank. With nothing pinned or hidden, the wire order is the hero, so
  no app change goes with this. Heavy Rotation is the next shelf.
  **Don't put Heavy Rotation back in front of Daily Mix.** Tests:
  `TestGenerate_DefaultOrderLeadsWithDailyMix`,
  `TestGenerate_AllFamiliesInOrder`.
