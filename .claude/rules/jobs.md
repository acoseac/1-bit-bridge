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

> **Condensed 2026-10-18.** Each long entry here is one line — its bold title plus its own bolded "Don't…/Never…" sentences — ending `→ History-rules-jobs.md`: that is `ops/History-rules-jobs.md`, where the entry sits verbatim with its reasoning, measurements and rejected alternatives. **Still binding.** Edit the line here and its long form there in the same commit; new long-form detail goes to that file, never back into this one.

The transcode and analysis pools and their gates, the PCM and DSD renditions, the rendition stamps and staleness, and the sidecar, variant and artwork reapers that delete what the jobs wrote.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Job pools — upscale, optimize, analysis

- **THREE CLI commands feed the same classifier and worker pool, split by tier, and their DSD arms are gated per RUN.** → History-rules-jobs.md
- **"We could not read the decoder listing" is not "your ffmpeg lacks the decoders", and `HasDSD == false` means both.** → History-rules-jobs.md
- **A DSD source SKIPS the `soxInfo.CanDecode` check in `classifyUpscaleTrack`.** → History-rules-jobs.md

- **The DSD decimation was MEASURED and it does not alias — but the test that says so is a DIFFERENTIAL, and an absolute in-band bar cannot replace it** (PR B4; numbers + method in `ops/engineering-log.md`) → History-rules-jobs.md
- **`sinc -a 110 -t 10000 -35000`: `-t` is the FULL transition width CENTRED on the cutoff, and the stopband is -116.8 dB.** → History-rules-jobs.md
- **⚠️ `sox … stats` over a SHORT file after a steep FIR reports the edge TRANSIENT, not the stopband — a 61 dB error, in the direction that makes a good filter look broken.** → History-rules-jobs.md
- **Assert the clip guard on the TRUE PEAK; only log the spectrum.** → History-rules-jobs.md
- **The fixture generator's modulator is a low-distortion CIFF and it was verified BEFORE anything was generated.** → History-rules-jobs.md
- **The album-level gain's parts (2026-09-27, #1053; switched on as the DSD `v2` schema by the PR after it; `ops/plan-2026-09-27-dsd-album-gain.md`).** **never read one without that join** **don't change `DSDPeakProfileFor` without the seed** **Don't fork a second decode path for measuring.** **never above the track's own guard** **Don't make a render wait before resolving its own claim, don't hold a survey claim across a wait, and don't release a claim anywhere but on every exit.** **Don't leave a mate out for a failure that is not a fact about it, and don't strike the rendering file for its album-mate** **Don't write the memory into the variant-strike table** **Don't drop the re-read:** **Don't group by folder.** **Don't add a reaper until the app records the gain with its downloaded copy, plus a grace period.** → History-rules-jobs.md

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
- **In the analysis pool, a job's count and its dedup release are ONE step, taken after its bookkeeping** (#987) → History-rules-jobs.md
- **In the transcode pool, the count, the release AND the job's event are one ordered tail** (#988) → History-rules-jobs.md
- **Both pools answer a held path with `ErrDuplicateInflight`, and a caller that counts gives it a bucket of its own** (#992) → History-rules-jobs.md
- **A job a worker takes while its live gate is closed is DROPPED, and a drop is an outcome of its own in both pools** (2026-10-15, backlog B155; the rule, its measurement and the gate a job answers to are under The CLI and the serve wiring) **Don't count a drop as a failure** → History-rules-jobs.md
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
- **ALAC reaches the pipeline through an ffmpeg pipe, and the completeness guard on it is TWO-SIDED.** → History-rules-jobs.md
- **`redactSoxErr` must know EVERY absolute-path family a job can name, and the journal gets the redacted message too.** → History-rules-jobs.md
- **Analysis commits only on a length-complete decode** → History-rules-jobs.md
- **AAC encoder delay is an absolute allowance on that ratio, and the failure names both durations to the millisecond** (2026-10-13, backlog B192) → History-rules-jobs.md
- **…and a source that will NEVER decode stops being offered, on the decoder's own verdict.** → History-rules-jobs.md
- **The strike is stamped FROM the manifest row; the walk's LIVE stat overrules it.** → History-rules-jobs.md
- **The WARN is kept and deduplicated to one line per (path, size, mtime)** → History-rules-jobs.md
- **…and the TRANSCODE pool strikes a file only for what a tool said about it: a tool the host lacks strikes nothing** (2026-09-28) → History-rules-jobs.md
- **…and a directory this host will not let a job WRITE strikes nothing either** (2026-10-02, backlog B211) **Don't key a permission outage per folder** → History-rules-jobs.md
- **`OpenStore` refuses a symlink at the database or either sidecar before SQLite opens it** (2026-10-12, backlog B262; measured on a Linux host as root over an install another user owns) **Don't restore the Precreate-gated-on-ErrNotExist shape without this guard, and don't echo the link's target into the error** → History-rules-jobs.md
- **…and that refusal holds through SQLite's own open of the database, `-wal` and `-shm`** (2026-10-12, backlog B346) → History-rules-jobs.md
- **…and a rendition is published only WHOLE, read from its frames, whatever the tool's exit status** (2026-10-02, backlog B264) → History-rules-jobs.md
- **…and the published file's bit depth is the depth its STREAMINFO names, compared with the job's target before the publish** (2026-10-11, backlog B271) → History-rules-jobs.md
- **A PCM job's `-G` file lands in the configured temp dir, and a short rendition whose temp volume cannot hold it is the host's** (2026-10-08, backlog B270) → History-rules-jobs.md
- **A DSD render holds two Stage A scratches while it surveys, and every pre-flight reserves both** (2026-10-09, backlog B210) → History-rules-jobs.md
- **A NEGATED condition over a LEFT JOIN needs COALESCE, and the sibling terms that do not are why it is easy to miss.** → History-rules-jobs.md
- **The auto-optimize candidate query is its own thing, not the Inspector's** → History-rules-jobs.md
- **…and EVERY rendition writer stamps the row, and the on-demand path, the sweeper and the CLI queue a render only while the file still matches it** (#1077, backlog B24) **Don't stamp a live stat anywhere** → History-rules-jobs.md
- **…and the batch walks check too, `transcode.Run` checks again when it starts and before it publishes, and a stale download asks for the rescan** (backlog B53) → History-rules-jobs.md
- **…and a download that finds a rendition stale has it rendered again** (backlog B82; until B100, the next bullet, this title went on "a batch and the coverage bars still count a stale rendition as covered") **Don't render from a download whose row is behind** → History-rules-jobs.md
- **…and a batch, and every coverage bar, counts a rendition only while it is FRESH** **Never write the comparison out at a site.** → History-rules-jobs.md
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
- **Every reaper fails closed on an EMPTY referenced set, and the CLI ones offer a way past it.** → History-rules-jobs.md
- **Every walk of the artwork cache starts where the cache directory resolves, the size cap goes on over what it cannot list, and the GC keeps the thumbnails of an artist a track row names** (2026-09-29, backlog B64) **Never evict on a keep set the pass could not read** **Never evict a file modified within `artworkCapGrace`** → History-rules-jobs.md
- **`artwork --gc` collects the portraits of artists no track row names** (2026-10-14, backlog B85) **never from the name** → History-rules-jobs.md
- **A sidecar walk passes over, at the WALK, every directory a library walk skips (`manifest.ShouldSkipDir`) but lost+found, by its NAME before it lists it** (2026-10-02, backlog B207) → History-rules-jobs.md
- **The artwork cache's three walks pass over the same directories** (2026-10-14, backlog B260) → History-rules-jobs.md
- **…and a forward sweep compares a walked file with its rows in ONE spelling, `integrity.KnownSidecarKey`: cleaned, NFC-composed, THEN lowercased** (2026-10-02, backlog B206) → History-rules-jobs.md
- **No server-side transcoding, ever.** Conversion is offline; `/v1/download`
  serves bit-exact via `http.ServeContent`. The DSD → PCM renditions (PR #863)
  are conversion in exactly that sense — a sidecar the job pool built earlier,
  served as a file — never a decode in the request path.

- **Every consumer of `variantsDir` resolves it LIVE — the integrity watchers included.** → History-rules-jobs.md

- **A recorded sidecar path is a CLAIM about where the file was, never proof that it is gone.** → History-rules-jobs.md
- **A sidecar walk that feeds a deletion RESOLVES ITS ROOT, and reports under the configured one.** → History-rules-jobs.md
- **A sweep that would reap more than `integrity.variantSweepMaxDeletePercent` (default 20, floor 10 rows) of the catalog while the tree still holds sidecar-shaped files is REFUSED, and every tick that saw rows logs one summary line** → History-rules-jobs.md
- **…and the watcher's refusal goes through the orphan sweep's latch** (2026-09-29, backlog B65) → History-rules-jobs.md
- **…and the mount-loss skip is the latch's second kind, and both of the watcher's refusals reach the Jobs card** (2026-09-29, backlog B131) → History-rules-jobs.md
- **…and the watcher asks again, BY IDENTITY, as each row reads as missing and before it deletes: the probe at the start of a tick says nothing about the rows after it** (2026-09-29, backlog B203) → History-rules-jobs.md
- **…and `upscale --gc` deletes a row only while the variants directory is still the directory it classified** (2026-10-12, backlog B224) → History-rules-jobs.md
- **…and the forward sweep unlinks only while the variants directory is still the one the sweep began on** (2026-10-12, backlog B336) → History-rules-jobs.md
- **…and the probe counts RENDITIONS, not entries: a variants directory holding none is what an unmounted volume looks like, whatever else it holds** (2026-09-29, backlog B223) **Don't take a lost+found as proof the volume is mounted** → History-rules-jobs.md
- **…and the watcher deletes a row only while it is still the row it LISTED: the DELETE itself compares the columns a writer moves** (2026-10-02, backlog B204) → History-rules-jobs.md
- **A move puts the file at the destination before it moves the row, and removes the source name only after the row points there** (2026-10-09, backlog B249) → History-rules-jobs.md
- **…and the serve reap deletes the row it looked up** (2026-10-09, backlog B251) → History-rules-jobs.md
- **…and so does `upscale --gc`'s reverse sweep** (2026-10-02, backlog B250): it deletes through `DeleteVariantIfUnchanged` with the row as listed, not the unconditional `DeleteVariant`, and counts `ErrVariantChanged` as `changed`, which is no failure. → History-rules-jobs.md
- **`sidecar-paths` counts RECORDED PATHS and stats nothing, so it must not be described as a list of files that are gone** (#972) → History-rules-jobs.md
- **The delete handler LOCATES before it judges the volume, and only the already-gone path asks** (#968) → History-rules-jobs.md
- **EMPTY is explicable only by an unlink from the SAME directory INSTANCE.** → History-rules-jobs.md
- **A sidecar walk tests "not a REGULAR file", never "is a symlink"** (#969) → History-rules-jobs.md
- **`SidecarInventory.Unreadable` is a count of ENTRIES, and every place that prints it says so.** → History-rules-jobs.md
- **A forward sweep's denominator is the TREE, never the catalog, and the term that knows a lost index is `orphans > rows`.** → History-rules-jobs.md
- **…and the background `OrphanSidecarSweeper` makes the same decision every tick, with no override** (2026-09-28) → History-rules-jobs.md
- **…and a partial walk is refused by every forward sweep, weighed by what it can hide** (2026-09-28) → History-rules-jobs.md
- **…and a CLI forward sweep removes a file only once it is older than the grace, asked again just before its unlink: `--gc` is safe beside a running bridge** (2026-10-02, backlog B205) → History-rules-jobs.md
- **The Jobs card shows the background orphan sweep's refusal** (2026-09-28) **Don't decide it from `dirIsEmpty`** → History-rules-jobs.md
- **`bridge doctor`'s `variants-index` is the other side of `sidecar-paths`, and its walk is BOUNDED.** → History-rules-jobs.md
- **A guard that reads the world AFTER a sweep has to be told what the sweep did.** → History-rules-jobs.md

## Auth, pairing, TLS and security posture

- **The disk probe's missing-directory warning names the directory by its role, never by its path** (2026-10-13, backlog B273), **and so do the probes' errors and a no-room refusal** (2026-10-30, B604): **Don't quote a directory in a probe's error or in `InsufficientDiskSpaceError`'s text; name its role, and keep `Dir` for a caller that compares it.** → History-rules-jobs.md
