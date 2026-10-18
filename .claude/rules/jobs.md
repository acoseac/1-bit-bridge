---
paths:
  - "internal/transcode/**"
  - "internal/analyze/**"
  - "internal/albumgain/**"
  - "internal/integrity/**"
  - "cmd/bridge/upscale*.go"
  - "cmd/bridge/optimize*.go"
  - "cmd/bridge/analyze*.go"
  - "cmd/bridge/render*.go"
  - "cmd/bridge/variants*.go"
  - "cmd/bridge/auto_optimize*.go"
  - "cmd/bridge/rendition_*.go"
  - "cmd/bridge/artwork*.go"
  - "cmd/bridge/player_wiring*.go"
  - "cmd/bridge/main.go"
  - "internal/api/upscale*.go"
  - "internal/api/waveform*.go"
  - "internal/api/spectrum*.go"
  - "internal/api/files.go"
  - "internal/api/api.go"
  - "internal/admin/static/app.js"
  - "internal/manifest/variant_failure*.go"
  - "internal/manifest/analysis_failure*.go"
  - "internal/manifest/auto_optimize*.go"
  - "internal/manifest/rendition_coverage*.go"
  - "internal/manifest/dsd_peaks*.go"
  - "internal/manifest/catalog_refs*.go"
  - "internal/manifest/store.go"
  - "internal/manifest/store_vfs*.go"
  - "internal/fsutil/identity*.go"
  - "internal/admin/handlers_upscale*.go"
  - "internal/admin/handlers_variants_dir*.go"
  - "internal/admin/handlers_jobs*.go"
  - "internal/admin/player_variants*.go"
  - "internal/admin/handlers_analysis_unreadable*.go"
  - "internal/admin/variant_scope*.go"
---
# Job pools — upscale, optimize, analysis

The transcode and analysis pools and their gates, the PCM and DSD renditions, the rendition stamps and staleness, and the sidecar, variant and artwork reapers that delete what the jobs wrote.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Job pools — upscale, optimize, analysis

- **THREE CLI commands feed the same classifier and worker pool, split by
  tier, and their DSD arms are gated per RUN.** `bridge upscale` (PCM →
  higher-rate PCM), `bridge optimize` (PCM → 16/44.1|48, and DSD →
  `optimized-dsd-v2-*` under the flag) and `bridge render` (DSD →
  `pcm-v2-*-24`). `runUpscaleParams.dsdCaps` is probed once per run and its
  ZERO VALUE refuses every DSD source, which is what keeps `upscale` and a
  flag-less `optimize` byte-for-byte unchanged. **The admission itself
  delegates to `transcode.OptimizeEligibleFor` / `PCMRenderEligible`** — the
  same predicates the coordinator's walks and the auto-optimize sweeper use.
  Don't hand-roll a local reading: a source the sweep renders and the CLI
  refuses (or the reverse) is precisely the drift that indirection prevents.
- **"We could not read the decoder listing" is not "your ffmpeg lacks the
  decoders", and `HasDSD == false` means both.** `ProbeFFmpeg` sets
  `DecodersKnown=false` on a timeout, an unparseable listing or a failed exec,
  and `HasDSD` is false in every one of those — so a ladder without its own
  `!DecodersKnown` case reports a confident fact about the operator's BUILD
  that the bridge never established. Three surfaces phrase this verdict;
  `bridge doctor` and `bridge render`'s precheck both call `ProbeFFmpeg`
  directly and keep the error, and both said it correctly. The CONSOLE read
  `FFmpegSnapshot()`, which **discards** the error — hence `FFmpegInfo.ProbeErr`,
  so the cached path can say why. Field-reported against the v0.2.0 Docker
  image, whose Dockerfile asserts all four `dsd_*` decoders plus `dst` at BUILD
  time and fails without them — making the build the one explanation it could
  not have been. The control reproduces the reported message verbatim from a
  probe that merely timed out.
- **A DSD source SKIPS the `soxInfo.CanDecode` check in
  `classifyUpscaleTrack`.** sox cannot open DSF/DFF at all — ffmpeg decodes
  and sox takes the raw pipe — so asking sox would refuse every DSD
  candidate. The route is decided, and fails CLOSED, inside `transcode.Run`
  (`ErrDSDDecodeUnavailable`); this check is layering for the PCM path, never
  the verdict. `ffmpegDSDCLIReady` refuses BEFORE the library walk, which is
  the difference between one honest message and one failed job per DSD track;
  `dst` is a NOTE there, never a refusal, because a build with the `dsd_*`
  decoders and no `dst` renders plain DSF/DFF and skips only DST-compressed
  DSDIFF.

- **The DSD decimation was MEASURED and it does not alias — but the test that
  says so is a DIFFERENTIAL, and an absolute in-band bar cannot replace it**
  (PR B4; numbers + method in `ops/engineering-log.md`). The 5th-order fixtures
  carry **about -104 dBFS of their OWN in-band content** near 20 kHz (the NTF
  rising at the band edge, plus odd harmonics of the test tone — ordinary for a
  1-bit quantizer), so an absolute floor measures the MODULATOR: the first draft
  asserted -110 dBFS and duly "failed" at -91.8 with the pipeline blameless.
  `TestDSDRender_AliasRejection` instead renders the 50 kHz probe through Stage A
  twice — the shipping effect order (taken from `dsdStageAArgs`, not retyped) and
  a reference that low-passes at the 352.8 kHz intermediate BEFORE decimating —
  and asserts the difference. Measured **+0.00 dB** (faithful) and **+0.38 dB
  peak / +0.46 dB energy** (compact), against a 1.0 dB bar. **So the design's
  recorded "`sinc` BEFORE `rate`" fallback is NOT needed; don't take it without
  re-running this.** The coarse `peak <= -80 dBFS` pin beside it exists only to
  catch an anti-alias filter that vanished entirely.
