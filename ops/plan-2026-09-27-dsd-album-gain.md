# Plan — an album-level gain for DSD renditions (2026-09-27)

**Status: plan, nothing built.** Branch `feat/dsd-album-gain` holds this file only.

## Why

A DSD rendition bakes a clip-guarded boost into its FLAC:
`G = clamp(0, 6, −TP_unity − 1 dBTP)` (`ClipGuardedGainDB`,
`internal/transcode/dsd_render_chain.go`), where `TP_unity` is the track's own
true peak. The boost is decided **per track**, so tracks of one album get
different boosts. On a master cut hotter than SACD's −6 dB convention, a loud
track and a quiet one can end up several dB apart, which changes the album's
internal balance. B1 recorded this in `ops/engineering-log.md` ("album-level gain
consistency (per-track clip guard, recorded for a later pass)"). It resurfaced
through a listener whose SACD rip peaks at −2.55 dBFS.

The iOS side has just shipped every whole dB from +6 to 0 as the user's
conversion level (iOS #1970). For a rendition the phone plays
`min(level, appliedGainDB)`, because it can attenuate but never boost. So today a
listener can even out a hot album only by choosing a level at or below that
album's lowest per-track boost.

**Goal:** every track of an album gets the same boost within a tier, the lowest
clip-safe one:

    G_album(tier) = clamp(0, 6, −max over the album's tracks of TP_unity(tier) − 1)

Rounded to 0.1 dB as today. The track that constrains the album lands at −1 dBTP;
every other track lands lower by exactly its peak difference, so the album keeps
its internal balance.

**The trade-off, stated plainly:** on a hot album the quiet tracks lose the extra
boost they get today. That is what "album level" means; ReplayGain's album mode
makes the same trade. A compliant album (peaks at or below −7 dBTP) is unchanged:
+6 everywhere.

## What the code does today (verified 2026-09-27 at `44a897b0`)

**Rendering**
- **One job per (track, tier).** A job is keyed by
  `SourceLibraryRel|VariantID` (`pool.go`). Nothing groups tracks for rendering.
- **Four entry points**, all per track:
  - the iOS lazy request (`POST /v1/upscale`, foreground lane);
  - the batch Coordinator (DSD goes to the background lane);
  - the auto-optimize sweeper, which covers the compact tier only (`optimized-dsd-*`);
  - the CLI (`bridge optimize`, `bridge render`, `runUpscaleBatch`, no pool).
- **The peak is measured per tier.** Stage B measures `TP_unity` on that tier's
  own decimated intermediate: 44.1/48 kHz for compact, 176.4/192 kHz after the
  35 kHz `sinc` for faithful. The two differ slightly: a −6 dBFS tone gets
  G = 5.0 (compact) and 4.8 (faithful).
- **No way to pass a gain in.** `JobSpec` has no gain field. Stage A's scratch is
  deleted with each job; only the row's `applied_gain_db` and `true_peak_dbtp`
  outlive it.
- **The measured peak is already stored for every DSD rendition.**
  `track_variants.true_peak_dbtp` (migration v43) holds it; it is NULL only for a
  digitally silent source. No production code reads it yet.
- **There is NO pre-render peak for DSD.** The analysis skips `.dsf`/`.dff`
  ("sox can't decode 1-bit DSD streams", `cmd/bridge/analyze.go`). So an album's
  peaks can only come from renders or from a new measure-only pass.
- **The guard is one-sided.** Its clamp is `[0, 6]`, so a master whose peak is
  above 0 dBTP at unity never gets a rendition. It fails `ErrDSDClipped`. Recorded
  as an escalated product decision (`plan-2026-09-09-loupe-w5.md` P0d), not taken.
  An album-level boost inherits this.

**Album identity**
- **The album is computed, never stored.** It is
  `dupes.AlbumIDOf(dupes.Resolve(row))`: primary album artist | album | year,
  normalised. This is a verbatim mirror of the iOS album key, and it is the album
  the listener sees in the app.
- **Disc number is not part of it.** A multi-disc set with the same tags is one
  album.
- **CLAUDE.md: "An album is a SET of tracks, never a path prefix".** About 8 % of
  albums share a folder with another album. So a folder is the wrong grouping.
- **Only the admin catalog can list an album's tracks** today
  (`librarycat.Catalog.AlbumIDForPath`, served rows only). There is no store-level
  equivalent.
- **One key can span editions.** DSD and PCM copies are never cross-suppressed, so
  a DSD edition and a FLAC edition with identical tags share one album key.

**Versioning, the wire, and iOS**
- **The DSD schema version lives in the variant id.** It is
  `DSDRenditionSchemaVersion = "v1"`, in ids like `optimized-dsd-v1-44100-16` and
  `pcm-v1-176400-24`. PROTOCOL.md already says a change to the "gain policy" bumps
  it, and that clients resolve ids by prefix only.
- **The manifest lists a track's variants in primary-key order.**
  `variantsAggSQL` has no ORDER BY, so a `-v1` row is listed before a `-v2` row.
- **iOS takes the FIRST variant of a family.**
  - It recognises `-v2` by prefix, with no iOS change needed.
  - It never prefers a newer schema.
  - Its request gate treats any id of the family as already built, so a phone
    never asks for `-v2` while a `-v1` exists.
- **The sweeper's coverage is prefix-based.** A fresh row of ANY `optimized-%` id
  counts, so a schema bump alone re-renders nothing. The faithful tier is never
  swept.
- **Re-rendering under the SAME id hurts phones that downloaded the file.**
  - A re-render replaces the file atomically and updates the row in place, and
    `indexed_at` advances, so phones sync the new `appliedGainDB`.
  - But a phone that downloaded the rendition keeps the OLD file (it never
    re-checks size or content) and computes its trim from the NEW gain. With a
    lower album gain, that file plays too loud by the difference, and Signal
    Path misstates the gain.
- **A vanished id also hurts downloaded copies.** If an id disappears from the
  manifest, a downloaded copy still plays, but untrimmed and described as plain
  PCM.

## Design

### D1. Album = the app's album, DSD members eligible for the tier

**Membership:** the served tracks whose `dupes.AlbumIDOf(dupes.Resolve(row))`
matches, filtered to:
- DSD sources that pass `DSDRenderEligible` for this tier;
- local files only: not UPnP-routed, not SACD ISO virtual tracks.

**Why:**
- It is what the listener sees as one album in the app.
- It is the bridge's only album identity.
- Grouping by folder breaks the house rule above.

**Consequences:**
- **Multi-disc:** discs of one set share a boost.
- **Mixed editions:** a DSD edition and a CD rip that share a key contribute only
  their DSD tracks.
- **Mixed DSD families:** tracks in the 44.1k and 48k families render to
  different target rates, so they fall in different tier profiles. Each profile
  gets its own boost. This is rare.

**How to get the membership:** a small DSD album index. Stream the DSD rows, group
them by `AlbumIDOf(Resolve(row))`, cache the result, and rebuild it when a scan
completes. It uses the same two `dupes` functions the catalog uses, so the
grouping cannot drift from the catalog's.
- Don't reach into `internal/admin`'s catalog snapshot.
- Don't change `internal/dupes`. It is a verbatim mirror of the iOS normaliser,
  so "better" than the client counts as wrong.

### D2. Peaks: one cached measurement per (track, tier profile)

**A new table:**

    dsd_peaks(source_path, profile, true_peak_unity_dbtp REAL NULL,
              source_mtime_ns, source_size, recipe_version, measured_at)
    PRIMARY KEY (source_path, profile)

- `profile` names the Stage A recipe the peak was measured on, e.g.
  `compact-44100` or `faithful-176400`.
- `recipe_version` invalidates measurements when the decode recipe changes.
- NULL means digitally silent. A silent track doesn't constrain the album, and an
  all-silent album gets +6, as a silent track does today.

**Three sources fill it:**
1. **Every render** writes its own Stage B peak, which it measures anyway, at no
   extra cost.
2. **Seeding from existing renditions,** free: every fresh DSD rendition row
   already carries `true_peak_dbtp` for its tier. This makes the first album-gain
   pass over an already-rendered library cost no extra decoding.
3. **A new measure-only pass,** `transcode.MeasureDSDPeak(ctx, spec)`: Stage A +
   Stage B with no Stage C, written by the same chain helpers. It costs about one
   render minus the FLAC encode, and runs only for album members that have no
   fresh peak.

Freshness is the same rule the variants use: the source's mtime and size.

### D3. The boost is derived at render time, not stored

`G_album` is recomputed from the cached peaks for each render. There is no album
gain table, following the house rule "the catalog is computed, not stored".

**Consistency:**
- While an album's membership and sources are unchanged, every render derives the
  same number.
- When membership changes (a track added, re-tagged into or out of the album), new
  renders follow the new boost, and existing renditions keep theirs until
  deliberately re-rendered (D5).
- A `bridge doctor` / console line reports albums whose renditions in one profile
  carry different boosts. That is how the operator finds them.

**Safety:** each render still measures its own peak in Stage B and uses
`min(G_album, ClipGuardedGainDB(own))`. A stale cache can therefore never make a
file clip. The row's `sox_settings` records `gainScope: "album"`, the album's
member count and the constraining peak, for debugging.

### D4. The gain enters the job through the worker, not the HTTP path

- **The hook:** add `JobSpec.GainDB *float64`, with `nil` meaning today's per-track
  guard.
- **Where it resolves:** an injected resolver runs in the pool worker before
  `Run`, and in the CLI walk. It:
  1. finds the members;
  2. reads the cached peaks;
  3. measures the missing ones;
  4. derives the boost.
- **Why not earlier:** a missing peak costs a Stage A decode, which must never run
  in a request handler or the enqueue path.
- **Cooperative survey.** Jobs for the same (album, profile) share one survey. A
  worker that arrives takes the next unmeasured member rather than blocking, so
  the pool's workers measure an album in parallel, and every job proceeds when
  the survey completes.
- **Cross-process runs.** A CLI run alongside `serve` can duplicate a measurement.
  That is harmless: it is deterministic and the write is an upsert.
- **Cost:**
  - A first-time render of an album now decodes each member twice (measure, then
    render): about 2× Stage A CPU for DSD tracks never rendered before.
  - A library already rendered as `-v1` pays nothing extra to survey, thanks to
    the seeding in D2.
  - An album-unit render that keeps every member's scratch (1× decode, at
    ~1.3 GB per hour of audio for compact and ~5 GB for faithful) is possible
    later. It is not needed to ship.

### D5. Migration: a DSD schema bump, current version listed first, `-v1` kept

1. **`DSDRenditionSchemaVersion = "v2"`.** This is PROTOCOL.md's own rule for a
   gain-policy change. New renders are `optimized-dsd-v2-*` / `pcm-v2-*`.
2. **The manifest lists the current DSD schema first within a family.** Add an
   explicit ORDER BY to `variantsAggSQL`, or a subquery if the bundled SQLite (via
   modernc v1.59.0) turns out not to support ORDER BY inside `json_group_array`.
   This is what makes every shipped iOS version stream and download `-v2`, and it
   is documented in PROTOCOL.md as a guarantee.
3. **Sweeper coverage for DSD = a fresh row of the CURRENT DSD schema.** PCM
   coverage is unchanged. `TestListAutoOptimizeCandidatesIgnoresSupersededVariantRows`'
   intent (no regenerate-every-sweep loop) still holds, because the sweeper writes
   the current id and its facts advance.
4. **A one-shot pass re-renders existing `pcm-v1-*` rows as `pcm-v2-*`.**
   - Background lane, only for tracks that already have one. The faithful tier is
     large and on demand, so the pass creates none that weren't asked for.
   - Phones never request `-v2` while `-v1` exists, so the bridge has to drive this.
5. **`-v1` rows and files STAY.**
   - While they do, a phone's downloaded `-v1` copy finds its own id with its own
     `appliedGainDB` and plays at the right level.
   - Retiring `-v1` is a separate decision (PR 3), to be made only after iOS
     snapshots the gain with downloaded copies (M2 below).
   - Until then, the disk cost is `-v1` + `-v2` side by side (compact runs about
     24 MB per 5-minute DSD64 track; Phase 0 measures the real total).
6. **Re-gaining an album whose membership changed** is an explicit operator action
   (`bridge render --regain <album>` or a console button). It re-renders under the
   same `-v2` ids, so it carries the downloaded-copy hazard above. It is safe only
   for phones that have M2.

### D6. The one-sided guard is not decided here

- **Under today's clamp:** a single member with a peak above −1 dBTP gives its
  whole album +0, and a member above 0 dBTP still fails to render, as it does
  alone today.
- **If the escalated P0d decision is later taken** (let the clamp go negative), the
  album boost composes with it unchanged: `G_album = −maxTP − 1` even when
  negative.
- **Deciding them together is possible but not required.**

## iOS side (Mirror-PR pair with the bridge PR 2, plus one follow-up)

- **M1 — docs and copy (with bridge PR 2):**
  - `docs/BridgeProtocol.md` byte-identical to PROTOCOL.md: `appliedGainDB`
    becomes "clip-guarded per album, per tier", plus the `-v2` family and the
    current-first ordering guarantee.
  - The Advanced audio footer's "(up to +6 dB, clip-guarded per track)" becomes
    "(up to +6 dB, clip-guarded)", which is true before and after the migration.
  - Update the rules/History text that says "per track" (`dsd-dsp.md`,
    `bridge-smb-dlna.md:134`, the History files).
  - No behaviour change: prefix resolution plus the bridge's ordering handle `-v2`.
- **M2 — downloaded copies describe themselves (before any `-v1` retirement or
  `-v2` re-gain):**
  - Record the variant's `appliedGainDB` (and id and size) with the downloaded
    copy, and use the recorded gain for that file at play time.
  - Keep stamping a downloaded rendition whose id has left the manifest, from the
    recorded metadata.
  - This closes the stale-gain and vanished-id hazards for every future
    re-render, not just this one.
- **Optional:** `DSDPlaybackSource.bestVariant` prefers the highest schema version
  within a family, as defence in depth behind the bridge's ordering.

## Phase 0 — measure before building (read-only)

On a snapshot of the production bridge's database, taken per
`ops/deployment-runbook.md` (one SSH connection) and never queried live because
of the WAL trap, run a throwaway Go helper in a `_`-prefixed dir that imports
`internal/dupes`:

1. **Scale:** DSD tracks and DSD albums (by `AlbumIDOf`), and `optimized-dsd-v1`
   / `pcm-v1` rows per tier.
2. **The problem today:** per album, the spread (max − min) of `applied_gain_db`
   across its compact renditions, and the share of albums with a spread above
   0.5 / 1 / 3 dB.
3. **The cost of the fix:** the mean and max boost each track loses
   (`G_track − G_album`), and how many albums a single hot track pulls to +0.
4. **The migration bill:** renditions to re-render per tier, CPU at the measured
   render speed, and disk while `-v1` and `-v2` coexist.

If the spreads are small, a lighter option (for example, only the console report
plus a manual re-gain) may be enough. That is the go/no-go.

### Phase 0 results (2026-09-27): GO

**Setup.** The operator's bridge (`v0.2.0-58-gaca8be3`, 2 upscale workers,
`dsdRender` and `autoOptimize` on). A backup-API snapshot was taken on the host,
copied off, and analysed with the throwaway `_phase0` helper. Membership is
exactly the admin catalog's (`StreamCatalogRefs` → `dupes.Resolve` →
`AlbumIDOf`).

