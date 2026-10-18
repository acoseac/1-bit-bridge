---
paths:
  - "internal/lyrics/**"
  - "internal/manifest/lyrics_extract*.go"
  - "internal/manifest/atlas_lyrics*.go"
  - "internal/atlasharvest/lyrics*.go"
  - "internal/atlasharvest/client*.go"
  - "internal/api/lyrics*.go"
  - "internal/manifest/store.go"
---
# Lyrics — the one document per track

Embedded and sidecar lyrics at extraction, the `track_lyrics` rows, `/v1/lyrics`, and the network tier that fetches documents from Atlas.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Lyrics — the one document per track

The surface landed whole in ONE PR (#840) and had no section here until a LOUPE
run swept it; every rule below is from that sweep (PRs #849 / #850 / #851).
**Three of the four defects were permanent and silent** — no error, no log line,
no failing test — which is the shape to expect in this area.

- **A bump-only writer uses `bumpIndexedAtByPathSQL`, never a hand-rolled
  `CASE`.** `writeLyricsRowTx` shipped the exact
  `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END` form that
  `indexedAtAdvanceSQL`'s docblock quotes verbatim as dead, so lyrics-changed
  rows landed on a cursor clients already held and the track never reached the
  phone. `StampExtractorVersionBatch` takes ONE `now` for a whole batch, which
  is precisely the caller shape that collides. `TestIndexedAtAdvanceIsShared`
  could not see it — it walks named CONSTS and this was an inline literal — so
  `TestNoHandRolledIndexedAtBump` now sweeps every non-test file in the package
  and classifies each `indexed_at =` against the SQL literal that contains it.
- **The skip gate and the extractor must resolve a sidecar the SAME way, and
  the gate's answer must be able to change.** Two non-convergent shapes both
  re-extracted the audio file on every scan forever, invisibly (they land on
  `reExtractUnchanged` → `versionStampOnly`, so there is not even `indexed_at`
  churn to notice): a stale `sidecar-rejected` row that the nil branch declined
  to DELETE because it gated on `oldTag == ""` rather than `!hadRow`; and
  `sidecarLyricsDrifted` comparing the sidecar's EXTENSION rank when an empty,
  oversized, legacy-encoded or TAGLESS `.lrc` resolves to something that loses.
  `sidecarLyricsFile` answers WHICH file; `readSidecarCandidate` answers WHAT
  IT IS WORTH — both shared, neither duplicated.
- **`Pick`'s comparator is a strict TOTAL order.** Candidates arrive partly from
  `range m.Raw()`, a Go map, so any pair the comparator leaves equal flips the
  winner between scans → `lyricsTag` re-keys → `indexed_at` bumps → the track
  re-enters every device's delta on every scan. Same rule as the dupe elector,
  same reason. `mergeDuplicate` picks its surviving base with that same order,
  not by arrival — the dedup key is only `(Source, Body)`, so a second sighting
  can still differ in the derived fields.
- **`m.Lyrics()`'s `Priority: 0` is FABRICATED.** dhowden's
  `metadataID3v2.Lyrics()` returns `m.frames["USLT"].(*Comm).Text` — literally
  the frame the raw walk then re-reports with its real `DescriptorPriority` —
  so keeping the first sighting let an "Amazon" / "Song ID" descriptor launder
  itself back to the best rank and `junkExact` / `junkSubstring` silently did
  nothing. The merge keeps the LARGER priority: the real classification always
  beats the fabricated one.
- **Everything `lrcTime` emits must match `lineTag` or `hoursTag`.** `ParseSYLT`
  reads a raw uint32 of milliseconds from an untrusted frame, so past 999
  minutes it rendered `[1000:00.000]`, which neither regex accepts, inside a
  document `syltCandidate` stamps `synced: true` regardless — the phone drops
  the line. It is CLAMPED to 999:59.999, not promoted to `[hh:mm:ss.xxx]`:
  that form would rewrite every legitimately >1 h track (a delta wave for
  content that works today) and `renderLine` uses the same helper for the
  enhanced `<mm:ss.xxx>` WORD tags, whose iOS grammar is not mirrored here.
- **`Normalize` must be IDEMPOTENT, and `stripBOMs` must stay LINEAR.**
  `resolveLyrics` normalises an already-normalised body, so the two passes
  disagreeing silently drops a document that was accepted as a candidate.
  Deleting a U+FEFF splices its neighbours and can form a NEW one
  (`"\xef\xbb" + BOM + "\xbf"`), and Go's `unicode.IsSpace` does NOT count
  U+FEFF, so `TrimSpace` keeps a lone BOM. **Don't "simplify" the byte-wise
  reducer back to `ReplaceAll` in a loop** — that is quadratic on a nested
  input, `MaxBodyBytes` is only checked AFTER it, and the USLT / ©lyr / Vorbis
  path reaches `Normalize` with no size gate at all. Measured on a
  180,003-byte nest: 3.967 s against 640 µs.
- **The lyrics parsers are fuzzed, and the targets carry PROPERTIES.** The
  package had zero targets against a policy that names the audio extractors as
  one of the three untrusted-input surfaces. `FuzzParseSYLTToLRC` asserts every
  emitted line is LRC-parseable (which is what catches the clamp class),
  `FuzzNormalize` asserts idempotence + the cap + no CR,
  `FuzzPickIsShuffleInvariant` asserts the winner survives shuffling — that one
  found a SECOND order-dependence, in `mergeDuplicate`, that no
  extractor-driven test could reach — and `FuzzTextCandidateClassification`
  pins the synced verdict to `LooksLikeLRC && !sparse` on both halves, with
  format / synced / source agreeing.
- **A sparse-timed body is PLAIN, and the rule lives in the CLASSIFICATION,
  not in `LooksLikeLRC`.** `LooksLikeLRC` is any-line on both sides and must
  stay so; the app's `LRCParser.parse` returns the plain document when
  `timed.isEmpty || timedCoverageIsTooSparse(...)` (fewer than 2 timed TEXT
  lines beside any untimed text, or under 25 % of the non-blank lines), and
  `IsSyncedLRCBody` mirrors BOTH arms — `TimedCoverage` for the counts,
  `TimedCoverageIsTooSparse` line for line with the app's function, guard
  included, constants and truth table lifted from the Swift tests verbatim
  (iOS #1759). **Keep the `timed > 0` arm outside the sparse function**: the
  app's guard answers "not sparse" for zero timed lines because the app never
  asks it that, so a body whose only tags are clear events (`Prose\n[00:12.00]`)
  is caught by `timed.isEmpty` there and must be caught by `timed > 0` here —
  Gemini saw the outcome on #904 and proposed dropping the guard, which would
  have broken the verbatim truth table. Before it, a Genius transcript with
  one `[4:20]` cue was a rank-4 `text-lrc` stub outranking the complete plain
  document in the same file. The phone was never at risk — it re-parses the
  body and reads `synced` as advisory — so a divergence here shows up only
  as the bridge's election and stored verdict, silently. `ExtractorVersion`
  went to 9 for it, per the every-extraction-change rule and v8's precedent.
- **The SYLT marker set is `\r`, `\n`, `\r\n` — the phone's — and a "VERBATIM
  mirror" has to be re-read when the original moves.** iOS #1564 (2026-09-03)
  made a bare `\r` count in `entriesLookLikeWholeLines`, `carriesMarker` and
  the prefix/suffix strip because CR-only taggers exist; `hasNewlineMarker` /
  `splitMarkers` stayed on `\n` / `\r\n` for nine days while
  `strings.Trim(text, "\r\n")` kept stripping the CR from the body — so
  `leading` / `trailing` read false and ToLRC merged the next entry onto the
  open line. `TestToLRCTreatsABareCarriageReturnAsAMarker` pins it; the
  two-entry cases were rescued by the whole-line gap heuristic by accident,
  which is why the mixed-marker fixture is the one that fails.
  `ExtractorVersion` went to 10 for it. When an iOS lyrics PR touches
  `ID3v2Parser`'s SYLT arm, diff `sylt.go` against it in the same week.
- **`sidecarLyricsExts` is `.ttml` > `.lrc` > `.txt`** — `Source.Rank()` order,
  PROTOCOL.md's order, the app's order. `sidecarLyricsFile`'s own doc comment
  said the opposite for three days.
- **The scanner compares mtime EXACTLY; the API's 410 check uses a 2 s
  tolerance.** Both are correct and they are not the same question. An external
  review proposed importing the tolerance into `sidecarLyricsDrifted` — declined:
  the primary skip gate compares the AUDIO file byte-exactly
  (`scanner.go`, `existing.MTimeNS == pi.info.ModTime().UnixNano()`), so
  loosening only the sidecar half would be the inconsistency, not the fix.
- **⚠️ "Atlas cannot supply lyrics" is STALE and was corrected 2026-09-09.**
  It was true when measured on 2026-09-06 — `atlas_qobuz_track` held 679,296
  rows with 0 non-empty `lyrics` — but that measured the QOBUZ mirror, and
  Atlas has since grown an LRCLIB-backed surface at
  `/v1/atlas/recording/{mbid}` that is nothing to do with it. The fifth stale
  claim this file has shipped. **A negative result is about the thing you
  measured, at the time you measured it** — scope such a bullet to the table
  it looked in, or it reads as a standing verdict on the whole upstream.

### The network lyrics tier (`atlas-lrc` / `atlas`)

- **A network row is not the scanner's to reap.** Every branch of
  `writeLyricsRowTx` reasons from a LOCAL extraction, and its "nothing found"
  arm DELETEs — right for a removed sidecar, and about a document Atlas
  supplied it is evidence about nothing. Unguarded it reaps the row on the
  very next scan and every scan after. Arbitration is `Source.Rank()`'s, not
  a second copy of it: `atlas-lrc` at 5 sits above every UNTIMED local
  document and below every timed one (the app's DD3 rule expressed in the
  ladder), plain `atlas` at 8 beats nothing. `UpsertAtlasLyrics` applies the
  same order in reverse and **refuses to demote**, so two sweeps cannot
  alternate a track between documents — every write strict-advances
  `indexed_at`, and a flap is a delta to every paired device per cycle.
- **A release is not the RUN.** Any release-fetch error used to abort the whole
  sweep, and the candidate query is `ORDER BY albumMBID, t.path` — deterministic
  — so one unanswerable release aborted at the same point on every 60-second
  tick while healthy albums drained out of the candidate set and the failing one
  migrated toward the front. The tier went silent for the whole library with one
  warn line. Skip the ALBUM and continue; abort only on auth or a cancelled
  context, which are facts about the run. **The skip needs a cooldown or it
  trades a stalled tier for a hammered upstream**, and the cooldown must not
  RE-cool on a candidate it is already suppressing: pushing `until` forward every
  tick means it never expires, which is the negative cache it exists not to be.
  Two sentinels, not one.
- **`isUpstreamAnswered` governs BOTH legs.** The release leg had it and the
  recording leg did not, so a durable 404 wrote no verdict — and the candidate
  query gates on the ABSENCE of a row, so the track returned every sweep forever.
  At `LIMIT 400` over a deterministic order, a few hundred stale
  `musicBrainzTrackID`s hold every slot and nothing else is ever considered.
- **An UNRECOGNISED upstream status is transient, and never written verbatim.**
  Falling through to `documentFrom` stamped `unavailable` with a thirty-day
  backoff — a durable verdict about the TRACK from a sentence about the UPSTREAM
  this build could not read. Stamping nothing is the other half of the trap (the
  candidate returns every tick), so it takes `pending`'s short backoff. Never the
  upstream's own string: `status` is a column the candidate and stats queries
  switch on.
- **The scan guard is per CANDIDATE, not per sweep.** A pass is
  `lyricsCandidateBatch` × `lyricsPacing` of pure pacing — longer than the poll
  interval at the defaults — so a scan starting a second in ran entirely inside
  the sweep, which is the double `indexed_at` bump the guard exists to prevent.
- **`Addressable` must carry the candidate query's `instrumental` arm.**
  `instrumental` is a success that correctly leaves no `track_lyrics` row, so a
  count that only asks "no lyrics, has an MBID" counts it forever: a quarter of
  Atlas's measured mix, and a finished tier renders as a stalled job. It is a
  named const beside the candidate query, with the same retag invalidation, so
  the two cannot drift.
- **`atlas.lyricsEnabled` is LIVE, through `AtlasConfig.LyricsTierActive`.** Its
  two halves disagreed: `/api/jobs` read the flag live while the sweeper took it
  at boot inside the `if` that decided whether to wire the sink. Invisible while
  a restart was the only way to change it — and adding a `settingsPatch` field is
  exactly what makes it bite. One predicate, both readers.
- **`/v1/lyrics` exempts network rows from the drift check**, and that is what
  makes the tier work at all rather than a nicety. `lyricsSourceInfo` returns
  the AUDIO file's stat for any non-sidecar source, so a row with no local
  provenance compares zero against a real mtime and answers **410 forever**.
  Binding the row to the audio file only moves the bug — a tagger writing a
  genre changes that mtime and stales a document that never came from there.
  Server-side only; `lyricsDocument` never carried the stat fields.
- **The sweeper's gate is the ABSENCE of a `track_lyrics` row, never the
  verdict.** That is what lets a track whose Atlas document was displaced by a
  local one — or whose local one its owner deleted — come back and be
  re-fetched from its cached recording MBID in one call. `instrumental` is the
  ONE verdict that must exclude by status, because it is a success that
  correctly leaves no row. And the attempt row records the identity it was
  made AGAINST, not just the answer, so a retag invalidates a stale verdict by
  construction — no scanner hook, nothing to remember to call.
- **Corroboration, not duration, is the matcher's discriminator.** Atlas's
  release listing carries no ISRC and no per-track artist, so the keys are
  position, number, title and length. Measured over 752 tracks on 30 releases:
  position+title agreeing gives a 134 ms median duration delta and **100%
  within 30 s**; title-only and position-only give ~1.5 s medians and 82/87%.
  A hard 4 s duration gate was proposed and is WRONG — only 77.9% of 447
  position-matched pairs agree within 4 s (p90 **21.2 s**), so it discards a
  fifth of correct matches. The veto applies ONLY to the uncorroborated tiers.
  A file with no disc number must never be assumed onto medium 1 (3,752 tracks
  here have none), and a title is used alone only when UNIQUE on the release
  (1,275 albums here carry a duplicate title across 2,987 tracks).
- **`pending` is a fact about the upstream, never about the track.** Atlas
  warms on demand: over 32 unseen recordings every one resolved, median
  **6.5 s**, p90 15.2 s, max 18.8 s. So a fixed 2-second re-probe — the
  obvious design — sits below the median and misses most. The sweep warms its
  batch on pass one and collects on pass two, and the 150 ms politeness
  interval IS the delay. Never negative-cache it.
- **Only an upstream that ANSWERED may write a verdict.** `doCapped` returns a
  `*httpStatusError` and `isUpstreamAnswered` reads the CODE: 4xx durable,
  **except 429 and 408**, which are 4xx by number and transient by meaning.
  A 502 on the release fetch otherwise reaches `MatchNone` and parks a real
  album for a fortnight. Same reason `isHTTPNotFound` no longer substring
  matches `": http 404:"` — the error's last field is 512 bytes of response
  BODY, so a failing upstream quoting a 404 read as a clean miss.
- **The sweep stands down while a scan is in flight** (`ScanInProgress`, the
  booklet GC's hook): both write `track_lyrics`, and a sweep landing seconds
  before the scanner extracts a local document bumps `indexed_at` twice for
  one track. The write budget is not a queue guard either — 9,454 addressable
  tracks uncapped is 9,454 delta rows to every device at once.
- **`/api/jobs` snapshots use TTL + singleflight, NOT the diagnostics cache's
  mutex-across-the-query.** They look interchangeable and are not: the db
  context is DETACHED (`context.WithoutCancel`), because the result is shared
  by every queued caller and one client hanging up must not synthesize a
  failure for the rest; a failure serves LAST-GOOD rather than blanking a
  card; and the clock is stamped on failure TOO, or the TTL never trips and
  every poll re-runs a scan that is already failing. `AtlasLyricsStats` is
  25.8 ms at 21,000 tracks — the JSON predicate cannot use the functional
  index on `$.musicBrainzAlbumID` because it is wrapped in COALESCE.

- **Every pass that WRITES checks the scan latch, not just the first.**
  `tickLyrics` re-checked `ScanInProgress` per candidate in pass one and
  broke — leaving everything it had already warmed in `pending`, which
  pass two then drained into `track_lyrics` with the scan under way: the
  double `indexed_at` bump the guard exists to prevent, one loop later.
  The stand-down test seeded only `available` recordings, so its pass two
  was always empty and could not see it. Nothing is stamped for what a
  stood-down pass leaves behind; the candidate query offers it again next
  tick, warm. (#916)