- **`sinc -a 110 -t 10000 -35000`: `-t` is the FULL transition width CENTRED on
  the cutoff, and the stopband is -116.8 dB.** Measured — -6.02 dB at exactly
  35 kHz, flat to 31 kHz, -131.4 dB by 60 kHz — so the 30 kHz passband edge /
  40 kHz stop edge the docblock claims is what ships.
  `TestDSDLowpassSincConvention` is gated on **sox alone** — it synthesises and
  filters with sox and never decodes DSD, so gating it on ffmpeg too would skip
  the convention pin on a host that can run it (CodeRabbit on #866).
- **⚠️ `sox … stats` over a SHORT file after a steep FIR reports the edge
  TRANSIENT, not the stopband — a 61 dB error, in the direction that makes a good
  filter look broken.** The filtered 40 kHz tone reads **-64.56 dBFS**
  whole-file against **-125.85 dBFS** steady-state, i.e. a 117 dB filter measures
  as 55 dB. `soxSteadyStateRMSdB` takes the middle half for exactly this reason.
  Sibling trap in the same sweep: **`-r` must PRECEDE `-n`** — sox binds options
  to the file that FOLLOWS them, so `sox -n -r 352800 … synth … sine 80000` sets
  the OUTPUT rate, generates at sox's 48 kHz default, and aliases the request to
  16 kHz; every stopband frequency then silently lands in the passband and the
  whole sweep reads -0.00 dB.
- **Assert the clip guard on the TRUE PEAK; only log the spectrum.** The
  rendition lands at **-0.97 dBTP** against its -1.0 target (applied gain
  +5.00 dB on the -6 dBFS fixture), which is the end-to-end pin that Stage C's
  `6.0206 + G` agrees with `G = clamp(0, 6, -TP_unity - 1)` — the arithmetic an
  earlier draft got wrong as `6.0206 + G - 6`, which would have shipped every
  rendition 6 dB quiet. The spectrum reads the tone 0.76 dB lower because 1 kHz
  does not land on a bin and Blackman-Harris costs up to 0.83 dB of scalloping,
  so a tone-based bar would have to be loose enough to hide a real error.
- **The fixture generator's modulator is a low-distortion CIFF and it was
  verified BEFORE anything was generated.** A first attempt used CIFB with
  feedback at all five integrators: stable, linear, and **DC gain 0.257**, so
  every fixture decoded ~12 dB low and all three tests failed on real clipping.
  The shipped form is checked by DC probe (`mean(y) == u` exactly,
  `mean(e) = 0.000000`). ⚠️ Between the two, a throwaway harness reported
  "gain 2.0" across three different coefficient sets — **a constant factor across
  every variant of a parameter is evidence about the MEASUREMENT, not the
  subject**; it was a print bug (`want {dc*0.5}` while passing `u = dc`).
- **The album-level gain's parts (2026-09-27, #1053; switched on as the DSD
  `v2` schema by the PR after it; `ops/plan-2026-09-27-dsd-album-gain.md`).**
  Stage B's guard decides each track's boost alone, so an album's tracks shift
  against each other (Phase 0, on the operator's library: 57 % of DSD albums
  by more than 1 dB). The album gain gives every track the boost its album's
  hottest track allows.
  - **`dsd_peaks` (v47)** holds one true peak at unity per (track,
    `DSDPeakProfile` = recipe|tier|rate|rateFlag). NULL means silent and a
    missing row means never measured. A peak is fresh only while its mtime and
    size match the track row, so **never read one without that join**
    (`FreshDSDPeaks`).
  - Three writers fill it: every render, through `UpsertVariant`
    (`VariantRow.PeakProfile`, in the same transaction); the survey's
    `UpsertDSDPeak`; and v47's one-time seed from `track_variants.true_peak_dbtp`.
    The seed spells the profile in SQL, so **don't change `DSDPeakProfileFor`
    without the seed**. Both pin `"a1|compact|44100|-v"`.
  - **`DSDPeakRecipe` is not `DSDRenditionSchemaVersion`.** Bump the recipe
    only when Stage A changes what it produces, which makes every stored peak
    stale. A gain-policy change keeps them all.
  - **`MeasureDSDPeak` is the render's own Stages A and B** (the shared
    `decodeAndMeasure`), so a measured peak equals the render's exactly
    (`TestMeasureDSDPeakMatchesTheRender`). **Don't fork a second decode path
    for measuring.**
  - `JobSpec.AlbumGain` is injected by the POOL when the job runs, never at an
    enqueue site. The pool also widens the job's deadline by `SurveyBudget`.
  - Stage C applies the album figure **never above the track's own guard**
    (`albumBoundedGain`), so a stale figure cannot make a file clip.
  - **Claims cannot deadlock, by construction.** A render claims its own track
    before Stage A and resolves the claim on EVERY exit (a deferred error
    covers early failures). A survey claims an album-mate while it measures
    it, and releases it on every exit too, a panic included: `measureClaimed`
    defers the release as its first statement (2026-09-28). Released only by
    plain calls after the measurement, a panic in the decode left the claim
    registered, and because `transcode.Pool.processJob` recovers a runner
    panic and keeps the worker, every later render of that album waited on it
    until its own deadline, until a restart
    (`TestAMateWhoseMeasurementPanicsReleasesItsClaim`: 2 s of waiting, then
    `context deadline exceeded`, before the defer). So a claim is only ever
    held by work that is decoding, and nothing waits while holding one.
    **Don't make a render wait before resolving its own claim, don't hold a
    survey claim across a wait, and don't release a claim anywhere but on
    every exit.**
  - A waiter whose claim resolved without a peak measures that mate itself,
    because the holder's failure may have been transient. A length-incomplete
    decode is remembered, so that measurement is the memory. Only the waiter's
    OWN failed measurement decides about the mate (next bullet).
  - **A failed album-mate measurement leaves the mate out only when it is a
    fact about the mate; any other failure decides NO album figure**
    (2026-10-14, backlog B215). The facts (`leftOutBecause`): a short decode
    or a malformed header (`transcode.RepeatedFileFact`), a mate this bridge
    would not render on the profile (`albumgain.ErrMateIneligible`, which
    `SpecFor` returns for no track row or a source the rendition gate
    refuses, and `measure` for a mate whose row moved to another profile,
    which also drops the index), and a mate whose renders are suppressed by
    their strikes (`VariantFailureSuppressed`). Anything else (a full
    scratch volume, an I/O error, a store read, a path that does not
    resolve) says nothing about the mate, and a boost decided without it is
    too high when it is the hottest, kept by the rendition for good: the
    survey still measures the other mates (their peaks are recorded for the
    next attempt), then `AlbumGainDB` returns an error wrapping
    `transcode.ErrAlbumMateUnmeasured`, unless a later pass of the call
    found the mate's peak recorded after all. The render fails, and **the
    pool strikes nothing for it** (it is no verdict on the RENDERING file;
    three strikes would suppress a good file over its mate's full scratch
    volume), counted and announced like any other failure, so the next sweep
    or request renders it again, by when the mate's own render has usually
    recorded its peak. A transient mate is measured at most once per call.
    **A mate stops holding its album back once it has failed transiently
    `mateTransientFailures` (3) times, the first of them `mateTransientGrace`
    (24 h) ago or longer**, in memory, ended by a peak of it measured or
    found recorded, whoever recorded it (a found one too since 2026-10-14):
    the bound for a mate whose own render never runs (it already has a
    rendition) and that fails the same way on every survey. Both terms, so
    two failures a day apart leave nothing out. **A host fault
    (`transcode.HostFault`: a tool this host lacks, an output it refuses)
    never counts toward it**, however long it lasts, and still takes the
    pool's earlier exits, since the error wraps the mate's own. **Don't
    leave a mate out for a failure that is not a fact about it, and don't
    strike the rendering file for its album-mate**
    (`TestATransientMateFailureDecidesNoAlbumFigure`,
    `TestATransientMateIsMeasuredOncePerCall`,
    `TestATransientMateRecordedSinceDecidesTheFigure`,
    `TestAnIneligibleMateDoesNotConstrain`,
    `TestAMateOnAnotherProfileDoesNotConstrain`,
    `TestASuppressedMateDoesNotConstrain`,
    `TestATransientMateStopsHoldingTheAlbumAfterTheGrace`,
    `TestTwoFailuresADayApartLeaveNothingOut`,
    `TestAHostFaultNeverLeavesTheMateOut`,
    `TestAPeakFoundInTheStoreEndsTheGrace`,
    `TestAPeakFoundUnderTheClaimEndsTheGrace`,
    `TestAnAlbumMateWaitStrikesNothing`).
  - **A mate whose decode finished short of its container is remembered for
    the process, keyed on the mate spec's path, size and mtime** (2026-10-13,
    backlog B241). `SpecFor` stamps those from the track row, the same facts a
    peak's freshness uses. The next render in the album uses the memory: it
    does not decode the file and it does not log it. The line is written when
    the failure is first remembered. A changed size or mtime is measured
    again. A cancelled context, a deadline and an output fault are not
    remembered. A malformed DSF header is remembered on this same map
    (`RepeatedFileFact`). `LengthIncompleteFile` stays the short decode.
    A restart measures the file once more. The
    mate still does not constrain the album (a file fact, B215). **Don't write the memory into
    the variant-strike table**: the file's own v2 render already strikes a
    length-incomplete decode, and a survey failure must not add a strike.
    (`TestAShortMateIsMeasuredOnceAcrossTwoRenders`,
    `TestAMalformedMateIsMeasuredOnceAcrossTwoRenders`,
    `TestAChangedShortMateIsMeasuredAgain`,
    `TestATransientMateFailureIsMeasuredAgain`,
    `TestLengthIncompleteFileIsTheDecodeAndNotAnOutputFault`,
    `TestRepeatedFileFactKeepsAMalformedHeaderDistinct`.)
  - **A DSF's completeness reference is its fmt sample count, and an
    uncompressed DFF's is the DSD chunk's declared payload** (2026-10-13,
    backlog B241). ffprobe's container duration counts every byte from the
    data chunk to EOF, so a trailing ID3 chunk is audio to it. A complete
    file then fails the 2% bar, and a file cut inside the data chunk,
    whose header still names the whole sample count, is accepted.
    `JobSpec.dsdReferenceDuration` reads that header from the file being
    decoded, before Stage A, and `decodeAndMeasure` uses it for the render
    and the album survey. A DSF whose sample count and data-chunk size
    disagree by more than one block, and a data chunk that declares no
    audio, is malformed (`manifest.ErrDSFAudioMalformed`). The survey
    remembers that mate through `RepeatedFileFact`. The render strikes it
    on the v39 path: the error is not an output fault, a missing tool, a
    depth mismatch or a short decode. A DFF payload of no bytes, and a
    compression other than absent or `DSD `, is no reference, so the
    caller keeps the probed duration. DST stays on ffprobe's duration: a
    synthetic DST stream reports none, and the DSD chunk's byte length is
    the playing time of an uncompressed stream. Analysis uses the same
    header on the ffmpeg path when sox cannot open the file, and a header
    that fails `Check` is not a reference there.
    `verifyRendition` never sees a DSF or a DFF: those routes return from
    `renderDSD`. A strike written under the old reference stores no error
    class (v39 is count, time, size and mtime). After this ships, the
    operator clears those rows with `POST /api/upscale/failures/retry`.
    (`TestACompleteDSFWithATrailingID3PassesTheRenderAndTheSurvey`,
    `TestADSFTruncatedMidDataIsRefused`,
    `TestADSFHeaderThatDisagreesWithItsDataChunkIsMalformed`,
    `TestAnUncompressedDFFWithATrailingID3PassesTheRenderAndTheSurvey`,
    `TestAHeaderWithNoAudioDataIsMalformed`,
    `TestAZeroPayloadDFFNamesNoDuration`,
    `TestAnUnknownDFFCompressionNamesNoDuration`,
    `TestADFFWithNoPayloadKeepsTheProbedDuration`,
    `TestAnUnknownDFFCompressionKeepsTheProbedDuration`,
    `TestAMalformedDSFHeaderIsNotTheAnalysisReference`.)
  - **A survey reads the store again after it takes a claim, and before it
    measures.** Its pass's read can be minutes old by then, because it
    measures mates one after another, and another survey may have measured,
    recorded and released that mate in between. A measurement records before
    it releases, so the second read sees it. **Don't drop the re-read:**
    without it the mate is decoded twice (`TestASurveyRereadsThePeakUnderItsClaim`).
  - **Membership is the admin catalog's album identity** over served, local
    DSD rows. `StreamDSDCatalogRefs` shares `StreamCatalogRefs`'s scan, and
    `TestIndexGroupsLikeTheAdminCatalog` pins the grouping against
    `librarycat`. **Don't group by folder.** Routed rows and SACD virtual
    tracks never count: neither is rendered here.
  - **The switch-on is the DSD schema `v2`, never a re-render under `v1`.** A
    phone applies a rendition's `appliedGainDB` to the bytes of its
    downloaded copy, looked up by id, so re-rendering an id in place gives old
    bytes the new gain. v2 mints new ids, and **a `v1` row stays while its file
    exists** (nothing reaps it). **Don't add a reaper until the app records the
    gain with its downloaded copy, plus a grace period.** `variantsAggSQL`
    lists a track's renditions newest first (`created_at DESC, variant_id
    DESC`), because iOS takes the first prefix match
    (`TestVariantsListTheNewestRenditionFirst`).
  - **A DSD source's coverage needs a fresh row of the CURRENT DSD schema**
    (`manifest.DSDRenditionSchemaVersion`, the mirror
    `TestManifestMirrorsTheDSDRenditionSchema` pins); a PCM source's stays
    version-agnostic. A phone never requests a family it holds, so the bridge
    drives the move. The sweeper's compact pass moves by that coverage rule.
    `drainSupersededPCMRenditions` moves the faithful tier after it, under the
    same cap and disk budgets, and **only for tracks that already hold a
    `pcm-` row** (never a new 5 GB-an-hour tier). The card's "remaining"
    counts both. Without auto-optimize, `bridge optimize` / `bridge render`
    move them: their skip check is the exact current id.
  - **Wiring.** Serve: `wireAlbumGain` (`albumgain.New` with
    `upscaleEnqueuerAdapter.albumMateSpec`, then `Pool.SetAlbumGainer`), and
    `Invalidate()` in the post-scan hook. CLI: `newCLIAlbumGainer` /
    `cliAlbumMateSpec`, the run's own classifier with `--filter` and the resume
    check lifted, because a mate outside the filter still bounds the album.
    Both build a mate's spec the way a render of it would be built, with the
    source facts from the TRACK ROW, which a peak's freshness is judged
    against. `TestAlbumGainEndToEnd_RealToolchain` (serve) and
    `TestRenderCLIAlbumGainEndToEnd_RealToolchain` (CLI) render one album of two
    real tones and check the published files keep the source's 9 dB between them.

- **`Enqueue` fires `fireStateChange()` UNDER the lock, before the unlock**, in
  both pools. Workers are bounded by `Stop`'s `wg.Wait()`; `Enqueue` is not, so
  firing after the unlock lets a preempted enqueuer resume after `Stop` closed
  the channel and panic on send-to-closed — which fires even inside a `select`
  with a `default` (that guard covers a FULL channel, not a CLOSED one). Don't
  move it back to "shrink the lock window".
- **The publisher is one long-lived goroutine, never `go fire()` per
  transition** (which fanned thousands of goroutines under burst). State changes
  coalesce on a cap-1 channel; job-completions do NOT coalesce (each carries a
  unique path iOS keys on) and block for fidelity. `Stop`'s five-step ordering
  is load-bearing, and the publisher must NOT exit on `stopCtx.Done()` or a
  blocking worker send deadlocks.
- **`finishJob` clears the worker slot and releases dedup BEFORE each terminal
  `fireStateChange`** — a `defer Store(nil)` registered ahead of `processJob`'s
  tail runs LIFO *after* that tail's fire and publishes a stale "active"
  frame. `ActiveJob` is immutable after `Store()` and carries a start
  timestamp, not a ticking `elapsedSec`, so the SSE frame stays diff-stable;
  the browser ticks elapsed.
- **In the analysis pool, a job's count and its dedup release are ONE step,
  taken after its bookkeeping** (#987). `processJob` counted a failure, THEN
  wrote the strike and its WARN, and released the path LAST, and `Enqueue`
  then answered a held path with a silent nil. So a caller that acted on the count,
  which is what a retry is, queued nothing:
  `TestASuccessfulAnalysisClearsTheStrikes` timed out on a CI runner after 900
  clean runs on a laptop, and under 3× CPU oversubscription the old pool
  failed 9 runs in 720, six of them on the strike not yet written when
  `Failed` said 1. `analyze.Pool.finishJob` releases and counts in one `p.mu`
  critical section, and `Stats` reads under the same lock. Split the two and
  a window opens in EITHER order: count-first drops the retry, release-first
  shows the job nowhere. Every exit reaches `finishJob` through ONE deferred
  call, a panic included, so no new path picks its own order. The counters
  are plain integers under `p.mu`, so an increment outside the lock is a race
  the detector reports (measured: 15 reports across 11 tests). **Decide the
  outcome where `p.closed` is read, never after the release**: `bridge
  analyze` answers an idle pool with `Stop`, so a later read un-counts the
  run's last job. #947 met this window first (its helper's tests failed 1 run
  in 5) and fixed it in the test HELPER, leaving the pool and the one sibling
  test that did not use the helper. The transcode pool had the same order,
  plus events sent before the release, until #988 (next bullet).
- **In the transcode pool, the count, the release AND the job's event are one
  ordered tail** (#988). Every `processJob` exit counted (atomics) before
  `finishJob`, and the fsync and store exits also sent their blocking
  `jobFailed` before it, under a `fireJobFailed` docblock that said "after
  releaseDedup". Each exit now records one `jobEnd` and returns. The deferred
  tail runs `finishJob` (the ownership-checked `releaseDedupLocked` plus the
  count, one `p.mu` section), THEN `announce` (the event and the state
  change, outside the lock, since the sends block). So a count or an event
  means the path is free, and three consumers act on exactly that. The batch
  Coordinator answers `ErrDuplicateInflight` by DROPPING the path from the
  batch it is building. `POST /v1/upscale` treats it as accepted. And the
  console's `notifyUpscaleProgress` bypasses its refresh throttle only when
  the frame that advanced Done+Failed also shows nothing in flight, so a count
  that arrived first could throttle away a batch's final refresh. **A counted
  job is always announced, an uncounted one never**: `p.closed` is read where
  each exit decides, never after the release (the old runner-error tail
  re-read it there and could count a failure and drop its event). The success
  exit reads it not at all: its row has committed, so it counts and announces
  during shutdown and `Stop` drains the event. The strike and the orphan
  sidecar's removal stay INSIDE the claim, or a retry's fresh output is what
  the old attempt deletes. Measured on the old pool: a busy poll saw the job
  counted while in flight 400 times in 400, and every immediate retry was
  refused, while a 1 ms poll saw it 0 times in 100.
- **Both pools answer a held path with `ErrDuplicateInflight`, and a caller
  that counts gives it a bucket of its own** (#992). The analysis pool
  answered with nil, and `analysisSweeper.enqueueAll` counts every nil as
  enqueued. A track has no analysis row until its job finishes, so every
  sweep during a long first analysis re-offers the whole backlog: the
  `auto-analysis sweep enqueued tracks count=N` line and the Jobs card's
  `enqueued` reported a full queue of old work as new, every sweep, in a line
  whose parts must add up to the track total. They are `AlreadyQueued` now.
  **A new `Enqueue` answer reaches every caller in the same change**: this
  pool has two, and `bridge analyze`'s dispatch stops on any error it does
  not know, so the sentinel alone would have ended a run at the first
  duplicate (unreachable today: each candidate is a distinct track, offered
  once to a pool the run created). `AnalysisSweepCounts` states the contract
  the line relies on (every int field but `Total` is a bucket, and the
  buckets partition `Total`), and `TestDescribeAnalysisSweepAccountsForEveryTrack`
  runs the shipped `describeAnalysisSweep` under node with each bucket
  holding a distinct power of two. A count then identifies its bucket, so
  the test names a bucket the line leaves out or shows under another's
  label, and a part that is no bucket at all, such as one read from a field
  the server does not send.
- **A job a worker takes while its live gate is closed is DROPPED, and a
  drop is an outcome of its own in both pools** (2026-10-15, backlog B155;
  the rule, its measurement and the gate a job answers to are under The
  CLI and the serve wiring). `outcomeDropped`: the job does not run, and
  finishJob gives its path back and counts it (`droppedCnt`) in its one
  critical section, so every accepted job is still in exactly one of in
  flight, done, failed or dropped (both pools' tests check it at every
  snapshot they take). The transcode pool announces it on `jobDroppedChan`,
  the same blocking 2×workers buffer a failure has, drained by the one
  publisher and closed by Stop after `wg.Wait`. **Don't count a drop as a
  failure** (a batch would read the file as broken, and the throughput
  window would take a sample of a job that never ran) **or announce
  nothing for it** (a batch waits on every job it enqueued).
- **Every job gets its own `context.WithTimeout`, cancelled per job**, or one
  pathological file consumes a worker slot until restart. Shutdown gating reads
  the monotonic `p.closed` flag, NOT `stopCtx.Err()` — `Stop` flips the flag
  before cancelling the context, and in that gap a graceful shutdown would be
  misclassified as a real failure.
- **`JobSpec.Background` is load-bearing and `Kind` cannot express it.** A swept
  auto-optimize job is the same KIND of work as an on-demand CarPlay request
  with the opposite urgency; enqueuing a library-wide sweep on the foreground
  lane re-opens exactly the head-of-line blocking the two-channel queue exists
  to prevent, with no bridge-side symptom.
- **ALAC reaches the pipeline through an ffmpeg pipe, and the completeness
  guard on it is TWO-SIDED.** No stock sox build has an MP4 demuxer, and ALAC
  is the one LOSSLESS format that clears every upstream gate and then cannot be
  decoded; `ffmpeg … -f f32le -` into `sox -t raw` fixes it with the effects
  chain shared byte-for-byte (`soxArgsFrom` takes the input argv, so both routes
  have ONE definition of gain-guard/rate/dither). Three things are measured, not
  assumed: **ffmpeg exits 0 on a truncated source** — a half-truncated faststart
  `.m4a` still reports its full 8.000s from an intact moov and produced 3.901s of
  audio, silently, and the sidecar is keyed on source mtime+size so it would
  never regenerate; **a complete decode is EXACTLY 1.000000** across 44.1/48/96/
  192 kHz, mono and stereo, so a 2% tolerance is generous; and **an input rate
  declared too LOW makes the output LONGER** (a 44.1 kHz source described as
  22050 produced exactly 2.0x), which a lower-bound-only guard accepts while
  committing a half-speed variant — `internal/analyze`'s one-sided form is right
  for its own purpose, this one needs both bounds. **Raw, never `-f wav`**:
  ffmpeg cannot seek back to patch a header on a pipe so it writes RIFF size
  `0xFFFFFFFF`, and sox then prints `WARN wav: Premature EOF` on EVERY successful
  job — noise, and indistinguishable from the real truncation this guards. The
  fallback is an **allowlist of the MP4 family, not "anything sox refused"**:
  lossy is excluded upstream and DSD takes its OWN route (`routeFFmpegDSDPipe`,
  granted only by the fail-closed decoder probe — PR #863; this sentence read
  "lossy and DSD are already excluded upstream" until then), so anything else
  reaching a refusal is a shape neither decoder was chosen for, and routing it
  would turn an honest refusal into a mystery failure. **The probe is gated on the extension**
  — `ProbeSox` is a fork+exec (7.9 ms; `FFmpegAvailable` is 17 µs) and `RunSox`
  documents that it does not probe per iteration, so only a source whose decoder
  is genuinely undecided pays one. `RunSox` returns the settings it actually
  used — the persist site cannot know the route, and a forensic record that
  names the wrong decoder is worse than one that names none. **The doctor
  warning names WHICH binary is absent**: `ffmpeg` and `ffprobe` are both
  required (ffprobe supplies the pipe's geometry and the guard's duration) and
  some distros package them apart, so blaming ffmpeg sends that operator to a
  binary they already have.
- **`redactSoxErr` must know EVERY absolute-path family a job can name, and the
  journal gets the redacted message too.** sox's stderr quotes its argv, so a
  failing job names the absolute source path and — for a DSD render — the
  Stage A scratch under `upscale.tempDir` (or the OS temp dir): a THIRD family
  the two original passes (source, `OutputDir`) could not see, added with the
  renditions and found by the v0.2.0 logging audit. Pass 2b strips the scratch
  directory and the configured tempDir; and `pool: sox failed` logs
  `redactSoxErr(err.Error(), spec)`, not the raw `err` — the raw form put the
  absolute path in the journal beside a `path` attribute that already named the
  file the way the privacy page promises (library-relative). A new absolute
  path in any job's argv needs a pass here in the same PR. **And every exit's
  message goes through it, not only sox's** (#1055): the fsync exit put the
  sidecar's absolute path on the batch row `GET /v1/upscale/batches` serves,
  and the store exit, a recovered panic and the timeout warning (ffmpeg's
  stderr) could do the same. A new exit that builds a message from an error
  redacts it here. **And a bare directory is a path too**: a root-level
  source's sidecar sits directly in the variants directory, so a failed
  parent-directory fsync names that directory with no separator after it,
  which the prefix strip cannot see (CodeRabbit on #1055). Pass 2c turns
  each bare directory (the render scratch, tempDir, the variants
  directory) into a placeholder, longest first, because the variants
  directory and tempDir can nest either way or share a string prefix.
  **And a job's error logged outside the pool takes the same redaction**:
  the album survey logs an album-mate it could not measure, and
  `MeasureDSDPeak`'s error names the mate's absolute path and the scratch
  under the tempDir, so `albumgain`'s `measure` returns it through
  `JobSpec.RedactError`, by the mate's own spec (2026-09-28).
- **Analysis commits only on a length-complete decode**, gated by the probed
  duration — NOT exit code, `-xerror`, or stderr matching. Both decoders exit 0
  on a truncated-but-openable source, and a partial commit is keyed to
  mtime+size so the skip gate never re-analyzes it. Two alternatives are
  empirically disproven: `-xerror` also fails a glitchy-but-COMPLETE file
  (permanent treadmill), and sox stderr markers cannot distinguish truncation
  from a resynced complete file.
- **AAC encoder delay is an absolute allowance on that ratio, and the
  failure names both durations to the millisecond** (2026-10-13, backlog
  B192). Apple AAC's container duration counts priming and padding the
  decoder strips. `itunes_aac_pop.m4a` (116 ms) probes 0.116 s and decodes
  to 0.050 s: iTunSMPB is 2112 samples of priming, 803 of padding and 2205
  valid. afconvert clips from 0.1 s to 5 s are short by 57–68 ms, so the
  0.90 bar fails a complete Apple AAC file under about 0.6 s. ffmpeg AAC
  with its edit list, ALAC, FLAC and MP3 (sox duration against the sox
  decode) match. A duration floor under that 0.6 s would accept a 60% byte
  cut of the same Apple files: the probe stays at the container duration
  and the decode is empty, exit 0. `aacPrimingAllowance` is 80 ms, added to
  the 10% slack (`shortfall > expected*0.10 + allowance`), and only when
  ffprobe's codec name is `aac`. ALAC shares the m4a container and the
  ffmpeg path and its gap is 0, so the allowance is the codec name. A probe
  that names nothing grants 0. The sox path grants 0, because its duration
  already matches the samples it writes. A shortfall equal to the slack is
  still accepted. A header that claims 5 s over 3 s of audio fails, and so
  does a 60% byte cut at 0.5 s, 1 s and 5 s. A synthetic 0.1 s AAC whose
  decode is 40 ms short of a 0.1 s probe sits inside 80 ms; that shape is
  the residual a duration check cannot separate from priming, and the 0.1 s
  cut that is pinned decodes to nothing while the probe stays. The album
  survey does not use this bar. Its check is `durationTolerance` 0.02,
  two-sided, on DSD mates, and AAC is not rendered there. The text is
  `decoded %.3fs of %.3fs probed`, so 0.050 and 0.116 are not both printed
  as 0.1. **Zero frames against a known duration always fail**: under about
  88.9 ms the allowance alone covers the whole probe, so without that rule an
  empty decode would store an empty waveform (CodeRabbit on #1205). The
  ffmpeg path probes codec, channels and duration in ONE ffprobe run
  (`ffprobeAudio`). No `ExtractorVersion` bump.
  (`TestShortClips`, `TestDecodedShortOfDuration`,
  `TestPrimingAllowanceIsAACOnly`.)
- **…and a source that will NEVER decode stops being offered, on the decoder's
  own verdict.** A failed analysis writes no `track_analysis` row and every
  candidate query selects tracks that LACK a fresh waveform, so a truncated
  file was re-selected on every sweep forever: 30 of them produced **1,385 WARN
  lines in 7 days** on one host and re-failed at every start on the next, with
  nothing naming which files to replace. `internal/analyze` classifies the
  failure at the site where the fact is KNOWN (never by matching the message
  later — the enricher's "a 4xx whose body mentions HTTP 503" trap):
  `ErrSourceUnreadable` covers a clean-exit decode materially short of the
  probed duration, and a decoder that RAN and EXITED non-zero. A decoder killed
  by a SIGNAL is excluded — `ProcessState.Exited()` is the split, because the
  per-job timeout and the OOM killer reach no verdict, and the OOM killer picks
  the biggest decode. **Everything unclassified is transient**, so a missing
  sox, a faulted read and a full output volume record nothing; the permissive
  answer is the safe one here, since a transient verdict costs one decode and a
  permanent one costs the operator a file that silently stops being analysed.
  `markUnreadable` deliberately does NOT prefix the message — these strings are
  logged, persisted as `analysis_fail_reason` and rendered in the console.
- **The strike is stamped FROM the manifest row; the walk's LIVE stat overrules
  it.** `RecordAnalysisFailure` takes no size/mtime argument: it writes
  `analysis_fail_size = size, analysis_fail_mtime_ns = mtime_ns` in the
  statement that reads them, so the version a strike records IS the version the
  predicates compare against — binding the live stat would stamp one world and
  compare in another, and a strike written while the scanner is behind could
  never match. (The auto-optimize rule below, one subsystem over.) The
  candidate walk then re-checks against `info`, because it knows what is on
  disk RIGHT NOW: between a repair and the next scan the row still describes
  the broken file, and any disagreement resolves toward ANALYSING. Three
  consecutive verdicts suppress, a 7-day TTL retries (the toolchain is what can
  change the answer for a file nobody touched), and replacing the file re-opens
  it with no flag — that last one is the remedy the console list exists to
  prompt, so a debounce outliving it would be worse than the bug. `--force`
  does NOT bypass it, like the zero-byte gate; `--retry-failed` does, honouring
  `--filter` through the explicit-path form because `--filter` is a SUBSTRING
  match no byte range expresses, and COUNTING under `--dry-run`.
- **The WARN is kept and deduplicated to one line per (path, size, mtime)** —
  the strike count is the log gate (`count == 1`). Identical lines forever is
  the M-SEARCH shape, not the playlist mass-delete shape: every line here is
  the same by construction, so it is suppressed rather than repeated. A
  TRANSIENT failure still warns every time, deliberately — it has no marker to
  dedup against, and silencing a missing sox behind the fix for truncated files
  is how one alarm hides another. Measured end to end with a real decoder: 4
  WARN lines over 5 runs of 4 broken files, and zero work at all from run 4.
- **…and the TRANSCODE pool strikes a file only for what a tool said about
  it: a tool the host lacks strikes nothing** (2026-09-28). `processJob`
  struck the source for every runner error but a timeout and a shutdown, a
  missing sox included, and three strikes on one (size, mtime) suppress the
  file from every candidate query for 30 days, keyed on what installing sox
  does not change. The live upscale gate refuses NEW work within its 30 s
  probe, and that is all it can do: a job already queued (up to 5,000 a lane)
  or queued inside the probe window still reached the runner. Measured with the
  real `serve`: sox removed after the gate probed it, three `POST /v1/upscale`
  of a 3-track album failed 9 jobs, logged 9 identical WARNs, suppressed all 3
  files, and with sox back the batch over the album found `totalFiles: 0`.
  `unavailableTool` (internal/transcode/tool_unavailable.go) classifies at the
  site where the fact is known, by TYPE, never by message: an `*exec.Error`
  (exec's PATH lookup), a fork/exec `*fs.PathError` (the OS could not start
  the tool: gone since the lookup, a missing interpreter, the wrong
  architecture), and `Run`'s own route mark (`markToolUnavailable`) where the
  decoder probe found no route on this host's toolchain: a `.dsf` / `.dff`
  whose ffmpeg is missing, lacks the `dsd_*` decoders or has an unreadable
  listing, and an MP4 source sox has no reader for with ffmpeg or ffprobe
  missing. **A DSD-flagged row under another extension keeps its strike**: the
  same `ErrDSDDecodeUnavailable`, but a fact about the row. Such a job is still
  counted and announced (its batch row and `jobFailed` event say what
  happened), and #988's ordered tail is unchanged; the exit is a row in both
  terminal-order tables. **The default is the opposite of the analysis
  pool's**, which strikes only on a classified verdict: this debounce predates
  classification, so everything not classified as the host's (a tool it
  lacks, or since B211 an output folder it will not let the job write: the
  next bullet), or, since B53, as a source newer than its row
  (`ErrSourceChanged`: the B53 bullet below), still strikes, a tool that
  ran and refused the file included, which is what it is for. **The
  outage is reported per TOOL, once**: one Warn when it starts, the jobs after
  it at Debug under the same message, one Info when a job whose chain ran the
  tool succeeds (read from the settings' `decoder`, the route the run took),
  and a re-Warn after 24 h of silence. Per tool, because a FLAC success ran
  sox alone: ending every outage on any success reported ffmpeg back, then
  missing again, once per DSD job. This is not the analysis pool's
  per-FILE dedup the bullet above warns against: the key is the tool, so the
  alarm stays up until the tool is proven back. **v48 expired every
  suppression once**, as the TTL would (`variant_fail_at = 0`, the count
  kept): the strike records carry no reason, so a missing tool's cannot be
  told from the rest; a still-broken file costs one more attempt and is
  suppressed again. Measured on main's struck v47 database: the branch
  binary's first batch converted all 3. It leans on #1067 for the sweeper:
  with no strike, only the gate keeps the sweeper from re-offering the
  backlog to a host without sox (each such job fails in microseconds at exec).
  `TestAJobThatCannotRunItsToolStrikesNoSource` (the real pool and runner on an
  empty PATH), `TestAToolThatRanAndRefusedTheFileStillStrikesIt` (the positive
  control), `TestAToolOutageIsReportedWhenItStartsAndWhenAJobProvesItBack`,
  `TestMigration48ExpiresEverySuppressionOnceAndKeepsTheCount`.
- **…and a directory this host will not let a job WRITE strikes nothing
  either** (2026-10-02, backlog B211). A render writes in two places the
  source has no say in: the sidecar's folder under the variants directory,
  and a DSD render's Stage A scratch under the temp dir. `processJob` struck
  the source for every failure not classified as the host's, so a folder
  this user may not write struck every file rendered into it: measured
  through the real pool and `Run` on main 5f033c02, one source suppressed
  after three jobs in each of five shapes (the variants directory
  read-only, an album folder in it read-only for a PCM and a DSD render, a
  temp dir the scratch cannot be made in, the scratch made and read-only).
  The ordinary way there is a `sudo bridge render` before v0.2.1, which left
  root-owned album folders and a root-owned `1-bit-bridge-render` (the B17
  bullet): every file of those albums, and every DSD source, suppressed for
  30 days past the chown. **Each step that writes to the output side marks
  its own failure** (`markOutputFault`, output_fault.go,
  `toolUnavailableError`'s shape, so the error says what it said): making
  the sidecar's folder and the scratch directory, creating the sidecar's
  temp file and the scratch file, and the publish rename and the stat after
  it. The pool asks `unwritableOutput` after `unavailableTool`, counts and
  announces the job, and strikes nothing; the exit is a row in both
  terminal-order tables. **By type, a closed set**: a permission, asked of
  the whole chain (`fs.ErrPermission`: EACCES, EPERM, ERROR_ACCESS_DENIED),
  or the first `syscall.Errno` in the chain, a cause of the volume's in the
  platform's table (`outputFaultErrnos`: EROFS, ENOSPC, EDQUOT, EIO,
  ENOTCONN, ESTALE; on Windows the write protect, the two disk-full codes,
  the quota and a device or share gone). **A cause the source's NAME
  produces is not in it and keeps its strike** (ENAMETOOLONG, EILSEQ,
  EINVAL), and so does EFBIG, a file too large for the volume: retrying
  never fixes either. **The bridge creates the tool's output before the
  tool runs** (`createOutput`: `O_EXCL`, then `fsutil.KeepOwner`; it was
  `fsutil.Precreate`, which created nothing unless root): a folder that
  exists and refuses new files failed in sox's own open, a message the pool
  cannot classify, and now fails in the bridge's, before any decode, so a
  DSD render no longer spends its Stage A on a sidecar it can never write.
  As root, the scratch file takes its directory's owner. **Not on
  Windows**, where the tool still creates its output (`createOutput` does
  nothing): a file freshly closed there can be held by Defender or the
  indexer (`atomicwrite.RenameWithRetry`), and whether that fails a tool's
  open of it for writing is not measured, so an ACL that refuses new files
  in an existing folder keeps its strike there; making a folder and the
  publish rename are marked on every platform. **The outage is reported per
  output and kind** through the streak record the tool report now shares
  (`outageStreaks`): one Warn when it starts, Debug after, one Info when a
  job proves it over, a re-Warn after 24 h, the error redacted as every job
  failure is. **`end` walks the open streaks in the order they STARTED,
  never the map's**, so the lines one proof logs come out in one order
  (`TestOutageStreaksEndInTheOrderTheyStarted`), and what its one predicate
  needs from the job is worked out through `lazily` (a `sync.Once`), once
  and only while a streak is open: a success on a healthy host costs a lock
  and a length check. **A volume fault is proven over by any job that writes on the
  volume; a permission only by a job that writes in the folder that refused
  it**: in a variants tree where one album folder is root's, every other
  album still renders, and ending the outage on any success would Warn again
  at the next failure. **Don't key a permission outage per folder**: a
  pre-v0.2.1 sudo run left every album folder it touched root's, and a key
  per folder Warns once per folder, and again every 24 h while the unstruck
  files are re-offered; another folder that still refuses after the example
  one is fixed Warns at its next failure, as a new outage. Proven after the
  fsync, since a volume failing its writes can still take a rename; a DSD
  success proves the scratch too (the
  settings' `decoder`, as for the tools). A write a tool makes after its
  output exists (a volume that fills during the render) was not covered
  until B264 (the next bullet); measured on a full HFS+ image, a mkdir and
  an empty create both succeed on a volume full of data, so a full volume
  seldom reaches a marked step.
  `TestAnOutputDirectoryTheBridgeCannotWriteStrikesNoSource` (six shapes, the
  real pool and `Run` with stand-in tools; POSIX, skipped as root),
  `TestFixingTheOutputDirectoryBringsTheSourceBackAtTheNextJob`,
  `TestAnOutputOutageIsReportedWhenItStartsAndWhenAJobProvesItBack`,
  `TestAnOutputOutageIsNotOverAtAJobWhoseSidecarFailedItsFsync`,
  `TestMarkOutputFaultMarksOnlyTheOutputSidesCauses`,
  `TestMarkOutputFaultReturnsEveryOtherErrorAsItIs`,
  `TestMarkOutputFaultReadsWhatTheOSReports`; the positive control is still
  `TestAToolThatRanAndRefusedTheFileStillStrikesIt`.
- **`OpenStore` refuses a symlink at the database or either sidecar before
  SQLite opens it** (2026-10-12, backlog B262; measured on a Linux host as
  root over an install another user owns). `fsutil.Precreate` creates the
  main file `O_EXCL`, so it already refuses a link there — but only when the
  file is MISSING, because the `os.Lstat(path); errors.Is(err, fs.ErrNotExist)`
  gate that reaches it is false for a symlink (`Lstat` does not follow one),
  so a planted link SKIPPED `Precreate` and `sql.Open` opened the link's
  target; as root that wrote the database wherever the data dir's owner
  aimed it, and SQLite opened `-wal`/`-shm` by their own paths, so a link at
  either followed the same way (the main file was followed in the field;
  the sidecars erased SQLite's own open as `unable to open database file`).
  `OpenStore` now `os.Lstat`s all three and refuses a `ModeSymlink` outright
  (`errStoreSymlink`): none of the three is a symlink in any supported
  layout. **Don't restore the Precreate-gated-on-ErrNotExist shape without
  this guard, and don't echo the link's target into the error** — the base
  name only (`TestOpenStoreRefusesASymlinkAtTheDatabaseOrASidecar`, red via
  the guard removed; `TestOpenStoreOpensARealExistingDatabase` is the
  positive control). The lock file already opens through `os.OpenRoot`
  (#1140); the TLS pair and `tokens.json` resist through their own
  precreate / atomic-rename-in-place. The Lstat is the refusal for a link
  present when `OpenStore` starts.
- **…and that refusal holds through SQLite's own open of the database,
  `-wal` and `-shm`** (2026-10-12, backlog B346). modernc.org/sqlite
  v1.60.1 (SQLite 3.53.4) never sets `SQLITE_OPEN_NOFOLLOW`, and the
  amalgamation has no `nofollow` URI key, so `nofollow=1` does not change
  the open (measured: a final-component symlink is still followed, the
  decoy created). The unix VFS already opens with `O_NOFOLLOW`, but its
  full-pathname resolves every symlink component and the pager keeps the
  resolved path, so the open then sees a regular file. `OpenStore`
  registers a private copy of the default VFS (`storeVFSName`) whose
  full-pathname only absolutizes (`filepath.Abs` + `filepath.Clean`) and
  passes `vfs=` on its DSN. The pager opens the name it was given;
  `O_NOFOLLOW` refuses the final component of the database, `-wal` and
  `-shm`. A parent that is a symlink still opens, because the kernel
  follows parents (`TestOpenStoreOpensADatabaseWhoseParentIsASymlink`).
  `-journal` is not opened in WAL mode, so a link swapped in there is not
  written through; one present at the start is still the Lstat refusal.
  Windows `winOpen` does not pass `FILE_FLAG_OPEN_REPARSE_POINT`, and its
  full-pathname is `GetFullPathNameW`, so `OpenStore` does not install
  this VFS there; the Lstat check remains that platform's refusal, and
  the swap test skips, as B262's does. A ping or migrate that fails while one of the four paths
  is a symlink returns `errStoreSymlink` and the base name only.
  (`TestOpenStoreRefusesASymlinkSwappedInBeforeTheOpen`, red with `vfs=`
  left off the DSN: the decoy was created.)
- **…and a rendition is published only WHOLE, read from its frames, whatever
  the tool's exit status** (2026-10-02, backlog B264). sox 14.4.2 does not
  fail when a write to its output fails: it prints "error writing output file:
  No space left on device" and EXITS 0, leaving the file it began (Homebrew's
  and Debian's). Through the real pool and tools on a full volume (main
  95c894c3 on an HFS+ image, 6e9f5fc3 on a Linux tmpfs), the sox-direct, ALAC
  and DSD Stage C routes announced `done` over a truncated rendition, stamped
  fresh, so nothing rendered it again; and a DSD Stage A on a full scratch
  volume held its worker until the job's timeout: `runFFmpegPipe` kept this
  process's copy of the pipe's read end, so ffmpeg never met EPIPE when sox
  stopped reading with status 0 (closed now, once sox has started). **The
  header is no witness**: sox writes the expected length into STREAMINFO
  before the audio and only a finished stream rewrites it, so a cut sox-direct
  stream declared all 3,840,000 samples over a third of them and ffprobe
  reported the full 20 s; a piped encode declares 0, ffprobe reports no
  duration, and the ALAC guard read that as no verdict. `readFLACStream`
  (flac_stream.go) reads STREAMINFO and the file's tail: the frame that ends
  the file, by its header (sync, CRC-8, agreeing with STREAMINFO) and its
  CRC-16 to the last byte. **Whole is that frame ending at the sample
  STREAMINFO declares (> 0)**; `wholeRendition` also wants the job's rate, in
  `Run` and `renderDSD` before every publish. Then the length, two-sided:
  sox-direct against ffprobe's duration of the source and the ALAC pipe
  against `geo.Duration`, both within `durationTolerance` (measured exact: 72
  sox-direct renders within half a sample of N×out/in, 32 ALAC within 1e-6),
  and Stage C EXACTLY the scratch's samples (its gain and dither keep the
  length). **Not `sox --i` for the source**: it takes a streaming WAV's
  0xFFFFFFFF data size at its word (1,073,741,823 samples) while sox renders
  every sample. **A stream cut short is the output side's** (B211's marking):
  only a tool that EXITED 0 is read (a crash, a kill or a failure is the run's
  failure first): a write it made failed, and nothing in a source fails a
  write but its size, so the volume is asked with a 64 KiB write past the
  file's end (`probeVolumeRoom`, not zeros, which a compressing filesystem
  stores as nothing): a cause `hostOutputFault` names is that fault, EFBIG or
  any other refusal keeps the strike, and **a volume that takes the bytes is
  the output side's too** (`reasonWriteFailedThenRoom`): sox frees its `-G`
  temporary file as it exits, which on a volume the variants share is most of
  the room, and a concurrent job's cleanup frees its partial, so room after
  the fact is no evidence the source failed. **A whole stream of the wrong
  length strikes**, as any tool that finished what it read does. **Stage A's
  scratch is no witness either way**: sox rewrites a `.sox` file's length when
  it closes it, so a scratch a full volume cut and a source that decoded short
  look alike; a Stage A that failed (its pipe, or a short scratch) asks the
  scratch volume (`scratchOutput`), and only a refusal `hostOutputFault` names
  is the scratch's. **The on-demand enqueue checks room before it queues**,
  as the batch and the sweep check theirs (`roomForRendition`:
  `ProjectedSize` and `RequiredBytesWithMargin` on the variants volume,
  `RenderScratchBytes` on the scratch volume, and **their sum where the two
  are one volume**, `transcode.SameVolume`, each judged where the probe judges
  it: a DSD render holds its scratch while Stage C writes the rendition;
  CodeRabbit on #1145); a refusal is `api.ErrUpscaleNoRoom`, rejected, one
  Warn per request, no wire change. **It probes without the missing-directory
  warning** (`quietDiskFree`, `transcode.NearestExistingDir`): it runs per
  request, and a variants directory or a render scratch nothing has written
  yet is missing on every request until the first render makes it; the sweep
  and the batch, which probe once a pass, keep it. **A stand-in sox that "renders" writes a whole
  FLAC** (`internal/flactest`, `shWrites`): a payload like `fLaC` is refused
  now. Tests: `TestARenditionThatCouldNotBeWrittenWholeIsNotPublished` (real
  tools on a full volume: an HFS+ image on macOS, `BRIDGE_TEST_SMALL_VOLUME`
  elsewhere, CI's dsd-measure job mounts a tmpfs),
  `TestRunPublishesOnlyAWholeRendition` and
  `TestACutRenditionStrikesNothingAndAShortOneStrikes` (the test binary as sox
  and ffprobe, every platform), `TestADSDRenditionHoldsWhatItsScratchHeld`,
  `TestAStageAThatLeftAShortScratchAsksTheScratchVolume`,
  `TestACutStreamIsClassifiedByWhatTheVolumeAnswers`,
  `TestACutStreamIsNotWhole`,
  `TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom`,
  `TestADSDRenderOnOneVolumeNeedsRoomForItsScratchAndItsRendition`,
  `TestTheOnDemandPreFlightSaysNothingOfADirectoryNothingHasWrittenYet`,
  `TestSameVolumeJudgesEachDirectoryWhereTheProbeDoes`.
- **…and the published file's bit depth is the depth its STREAMINFO names,
  compared with the job's target before the publish** (2026-10-11, backlog
  B271). B264's check compared the rate and the frame, and the pool recorded
  `job.spec.TargetBits`. sox 14.4.2 exits 0 after `flac can't encode to
  32-bit` and writes a shallower stream: on this Mac (Homebrew sox
  14.4.2_6, libFLAC 1.5.0) a 16-bit source came back 16-bit and a 24-bit
  source came back 24-bit, both with that warning. `wholeRendition` asks
  whether the stream is whole after the rate check and before it compares
  `bitsPerSample` with `TargetBits`, so `Run` and `renderDSD` both see
  it. A truncated stream at the wrong depth is a cut (`cutOutput`, the
  output-fault probe). Only a whole stream at the wrong depth is the host
  fact. The row and
  the completion event take `RunResult.BitsPerSample` (the stream that
  passed); a stub that reports 0 keeps the job's target. **32 stays
  accepted** wherever an upscale target is chosen (the CLI flag, the
  settings option, `validateUpscaleTarget`, `SetUpscaleTarget`,
  `EffectiveTargetBits`): a build whose STREAMINFO is 32 publishes that
  file, and one that falls back fails the job. DSD optimize targets 16 and
  the PCM render targets 24, both through the same check.   The publish is
  refused, the file is labelled from STREAMINFO, and a depth fallback is a
  host fact: no strike, one report, and not planned at all while the host's
  sox cannot encode the depth. A 32-bit planning pass asks `PlanFLAC32`
  once, cached for 30s: the batch planner (`submitUpscaleProjections`, so
  the API batch and the console batch both refuse before a row is inserted)
  and the CLI (`runUpscaleBatch`, before the walk). `(false, nil)` enqueues
  nothing, reports once through the same outage (`FLAC32EncodeFix`), and
  the CLI exits non-zero with that sentence; the console's submit body is
  that error. A probe error is not a refusal, and a host whose sox gains
  the depth is planned again at the next pass, with no restart. On-demand
  single-track enqueue stays on the per-job backstop. The auto-optimize
  sweeper never plans 32 (compact is 16, the faithful DSD render is 24),
  and the startup seed only writes the target. `depthMismatchError` wraps
  `ErrRenditionIncomplete` (a batch still drains on that error) and
  `processJob` classifies it by type, after an unwritable output and before
  a changed source: counted and announced, `RecordVariantFailure` not
  called. The outage is keyed on the depth asked for. The first job Warns
  (`FLAC32EncodeFix` when that depth is 32: set the target to 24, or
  install a sox that can encode 32-bit FLAC); later jobs of that depth are
  Debug. A published file proves only the depth its STREAMINFO names, so a
  16-bit success does not end a 32-bit streak. A wrong rate keeps its
  strike: sox writes ordinary rates, and that mismatch is the pipeline.
  A config that names 32 still loads. `bridge doctor`'s `flac-32-bit` line
  probes only when the target is 32 and warns, never fails. A probe that
  ran and measured a shallower or missing file warns with the same
  sentence. A probe that did not measure (its deadline, a missing sox, a
  setup failure) warns that the check could not run, with the error and
  without that sentence. A cancelled check stays ok. Three fallback jobs leave the source unstruck; a later job at
  a depth the writer can make publishes.
  `TestRunPublishesOnlyAWholeRendition` ("another bit depth", "cut short, another bit depth"),
  `TestARenditionOfAnotherBitDepthIsNotPublished`,
  `TestAWholeRenditionIsLabelledFromItsStream`,
  `TestPoolLabelsARenditionFromTheDepthTheRunnerRead`,
  `TestCheckFLAC32Bit`, `TestCheckFLAC32BitSkipsTheProbeUnlessTheTargetIs32`,
  `TestAFLAC32ProbeThatDidNotRunIsInconclusive`, `TestCheckFLAC32BitOnTheHost`,
  `TestA32BitBatchIsNotPlannedWhenTheHostCannotEncodeIt`,
  `TestA32BitProbeThatDidNotRunStillPlans`,
  `TestUpscaleCLIRefuses32BitWhenTheHostCannotEncodeIt`,
  `TestFLAC32PlanningProbeIsCachedForAShortLifetime`,
  `TestACancelledFLAC32ProbeIsNotCached`.
- **A PCM job's `-G` file lands in the configured temp dir, and a short
  rendition whose temp volume cannot hold it is the host's** (2026-10-08,
  backlog B270). `soxArgsFrom` puts `--temp` and `renderScratchDir` ahead
  of `-G` on the direct route and the ffmpeg pipe, and `Run` creates that
  directory before sox. sox holds one `int32` sample per output frame
  there — duration × target rate × channels × 4 — and keeps the file
  open at that size while it writes the rendition. Measured on sox
  14.4.2: a 90 s stereo file to 176.4 kHz held exactly 127,008,000 bytes,
  one file, on both routes. The file is unlinked at `mkstemp` / `tmpfile`,
  so a directory listing stays empty and an earlier poll of `--temp`
  (the DSD chain's S1 note) saw nothing. A DSD job passes no `-G`, so
  `GuardTempBytes` is 0. One lane holds one job, and the pre-flight
  budgets `lanes × max(scratch, guard)` on the upscale submit, the
  rendition submit, the on-demand room check and the sweep. Since
  backlog B210 that scratch is two Stage A files (the next bullet).
  The sweep reads that
  volume's free space on every pass, a PCM-only bridge included: a known
  duration makes the guard budget positive, and a free figure left at 0
  is the disk floor, so no PCM job is submitted. A probe failure skips
  the sweep. The batch and the on-demand room check already read free
  space whenever the hold is positive. The settings blob is
  unchanged and names no path. When a finished stream is shorter than
  its source and the temp volume's free space is below that file, or a
  64 KiB write in the scratch directory fails for a cause
  `hostOutputFault` names, the job is an output fault
  (`reasonGuardTempShort`) and strikes nothing. A short stream whose
  temp volume has room for the file still strikes. sox exits 0 after
  `gain: error writing temporary file` and leaves a finished short FLAC;
  the temporary is gone by then, so the free space afterwards is what
  was left beside the write. The probe that asks the temp directory
  whether it will take a write closes the file before it removes it:
  `defer os.Remove` is registered first, then `defer f.Close`, so a
  panic or an early return does not leave the probe, and Windows can
  delete it.
  (`TestGuardTempBytesIsThePostRateInt32File`,
  `TestSoxArgsShape`, `TestSoxArgsFromSharesOneChain`,
  `TestRunPublishesOnlyAWholeRendition`,
  `TestACutRenditionStrikesNothingAndAShortOneStrikes`,
  `TestAGainGuardWhoseTempVolumeCannotHoldItStrikesNothing`,
  `TestSubmitRefusesAPCMJobWhoseGainGuardDoesNotFit`,
  `TestListAutoOptimizeCandidatesReadsAPCMRowsDuration`,
  `TestAutoOptimizeSweepSubmitsAPCMJobWhenTheTempVolumeHasRoom`,
  `TestAutoOptimizeSweepProbesTheScratchVolumeOnEverySweep`,
  `TestProbeTempDirRoomClosesBeforeItRemoves`.)
- **A DSD render holds two Stage A scratches while it surveys, and every
  pre-flight reserves both** (2026-10-09, backlog B210). `renderDSD` keeps
  its scratch from Stage A through Stage C. The survey runs after the
  claim is released, and `MeasureDSDPeak` creates a second scratch in the
  same directory for the mate decode. Freeing the first scratch before
  the survey drops the file Stage C reads. Surveying before the claim is
  released deadlocks: a render must not wait while it holds its own claim.
  A 3 s stereo DSD64 optimize holds 4,233,600 bytes per file and 8,467,200
  for the two, and the pre-flights reserved one file per lane, so a temp
  volume with room for one and not two was admitted and ran out
  mid-render. `TempVolumeBytes` for a DSD job is `SurveyScratchBytes`
  (twice `RenderScratchBytes`, saturating at MaxInt64). A PCM job stays
  `GuardTempBytes`, one `-G` file; DSD `GuardTempBytes` is 0 and the chain
  passes no `-G`. `LaneTempBytes` is the larger of the two. One lane runs
  one job, so adding the guard on top of the two scratches would refuse a
  job that fits. When the rendition directory and the scratch directory
  are different volumes, the batch and the sweep multiply that hold by
  the lane count through `BytesForLanes`, which saturates: `MaxInt64`
  times two lanes wraps negative, and `DiskHasHeadroom` treats a
  non-positive budget as no work. On one volume the peak is the survey's
  two scratches, or one scratch beside the rendition, whichever is
  larger. On demand that is `RenditionHoldOnOneVolume`. The batch
  (`preflightVolumes`, the rendition submit and the PCM submit) and the
  sweep (`drainCandidates`) ask `SameVolume` and take `SharedVolumeHold`:
  the survey's lanes of two scratches, or every rendition of the
  admission plus one scratch per lane. Multiplying the whole rendition
  total by the lane count would reserve a sidecar per lane; the sidecars
  accumulate once. Adding the rendition on top of both scratches refuses
  a job that fits. A short volume is `api.ErrUpscaleNoRoom` on demand,
  `InsufficientDiskSpaceError` from the batch, and `DiskFloorReached`
  from the sweep, before enqueue, so the source is not struck. Album gain is
  attached when the job runs, so the pre-flight cannot see whether this
  album has a mate and every DSD job reserves two. A solo album
  over-reserves by one file. A mate longer than the job can still
  overflow.
  (`TestALaneHoldsTwoDSDScratchesOrOnePCMGuard`,
  `TestGuardTempBytesIsThePostRateInt32File`,
  `TestAutoOptimizeSweepStopsWhenScratchDoesNotFit`,
  `TestADSDRenderOnOneVolumeNeedsRoomForItsScratchAndItsRendition`,
  `TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom`,
  `TestSubmitPCMRender_ScratchPreflightGradesTheTempVolume`,
  `TestSubmitOptimizeRefusesASharedVolumeThatFitsEachHoldApart`,
  `TestAutoOptimizeSweepRefusesASharedVolumeThatFitsEachHoldApart`.)
- **A NEGATED condition over a LEFT JOIN needs COALESCE, and the sibling terms
  that do not are why it is easy to miss.** `AnalysisCoverage`'s four existing
  terms test `ta.waveform_tag != ''` POSITIVELY, so a join miss yields NULL,
  the CASE takes ELSE and the row scores 0 — right for "is it analysed". The
  unreadable term NEGATES that, and `NOT NULL` is still NULL, so it scored 0 on
  the very population it exists to count: no error, a plausible number, and the
  coverage bar silently unchanged. `TestCoverageSubtractsTheGivenUpOnSet` found
  it. Suppressed tracks are SUBTRACTED from eligible (a remainder that can
  never drain reads as a stuck job) while tracks still being retried stay in
  it.
- **The auto-optimize candidate query is its own thing, not the Inspector's** —
  it must exclude UPnP-routed rows and suppressed rows, and select on "NO FRESH
  variant exists" via a correlated subquery, never a JOIN and never "some row is
  stale". One track can hold several `optimized-%` rows, so the stale form
  re-selects forever and pushes a delta to every device on every sweep.
  Staleness compares against the TRACK ROW, and the sweeper stamps from that
  same row — a live stat makes every variant read stale whenever the scanner
  hasn't caught up.
- **…and EVERY rendition writer stamps the row, and the on-demand path, the
  sweeper and the CLI queue a render only while the file still matches it**
  (#1077, backlog B24). A rendition records ONE
  version (`source_mtime_ns` / `source_size`, and a DSD one's peak in
  `dsd_peaks`) and two clocks judge it: the sweep's candidate queries and
  `FreshDSDPeaks` against the row, the serve path against the file on disk (a
  sidecar of other bytes must never be served). Between a change to a file and
  the scan that reads it no stamp satisfies both. The on-demand path (POST
  /v1/upscale) and the CLI stamped a live stat while the sweeper, the batch
  coordinator and the album survey stamped the row, so each re-rendered what
  the other wrote: measured on a real bridge (`autoOptimize.intervalSec: 20`),
  three rounds of a request for three renditions and a sweep made 18 renders,
  a delta for both tracks on every step, and the downloads answered 200 after
  each request and 410 after each sweep; with the fix, 3 renders, all 200.
  With the defaults the sweep's periodic tick fires milliseconds after each
  periodic scan STARTS, on the previous scan's rows, so the flip came once per
  scan interval; the faithful tier took part through
  `drainSupersededPCMRenditions` (a live-stamped `pcm-v2` row is not "fresh"),
  and a live-stamped peak was re-decoded by every album-mate render. The phone
  asks for a family only while it lists none of it (PlayerService tier 0,
  `shouldAutoGenerateVariant`, `BridgeRenditionRequestGate`; what it does
  after a 410 is in the B82 bullet two down), so one of its requests cost 3
  renders and up to a scan interval of 410; the CLI, a folder POST or a
  script repeat it. `transcode.SourceIsAtRow` is the one check
  (`sourceIsAtRow` in cmd/bridge until B53), the scanner's
  EXACT skip-gate comparison, never serve's 2 s tolerance: the on-demand path
  refuses (`errSourceAheadOfRow`, counted `rejected`, no wire change) and
  queues a rescan of the file's directory (`sourceRescanner`, bgWriters-joined),
  so the next request renders; the sweeper counts `changedSinceScan` and passes
  over; the CLI lists the file as needing no run, `--force` included. **The
  rescanner's queue is its pending set**: a directory is queued once at a time
  (and again once its scan has started), drained oldest first, up to 1,024.
  Until review round 1 it was a 64-slot channel that dropped the 65th, and a
  dropped directory was rescanned only if a later request named a file in it,
  so a client that asked once (a folder POST over more album directories, or
  plays piling up while a full scan holds the scanner's lock) waited for the
  periodic scan. The bound is also the work one burst queues: a rescan that
  re-reads a changed file runs the whole-library duplicate restamp (1.1 s over
  50,012 rows on the dev Mac, 7 ms for an album with nothing changed). **Don't stamp a
  live stat anywhere** (`FreshnessFromFile` is gone), **and don't render a file
  its row no longer describes**: a row-stamped render of new bytes is the one
  the serve path refuses, and with auto-optimize off (the default) nothing
  rendered it again until B82 (two bullets down). The album survey still MEASURES a
  changed mate (a peak describes the bytes on disk; `cliAlbumMateSpec` ignores
  needsRun). Until B53 (the next bullet) this bullet also said the batch
  coordinator does not check and that nothing checks when the pool starts a
  job; both were true then and are not now. `TestAChangedFileIsNotRenderedUntilItsRowIsReRead`
  drives the loop through the real handler, sweeper and download path, with
  `committingQueue` committing each job as `Pool.processJob` does (the adapter
  takes its pool through `renditionQueue` for that).
- **…and the batch walks check too, `transcode.Run` checks again when it
  starts and before it publishes, and a stale download asks for the rescan**
  (backlog B53). Measured on main at 6dfba62c: a batch over a changed file
  rendered the new bytes under the row's stamp (410 `variant_stale`), and a
  batch after the scan counted the track covered (a projection's `HasVariant`
  is ANY rendition of the family) and rendered nothing, so it stayed 410; a
  job whose file changed while it waited in the queue, or while it rendered,
  succeeded with a rendition the serve path refused; and five downloads of a
  pre-generated rendition after a retag answered 410 while nothing rescanned
  and the sweep passed the file over (`changedSinceScan`) until the periodic
  scan. Now the walks compare the resolver's stat (`ResolverFunc` returns
  `ResolveChecked`'s triple, the stat production already took) with each
  projection, pass a changed file over into the row's skipped count and ask
  for its rescan. `Run` answers `ErrSourceChanged` FIRST (before a decoder
  probe, a scratch file or an album-gain claim) and again in
  `JobSpec.publishSidecar`, the one publish helper both chains call, so bytes
  the stamp does not describe are never renamed into place. A source that
  is no longer there, or whose stat fails at all (a stale NFS handle, a FUSE
  mount's ENOTCONN, EIO), answers it too (review rounds 5 and 6): rendered
  on, the tool failed on the input and the pool struck the file, which a
  mount dropping under a queued batch did to every file behind it. Every
  enqueuer stats before it queues, so a file that stays unreadable is refused
  at its next enqueue rather than re-offered to `Run`. The pool
  classifies it BY TYPE: counted and announced like any failure (a batch must
  hear it to drain; #988's tail is unchanged, and the exit is a row in both
  terminal-order tables), it **strikes nothing** (a newer version is not a bad
  file; three strikes would suppress a good one for 30 days), and it asks for
  the rescan inside the claim. The CLI worker renders through `Run`, so a file
  retagged during a long `bridge render` fails with the change named. A 410
  `variant_stale` tells `api.StaleRenditionFunc`, and its hook
  (`staleRenditionRescan` until B82, `staleRenditionHeal` since) asks for a
  rescan only while the ROW is behind the file (once a scan read the change a
  rescan changes nothing) and once per directory per minute, bounded at 1,024
  directories: every play, device and range request makes that GET. The
  minute is spent only on a rescan the rescanner queued or already had
  waiting (`sourceRescanner.queue`); one its full queue dropped leaves the
  next GET free to ask (review round 2). EVERY
  rescan the shutdown did not interrupt drops the album-gain index, queues
  the renders stale downloads waited for (B82, the next bullet), and THEN
  nudges the auto-optimize sweep, whose tick otherwise follows the periodic
  scan (`afterRescan`): only a full scan dropped that index, so a DSD render
  the nudge starts could take a retagged track's old album-mates for its gain
  (review round 3), and not only a rescan that committed rows, since one whose
  file was deleted before it ran deletes a row and counts none (round 4).
  **A rescan request names the file by its ROW's path, and
  the rescanner resolves the directory itself**: the scanner makes each row's
  path from the spelling of the directory it is handed, and on main a request
  naming a changed file in lower case, on a filesystem that opens it, left the
  rows `Fixture/DSD/01.dsf` and `fixture/dsd/01.dsf`. A path that comes from
  a client and may name no row (an upload's folder, a delete's track) is
  mapped through `fsutil.Speller` instead (the B219 bullet under
  **Scanner**). **Still open**: a
  watcher-driven subtree scan nudges neither the sweep nor the player's
  catalog, nor drops the album-gain index (B83; `player_wiring.go` said it
  nudged the catalog until B53). This said a stale rendition whose row has
  caught up is rendered again by nothing (B82) until the next bullet. Tests:
  `TestABatchPassesOverAFileThatChangedSinceItsScan`,
  `TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep`,
  `TestEveryRescanRunsItsAfterStepHoweverFewRowsItWrote`,
  `TestRunRendersNothingFromASourceThatChangedSinceItsStamp`,
  `TestPublishingRefusesASourceThatChangedWhileItRendered`,
  `TestAJobWhoseSourceChangedIsNotRenderedAndStrikesNothing` (the real pool and
  `Run` over a stand-in sox), `TestTheCLIRendersNothingFromAFileThatChangedDuringItsRun`,
  `TestAStaleDownloadRescansItsSourceAndRendersItAgain` (renamed by B82),
  `TestAStaleDownloadAsksForARescanOnlyWhileItsRowIsBehindAndOncePerMinute`,
  `TestARescanIndexesNoSecondSpellingOfTheDirectory`.
- **…and a download that finds a rendition stale has it rendered again**
  (backlog B82; until B100, the next bullet, this title went on "a batch
  and the coverage bars still count a stale rendition as covered").
  Measured on main at 6bc4605a: with auto-optimize off (the
  default), renditions made on request before a retag answered 410
  `variant_stale` once a scan read the change and nothing rendered them
  again: the stale downloads rendered nothing, and a batch counted a track
  with ANY rendition of the family covered (`TrackProjection.HasVariant`
  until B100).
  On a real `bridge serve` with sox, after a retag and a scan the batch
  answered `enqueuedCount: 0, alreadyCovered: 1` and 15 downloads over 30 s
  all answered 410; with the fix the second download was served (the render
  took under 2 s), and so it was after a second retag with no scan.
  What the app does after a 410 (read in the iOS source, 2026-09-29): on a
  PLAYBACK 410 it drops the id from its local row, in memory and persisted,
  and retries; the retry asks for the family again only under CarPlay or
  cellular routing of a PCM source (the Tier 0 lazy POST) or where a DSD
  source's route wants a rendition (`BridgeRenditionRequestGate`); an offline
  download keeps the id and fails, the upscaled toggle and a picked rendition
  never ask, and every manifest delta for the track lists the stale id again.
  So "the phone never asks again for a family the manifest lists", B82's
  premise and this file's until now, holds only while the app still lists
  it. Now `staleRenditionHeal` (B53's hook,
  renamed) asks for the render at once when the row is current, and when the
  file is ahead of its row it keeps the render until the rescan it asks for:
  the rescanner's `after` step takes the directory it read, and `afterRescan`
  drops the album-gain index, queues the waiting renders whose rows the
  rescan brought level (`rescanned`; one still behind keeps waiting for a
  later rescan, at most an hour from the first ask, and one whose row is
  gone is dropped), and THEN nudges the sweep, so a DSD render never takes a
  stale album index. `staleRerender.rerender`
  reads the kind off the id's prefix (`renditionKindOf`; `optimized-` covers
  the DSD compact tier), and refuses unless the kind's LIVE gate is open
  (`renditionGates`: the very closures `/v1` reads, `dsdRenderActiveFn` among
  them, pinned by `TestAStaleDownloadRendersUnderTheV1KindGates`), never on a
  demo bridge (its POST `/v1/upscale` answers 403: every bearer there is
  public), never for a suppressed file (`Store.VariantFailureSuppressed`, the
  one-row form of `variantFailureSuppressedSQL`), and only then calls the
  adapter's entry point for the kind (`enqueueKind`), which renders what a
  request for the family would: the family's CURRENT id, stamped from the row
  (a DSD `v1` rendition is rendered as the `v2` one, which the app, taking the
  newest of a family, plays; the `v1` row stays, and its later downloads
  render nothing). **On the BACKGROUND lane** (`JobSpec.Background`), the
  sweep's: nobody waits on it, since the download that asked has already
  played the source, and on the foreground lane a library retagged at once
  would queue its renditions ahead of a request a client does wait on (a
  CarPlay plug-in). That is also what bounds a token holder's GETs, which
  pass no write bucket: they can queue at most one render per stale
  rendition and file version, all behind the waiting work (CodeRabbit's
  review of #1097). **At most once per rendition AND VERSION of the file per
  minute**: the minute bounds the tries of a render that fails (a failure
  writes no row), and a file retagged again is a render not yet tried.
  Keyed without the version, on a real bridge a second retag 30 s after the
  first answered 410 on 15 downloads over 30 s: the render its rescan queued
  was refused as asked for within the minute. The minute is not spent when
  nothing was tried (the pool's queue full, the kind off), and a file that
  changed again before the enqueue (`errSourceAheadOfRow`) waits for a
  rescan: the wait is recorded, THEN the heal asks for the rescan (folded
  into the one the enqueue asked for), since the enqueue's request comes
  first and a quick rescan could otherwise go by before the wait exists.
  **A rescan that brought every waiting file
  level frees its directory's rescan minute** (B53's), so the next change
  asks at once; one that left a file behind keeps it, which is the minute's
  job (a file still being written, a directory the scan cannot read). At
  most 1,024 renders wait, those older than an hour forgotten first.
  **No loop**: the render is stamped with the row, fresh to the serve path,
  the sweep and the album gain alike (#1077's one clock), and a served
  rendition calls no hook. B82 left a batch and the Inspector's coverage
  bars counting a stale rendition as covered, for the owner to decide; B100
  (the next bullet) made them freshness-aware. **Don't render from a download whose row is behind** (the
  render would record a version the serve path refuses), **and don't give
  the re-render gates of its own**: a copy is how a kind's sox half has
  drifted before. Residual: the render replaces a same-id rendition in place,
  so an offline copy of the old bytes on a phone takes the new
  `appliedGainDB` by id, as the auto-optimize sweep's in-place re-render
  already did. Tests: `TestAStaleRenditionIsRenderedAgainWhenADownloadFindsItsRowCurrent`,
  `TestAStaleDownloadWhoseRowIsBehindRendersAgainAfterItsRescan`,
  `TestAStaleDownloadRescansItsSourceAndRendersItAgain`,
  `TestAStaleDownloadRendersEveryNewVersionOfItsFileAgain`,
  `TestAStaleDownloadRendersNothingForAKindThatIsSwitchedOff`,
  `TestAStaleDownloadRendersNothingForAFileWhoseRendersKeepFailing`,
  `TestAStaleRenditionOfAnOlderSchemaIsRenderedAsTheCurrentOne`,
  `TestAStaleRerenderGoesThroughTheKindsGateAndTheSuppression`,
  `TestAStaleDownloadAsksForARenderOncePerMinuteAndWaitsForItsRescan`,
  `TestAtMostACapOfRendersWaitForARescan`,
  `TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep`,
  `TestAStaleDownloadRendersUnderTheV1KindGates`.
- **…and a batch, and every coverage bar, counts a rendition only while it
  is FRESH** (2026-10-16, backlog B100, the owner's option (c) of
  2026-10-14). A rendition covers its track only while its
  `source_mtime_ns` and `source_size` equal the track ROW's (#1077's one
  clock, never a stat). Measured on main at fe6d68d1: after a retag and a
  scan a batch answered `enqueuedCount: 0, alreadyCovered: N`, and every
  bar read 2/2 over copies that answer 410. **One test, written once in
  `manifest/rendition_coverage.go`**: `renditionFreshSQL`, and the per-kind
  EXISTS built from it (`fresh{Upscaled,Optimized,PCM}SQL`,
  `freshOfBoundKindSQL`), which the projection the batch walks
  (`HasVariant`), the browse rows' marks and the covered arm of every
  covered-or-eligible denominator compose. The reads that count
  `track_variants` rows themselves (`childFolderRollupSelect`,
  `RollupByPrefix`, `variantPresenceSelect`) compose the term through a
  JOIN to the row, measured cheaper than the EXISTS over every track row.
  **Never write the comparison out at a site.** **The kind test stays the
  version-agnostic id prefix** (any `pcm-` / `optimized-` row of the family
  counts; the current-DSD-schema rule is the sweep's alone), and **every
  size still sums every rendition, fresh or stale**: their disk is real.
  `foldPresence` counts a fresh sidecar as `covered` and a track holding
  only stale ones as `stale`, which no longer overlap, so the console's
  Delete counts `covered + stale`, and `RollupByPrefix` returns the stale
  counts the folder panel needs. Cost over 50,000 tracks and 45,000
  renditions, medians of seven: the whole-library projection 348 → 371 ms,
  `AllEligibleKinds` 91 → 111, the root folders' denominators 89 → 108,
  the root listing 74 → 111 and `RollupByPrefix("")` 54 → 91 (138 and 131
  through the EXISTS; the rollup groups per track, 130 with four
  `COUNT(DISTINCT …)`). A covering index on `track_variants(source_path,
  variant_id, source_mtime_ns, source_size)` would take the per-track reads
  back to main's cost; proposed, not added (backlog B419). Pins:
  `TestEveryCoverageReadAgreesWithTheRowFreshnessOfItsRenditions` (the
  lockstep table: every eligibility case in nine rendition states, against
  `describeVariants` and the Go gates),
  `TestABatchRendersARenditionThatARetagAndAScanMadeStale`,
  `TestTheCoverageBarsDropForAStaleRenditionUntilItIsRenderedAgain`.
- **`maxPerSweep` is not just a queue guard**: `UpsertVariant` strict-advances
  `indexed_at`, so an uncapped first sweep pushes one delta row per variant to
  every paired device at once. The disk floor is a RUNNING budget and the probe
  fails CLOSED.
- **Disk pre-flight grades the VARIANTS volume, resolved per call** from the
  live config holder — not `DataDir`, and never snapshotted at adapter
  construction. `AvailableDiskSpaceNearest` walks to the nearest existing
  ancestor, advancing only on `os.IsNotExist` so a real EACCES still surfaces.
- **The eligibility SQL and the Go gates are LOCKSTEP MIRRORS** — change one and
  the other in the same commit; a test in `internal/admin` (the only package
  importing both) fails on divergence.
- **`SetPostScanHook` REPLACES.** Append to `postScanNudges` and register one
  fan-out hook; a second registration silently unhooks the previous sweeper.
- **Every reaper fails closed on an EMPTY referenced set, and the CLI ones offer
  a way past it.** A query that succeeds and returns no rows is not a fault, and
  the routes to it are ordinary: `rm -f bridge.db*` + restart (the reset this
  file documents, and `run` takes a boot tick), or the window between a root
  flip's `WipeFilesystemTracks` — which CASCADEs `track_variants` — and the
  rescan. `runArtworkGC` and `VariantWatcher.tick` had the guard;
  `OrphanSidecarSweeper.tick`, `upscale --gc`'s FORWARD sweep and `analyze --gc`
  did not, and `upscale --gc`'s reverse guard fires only after the forward sweep
  has already unlinked. An empty set over an EMPTY directory stays a silent
  no-op, and "empty" means nothing the reaper would remove, never "no entry at
  all" (2026-09-28): the background sweep asks its inventory
  (`emptyCatalogRefusal`), so a directory left holding empty folders, a
  `.DS_Store`, a Trash or the filesystem's `lost+found` is quiet, and
  `artwork --gc`'s empty-store guard reads the cache as its walk does. **The
  CLI `--gc` sweeps decide it the same way since 2026-09-29** (backlog B65),
  after the walk and ahead of the mass-orphan check (`gcRefuseEmptyCatalog`,
  which replaced `gcRefuseEmptyKnownSetOverPopulatedDir`, whose "any entry at
  all" read made empty folders or a `lost+found` need `--allow-empty`).
  **`integrity.EmptyCatalogOrphans` is the one rule of all three**: a file
  the sweep would remove BY ITS OWN Consider, or an entry the walk could not
  stat, weighed as one; never a directory it could not list (the partial
  walk's refusal) nor a scratch `.tmp`, which is removed whatever the catalog
  says (once past the forward sweeps' grace, backlog B205). Until backlog
  B132, `upscale --gc` passed a nil Consider and removed every file, so it
  still refused over a lone `.DS_Store` while `analyze --gc` passed over one.
  The rule then was not to count only rendition-shaped files while the sweep
  went on removing every file: the refusal would wave through a run that
  removes files it never weighed. Since B132 the variants sweep's Consider is
  `integrity.ConsiderVariantSidecar`, the probe's own notion of a rendition
  in either layout, so a `.DS_Store`, a `.keep` or a README is never a
  candidate and an empty catalog over only those files proceeds. The
  mass-orphan ratio weighs the same set. A rendition no row names is still
  reclaimed, and a directory of renditions with an empty catalog still
  refuses. The refusal names
  `--allow-mass-orphans` beside `--allow-empty` when the mass-orphan check
  would refuse the same run (ten files or more with no rows). A BACKGROUND sweeper gets no override (nobody is in the loop to
  express intent); the CLI ones take `--allow-empty`, and a sweep test pins that every
  `--gc` command offers it — which is how `artwork --gc` was found to have
  carried an un-escapable refusal since it was written.
- **Every walk of the artwork cache starts where the cache directory
  resolves, the size cap goes on over what it cannot list, and the GC keeps
  the thumbnails of an artist a track row names** (2026-09-29, backlog
  B64). The three walks (`artwork --gc`, its empty-store guard, the serve
  cap `sweepArtworkCache`) share `artworkWalkRoot`: `fsutil.ResolveLinks`,
  as `resolveSidecarRoot` resolves, removing by the WALKED path and
  reporting under the configured one (`artworkReportPath`). A cache
  directory that is a symlink or a Windows junction was one entry to all
  three: the GC removed nothing ("1 skipped") and the cap never evicted.
  **The guard and the GC resolve together, or not at all**: a guard that
  reads the link as one entry beside a GC that walks its target waves an
  empty store through over the whole cache
  (`TestArtworkGCEmptyStoreGuardResolvesALinkedCache`). A root that
  resolves to a file is refused (resolved, the GC would judge the target by
  its own name, and removed it in the control), and "" before anything
  resolves it. **The cap steps over a directory it cannot list, and a cache
  file it cannot stat**: it returned the first such error before evicting
  anything, so on a cache that is an ext4 volume's mount root it was never
  enforced and WARNed every 15 minutes. The filesystem's `lost+found` goes
  without a word (`IsFilesystemLostFound`); anything else is logged once
  when a streak begins, at most daily while it lasts, and once at Info when
  it ends. **Going on is sound only because the cap evicts oldest-first**:
  a pass that cannot see some files evicts a subset of what a pass over the
  whole cache would, never a file that pass would keep, and never evicts
  what it sees to make up for what it cannot
  (`TestSweepArtworkCacheEvictsOnlyWhatAWholeCachePassWould`), so its one
  error is staying over the cap, which it reports. That is the cap's
  argument, not a sweep's: a sidecar sweep decides from a ratio over the
  tree, which an unseen part can flip (`PartialWalkRefusal`). **The GC's
  keep set holds `manifest.ArtistThumbKey` of every `$.artistMBID`**
  (`artworkKeysInUse`), the key the enricher fetched the portrait under and
  the console files its thumbnails under; it held artwork keys alone and
  removed every artist thumbnail in `thumbs/`. From the store, never from
  the file name: keeping whatever is named like an artist's thumbnail keeps
  one whose artist no row names for ever. The empty-set refusal stays keyed
  on the ARTWORK keys (the covers are what it protects) and asks whether
  the walk would remove a file. A thumbnail under a 16-hex artworkVersion
  alias stays an orphan: the console resolves an alias before it derives
  anything (`TestAnArtworkAliasFilesItsThumbUnderTheResolvedKey`), so no
  build files or reads one. **The cap never evicts a cover a track row
  names, nor a portrait `artwork --gc` keeps** (2026-10-15, backlog B84,
  the owner's decision of 2026-10-14). It evicted the oldest `.jpg` whatever
  it was: an enriched track's MBID cover or portrait then answered the
  terminal 404 `no_image` for good (the enricher takes only rows at
  `enriched_at = 0`, which this must not reset), and a `local-` cover 202
  until the next scan wrote it again (`needsLocalArtworkRecovery`), to go
  again once it was the oldest. This sentence said a `local-` cover never
  comes back; measured on 67154340 it answered 202 after the pass and 200
  after the next scan. After the walk, and only over the cap, a pass reads
  the GC's own keep set (`readArtworkCapKeep`: `artworkKeysInUse` for
  covers, `artistMBIDsInUse` for `portraitVerdicts`) and evicts, oldest
  first, only what no row needs: a thumbnail (`thumbs/`, which the console
  derives again on the next request, `deriveThumb` → `manifest.EnsureThumb`),
  a cover or portrait no row names, any other `.jpg`. **Never evict on a
  keep set the pass could not read**: a failed store read, or no store,
  evicts nothing that pass and WARNs. **Never evict a file modified within
  `artworkCapGrace`** (`portraitGrace`, ten minutes, counted back from the
  pass's start): every writer puts its file down before its row commits,
  the enricher seconds to a few minutes before by its clients' timeouts,
  the scanner a batch of 500 rows, or the rest of the scan, before (its
  `local-` covers come back anyway). Left open, as in B85: a writer
  ADOPTING an old file (a cover already on disk, a new hard link to an old
  canonical) between its stat and its row's commit. **A pass that could
  not stat a portrait at the top keeps every canonical**
  (`artworkCapCandidates`), or a partial pass could evict a canonical a
  whole one keeps. When what rows name is more than the cap it evicts all
  the rest and stays over, with one WARN when the streak begins and at most
  one a day (`msgArtworkCapOver`, naming `artwork.cacheMaxBytes`), and one
  Info when it ends. Don't fold the
  thumbnails into the keep set (they are what the cap can always reclaim),
  and don't read the keep set before the walk (a row that commits during it
  is missed, and its cover can go)
  (`TestTheSizeCapNeverEvictsACoverATrackRowNames`,
  `TestTheSizeCapNeverEvictsAPortraitTheGCKeeps`,
  `TestTheSizeCapEvictsUnreferencedFilesAndThumbnailsOldestFirst`,
  `TestTheSizeCapEvictsNothingWhenItCannotReadTheStore`,
  `TestTheSizeCapKeepsAFileWrittenWithinTheGrace`,
  `TestTheSizeCapKeepsEveryCanonicalWhenAPortraitWentUnseen`,
  `TestArtworkCapSweeperWarnsOncePerStreakWhenTrackRowsNameMoreThanTheCap`,
  `TestSweepArtworkCacheEvictsOnlyWhatAWholeCachePassWould`).
- **`artwork --gc` collects the portraits of artists no track row names**
  (2026-10-14, backlog B85). The GC judged a file by its size suffix, so the
  portraits at the top of the cache, `artist-<mbid>.jpg` (what the console
  and `/v1/artist-image` read) and the name-hashed canonical
  `artist-name-<sha256>.jpg` the enricher fetches once per artist name and
  hard-links each MBID path to, were "skipped (non-cache file)" whatever the
  store said, and an artist whose tracks were removed or re-tagged kept both
  for ever. The portrait pass (`collectPortraits`, after the cover walk,
  which leaves top-level portraits to it) keeps an MBID portrait while
  `$.artistMBID` names it (the same set as the thumbnails; the enricher
  fetches a portrait only under the MBID it stamps on the track), and a
  canonical while a portrait that STAYS is the same file (`os.SameFile`,
  compared within one size): **never from the name** (the hash is one-way
  and hashes `artistImageQueryName`, which no row stores), and **against
  the kept portraits, not every portrait**, so a canonical whose last link
  goes in this run goes with it, in a dry run too. Where the filesystem
  refused the hard link, `linkOrCopy` copied, so that canonical shares no
  file and is removed: the MBID copy stays and serves, at the cost of one
  Deezer fetch if a new MBID of that name ever needs it. **The pass reads
  the artist set right before it runs, and never removes a portrait written
  in the last ten minutes** (`portraitGrace`; a pre-merge review): `artwork
  --gc` can run beside `bridge serve`, whose enricher writes a canonical and
  links it before the track row names the artist, so a set read before the
  cover walk, or a fresh file, would delete a portrait being served. Left
  open: a NEW hard link to an OLD canonical (its mtime is the canonical's)
  in the milliseconds between the link and the row's commit; run the GC
  with the bridge idle to rule it out. **An empty artist
  set removes no portrait without `--allow-empty`**: it keeps them with a
  line on stderr and the cover GC goes on (exit 0), since covers have their
  own refusal and a portrait comes back only through a fetch. Only the
  enricher's names at the top of the cache are portraits; anything else is
  skipped as before. **Which entries can be one is a list of kinds refused,
  never "is a regular file"** (`canBePortrait`: `fsutil.NotAFile`'s kinds,
  and a symlink, which the enricher never writes): on Windows a portrait
  OneDrive has dehydrated is `ModeIrregular` and is still the enricher's
  file. **A cancel stops the pass before its next removal**, and the run
  ends as the cover walk's interruption does (`artwork gc interrupted`,
  exit 1, no summary) (`TestArtworkGCRemovesThePortraitsOfAnArtistNoTrackNames`,
  `TestArtworkGCPortraitsDryRunRemovesNothing`,
  `TestArtworkGCKeepsEveryPortraitWhenNoTrackNamesAnArtist`,
  `TestArtworkGCPortraitCopyKeepsTheMBIDFile`,
  `TestArtworkGCLeavesWhatIsNoPortrait`, `TestArtworkGCSharedCanonical`,
  `TestArtworkGCKeepsAPortraitWrittenWithinTheGrace`,
  `TestCanBePortraitRefusesKindsNotIrregular`,
  `TestArtworkGCLeavesASymlinkNamedLikeAPortrait`,
  `TestArtworkGCPortraitPassStopsWhenCancelled`).
- **A sidecar walk passes over, at the WALK, every directory a library walk
  skips (`manifest.ShouldSkipDir`) but lost+found, by its NAME before it
  lists it** (2026-10-02, backlog B207). With `variantsDir` on its own
  volume, `.Trashes/<uid>/` and `.Trash-1000/` sit under the walk root; on a
  NAS share, the share's `#recycle` or `@Recycle` (files an operator deleted
  to get back), its `#snapshot`, `@Recently-Snapshot` or `~snapshot` (a copy
  of the whole tree per snapshot) and Synology's `@eaDir` beside every
  folder; on a Windows drive root, `$RECYCLE.BIN` and `System Volume
  Information`. The walks pruned dot-directories alone until then.
  Measured on main: `upscale --gc` unlinked a recycle bin's renditions, a
  snapshot's copies and the @eaDir metadata (exit 0), `analyze --gc` the
  recycle bin's waveforms, the background sweep emptied a recycle bin under
  the floor of ten and, beside a visible snapshot, refused every tick as a
  lost index; `bridge doctor` called a healthy bridge with visible
  snapshots one that lost its index; a tree holding renditions only in such
  a directory, or a link named `#snapshot`, read to the mount-loss probe as
  healthy; and a recycle bin this user cannot list made every walk partial.
  `integrity.SkipsDerivedTreeDir` (`skipsSidecarDir` until B260) is the one
  predicate, asked by `TakeSidecarInventory` and by `scanForRenditions` at
  both of its sites (a directory, and a link to one), so the forward sweeps
  and the probe behind the mount-loss and relocation guards cannot
  disagree, and by the artwork cache's walks (next bullet). **lost+found keeps its own rule**
  (`IsFilesystemLostFound`, the partial-walk bullet below: the filesystem's
  own at the top is not counted, a readable one is walked, a locked one
  deeper is unlisted); don't fold it into the prune, which turns that
  bullet's tests red. **A recycle bin is no rendition to the probe, though
  it proves the volume is mounted**: its files are not the tree's, the
  reading `.Trashes` already had, so a tree whose every rendition went into
  one (deleted by hand over the share) holds none and is refused as B223
  refuses any tree deleted by hand (`--allow-mass-delete` is the way past),
  where main took them for renditions and reaped the rows of a small
  catalog. **The cost, accepted**: in multi-root mode the tree's
  first level is a library root's BASENAME, which the scanner walks
  whatever its name, so a root named like one of these directories has its
  renditions passed over, as a dot-named root's always were (a library AT a
  recycle bin or snapshot listing; a set of names to spare would need the
  catalog, which the probe does not have). Gate on `d.IsDir()`: `SkipDir`
  returned for a FILE skips the rest of its parent directory and ends the
  sweep early. `filepath.WalkDir` does not follow symlinks, so a symlinked
  `.Trashes` needs nothing extra
  (`TestSidecarInventoryPassesOverTheDirectoriesALibraryWalkSkips`,
  `TestTheRenditionProbeCountsNoRenditionInADirectoryALibraryWalkSkips`,
  `TestOrphanSidecarSweeperLeavesARecycleBinAndASnapshotAlone`,
  `TestSidecarWalksListNoDirectoryALibraryWalkSkips`,
  `TestEveryGCSweepPassesOverADirectoryALibraryWalkSkips`,
  `TestDoctorVariantsIndexPassesOverASnapshot`).