**Scale**
- 1,758 served DSD tracks. 4 are SACD virtual tracks, which leaves 1,754 local
  renderable tracks in 149 albums (144 with two or more tracks).
- DSD rates: DSD64 1,684, DSD256 53, DSD128 17.
- Renditions: 1,704 `optimized-dsd-v1-44100-16` (44.0 GB) and 3 `pcm-v1-176400-24`
  (0.2 GB).
- **`true_peak_dbtp` is exactly the input to the boost.**
  `ClipGuardedGainDB(true_peak_dbtp)` reproduces the stored `applied_gain_db` on
  1,707 of 1,707 rows. So seeding D2 from existing rows is sound, and 97 % of the
  local DSD tracks already have a fresh peak: the survey is free here.

**Per-track boost today (compact)**
- +6: 507 tracks. [+5, +6): 584. [+3, +5): 549. [+1, +3): 64. 0: none.
- No track peaks above −1 dBTP at unity, so P0d (the one-sided guard) never binds
  on this library, and no album would be pulled to +0.
- Two thirds of the tracks are already mastered hotter than the SACD convention.

**The problem (143 albums with two or more rendered tracks)**

| Spread between the album's highest and lowest per-track boost | Albums |
|---|---|
| 0 | 12 |
| (0, 0.5] dB | 15 |
| (0.5, 1] dB | 35 |
| (1, 3] dB | 72 |
| > 3 dB | 9 |

