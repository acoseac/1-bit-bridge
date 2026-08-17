package main

// Serve-side auto-optimize sweeper: background pre-generation of
// CarPlay `optimized-*` variants so a device never has to wait for one.
//
// **Why pre-generate at all.** iOS mints these lazily. Its CarPlay
// routing (`PlayerService.resolveVariantID` tier 0) asks for an
// optimized variant when the car is the active output and, finding
// none, plays the hi-res SOURCE while firing a fire-and-forget generate
// request — so only the NEXT play of that track is cheap. On a shuffle
// across a large library nearly every play is a first play, and the
// bandwidth saving the feature exists for almost never lands. The
// download path handles it better but still parks the job in a
// `generatingVariant` phase for up to 90 s, falling back to the source
// on timeout. A warm cache removes both cases.
//
// **Shape** mirrors runAnalysisSweeper (analyze.go): settle delay →
// post-scan nudge → periodic tick, ctx-scoped, bgWriters-joined.
//
// **Enqueues straight to the Pool, NOT through Coordinator.SubmitOptimize.**
// Three reasons, all load-bearing:
//
//  1. The Coordinator inserts an `upscale_batches` row per call. One per
//     sweep tick would bury the operator's own batches on the Jobs page
//     under a stream of automatic, usually-empty ones.
//  2. `SubmitOptimize` re-walks with `ListTrackProjectionsUnderPrefix`,
//     which has neither the UPnP anti-join nor the dupe-suppression
//     filter nor the staleness detection this sweeper needs (see
//     manifest/auto_optimize.go for why each matters).
//  3. `Pool.processJob` is what commits `UpsertVariant`, so a direct
//     enqueue is complete on its own — the Coordinator is batch-progress
//     bookkeeping, not part of the write path.

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// autoOptimizeSweeper bundles the sweeper's dependencies. The config
// readers are closures, not snapshots, for two different reasons:
//
//   - `enabled` / `maxPerSweep` / `minFreeBytes` are read LIVE so an
//     admin Settings PATCH hot-applies on the next nudge instead of
//     waiting for a restart (the `duplicates.filter` precedent).
//   - `outputDir` is live because the variants dir is operator-editable
//     at runtime (POST /api/upscale/variants-dir); a snapshot taken at
//     construction would keep writing to the old volume, which is the
//     bug PR #504 fixed for the disk pre-flight.
type autoOptimizeSweeper struct {
	store    *manifest.Store
	resolver *bridgefs.Resolver

	// enqueue is the pool's submit entry point, injected rather than
	// holding a *transcode.Pool so the sweep logic is testable without
	// spawning real sox children (the pool's own runner seam is
	// unexported and out of reach from this package). Production wires
	// upscalePool.Enqueue.
	enqueue func(transcode.JobSpec) error

	enabled      func() bool
	outputDir    func() string
	maxPerSweep  func() int
	minFreeBytes func() int64

	// diskFree is the free-space probe, injectable so the disk-floor
	// branch is testable without filling a real volume. Production wires
	// transcode.AvailableDiskSpaceNearest (which walks to the nearest
	// existing ancestor — the variants dir is created lazily, so a bare
	// statfs would ENOENT).
	diskFree func(dir string) (int64, error)
}