- **The artwork cache's three walks pass over the same directories**
  (2026-10-14, backlog B260): `artwork --gc`, its empty-store guard
  (`artworkCacheHasOrphans`) and the serve size cap (`sweepArtworkCache`)
  ask `artworkWalkSkips`, which is `SkipsDerivedTreeDir` for a directory
  below the walk root. They pruned nothing by name until then: on a cache
  kept at a NAS share's root, the GC removed whatever the share's
  `#recycle` or `#snapshot` held that was named like an orphan, the guard
  refused an empty store over a recycle bin's covers, and the cap counted
  their bytes and evicted them, oldest first (measured on main, every
  shape). Nothing of the bridge's own is passed over: it writes covers at
  the top of the cache and in `thumbs/` alone, and its temp files are
  dot-named FILES. **The cache directory itself is walked whatever its
  name** (a cache named `.artwork`), and lost+found keeps
  `artworkUnlistedDir`'s rule
  (`TestArtworkGCPassesOverTheDirectoriesALibraryWalkSkips`,
  `TestArtworkGCEmptyStoreGuardPassesOverTheDirectoriesALibraryWalkSkips`,
  `TestSweepArtworkCachePassesOverTheDirectoriesALibraryWalkSkips`,
  `TestArtworkWalksWalkACacheNamedLikeASkippedDirectory`).