- Median spread 1.2 dB, p90 2.9 dB, max 4.7 dB. **57 % of albums shift their
  tracks' relative levels by more than 1 dB.**
- A segued concept album gets a 3.5 dB level step at track boundaries that were
  mastered seamless.

**The cost of the fix**
- 1,407 of 1,699 rendered tracks would lose some boost: mean 0.8 dB, p90 1.8 dB,
  max 4.7 dB.
- 496 tracks lose more than 1 dB, and 31 lose more than 3 dB.

**Migration bill on this bridge**
- The compact sweep renders 25× realtime with 2 workers, and the whole local DSD
  library is 124 h of audio. So re-rendering the compact tier as `-v2` is about
  5 h of background work.
- Disk: +44 GB while `-v1` is kept, against about 296 GB free on the variants
  volume.
- The faithful tier is 3 files.

**Conclusion: build it.** The inconsistency is common and audible, the migration
is a few hours of background work, and the peaks it needs are already on disk.

## PRs

**PR 1, dark (no audible change):**
- the `dsd_peaks` migration and seeding;
- `MeasureDSDPeak`;
- renders record their peak;
- the DSD album index and the pure `AlbumClipGuardedGainDB`;
- the resolver with the cooperative survey;
- `JobSpec.GainDB`, still `nil` everywhere.