// sweepOnce enqueues up to `maxPerSweep` optimize jobs and returns the
// counts for the admin card. Returns nil when the sweep failed or was
// cancelled, so sweepStatus keeps the previous successful breakdown.
func (sw *autoOptimizeSweeper) sweepOnce(ctx context.Context) *admin.AutoOptimizeSweepCounts {
	if !sw.enabled() {
		// Report the disabled state rather than returning nil: nil means
		// "failed, keep the old numbers", and an operator who just turned
		// the feature off should see it reflected, not frozen.
		return &admin.AutoOptimizeSweepCounts{Disabled: true}
	}

	limit := sw.maxPerSweep()
	cands, err := sw.store.ListAutoOptimizeCandidates(ctx, limit)
	if err != nil {
		// A cancelled context here is a normal shutdown, not a fault —
		// the suppression the analysis + fingerprint sweepers apply.
		if ctx.Err() == nil {
			logger.Warn("auto-optimize sweep: list candidates", "err", err)
		}
		return nil
	}

	outputDir := sw.outputDir()
	freeBytes, derr := sw.diskFree(outputDir)
	if derr != nil {
		// Fail CLOSED. Without a free-space reading there is no way to
		// honour minFreeBytes, and the failure mode of guessing wrong is
		// filling the operator's volume with work nobody asked for. A
		// skipped sweep costs nothing — the next tick retries.
		if ctx.Err() == nil {
			logger.Warn("auto-optimize sweep: disk probe failed; skipping sweep",
				"dir", outputDir, "err", derr)
		}
		return nil
	}

	counts := &admin.AutoOptimizeSweepCounts{
		MinFreeBytes: sw.minFreeBytes(),
		FreeBytes:    freeBytes,
	}
	floor := counts.MinFreeBytes
	compression := transcode.DefaultCompressionFactor(optimizeTargetBits)
	var projectedTotal int64

	for _, c := range cands {
		if ctx.Err() != nil {
			return nil
		}
		// Re-run the GO gate. The SQL predicate that selected this row is
		// a documented MIRROR of it (pinned by the admin package's
		// lockstep test), and on a path that spends disk and CPU the Go
		// gate stays authoritative — a mirror drift must under-generate,
		// never mis-generate.
		if !transcode.OptimizeEligible(c.Path, c.Codec, c.SampleRate, c.BitsPerSample) {
			counts.Ineligible++
			continue
		}
		targetRate, terr := transcode.ResolveTargetRateForOptimize(c.SampleRate)
		if terr != nil {
			counts.Ineligible++
			continue
		}

		projected := transcode.ProjectedSize(c.Size, c.SampleRate, c.BitsPerSample,
			targetRate, optimizeTargetBits, compression)
		if freeBytes-(projectedTotal+projected) < floor {
			// Running budget, not a point check: the on-demand path's
			// per-batch diskPreflight can't bound a loop that runs
			// forever. Stop rather than skip-and-continue — candidates are
			// ordered newest-indexed-first, so everything after this point
			// is lower priority anyway, and continuing would let a run of
			// small files sneak past a floor a big one just hit.
			counts.DiskFloorReached = true
			break
		}

		abs, info, rerr := sw.resolver.ResolveChecked(c.Path)
		if rerr != nil || info.IsDir() {
			// Unresolvable: a deleted file the scanner's missing_count
			// debounce hasn't reaped yet, or a dropped mount. Skip
			// silently — every error is treated identically on purpose,
			// since distinguishing them is the seed of a "mark this
			// permanently un-optimizable" bug during a mount outage.
			counts.Unresolvable++
			continue
		}

		// SourceMTimeNS / SourceSize come from the TRACK ROW, not from
		// `info`. Matches Coordinator.buildOptimizeCandidates so a swept
		// variant is indistinguishable from an on-demand one — and it is
		// what keeps the staleness predicate self-consistent (see the
		// autoOptimizeCandidateSQL docblock: stamping a live stat would
		// make freshly built variants read as stale on the next tick
		// whenever the scanner hadn't caught up, regenerating them
		// forever).
		err := sw.enqueue(transcode.JobSpec{
			SourceAbsPath:    abs,
			SourceLibraryRel: c.Path,
			SourceMTimeNS:    c.MTimeNS,
			SourceSize:       c.Size,
			SourceSampleRate: c.SampleRate,
			SourceBits:       c.BitsPerSample,
			TargetSampleRate: targetRate,
			TargetBits:       optimizeTargetBits,
			Quality:          transcode.QualityVeryHigh,
			OutputDir:        outputDir,
			Kind:             transcode.JobKindOptimize,
			// Background demotes this to the Pool's LOW-priority lane.
			// Without it a library-wide sweep would head-of-line block the
			// on-demand CarPlay request the two-channel queue exists to
			// protect. See the JobSpec.Background docstring.
			Background: true,
		})
		switch {
		case err == nil:
			counts.Enqueued++
			projectedTotal += projected
			if c.StaleVariantID != "" {
				counts.Regenerated++
			}
		case errors.Is(err, transcode.ErrDuplicateInflight):
			// Already queued or running — an on-demand request for the
			// same track beat us to it. Not a failure.
			counts.AlreadyInflight++
		case errors.Is(err, transcode.ErrQueueFull), errors.Is(err, transcode.ErrPoolClosed):
			counts.QueueSaturated = true
		default:
			logger.Warn("auto-optimize sweep: enqueue failed", "path", c.Path, "err", err)
		}
		if counts.QueueSaturated {
			break
		}
	}
	counts.ProjectedBytes = projectedTotal

	// Remaining backlog for the admin card. A second pass over the same
	// predicate — deliberately, so the card's number and the sweeper's
	// work cannot drift — and affordable because it runs once per sweep
	// (default cadence: the scan interval), not per tick of anything hot.
	if remaining, cerr := sw.store.CountAutoOptimizeCandidates(ctx); cerr == nil {
		counts.Remaining = remaining
	} else if ctx.Err() == nil {
		logger.Warn("auto-optimize sweep: count remaining", "err", cerr)
	}

	if counts.Enqueued > 0 {
		logger.Info("auto-optimize sweep enqueued variants",
			"count", counts.Enqueued,
			"regenerated", counts.Regenerated,
			"remaining", counts.Remaining,
			"projectedBytes", counts.ProjectedBytes,
			"queueSaturated", counts.QueueSaturated,
			"diskFloorReached", counts.DiskFloorReached)
	} else if counts.DiskFloorReached {
		// Worth a line even with nothing enqueued: this is the state an
		// operator needs to see to know why pre-generation stalled.
		logger.Warn("auto-optimize sweep stopped at the free-space floor",
			"freeBytes", freeBytes, "minFreeBytes", floor, "remaining", counts.Remaining)
	}
	return counts
}