- **…and a forward sweep compares a walked file with its rows in ONE
  spelling, `integrity.KnownSidecarKey`: cleaned, NFC-composed, THEN
  lowercased** (2026-10-02, backlog B206). HFS+ stores every name
  decomposed whatever spelling created it (its own canonical
  decomposition: NFD but for a few ranges it leaves as they are, TN1150),
  and APFS keeps a decomposed name it is given (a tree copied from HFS+)
  while looking either spelling up, so a walk handed back a decomposed name
  where the row recorded the composed spelling the file was written by:
  `LocateSidecar`'s stat reached the file and the forward sweeps read it as
  an orphan. Every such spelling is canonically equivalent to the name, so
  NFC makes them one string, the ranges HFS+ leaves alone included. Measured on an
  hdiutil HFS+ image: the background sweep, `upscale --gc` and `analyze
  --gc` each unlinked a live file written by its row's own path, and `--gc`
  then reaped the row, so it was rendered again. **Every known set and the
  lookup go through it**: `KnownSidecarSet`, `TakeSidecarInventory`'s
  lookup and `analyze --gc`'s waveform keys. A set keyed any other way
  misses a row recorded decomposed (a library scanned from HFS+), because
  the lookup composes what it walks: with analyze's keys reverted, only that
  placement of `TestEveryGCSweepKeepsAFileItsRowSpellsInAnotherNormalization`
  goes red. **The composer is `manifest.NFCCompose`**, the one
  `unicode_lower` calls, never a second copy. **Compose BEFORE lowercasing**:
  `strings.ToLower` maps rune by rune and does not commute with
  decomposition (U+0130 lowers to "i", I+U+0307 to "i"+U+0307), so the
  other order keys one name two ways (`TestKnownSidecarKeyIsOneKeyPerName`).
  **A fold only makes more files known**, the safe direction for a sweep
  that unlinks the rest: where a name is looked up by its bytes (ext4, NTFS)
  a file spelt like a row's in the other form is kept until the reverse
  sweep reaps that row, whose path opens nothing; the mass-orphan ratio and
  the doctor's count read the walk's Known and Orphans, so they read fewer
  orphans. To reproduce the class, run a COMPILED test binary with TMPDIR on
  an `hdiutil create -fs HFS+` image (`go test` builds in TMPDIR too, and a
  small image fills: the linker's `ftruncate` failed with ENOSPC)
  (`TestTheKnownSetMatchesASidecarInEitherNormalization`).
- **No server-side transcoding, ever.** Conversion is offline; `/v1/download`
  serves bit-exact via `http.ServeContent`. The DSD → PCM renditions (PR #863)
  are conversion in exactly that sense — a sidecar the job pool built earlier,
  served as a file — never a decode in the request path.

- **Every consumer of `variantsDir` resolves it LIVE — the integrity
  watchers included.** `POST /api/upscale/variants-dir` is hot, and the
  enqueuer, coordinator and auto-optimize sweeper read `liveVariantsDir`;
  `VariantWatcher` and `OrphanSidecarSweeper` took the path at
  construction, so after a move the orphan GC walked the tree the operator
  had left while new sidecars landed where it never looked, and the
  mount-loss guard probed the wrong volume. Both constructors take a
  `func() string`. A root change also had to drop the sweeper's
  chunk-resume cursor, a position in ONE tree, until 2026-09-28, when the
  cursor went: every tick walks the whole tree (the "…and the background
  `OrphanSidecarSweeper`" bullet below). An empty answer is a
  refusal, taken before anything can resolve `""` to the working directory
  (the `ReapOrphans` bullet under Config; `WalkDir("")` itself only
  errors), and `TakeSidecarInventory`, the walker the sweep uses now,
  refuses it again. The Jobs chips gate on the INTERVAL, as the
  wiring does, not on `UpscaleStats()`, which is nil with upscale off while
  the watchers tick regardless. (#917) The orphan GC's chip also says when
  that sweep is refusing (2026-09-28, the Jobs-card bullet below), and the
  variant integrity chip when the watcher is (2026-09-29, the B131 bullet
  below).

- **A recorded sidecar path is a CLAIM about where the file was, never
  proof that it is gone.** `track_variants.sidecar_path` and
  `track_analysis.waveform_path` are ABSOLUTE, so a `bridge.db` copied to
  a host where the variants dir (or dataDir) has a different path reads
  ENOENT on every row while every file sits, byte-identical, at its
  source-mirrored place under the current directory. Field report
  2026-09-20: the boot sweep reaped all 10,248 rows (259.7 GiB of
  renditions) with NO log line — the mount-loss guard saw a healthy,
  full directory — and the auto-optimize sweeper re-rendered over 200
  good files before it was caught. **Every consumer that turns a missing
  recorded path into a deletion asks `integrity.LocateSidecar` first**
  (FIVE of them, and the enumeration has already been wrong twice: the
  three reapers `VariantWatcher.tick`, `upscale --gc`'s reverse sweep and
  the reactive reap in `serveVariant` via the cmd/bridge
  `variantStoreAdapter` — plus, since #959, `DELETE /v1/upscale/variants`,
  which unlinked the recorded path alone and read its ENOENT as
  already-gone, so a relocated catalog answered `deletedCount: 10248,
  freedBytes: 0` and stranded the tree where `--gc` then refuses it; and
  the serve reap's own MOUNT check, which the other two had and it did
  not, so an unmounted volume cost one row per PLAY), and **every forward sweep's known
  set carries the CANONICAL path beside the recorded one**
  (`integrity.KnownSidecarSet` — the orphan sweeper and `--gc`'s file
  walk; `analyze --gc` for waveforms), or a moved tree is 10k orphans to
  the file walk and gets UNLINKED, which is worse. `transcode.
  VariantSidecarPath` is the ONE layout (the pool, `variants move`, the
  probe); don't hand-copy the `Join(dir, Dir(rel), basename)` body.
  Relocated = canonical exists with the recorded `size_bytes` → adopt
  (`UpdateVariantSidecarPath`, no `indexed_at` bump, nothing on the wire);
  a size MISMATCH is a copy in flight → keep the row, adopt nothing,
  delete nothing (the serve lookup answers `api.ErrVariantSidecarUnavailable`
  → 410 without the reap); delete only when NEITHER location has the file.
- **A sidecar walk that feeds a deletion RESOLVES ITS ROOT, and reports
  under the configured one.** `filepath.WalkDir` `Lstat`s its root and
  follows no link, so a variants directory that is itself a symlink
  (`/srv/variants -> /mnt/vol/…`, the ordinary mountpoint alias) arrives
  as ONE non-directory entry and the walk ends — measured, not reasoned
  about. `upscale --gc` passes `SidecarInventoryOptions{}`, so `Consider`
  is nil and that entry is a candidate: the variants directory itself was
  counted, missed by the known set, and handed to the forward sweep to
  `os.Remove`. ONE orphan is below the mass-orphan floor, so no guard
  could see it, and `bridge doctor`'s `variants-index` probe reported it
  as reclaimable and named `--gc` in the hint. `TreeHoldsVariantSidecars`
  has resolved its root since #937; #940's shared walker — **the one that
  DELETES** — did not, so `resolveSidecarRoot` is now one body for both.
  It resolves a Windows junction too since 2026-09-29
  (`fsutil.ResolveLinks`; the junction bullet under **Scanner**).
  **Both halves are load-bearing**: walk the RESOLVED root so it descends,
  REPORT under the configured one, because `KnownSidecarSet` keys on the
  recorded `sidecar_path` and on `CanonicalSidecarPath(variantsDir, …)`
  and neither is symlink-resolved — emitting resolved paths would miss
  every key and classify a healthy tree as orphans, which is worse than
  the bug. **And the UNLINK goes by the WALKED path** (2026-09-28): each
  listed file keeps the path the walk visited beside it
  (`OrphanWalkedPaths`, `ScratchWalkedPaths`), and all three deleting
  sweeps (the background one, `upscale --gc`, `analyze --gc`) remove
  that. Unlinked through the configured spelling, a link repointed
  between the walk and the unlinks sent them into a tree the walk never
  counted, past the mass-orphan refusal, whose verdict was taken over the
  first tree (CodeRabbit on #1063, which named the background sweep;
  `TestAnOrphanSweepUnlinksInTheTreeItWalked` repoints the link in that
  window). An inventory whose two lists do not pair up is REFUSED before
  anything is removed (`SidecarInventory.CheckPaired`, the one check all
  three sweeps make; Gemini on #1063): falling back to the listed
  spelling is the defect, and indexing past the shorter list is a panic,
  which in the background sweep takes `bridge serve` down with it. This
  sentence said the panic was deliberate until the same review.
  `TakeSidecarInventory` cannot return such an inventory; the check is
  for a later change that builds or trims one. A dangling root is ENOENT
  and takes the missing-root reading; neither error branch may fall back
  to the unresolved path, which IS the
  defect. A symlinked SUBDIRECTORY is skipped rather than classified
  (unlinking it takes a subtree's only reference), and a symlink that
  cannot be STATTED is counted `Unreadable` rather than classified — "not
  there" and "could not find out" are different questions and only the
  first is junk. `analyze --gc` was never exposed: its `Consider` requires
  `analyze.WaveformExt`, `.waveform.bin` (this bullet said `.1bwf`, an
  extension nothing writes, until 2026-09-28). (#959, #1063)
- **A sweep that would reap more than `integrity.variantSweepMaxDeletePercent`
  (default 20, floor 10 rows) of the catalog while the tree still holds
  sidecar-shaped files is REFUSED, and every tick that saw rows logs one
  summary line** — `integrity.MassDeleteRefusal`, the one decision both
  reapers make; `--allow-mass-delete` is the CLI's way past it. **In `runGC`
  the guard is a PRE-FLIGHT over the whole gc, never inside the reverse
  half**: the forward sweep runs first, and for a tree copied into a
  layout the probe doesn't know it unlinks the very files that are the
  guard's evidence, then hands the reverse sweep a tree that "holds no
  sidecars" — the first draft's test went green while the files were
  already gone (`TestRunGCRefusesAMassDeleteUntilAllowed` now asserts on
  the FILES). The summary is Warn when it deleted, Info otherwise, a
  refused tick's included since 2026-09-29: its WARN is the refusal's,
  latched (the next bullet). Per-row lines are sampled at 10 per message
  per tick, the M-SEARCH lesson applied before the flood. `waveform_path` has the same
  shape and adopts too, by a narrower route: #954 wired
  `integrity.LocateWaveform` into `analysisStoreAdapter`, so a relocated
  curve rebinds on the first analysis lookup — served from canonical, row
  rewritten, no `indexed_at` bump, nothing re-decoded. There is no
  proactive waveform sweep, so it heals when the track is next asked for
  and not before, and the analysis skip gate still reads the row rather
  than the file — which is why a curve at NEITHER location stays silent
  and needs `bridge analyze --force`. `bridge doctor`'s `sidecar-paths`
  check reports both tables; the schema-relative follow-up is still #938.
  (#937, #954)
- **…and the watcher's refusal goes through the orphan sweep's latch**
  (2026-09-29, backlog B65). `VariantWatcher` logged the relocation
  refusal at WARN on every tick, beside a WARN summary: measured on a real
  serve at a 2 s interval over twelve rows whose sidecars were gone while
  the tree held one, fourteen WARN lines in 13 s (48 a day at the default
  hour) for a state that lasts until someone acts. **`refusalLatch`
  (internal/integrity/latch.go) is the ONE latch both background sweeps
  keep**, the M-SEARCH rule: one WARN when a streak starts or changes
  kind, at most one per `sweepRefusalRepeat` (a day) while it lasts, and
  one Info line (`no longer refusing`) from the first tick whose check
  proceeds. A refused tick still summarises, at Info (`refused=N`); a
  tick that DELETED still summarises at Warn. A tick that asked neither
  guard's question leaves the latch alone (a failed listing, a tick the
  shutdown stopped in its first pass; the mount-loss skip was one until
  B131, the next bullet); an EMPTY catalog ends a streak, since the rows
  it withheld are gone and a refusal after it must WARN at once, not a
  day later. **A third sweep that refuses takes a `refusalLatch` of its
  own kind type, never a copy of the fields.**
- **…and the mount-loss skip is the latch's second kind, and both of the
  watcher's refusals reach the Jobs card** (2026-09-29, backlog B131).
  The skip (`skipping sweep, variants dir unhealthy with rows in
  catalog`) WARNed on every tick and logged no summary: measured on a
  real serve at a 2 s interval over four rows and an empty variants
  directory, nine WARN lines in 16 s and 114 in under four minutes (24 a
  day at the default hour), for as long as the volume stays unmounted,
  while the "Variant integrity" chip read "on", as it did through a
  relocation streak. The skip is `VariantRefusalVariantsDir`
  (`variantsDirUnavailable`) now, beside `VariantRefusalRelocation`, in
  the one latch: one WARN per streak, with a hint (mount the volume, or
  point the variants directory at where the renditions are), at most one
  a day, an Info summary per tick (`skipped=true`), and one Info line
  when a tick passes both guards again, naming the kind that ended
  (`ended=`, which the orphan sweep's lifted line carries too). A streak
  that turns into the other kind WARNs at once. **Latched, not an hourly
  WARN, though an unmounted volume is the more urgent state**: every
  rendition download it breaks WARNs on its own (`variant sidecar
  missing, but the variants directory is unavailable; keeping the row`,
  a 410), and the card says it until it ends. **The latch publishes
  itself** (`RefusalStatus[K]`, `refusalLatch.status`) whenever a streak
  starts or ends, so a sweep cannot move it and forget the card; the
  orphan sweep published by hand. `VariantWatcher.Status` →
  `admin.Deps.VariantSweepStatus` (runServe) → `/api/jobs`
  `maintenance.variantIntegrityRefusal` (a key) and
  `variantIntegrityRefusingSince`, omitted while it is not refusing or its
  interval is off (`refusalOf`, both chips) → the line's "refusing" badge
  and a `hint warn` worded by `describeVariantIntegrityRefusal`, through
  the renderer the orphan GC's line shares (`renderMaintenanceLine`).
  `TestVariantWatcherLatchesItsVariantsDirRefusal`,
  `TestVariantWatcherSaysEachKindOfRefusalWhenItStarts`,
  `TestVariantWatcherStatusFollowsTheRefusalLatch`,
  `TestTheMaintenanceLinesSayWhenASweepRefuses` (the shipped renderJobCards
  under node, over the served payloads) and
  `TestServeReportsTheVariantWatcherRefusalOnTheJobsCard` (the wiring
  line; red alone with it nil).
- **…and the watcher asks again, BY IDENTITY, as each row reads as missing
  and before it deletes: the probe at the start of a tick says nothing
  about the rows after it** (2026-09-29, backlog B203). `VariantWatcher.tick`
  probed its variants directory once, before its first pass, so a clean
  unmount during the tick (the mountpoint reverts to a local directory)
  made every later row read as a rendition that is gone, the relocation
  check walked that directory and found no sidecars, and the tick deleted
  the rows: 39 of 40, through the real watcher. The tick keeps the probe's
  view of the directory (`VariantsDirBlock.Info`) and asks
  `VariantsDirChanged` as each row reads as missing (`classify`: the one
  verdict that leads to a deletion, so a tick whose rows are where they
  belong pays nothing, and a volume that goes and comes back inside the
  pass is seen if it is still gone when a missing read's check runs), and
  once more after `MassDeleteRefusal` and before pass
  two, since that check walks the tree after the last row is classified
  and a volume gone in between lets a relocation's deletions through. A
  change refuses the tick as the mount-loss kind (`variantsDirUnavailable`),
  deleting nothing, the rows it had found missing counted as refused; the
  next tick's own probe continues the streak. **Identity, never "does it
  LOOK unmounted" again**: the local directory need not be empty.
  **Read an identity kept for later from an open handle
  (`fsutil.DirIdentity`, or the handle the emptiness read uses), never from
  `os.Stat`**: on Windows `os.Stat` of a plain directory reads its volume
  serial and file index only when `os.SameFile` first asks, from whatever
  the path names then (measured on nomos: the first version, on `os.Stat`,
  deleted 39 rows there and passed on macOS;
  `TestOSStatLeavesTheWindowsIdentityToTheComparison` pins the premise). The
  delete handler's kept instance (`SidecarStoreState`, #968) was that
  `os.Stat` and compared as any directory later put at the path: it takes
  the probe's `Info` now, from the handle that read `Empty`, so the two are
  about one directory (a second lookup could take them from a directory
  swapped in between; CodeRabbit). **A test of a kept identity compares it with
  nothing before the change**: on Windows a comparison reads the lazy
  identity and fixes it at that moment, which passed the handler's test on
  the old line until the early comparison went. The unmount is stood in by
  moving the directory aside and making another at its path, from inside
  the tick's one call during pass one, an adoption (`mountHooks`):
  `TestVariantWatcherRefusesATickWhoseVolumeIsUnmountedDuringIt`,
  `TestVariantWatcherRefusesATickWhoseDirectoryIsReplacedByANonEmptyOne`,
  `TestVariantWatcherRefusesATickWhoseVolumeGoesAfterItsLastMissingRow`,
  `TestVariantWatcherRefusesATickWhoseVolumeWentAndCameBackDuringIt`,
  `TestSidecarStoreStateTellsTheDirectoryItKeptFromANewOne`,
  `TestDirIdentitySeesAnotherDirectoryAtThePath`. **Pass two
  asks `LocateSidecar` again just before each delete** (`classifyRow`, the
  one per-row verdict both passes use) and deletes only a row still at
  neither location: the checks see a change they observe and nothing
  between them, so a remount between one row's missing read and the check
  after it left that row's verdict stale with its sidecar back on the
  volume (CodeRabbit on #1127;
  `TestVariantWatcherKeepsARowWhoseSidecarIsBackBeforeItsDelete`). A row
  kept there is counted as pass one would have counted it. What stays open
  is a volume that is gone again at that recheck. A mountpoint that already
  held an entry when a tick STARTED was open here too, until the next
  bullet.
- **…and `upscale --gc` deletes a row only while the variants directory is still the directory it classified** (2026-10-12, backlog B224). `runGC` classified every row, then asked health again before the reverse sweep. A volume unmounted during classification onto a directory that still holds a rendition, with fewer than ten rows left to read as missing, passed the relocation floor and the health re-check, and the reverse sweep deleted those rows while their files sat on the volume. A volume that went and came back during classification matched again at the end, and only a check as each row read missing saw the gap. The Empty exception (`forwardRemoved > 0` or `--allow-mass-delete`) compared no identity, so a directory replaced after the forward sweep was one this run had emptied. `runGC` now takes `fsutil.DirIdentity` before `classifyGCRows`, re-checks it as each row reads missing (`integrity.VariantsDirChanged`, the watcher's check), and re-checks it in `gcCheckOutputDirBeforeReverseSweep` before the Empty exception. A changed identity refuses the sweep and deletes nothing. `TestRunGCRefusesASweepWhoseVolumeIsUnmountedDuringClassification`, `TestRunGCRefusesASweepWhoseVolumeWentAndCameBackDuringClassification`, `TestRunGCRefusesAnEmptyExceptionOnADirectoryItDidNotClassify`. The empty mountpoint the health check already refuses stays refused (`TestRunGCRefusesAnUnmountedVolumeWhoseMountpointHoldsNoRendition`). `optimize --gc` and `render --gc` call `runGC`.
- **…and the forward sweep unlinks only while the variants directory is still the one the sweep began on** (2026-10-12, backlog B336). `runGC` asked `integrity.VariantsDirChanged` as each row read missing and again before the reverse sweep. The forward sweep unlinks by the paths the inventory walked, and that check did not run before those unlinks. It now asks before `runGCForwardSweep` unlinks and before each unlink. A changed identity refuses: nothing further is unlinked and no catalog row is deleted. `analyze --gc` takes `fsutil.DirIdentity` before its inventory and asks the same check before it unlinks and before each unlink. `TestRunGCRefusesAForwardSweepOnAReplacedDirectory`, `TestRunGCRefusesAForwardSweepReplacedAfterTheFirstUnlink`, `TestAnalyzeGCRefusesASweepOnAReplacedDirectory`, `TestAnalyzeGCRefusesASweepReplacedAfterTheFirstUnlink`. `optimize --gc` and `render --gc` call `runGC`. No wire change.
- **…and the probe counts RENDITIONS, not entries: a variants directory
  holding none is what an unmounted volume looks like, whatever else it
  holds** (2026-09-29, backlog B223). `VariantsDirSweepBlock` called a
  directory healthy when it held any entry, and the local directory an
  unmount leaves holds what was written there while the volume was away
  (a Finder `.DS_Store`, a README, the folders a render makes before sox
  writes, which a failed render leaves): one watcher tick deleted all 40
  rows (measured, five shapes), and `upscale --gc` deleted all 40 after
  unlinking the `.DS_Store` as an orphan and reading the emptiness as its
  own work. The serve reap and the delete handler read the same probe.
  **One walk behind the probe and `TreeHoldsVariantSidecars`**,
  `scanForRenditions`: a rendition is a file `looksLikeVariantSidecar`
  names (regular, or a link to one) outside a directory the sidecar walks
  skip (a dot-directory until 2026-10-02, every `SkipsDerivedTreeDir` name since:
  the B207 bullet above); the filesystem's lost+found counts as nothing; a link to a directory, which
  it does not follow, keeps the directory healthy (it cannot say what is
  behind it); it reads each directory a batch at a time and stops at the
  first rendition (never `filepath.WalkDir`: a whole sorted listing per
  call, and the delete handler asks once per row that unlinked nothing);
  and it is ORDER-INDEPENDENT, a directory it cannot list noted and the
  walk going on, a rendition anywhere answering yes (the WalkDir form
  stopped at its first error). No rendition is Empty ("is empty" for no
  entry, "holds no rendition" otherwise); an error with no rendition
  refuses as unreadable. **The directory cannot tell a lost volume from a
  tree whose every rendition was deleted by hand, or from a fresh volume**,
  so all three are refused, and `bridge upscale --gc --allow-mass-delete`
  is the operator's way past (the background watcher has none): so the
  watcher mass-deletes only over a tree that still holds a rendition with
  the relocation guard disabled (`integrity.variantSweepMaxDeletePercent:
  100`), since a mass that leaves renditions is otherwise the relocation
  refusal and one that leaves none is this one, at any threshold. **Don't
  take a lost+found as proof the volume is mounted** (ext-only, stacked
  mounts, and the rule below makes it evidence neither way), so a fresh
  ext4 volume, which the sweeps reaped from 2026-09-28, needs the flag.
  **`upscale --gc` asks the probe BEFORE anything is classified or
  unlinked** (`gcRefuseUnavailableVariantsDir`), and its reverse guard's
  Empty exception counts the RENDITIONS the forward sweep's own unlink
  removed (`runGCForwardSweep`'s `renditionsUnlinked`): never every file,
  or a removed `.DS_Store` explains an unmounted volume, and never one
  whose unlink failed or found it already gone, since a volume unmounted
  between the inventory and the unlinks makes every unlink ENOENT
  (CodeRabbit on #1129); `--allow-mass-delete` waives
  the Empty refusal in both and nothing else. **The delete handler keeps a
  refusal for the rest of its request** until it next unlinks inside the
  store (`storeRefused`; a refusal only keeps rows), since a tree holding
  no rendition is read whole on every ask. Tests in eight files asserted the
  defect (a "healthy" directory holding `sidecar.flac`, a lone folder, the
  watcher fixtures' `decoy.flac`, a streak ended by removing the last
  sidecar); fixtures that stand for a mounted volume hold a rendition-named
  file. `TestVariantWatcherRefusesAMountpointHoldingNoRendition`,
  `TestRunGCRefusesAnUnmountedVolumeWhoseMountpointHoldsNoRendition`,
  `TestRunGCReapsATreeWhoseRenditionsWereDeletedByHandWhenAllowed`,
  `TestSidecarStoreStateReadsAMountpointHoldingNoRenditionAsUnavailable`,
  `TestTheVariantsDirProbeReadsLinksAndDirectoriesItCannotList`,
  `TestUpscaleDeleteAsksAnUnavailableStoreOnceUntilItUnlinksFromIt`.
- **…and the watcher deletes a row only while it is still the row it
  LISTED: the DELETE itself compares the columns a writer moves**
  (2026-10-02, backlog B204). The tick judges every row as `AllVariants`
  listed it, and deleted through `DeleteVariant`, which removes the row
  whatever it records by then. `bridge variants move --to X`, which the
  console's variants panel and `bridge doctor` tell an operator to run
  beside a serving bridge, rewrites rows the tick has listed, from a store
  of its own: a row moved after the listing reads as gone at both places
  the tick looks (its old path, and its canonical place under the variants
  directory, which is not X), arrives under the mass-delete floor, and was
  deleted with its file intact at X (the real watcher, the serve wiring's
  adapters and the real `moveOneVariant` on a second store: 6 of 6). The
  adapter deletes through `Store.DeleteVariantIfUnchanged`: one `DELETE
  … WHERE source_path AND variant_id AND sidecar_path AND size_bytes AND
  created_at`, in the transaction that bumps the parent
  (`deleteVariantRow`, the one body behind both deletes), so a write that
  commits between the listing and the delete keeps the row, from whichever
  process (`Store.mu` reaches no other). **Those three are what the
  writers move**: a move or an adoption rewrites `sidecar_path`, a render
  the whole row, `created_at` with it (the completion time in nanoseconds,
  so an unchanged source rendered again at the same path and size still
  compares as another row), and the path and size are what `LocateSidecar`
  judged. A miss writes nothing and answers `manifest.ErrVariantChanged`;
  the tick counts the row `changed` (the summary's attribute), logs it at
  Info, publishes nothing for it, and the next tick judges it as it is
  then. `VariantSnapshot.CreatedAt` carries the third column
  (`integritySnapshotsFromRows`). **Not a re-read before the delete**: a
  write between the read and the DELETE is the window again. **Nor a
  refusal of `variants move` while a bridge answers**: it would make the
  migration the console documents need downtime, and a probe is a guard,
  not mutual exclusion (the restore bullet under The CLI and the serve
  wiring). `DeleteVariant` keeps its unconditional delete for
  `DELETE /v1/upscale/variants`. The serve reap deletes through
  `DeleteVariantIfUnchanged` against the row it looked up (B251, two
  bullets down); the `upscale --gc` reverse sweep's went with B250.
  This docblock said the serve reap used the unconditional delete until
  B251, and that `DeleteVariant` had no production callers (#156), false
  since #209. `TestAVariantSweepDuringAMoveKeepsTheRowsTheMoveRelocated`
  (cmd/bridge; two hand-removed sidecars are the positive control),
  `TestVariantWatcherKeepsARowThatChangedSinceItsListing`,
  `TestVariantWatcherSaysNothingAtWarnWhenEveryMissingRowChanged`,
  `TestDeleteVariantIfUnchangedKeepsARowAnotherWriterChanged` (a case per
  writer and per compared column).
- **A move puts the file at the destination before it moves the row, and
  removes the source name only after the row points there** (2026-10-09,
  backlog B249). `moveOneVariant` renamed (or copied, then removed) and
  then updated `sidecar_path`. Between those the row named a path with no
  file, and the canonical place under the variants directory was not the
  destination, so a VariantWatcher tick that had already listed the
  catalog read the row as gone at both places and deleted it. B204's
  compare-and-delete cannot see that window: the row has not changed yet,
  so the listed snapshot still matches the DELETE. The backlog's stress
  (back-to-back ticks, a writer holding the store lock in 15 ms bursts)
  lost 1,425 of 6,000 moved rows on main af4f4569 and 122 on #1137's
  head. The suite proof is a hook between the second name and the row
  update that runs one watcher tick: on the rename-then-update path it
  deleted the moved row plus the two already-gone controls (3 of 40).
  Now the destination is a hard link of the source, or a copy with an
  fsync where the link fails (across volumes, where NTFS and every other
  filesystem refuse the link), the row is updated, and only then is the
  source name removed. The copy is a temp file in the destination's
  directory, fsynced, then renamed over the destination. The rename
  replaces the directory entry, so a destination that is a hard link of
  another live sidecar keeps that sidecar's bytes, and a crash mid-copy
  leaves the temp rather than a partial file under the final name. A
  crash leaves two copies, which the next run adopts, rather than a row
  whose file is already gone. **Same-file
  first**, `os.SameFile` before any link or copy: a case-only spelling,
  or a `--to` that is the variants directory through a symlink, is one
  file, and removing the "source" would remove the only copy. Judging
  SameFile after a link this call just made would skip removing the
  extra name. `TestAWatcherTickBetweenTheFileAndTheRowKeepsTheMovedRow`
  (the two gone rows are still deleted),
  `TestAMoveOntoTheSameFileUpdatesTheRowAndKeepsIt`,
  `TestAMoveToAnotherSpellingOfTheSameFileKeepsTheOnlyCopy`,
  `TestAMoveCopiesWhenTheLinkCannotBeMade`,
  `TestAMoveOntoAHardLinkOfAnotherSidecarLeavesThatSidecarsBytes`.
- **…and the serve reap deletes the row it looked up** (2026-10-09,
  backlog B251). `serveVariant` looked the row up, opened the path that
  lookup captured, and on ENOENT deleted by source path and variant id.
  A move that rewrites the row between the lookup and that delete takes
  the moved row: the open still fails on the old path, and the
  unconditional delete matches the new row. The reap now deletes through
  `DeleteVariantIfUnchanged` against the snapshot the lookup returned
  (sidecar path, size, completion time), filled with the request's
  source and variant when the record left them empty. A miss
  (`api.ErrVariantChanged`, the store's sentinel translated at the
  wiring) publishes nothing and still answers 410
  `variant_missing_on_disk`. An unchanged missing row is still deleted
  and published. `DELETE /v1/upscale/variants` stays on the unconditional
  `DeleteVariant`: that request names the row it means to remove.
  `TestADownloadWhoseRowMovesBeforeTheReapKeepsIt`,
  `TestADownloadOfAMissingSidecarStillDropsTheRow`.
- **…and so does `upscale --gc`'s reverse sweep** (2026-10-02, backlog
  B250). `runGCReverseSweep` deleted the rows its classification judged
  missing through the unconditional `DeleteVariant`, so a row a `bridge
  variants move` relocated after the run's listing (gone, as listed, at
  both places the run looks) or one a render rewrote after the
  classification was deleted on a verdict about the row it used to be:
  through the real `runGC`, with the real move on a second store, the six
  moved rows of forty went with their files intact at the destination, and
  a re-rendered row went from under its new rendition. It deletes through
  `DeleteVariantIfUnchanged` with the row as listed
  (`deleteMissingGCRows`); `ErrVariantChanged` is counted as `changed`
  (the summary's clause, and a `keep … the row changed` line), is no
  failure (a cron'd `--gc` beside a move is a healthy state, as a
  mismatched row is), and the next run judges the row as it is then.
  `runGC` takes its store through `gcVariantStore` and `runAnalyzeGC`
  through `analysisRowLister`, the interfaces of what each uses, so a test
  can act between the listing and the sweeps (`gcListThenAct`,
  `gcDeleteThenAct`). `TestAGCRunDuringAMoveKeepsTheRowsTheMoveRelocated`,
  `TestAGCRunKeepsARowARenderRewroteAfterTheRunJudgedItMissing` (each with
  a positive control: a row gone at both places is still deleted).
- **`sidecar-paths` counts RECORDED PATHS and stats nothing, so it must not
  be described as a list of files that are gone** (#972).
  `CountVariantsNotUnderPrefix` / `CountWaveformsNotUnderPrefix` are pure
  SQL, so a row whose file is still AT the path it records is counted too —
  served from there, never relocated, in the count forever, which is the
  ordinary shape of a dataDir change with the old tree still mounted. The
  waveform hint said "Rows still listed afterwards point at curves which are
  NOT there" and named `bridge analyze --force`: hours of decoding to
  rebuild curves that already play, for an operator who had just done what
  the same hint told them to. Say what the number MEASURES before saying
  what to do about it, and scope `--force` to a curve at NEITHER location.
  The variants half carried the same false claim beside a remedy that was
  already right — which is how the next change gets made on the same
  reasoning.
- **The delete handler LOCATES before it judges the volume, and only the
  already-gone path asks** (#968). `SidecarStoreState` answers "is a MISSING
  sidecar evidence about the file or about the volume" — a question about a
  file that is missing. Asked per row BEFORE the locate it also answered for
  rows whose file had just been found, and an EMPTY variants directory is
  one of its unavailable reasons: a directory repointed at a fresh folder
  with every rendition still at its recorded path made "delete all
  renditions" unlink nothing and leave the bytes, which is #959's own
  stranding one commit later. An empty recorded path is still asked — 
  `locateRecordedFile` stats `""` (ENOENT everywhere) and then the canonical
  path, which is exactly where a file with no recorded path would be found.
- **EMPTY is explicable only by an unlink from the SAME directory
  INSTANCE.** Two things had to be narrowed, both found by review. A
  recorded path can name the OLD tree, so unlinking there explains nothing
  about the current directory (`VariantSidecarLocation.WithinStore`,
  `filepath.Rel` containment so `/srv/variants-old` is not a child of
  `/srv/variants`). And a clean unmount reverts a mountpoint to an empty
  LOCAL directory, so an unlink at row k proves the volume was mounted at
  row k and says NOTHING about row k+1 — which is why the probe is per row
  in the first place. `SidecarStoreIdentifier` is opaque because identity is
  device+inode on POSIX and volume+file index on Windows, which
  `os.SameFile` compares and no exported type carries as a value; nil or
  foreign is NOT the same, because the compare exists to refuse an
  unmount. **Portably only for an identity read from an open handle**
  (`fsutil.DirIdentity`): this bullet said "answers portably" of the
  `os.Stat` the handler kept, and on Windows that stat's identity is read
  when it is first compared, from whatever the path names then (the B203
  bullet below). Captured on the FIRST in-store unlink only — a re-probe per row
  puts a stat on the happy path of a whole-library delete and a differing
  instance is refused by the comparison anyway. Missing, unreadable and
  not-a-directory still refuse however much was unlinked: the loop removes
  files and never directories, so it cannot be what took the root.
- **A sidecar walk tests "not a REGULAR file", never "is a symlink"** (#969).
  Since Go 1.23's `winsymlink` change a Windows directory JUNCTION
  (`mklink /J`, `IO_REPARSE_TAG_MOUNT_POINT`) gets `ModeIrregular` and is
  denied `ModeDir`, so it reports `IsDir()` false with NO symlink bit, fell
  through every arm in `TakeSidecarInventory`, and `upscale --gc`'s nil
  `Consider` unlinked it as an orphan file — taking the only reference to an
  album parked on another volume, which is the ORDINARY way to do that on
  Windows (a real symlink needs a privilege a service account lacks). One
  junction is below the mass-orphan floor, so no guard could see it.
  **`os.Lstat` is the wrong probe here**: it does not follow the link, so
  `IsDir()` is false for a link TO a directory and the skip never fires —
  proposed in review, and it turns two tests red. The decision is taken as
  `(mode, stat)` so the Windows shape is driveable on any platform; a plain
  file answers without a stat, and the closure does not escape (measured).
- **`SidecarInventory.Unreadable` is a count of ENTRIES, and every place
  that prints it says so.** It covers a directory the walk could not
  descend into AND a non-regular entry it could not stat. `upscale --gc`
  and `analyze --gc` print that one count from two places and both called
  it directories; a wording fix reaching one of them is the enumeration
  failure this file keeps recording, and it happened: `bridge doctor`'s
  variants-index summary said "director(y/ies)" until 2026-09-28. The two
  kinds are counted apart now (`UnlistedDirs` is the directories), because
  they bound different things: a directory the walk could not list may
  hold any number of files, a link it could not stat at most one (the
  partial-walk bullet below).
- **A forward sweep's denominator is the TREE, never the catalog, and the
  term that knows a lost index is `orphans > rows`.** Adoption and the
  canonical known set both need the ROWS; the 2026-09-20 aftermath had
  none — the boot sweep had dropped all 10,248 and the auto-optimize
  sweeper had written 200 fresh ones over the stranded tree, so
  the empty-catalog refusal (`gcRefuseEmptyCatalog` now; "is the catalog
  EMPTY?") said no, `gcRefuseRelocationInProgress` ("how many ROWS lost their file?")
  said none, and `--gc` unlinked 10,048 files at exit 0.
  `integrity.MassOrphanRefusal` is the one decision both file-deleting
  sweeps make: the same floor of ten, more than
  `variantSweepMaxDeletePercent` of the FILES (one knob, same meaning at
  both ends — leaving the unrecoverable half on a second number is a
  trap), AND more unreferenced files than the catalog has rows in total.
  That last term is what passes an ordinary crop — a naming-scheme change
  leaves one old file per CURRENT row, an interrupted bulk delete one per
  DELETED row — and it is load-bearing for `--allow-mass-delete` too,
  where `orphans == rows` must proceed. Unlike the reverse twin it is NOT
  gated on `TreeHoldsVariantSidecars`: there the probe tells a relocation
  from a deletion, here the files are the evidence. **The walk is split
  from the unlink** (`integrity.TakeSidecarInventory` classifies, the
  sweep removes the list it is handed) for #937's own reason one layer
  up — a guard inside the walk measures a ratio from the tree it has
  already destroyed, so the test asserts on the FILES.
  `--allow-mass-orphans` is the CLI's way past it on upscale / optimize /
  render / analyze (and `--allow-partial-walk` past the refusal of a walk
  that could not list part of the tree, below); `artwork --gc` is exempt
  BY NAME in the sweep test (content/MBID-keyed, no absolute path a
  relocation can strand). **So `artwork --gc` steps over a directory it
  cannot list** (2026-09-28): its verdict about a file is that file's name
  against the keys, with no count over the tree for an unseen part to
  flip, so it names the directory on stderr, counts it on the summary,
  exits 0 unless a removal failed, and steps over the filesystem's
  `lost+found` (`IsFilesystemLostFound`) without a word. It stopped at the
  first such directory with exit 1 and no summary, after removing the
  orphans that sort ahead of it; a cache root it cannot read still fails.
  #940 left
  the background `OrphanSidecarSweeper` for its own change, which is the
  next bullet. `analyze --gc` shares both
  halves and gained the dot-directory prune and a fail-closed walk error
  with them; its `.tmp` scratch is removed whatever the catalog says and
  kept OUT of the ratio, or a crashed run trips the guard on the next one.
  (#940) "Unconditionally", this said until backlog B205: the scratch
  takes the forward sweeps' grace now, like an orphan (the B205 bullet
  below).
- **…and the background `OrphanSidecarSweeper` makes the same decision
  every tick, with no override** (2026-09-28). It unlinked INSIDE a walk
  chunked at 5,000 entries, with a cursor across ticks, and its only guard
  was the empty-known-set one, so #940's own shape (200 rows over 10,048
  stranded files) went in three ticks on the old code: 4,800, 5,000, 248.
  Each tick now lists the catalog, takes the whole tree's inventory
  (`TakeSidecarInventory`, read-only, a `.flac` Consider, NO `MaxEntries`),
  refuses on `MassOrphanRefusalFor` of the FULL `Orphans` count, then on
  `PartialWalkRefusal`, and only then unlinks up to `gcChunkSize` files,
  each path re-checked first (`reclaimOrphan`: a fresh Lstat, the
  inventory's own `classifyWalkEntry`, the grace against the tick start).
  **The chunk caps SUCCESSFUL unlinks, never the walk and never the
  attempts** (2026-09-28): a tick keeps `gcRetainedPerUnlink` (4) chunks of
  paths and tries them in walk order until it has unlinked a chunk. It
  kept one chunk until then and every attempt spent a slot, so a chunk's
  worth of files the service user cannot remove at the head of the walk (a
  root-owned directory a `sudo bridge upscale` left) stalled every orphan
  behind them, every tick, where the old cursor had moved past them
  (measured with a chunk of 5: eight in a read-only directory ahead of ten
  deletable ones, four ticks, nothing unlinked). 20,000 retained paths
  measured 10.8 MB of heap at 205-byte paths, freed with the tick; a head
  of 20,000 or more stalls again, and the summary's `retained` beside
  `failed` shows it. **Nothing about a failure crosses a tick either.** The
  refusal reads the full count, never the retained list: fed its length,
  400 retained against 500 rows proceeds where 1,000 orphans refuse (the
  full-count test, whose fixture had to grow from 150 rows when the list
  did, or it bit only through its wording). The walk costs 128 ms per
  100,001 files warm on the dev Mac, every tick. **No verdict crosses a tick**: a tally across
  ticks was rejected, because its verdict can come from a partial walk (an
  unmount, a cancel, a pruned root) or a catalog that changed mid-pass, and
  a budget carried between passes can be spent on files it never counted.
  The one cross-tick state is a LOG latch (`refusalLatch`, which
  `VariantWatcher` shares since 2026-09-29), the M-SEARCH rule: the refusal
  WARNs once when a streak starts and at most daily while it lasts; a tick
  that decided nothing (a failed or stopped listing or walk) leaves the
  latch alone; the first tick that proceeds after it logs one Info line.
  An empty catalog is a verdict, not a tick that decided nothing: the
  third kind, under the Jobs-card bullet below. Its hint never names
  `bridge variants move` (it needs the rows a lost index lacks). **A stranded tree no larger than the
  catalog is still reaped**, exactly as `--gc` reaps it: the refusal needs
  its floor, `orphans > rows` AND the ratio. The shared walker changed one
  behaviour on purpose: a SYMLINKED variants directory is walked now (the
  old WalkDir Lstat'd the link and swept nothing). The `.flac` Consider
  stays, so the ratio is over the files this sweep would remove. The variants
  sweeps weigh renditions only (`ConsiderVariantSidecar`, backlog B132);
  until then `upscale --gc`'s nil Consider counted and removed every file.
  **A walk
  that could not LIST a directory refuses too** (`PartialWalkRefusal`),
  after the mass-orphan check and under a WARN of its own (CodeRabbit on
  #1063). What it could not list is missing from every count and may hold
  any number of orphans, so a verdict that proceeds over the part it saw
  can be a refusal over the whole: 1,000 stranded files behind a locked
  directory and 15 in view, against 20 rows, pass the check, and the
  review head unlinked the 15. The inventory's docblock said an unreadable
  entry "can only make the deletion set SMALLER", which is true of the
  list and false of the verdict. The latch keys on the KIND of refusal, so
  a streak that turns into the other kind logs at once. **The cost is
  deliberate**: a variants directory holding a directory the bridge may
  never list reclaims nothing until it is readable, and its hint says so
  once a day. The ordinary such directory, the root-owned `lost+found` of
  an ext4 volume mounted AS the variants directory, is the filesystem's
  and not counted (next bullet), and a link the walk could not stat is
  weighed, not refused. The CLI sweeps make the same refusal since
  2026-09-28 (next bullet).
- **…and a partial walk is refused by every forward sweep, weighed by what
  it can hide** (2026-09-28). `upscale --gc` (optimize, render) and
  `analyze --gc` took `MassOrphanRefusal` over the part of the tree they
  could read and went on: over #1063's shape both unlinked the 15 and
  exited 0. The inventory splits its unknowns, and the three sweeps and
  the doctor read them through two shared decisions. **A directory the
  walk could not list** (`UnlistedDirs`) is unbounded, so
  `PartialWalkRefusal` refuses the verdict, after `MassOrphanRefusalFor`,
  whose lost-index advice is the more urgent. The CLI's way past it is
  **`--allow-partial-walk`, which waives that refusal and nothing else**:
  the mass-orphan check still runs over what was read. Never
  `--allow-mass-orphans` as the way past it, which gives up the whole
  protection to get past one directory; but `--allow-mass-orphans` waives
  the partial-walk refusal too, since that refusal exists only to protect
  the verdict the flag has set aside. So does a threshold of 100, where no
  count can refuse. **A link the walk could not stat** is never walked
  into, so it is nothing, one known file or one orphan, and
  `MassOrphanRefusalFor` counts it as an orphan (o+k of f+k): a known file
  only lowers the ratio and an orphan only raises the refusal (100·(o+1) >
  pct·(f+1) follows from 100·o > pct·f for pct ≤ 100), so that refuses
  exactly when some reading would, pinned against every reading by brute
  force (`TestMassOrphanRefusalForWeighsWhatTheWalkCouldNotStat`). **The
  root-owned `lost+found` of an ext4 volume mounted AS the variants
  directory is not an unknown**: a directory named exactly that, directly
  under the RESOLVED walk root, that a PERMISSION error keeps this user
  out of, is not counted (`IsFilesystemLostFound`). Since #1063 it made
  the background sweep refuse every tick and `bridge doctor` warn on every
  run. Nothing the bridge writes can be in it: a `<variantsDir>/lost+found`
  the bridge made (a single-root library's top-level folder of that name,
  or a multi-root root whose basename it is) is its own and listable, so
  it is walked as before, and so is any readable one (a CLI run as root).
  **Residuals**: a render run as ROOT for a library folder named
  `lost+found` could put sidecars in one the service user cannot list
  (unexamined, as the filesystem's); a volume mounted deeper in the tree
  has its `lost+found` counted, since nothing about the walk root vouches
  for it; an I/O error on it counts. **The reverse guard's probe reads it
  by the same rule** (2026-09-28; `IsFilesystemLostFound` is exported for
  it and for `artwork --gc`). `TreeHoldsVariantSidecars` returned that
  directory's permission error whenever no sidecar sorted ahead of it, so
  on a fresh volume mounted as the variants directory `MassDeleteRefusal`
  refused to reap rows whose sidecars really went, on every
  `VariantWatcher` tick and in `upscale --gc` (exit 1, "could not be
  read"), and it refused a tree whose sidecars sort after it for the
  error rather than for the sidecars. It is evidence neither way there
  now; any other directory the probe cannot list still fails it closed.
  Since backlog B223 a fresh volume holding only its lost+found holds no
  rendition, which the mount-loss probe refuses before `MassDeleteRefusal`
  is asked (the B223 bullet above): its rows go with `--allow-mass-delete`.
  A Gemini consult was attempted for the `lost+found` trade-off and
  refused by the API's spending cap; the rule is the narrow one, decided
  here.
- **…and a CLI forward sweep removes a file only once it is older than
  the grace, asked again just before its unlink: `--gc` is safe beside a
  running bridge** (2026-10-02, backlog B205). `upscale --gc` (optimize,
  render) and `analyze --gc` list the catalog first and judge every file
  by that listing, and `bridge doctor`'s variants-index hint names `--gc`,
  so it runs beside a serving bridge, whose pools keep writing: a render
  renames its rendition into place and then commits the row, an analysis
  the same with a waveform, each writing a `.tmp` first. Both sweeps
  unlinked what their walk found and the listing did not name, with a bare
  `os.Remove`. Measured through the real `runGC` and `runAnalyzeGC`, with
  a store whose listing hook does what the live bridge does then: a
  rendition published after the listing, one renamed into place a minute
  before the run whose row committed after the listing, and a `.tmp`
  still being written were all unlinked (a row with no file; the render
  still writing fails at its rename and strikes a good file), and the
  waveforms likewise, which nothing brings back but `bridge analyze
  --force` (the skip gate reads the row). The review's scratch test left
  11 of 21 renditions published during a run with a row and no file.
  Both sweeps now hand each listed path, by its walked spelling, to
  `integrity.ReclaimOrphan`, the background sweep's re-check, exported
  with `OrphanOutcome` and `OrphanGracePeriod` (the old `gcGracePeriod`),
  so the three forward sweeps decide alike (`gcReclaimTally.reclaim`): a
  fresh Lstat, the inventory's `classifyWalkEntry` (a link to a directory
  put there since is left alone, and a directory in the list is no longer
  removed), and **no file modified less than `OrphanGracePeriod` (10
  minutes) before the run started**, sampled BEFORE the catalog listing,
  since the classification and the walk after it take minutes on a large
  tree over a network mount. **The `.tmp` scratch takes the grace too**:
  `analyze --gc` removed its scratch unconditionally, and until backlog B132
  `upscale --gc` (nil Consider) removed every `.tmp` as an orphan. A `.tmp`
  is not a rendition, so the variants sweep now leaves it; the grace is what
  still protects a rendition a job is writing, whose mtime moves with every
  write. A recent file the sweep does take is no failure:
  the summary counts it (`left N recent file(s)`) and one line says why.
  Gone is still a success; an unlink the filesystem refuses, or a path the
  re-check cannot stat, is still a failure. **The refusals still weigh
  every file the walk found**, recent ones included, so a batch rendering
  into a small catalog during the run can make it refuse, which unlinks
  nothing. **Preferred to refusing while a bridge answers** (`probeBridge`,
  as `restore` and `clear-missing` do): the shapes a live bridge makes are
  closed this way, a probe narrows a window and closes none (the restore
  bullet under The CLI and the serve wiring), and the doctor sends
  operators to `--gc` on a live install. **Residuals**: a writer that has
  written nothing for the whole grace, which is a DSD render, whose `.tmp`
  the bridge creates before Stages A and B and the album survey (as root
  only until B211, on POSIX since; unlinking it costs nothing: Stage C's sox
  creates the file again, without the owner it was created with as root),
  and a render stalled on
  a hung mount for ten minutes, whose rename then fails; and sampling the
  start BEFORE the listing is not pinned (no test can stretch a listing
  past the grace). The fixtures that stand for what a lost index or a
  crash left date their files an hour back (`ageFiles`, `writeAgedFile`),
  and a direct test of a sweep helper passes a start an hour ahead
  (`gcStartAfterTheGrace`, the integrity tests' convention).
  `TestAGCRunBesideALiveRenderKeepsWhatTheRenderWrites`,
  `TestAnAnalyzeGCRunBesideALiveAnalysisKeepsWhatTheAnalysisWrites` (each
  with a crashed job's leftovers an hour old as the positive control),
  `TestAGCForwardSweepCountsAnUnlinkTheFilesystemRefuses`.
- **The Jobs card shows the background orphan sweep's refusal**
  (2026-09-28). The "Orphan sidecar GC" line read "on" whenever the
  interval was positive, while every tick refused and only the journal
  said so, once a day. The sweep publishes its latch through an atomic
  snapshot (`OrphanSidecarSweeper.Status`: the kind,
  `integrity.OrphanRefusalKind`, and when the streak started; the latch
  publishes itself since 2026-09-29, when the variant watcher's chip
  joined, the B131 bullet above), runServe
  wires it into `admin.Deps.OrphanSweepStatus`, and `/api/jobs` carries
  `maintenance.orphanSidecarGCRefusal` (a KEY the console words) and
  `orphanSidecarGCRefusingSince`, omitted while the sweep is not refusing
  or not running. The card shows a "refusing" badge on the line and the
  reason in a `hint warn` under the list: a sentence in a list cell wrapped
  into a 163 px column 170 px tall at 375 px (measured in a browser).
  `TestEveryOrphanRefusalKindIsWorded` runs `describeOrphanGCRefusal`
  under node for every kind, because the leaf guard proves the key is READ,
  not that it is WORDED; `TestServeReportsTheOrphanSweepRefusalOnTheJobsCard`
  boots serve and is the only test that sees the wiring line (a new kind
  also joins `OrphanRefusalKinds`: the `/api/jobs` guard bullet under
  **Admin console**). **The empty catalog is the third kind**
  (`emptyCatalog`, 2026-09-28): it WARNed on every tick outside the latch
  (five lines in eight seconds at a 2 s interval, on a real serve) while
  the chip said "on". It is decided from the tick's inventory, ahead of
  the mass-orphan check (which would refuse most of the same trees, but
  none under its floor of ten): it refuses when the walk found a sidecar
  file it would remove, or an entry it could not stat, weighed as one
  (`emptyCatalogRefusal`, over `integrity.EmptyCatalogOrphans`, the rule
  the CLI `--gc` sweeps share since 2026-09-29); a tree with nothing it would remove ends a
  streak and is otherwise quiet; a directory the walk cannot list, with
  no file in view, is the partial walk's refusal. **Don't decide it from
  `dirIsEmpty`**, main's question and this change's first draft: over a
  directory left holding empty folders, a `.DS_Store`, a Trash or the
  filesystem's `lost+found`, main WARNed "holds files" on every tick, and
  the draft kept the card refusing forever (seen in a browser).
- **`bridge doctor`'s `variants-index` is the other side of
  `sidecar-paths`, and its walk is BOUNDED.** `sidecar-paths` counts rows
  recorded outside the current directory (a relocation the sweeps heal);
  this one counts FILES no row mentions (a relocation nothing can heal —
  the evidence that would connect them is what went missing). It reuses
  `KnownSidecarSet` + `TakeSidecarInventory`, which is what keeps it quiet
  on a merely relocated catalog, and takes its "the sweep would refuse
  this" wording from `MassOrphanRefusal` rather than restating the rule.
  `/api/doctor` is fetched on every settings-page render, so the walk caps
  at 20,000 TRAVERSED entries — directories and ignored files included,
  never just the candidates, or the cap bounds nothing on a tree of
  either and the guarantee holds only for callers that happen to pass a
  nil `Consider` (measured: 110 ms unbounded over 100,001 files in 111
  directories, 21 ms at the cap; ~1 s at the cap on the sweeper's
  recorded 50 µs/entry pathological tier) — and SCOPES its claim when it
  truncates: the
  all-clear is what gets scoped, never the alarm, because a lost index has
  orphans throughout. **The probe reports the budget it ran under**: a
  closure wired with 0 walks the whole tree on a page render and every
  count still looks right, which is the one mistake no number reveals.
  (#940) **Its hint said `--gc` "measures the whole tree"**, false past a
  directory it could not list; since 2026-09-28 it says `--gc` refuses
  such a walk (`GCRefusesPartialWalk`, sound on a truncated prefix, since
  a directory this walk could not list is one the whole walk cannot list
  either), names directories and links apart, and withholds the ratio
  verdict only for an unlisted directory: a link is weighed as `--gc`
  weighs it.
- **A guard that reads the world AFTER a sweep has to be told what the sweep
  did.** `gcCheckOutputDirBeforeReverseSweep` reads a missing-or-empty variants
  directory as a lost mount — right, except that on the legacy hash-flat layout
  (`<dir>/<hash>-<variantID>.flac`, no subdirectories) the FORWARD sweep runs
  first and can remove the last file, so it refused on a state that run had
  just created. No re-run cleared it — the directory was still empty, so it
  refused again having removed nothing — and **no flag reached it either**
  (measured: `--allow-mass-delete`, `--allow-mass-orphans` and `--allow-empty`
  all still wedged; only `mkdir "$dir/.keep"` got past — a plain `touch` did
  NOT, because the `--gc` inventory passed a nil `Consider`, so a dot-FILE was
  unlinked as an orphan while only dot-DIRECTORIES are pruned at the walk).
  Those rows could not be reaped by `--gc` at all, and `manifest clear-missing`
  is not a route past this refusal. Since backlog B132 the sweep's Consider
  is renditions only, so a `.keep` file is left where it is and that
  workaround is no longer needed; a nil Consider would still unlink the
  file, which is why the history stays. `ClearMissingCounts` deletes tracks with `missing_count > 0`,
  and with them the variant and analysis rows that cascade and their files. It
  does not delete the variant rows of tracks that are still present, so it
  never supplies the reverse sweep's removed count. The source-mirrored layout hid it for years: `WalkDir`
  removes no directories, so an emptied subtree still leaves dirents behind.
  It now takes the forward sweep's `removed` count and skips the refusal for
  **EMPTY alone**; missing, unreadable and not-a-directory still refuse however
  much was removed, because the forward sweep unlinks files and never
  directories (`TakeSidecarInventory` hands it none), so it cannot be what took
  the root — something else did, mid-run, which is the hazard.
  `integrity.VariantsDirSweepBlock` is the typed answer and
  `VariantsDirSweepBlockReason` delegates to it, so the two cannot drift. The
  serve-time `VariantWatcher` has the same shape one layer over —
  `OrphanSidecarSweeper` can empty a flat directory under it — and is
  deliberately left: it removes nothing itself, so it has no count to be told,
  and the CLI is the repair tool. (#941) **Since backlog B223 the count is of
  RENDITIONS unlinked, and `--allow-mass-delete` reaches this refusal**:
  "empty" means holds no rendition, and a count of every file let a removed
  `.DS_Store` explain an unmounted volume (the B223 bullet above).

## Auth, pairing, TLS and security posture

- **The disk probe's missing-directory warning names the directory by
  its role, never by its path** (2026-10-13, backlog B273).
  `AvailableDiskSpaceNearest` walks to the nearest existing ancestor
  when the directory is not there yet and Warns, so an operator can see
  the check is grading the parent volume. That line carried the absolute
  directory and the ancestor (`"dir"`, `"ancestor"`). It keeps the same
  message and logs `role` only: `ProbeRoleVariants`, `ProbeRoleScratch`,
  `ProbeRoleData`, `ProbeRoleLibrary`, `ProbeRoleUpload`; an empty role
  is `"directory"`. Callers that probe on every request keep
  `NearestExistingDir` and log nothing (`quietDiskFree`). The platform
  probes' returned errors still quote the path they failed to stat;
  they are not this line. The privacy page is unchanged.
  (`TestAShortRenditionWhoseScratchIsGoneNamesNoAbsoluteDirectory`.)