Tests:
- the gain math: max, clamp, rounding, silent members, all silent;
- index grouping: multi-disc tags, compilations, a DSD + FLAC edition sharing a
  key, path-derived DFF tags, routed rows and SACD virtual tracks excluded;
- the survey measures only missing members, runs one survey per (album, profile)
  under concurrency, and handles stale peaks;
- `MeasureDSDPeak` on the real toolchain, gated on ffmpeg + sox like
  `TestRunDSD_RealToolchain`, agrees with a full render's Stage B.

**PR 2, behaviour (Mirror-PR with iOS M1):**
- the resolver feeds `GainDB` on every DSD path;
- the schema goes to `v2`;
- manifest ordering;
- sweeper coverage by current schema and the one-shot `pcm-v1` pass;
- `gainScope` in `sox_settings`;
- the doctor/console consistency line;
- PROTOCOL.md, CLAUDE.md rules and the engineering log.

Tests:
- **The end-to-end pin:** two fixtures in one album (−3 dBFS and −12 dBFS)
  render to the SAME `appliedGainDB` (≈ +2 on compact), and the quiet one's
  rendition lands 9 dB below the loud one's.
- A track with `-v1` + `-v2` lists `-v2` first.
- A fresh `-v1`-only DSD track is a sweeper candidate; one with a fresh `-v2` is
  not.

Negative controls, red set predicted by name:
- the resolver answering per-track boosts;
- the ORDER BY removed;
- coverage back to prefix.

**PR 3, later (separate decision):** retire `-v1` after iOS M2 ships plus a grace
period, or keep it.

**Deploy:** via `bridge-ops`, one SSH connection. Watch the migration sweep's CPU
and disk. On a phone, confirm a hot album's tracks show one boost in Signal Path
and play at one level.

## Decisions for the operator

1. **Album definition:** the app's album key (recommended), a folder, or per disc.
2. **Derived vs frozen boost:** derived from cached peaks (recommended; no stored
   album gain) or frozen at first decision.
3. **`-v1` retirement:** keep until iOS M2 plus a grace period (recommended),
   delete on `-v2` arrival (downloaded copies play untrimmed), or keep forever.
4. **Faithful tier:** re-render only existing `pcm-v1` rows (recommended), or all
   eligible DSD.
5. **Take P0d (negative clamp) together or separately.**
6. **Run Phase 0 first** (recommended). Its spreads decide whether the full
   design is worth its CPU and disk.