// optimizeTargetBits is the CarPlay bit-depth floor every optimize job
// targets. Named rather than repeating the literal, matching what
// Coordinator.SubmitOptimize stamps on its batch rows.
const optimizeTargetBits = 16

// autoOptimizeSettleDelay lets any startup scan land before the first
// candidate walk. A var purely as the test seam — production never
// mutates it. Longer than the analysis sweeper's 90 s because these two
// both spawn sox and the analysis backfill is the one an operator
// notices missing first.
var autoOptimizeSettleDelay = 3 * time.Minute

// runAutoOptimizeSweeper is the sweeper loop. After a settle delay, and
// then on every `interval` tick — or immediately on a `nudge` (the
// scanner's post-scan hook and an admin Settings flip both send one) —
// it enqueues optimize jobs for tracks that want one.
//
// Idempotent: the candidate predicate excludes tracks already covered by
// a fresh variant, so a re-sweep over an unchanged library enqueues
// nothing. Honors ctx for clean shutdown; a saturated queue or an
// exhausted disk budget just defers the rest to the next sweep.
//
// nudge is buffered-1; senders use a non-blocking send so a pending
// nudge coalesces with the sweep about to run. status is nil-safe.
func runAutoOptimizeSweeper(ctx context.Context, sw *autoOptimizeSweeper, interval time.Duration, nudge <-chan struct{}, status *sweepStatus[admin.AutoOptimizeSweepCounts]) {
	sweep := func() {
		status.sweepStarted()
		var counts *admin.AutoOptimizeSweepCounts
		defer func() { status.sweepFinished(counts) }()
		counts = sw.sweepOnce(ctx)
	}

	// Cadence (settle delay, one-drain semantics, tick-or-nudge) lives in
	// the shared runSweepLoop — see its docstring for why the nudge is
	// drained exactly once.
	runSweepLoop(ctx, status, autoOptimizeSettleDelay, interval, nudge, sweep)
}
