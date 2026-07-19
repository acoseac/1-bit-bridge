// Coordinator wraps an existing *Pool with the v1.3 operator-driven
// batch-tracking surface. Where Pool knows about per-job dedup and
// worker scheduling, Coordinator knows about:
//
//   - per-batch enrollment (`upscale_batches` rows)
//   - pre-flight projection + disk-space refusal
//   - live per-batch counters bumped from pool callbacks
//   - rolling throughput average for ETA rendering
//   - SSE `upscale.batch` event emission on every counter change
//
// One Coordinator per Pool. Constructed at `bridge serve` boot via
// `NewCoordinator(pool, store, dataDir, log, publish)`; the
// publish closure injects the SSE broker so internal/transcode
// stays free of internal/api as a dependency.
//
// Threading: all DB writes hold their own per-row coordinator lock
// so concurrent pool callbacks (one per worker) can't interleave
// reads with writes on the same `upscale_batches` row. The pool's
// own write contracts (UpsertVariant under s.mu) are independent.

package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// CoordinatorPublishFunc is the broker-emit closure injected at
// NewCoordinator time. cmd/bridge/main.go wires this to
// `broker.Publish("upscale.batch", evt)` so internal/transcode
// doesn't import internal/api / internal/api/event_broker.
//
// Nil-safe: a nil closure (test harness with no broker) is silently
// dropped at the call site. Production deployments always wire it.
type CoordinatorPublishFunc func(evt BatchProgressEvent)

// BatchProgressEvent is the wire shape published on the SSE
// `upscale.batch` topic. Admin Library Inspector + Jobs page render
// from these; the iOS-facing API does NOT consume them (iOS uses
// the per-track `upscale.complete` event from PR #B2 / v1.3).
//
// `BatchID` is stringified UUID for JSON-decode friendliness; the
// raw 16-byte form lives in the SQLite BLOB column.
type BatchProgressEvent struct {
	BatchID        string    `json:"batchID"`
	Path           string    `json:"path"`
	Status         string    `json:"status"`
	TargetRate     int       `json:"targetRate"`
	TargetBits     int       `json:"targetBits"`
	TotalFiles     int       `json:"totalFiles"`
	ProcessedFiles int       `json:"processedFiles"`
	FailedFiles    int       `json:"failedFiles"`
	Error          string    `json:"error,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// ThroughputSnapshot carries the rolling-average derived values the
// admin dashboard renders. Returned by Throughput().
//
// `JobsPerHour` is the rate computed from the most recent
// `throughputWindowSize` completions; `EtaSeconds` is a forward
// projection for the currently-running batches' remaining files
// at that rate (zero when no batch is active or there's
// insufficient sample data).
type ThroughputSnapshot struct {
	JobsPerHour float64 `json:"jobsPerHour"`
	EtaSeconds  float64 `json:"etaSeconds"`
	Samples     int     `json:"samples"`
}

// throughputWindowSize is the rolling-average window. Ten jobs is
// enough to smooth out per-job variance (40 s SoX runs sit next to
// 10 s ones) without making the dashboard ETA lag for two minutes
// after a workload shift. Adjustable; not exposed via config because
// the value isn't operator-tunable in a useful sense.
const throughputWindowSize = 10

// throughputMinSamples is the minimum samples Throughput() needs
// before it produces a non-zero JobsPerHour. With < 3 completions
// the average is dominated by initial-load noise and produces a
// wildly wrong ETA. Surfaces as (0, 0, samples) until the window
// fills enough to be useful.
const throughputMinSamples = 3

// jobCompleteHook + jobFailedHook signatures match the pool's
// `onJobComplete` / `onJobFailed` exactly. The Coordinator implements
// these and `cmd/bridge/main.go` calls `pool.SetOnJobComplete(c.OnJobComplete)`
// / `pool.SetOnJobFailed(c.OnJobFailed)` at wiring time.

// Coordinator wraps a Pool with per-batch enrollment, live
// counters, throughput math, and SSE event emission.
type Coordinator struct {
	pool  *Pool
	store *manifest.Store
	// dataDir is the FALLBACK disk-check dir only — the pre-flight
	// headroom checks in Submit / SubmitOptimize grade the per-call
	// `outputDir` (the directory the variants are actually written
	// to, which may live on a different volume than the bridge data
	// dir). dataDir is used only when a caller passes outputDir=""
	// (test harnesses / legacy callers).
	dataDir  string
	publish  CoordinatorPublishFunc
	resolver ResolverFunc
	logger   *slog.Logger
	clock    func() time.Time // dependency-injected for tests

	mu sync.Mutex
	// liveBatches is the in-memory mirror of pending+running rows so
	// pool callbacks bump counters without re-reading SQLite on
	// every job completion. Coordinator writes through to
	// `upscale_batches` on every counter change so a restart
	// recovers the latest state via the rows directly.
	liveBatches map[uuid.UUID]*batchState

	// throughputSamples is a ring buffer of recent job durations in
	// seconds. Read by Throughput(); written by OnJobComplete /
	// OnJobFailed.
	throughputSamples [throughputWindowSize]float64
	throughputCount   int // total samples ever written (caps reads at min(count, window))
	throughputCursor  int // next write position
}

// ResolverFunc converts a library-relative path (e.g. `Music/Album/01.flac`)
// to its absolute filesystem path. Required at JobSpec construction
// time — `RunSox` consumes the absolute path directly and fails fast
// on empty input. Wired in cmd/bridge/main.go via a closure around
// `apiSrv.Resolver().Resolve(...)` so this package stays free of
// internal/fs.
//
// Returns the absolute path on success. On failure (unknown root,
// path traversal, IO error) the closure returns an error; Submit
// silently filters those tracks out of the batch and surfaces the
// rest, so a single bad path doesn't abort an otherwise-valid
// folder submission. The filtered count is reflected in the
// returned `TotalFiles` vs the input track count — operators can
// reconcile from logs if they care which tracks dropped.
type ResolverFunc func(libraryRel string) (absPath string, err error)

// batchState mirrors the live row's counters. Snapshot-only — every
// mutation immediately writes through to `upscale_batches` so a
// restart recovers the latest state. Keeping the in-memory copy
// avoids a SELECT-on-every-pool-callback hot path.
type batchState struct {
	Row          manifest.UpscaleBatchRow
	RemainingIDs map[string]struct{} // source-path keys for tracks the batch still expects callbacks for
}

// NewCoordinator constructs a Coordinator and runs the boot-time
// `RecoverInterruptedBatches` pass against `store`. The recovery
// pass MUST complete before the Coordinator accepts new Submit
// calls — repeated boots between which a batch was running would
// otherwise leave phantom in-flight rows.
//
// `publish` is the SSE broker emitter; nil disables event emission
// (test harness) but counter writes still land in the DB.
//
// `resolver` converts library-relative paths to absolute paths at
// JobSpec construction time. nil is permitted (test harness) but
// Submit then refuses with an explicit error rather than enqueueing
// broken JobSpecs.
func NewCoordinator(
	pool *Pool,
	store *manifest.Store,
	dataDir string,
	publish CoordinatorPublishFunc,
	resolver ResolverFunc,
) (*Coordinator, error) {
	c := &Coordinator{
		pool:        pool,
		store:       store,
		dataDir:     dataDir,
		publish:     publish,
		resolver:    resolver,
		logger:      logging.Component("transcode.coordinator"),
		clock:       func() time.Time { return time.Now().UTC() },
		liveBatches: make(map[uuid.UUID]*batchState),
	}
	// NewCoordinator runs at bridge boot — no caller ctx in scope.
	// Use Background since this is a one-shot init-time recovery.
	rows, err := store.RecoverInterruptedBatches(context.Background(), c.clock().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("recover interrupted batches: %w", err)
	}
	if rows > 0 {
		c.logger.Info("boot recovery: interrupted batches",
			"rowsAffected", rows)
	}
	return c, nil
}

// SubmitResult is returned by Submit on success. The caller (the
// HTTP handler) marshals it into the 202 Accepted body.
type SubmitResult struct {
	BatchID            uuid.UUID
	Path               string
	TargetRate         int
	TargetBits         int
	TotalFiles         int
	AlreadyCovered     int
	ProjectedSizeBytes int64
	AvailableBytes     int64
	EnqueuedCount      int
}

// diskPreflight grades the volume the variants will actually be
// WRITTEN to and returns its free bytes, refusing the batch if
// projected + the safety margin wouldn't fit.
//
// outputDir is the sidecar destination; it may live on a different
// volume than the bridge data dir (field case: bridge.ars.md's
// variants on a ~1 PB B2 mount while dataDir sits on a 29 GB root
// disk — grading dataDir refused batches that fit easily). c.dataDir
// is the documented fallback for callers that pass "".
//
// The typed *InsufficientDiskSpaceError surfaces UNWRAPPED (callers
// errors.As it for the 507 mapping); `op` prefixes only genuine probe
// failures, which callers must treat as "can't check, refuse" —
// silently proceeding risks a disk-full mid-batch.
func (c *Coordinator) diskPreflight(outputDir string, projected int64, op string) (available int64, err error) {
	checkDir := outputDir
	if checkDir == "" {
		checkDir = c.dataDir
	}
	_, available, err = DiskHasHeadroom(checkDir, projected, DefaultDiskSafetyMargin)
	if err != nil {
		var dskErr *InsufficientDiskSpaceError
		if errors.As(err, &dskErr) {
			return 0, err
		}
		return 0, fmt.Errorf("%s: disk probe: %w", op, err)
	}
	return available, nil
}

// Submit walks every track under `path`, filters ineligible /
// already-covered, computes the projected variant size, refuses on
// insufficient disk headroom, inserts an `upscale_batches` row,
// and enqueues filtered jobs into the pool with their `BatchID`
// stamped. Returns ErrInsufficientDiskSpace (wrapped) if pre-flight
// refuses.
//
// `targetRate` / `targetBits` come from the admin Settings (DB-
// backed via `Store.GetUpscaleTarget`); the caller is responsible
// for falling back to YAML bootstrap when scan_state is unseeded.
//
// `outputDir` is `<dataDir>/transcoded` — same path the legacy
// per-track CLI / HTTP path uses. The Pool dedups on `(source,
// variant_id)` so a track that already has a variant at the same
// target is silently skipped (counted as `AlreadyCovered`).
func (c *Coordinator) Submit(ctx context.Context, path string, targetRate, targetBits int, outputDir string) (*SubmitResult, error) {
	if targetRate <= 0 {
		return nil, fmt.Errorf("submit: target rate %d Hz: must be positive", targetRate)
	}
	switch targetBits {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("submit: target bits %d: must be 16/24/32", targetBits)
	}

	if c.resolver == nil {
		return nil, fmt.Errorf("submit: no resolver wired — Coordinator can't build JobSpec absolute paths")
	}

	projections, err := c.store.ListTrackProjectionsUnderPrefix(ctx, path, manifest.VariantKindPrefixUpscaled)
	if err != nil {
		return nil, fmt.Errorf("submit: list projections: %w", err)
	}

	// Filter + project. Tracks with `HasVariant` already covered;
	// tracks with zero source rate / bits are unknown-format and
	// skipped (the operator pre-flight surfaced them separately).
	type candidate struct {
		path       string
		absPath    string
		size       int64
		mtimeNS    int64
		sampleRate int
		bits       int
	}
	var (
		cands          []candidate
		alreadyCovered int
		totalProjected int64
		compressionFct = DefaultCompressionFactor(targetBits)
		resolveErrors  int
	)
	for _, t := range projections {
		if t.HasVariant {
			alreadyCovered++
			continue
		}
		if t.SampleRate <= 0 || t.BitsPerSample <= 0 {
			continue
		}
		// Lossy sources are never upscaled: sox would just resample
		// DECODED lossy audio into a FLAC several times the size, and
		// PROTOCOL.md documents the /v1/upscale gate as "PCM". The
		// geometry gate above already excludes almost all lossy files
		// (they carry no meaningful bit depth) — this closes the
		// bogus-bits-tag hole and matches the inspector's lossy_source
		// badge. manifest.IsLossyCodec is the single source of truth
		// (SQL mirror: upscaleEligibleSQL).
		if manifest.IsLossyCodec(t.Codec) {
			continue
		}
		// Eligibility for upscaling. The full skip predicate
		// covers three cases:
		//
		//   (1) source rate strictly > target rate — sox would
		//       downsample rate (e.g., 384 → 192). Pre-fix gate
		//       used `&&` here and missed this when bits would
		//       have upsampled (384/24 → 192/32 leaked through).
		//       Per CodeRabbit major on PR #204 round 2.
		//   (2) source bits strictly > target bits — sox would
		//       downsample bit depth (e.g., 32 → 24).
		//   (3) both axes equal to target — no-op pass; sox would
		//       produce a bit-identical variant at the same
		//       (rate, bits) as the source. UpsertVariant dedup
		//       catches this AFTER the run but skipping at the
		//       gate avoids the wasted CPU.
		//
		// Strict-greater on each axis (not `>=`) is load-bearing:
		// it lets same-on-one + upsample-the-other through —
		// 44.1/24 → 192/24 (rate-only upsample) and 192/16 → 192/24
		// (bit-only upsample) are both legitimate batches.
		if t.SampleRate > targetRate || t.BitsPerSample > targetBits {
			continue
		}
		if t.SampleRate == targetRate && t.BitsPerSample == targetBits {
			continue
		}
		// Resolve abs path BEFORE projecting size, so a resolver
		// failure (unknown root, traversal, etc.) drops the track
		// from the projection too — operators don't see a number
		// for work that won't actually run.
		absPath, err := c.resolver(t.Path)
		if err != nil {
			resolveErrors++
			c.logger.Warn("submit: resolve failed; skipping track",
				"path", t.Path, "err", err)
			continue
		}
		cands = append(cands, candidate{
			path:       t.Path,
			absPath:    absPath,
			size:       t.Size,
			mtimeNS:    t.MTimeNS,
			sampleRate: t.SampleRate,
			bits:       t.BitsPerSample,
		})
		totalProjected += ProjectedSize(t.Size, t.SampleRate, t.BitsPerSample,
			targetRate, targetBits, compressionFct)
	}
	if resolveErrors > 0 {
		c.logger.Info("submit: filtered tracks with resolver failures",
			"batchPath", path, "count", resolveErrors)
	}

	available, err := c.diskPreflight(outputDir, totalProjected, "submit")
	if err != nil {
		return nil, err
	}

	// Insert the batch row first so any pool callback that races
	// with the enqueue finds a row to attribute to.
	//
	// Propagate the (vanishingly rare) entropy failure instead of
	// uuid.Must's panic — this runs on the API submission path and a
	// transient CSPRNG error shouldn't crash the bridge. Gemini r4.
	batchID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate batch uuid: %w", err)
	}
	now := c.clock().UnixNano()
	// Skip count = projected − enqueued − already-covered. Captures
	// every `continue` arm of the loop above (rate>target, bits>target,
	// rate==target && bits==target no-op, missing rate/bits, resolver
	// failure) without threading a per-arm counter through the
	// branches. Surfaced on the Jobs page row as "X tracks skipped"
	// so operators who see a "completed 0/0" batch understand whether
	// nothing was eligible vs nothing was found.
	skipped := len(projections) - len(cands) - alreadyCovered
	if skipped < 0 {
		skipped = 0 // defensive — should be impossible
	}
	row := manifest.UpscaleBatchRow{
		ID:             batchID,
		Path:           path,
		TargetRate:     targetRate,
		TargetBits:     targetBits,
		Status:         "pending",
		TotalFiles:     len(cands),
		ProcessedFiles: 0,
		FailedFiles:    0,
		SkippedFiles:   skipped,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := c.store.InsertUpscaleBatch(ctx, row); err != nil {
		return nil, fmt.Errorf("submit: insert batch row: %w", err)
	}

	// Build in-memory state with the per-track remaining set so
	// pool callbacks can find the right row by path even when many
	// batches run concurrently.
	state := &batchState{
		Row:          row,
		RemainingIDs: make(map[string]struct{}, len(cands)),
	}
	for _, ca := range cands {
		state.RemainingIDs[ca.path] = struct{}{}
	}
	c.mu.Lock()
	c.liveBatches[batchID] = state
	c.mu.Unlock()

	// Empty-batch short-circuit: no candidates → no worker callback
	// will ever fire to transition the row out of `pending`. Mark
	// the batch completed synchronously so the admin row doesn't
	// sit `running` indefinitely. Mirrors SubmitOptimize's branch
	// (CodeRabbit major on PR #270 + PR #278 outside-diff).
	if len(cands) == 0 {
		if err := c.transitionStatus(batchID, "completed", "", c.clock()); err != nil {
			c.logger.Warn("submit: transition empty batch to completed",
				"batchID", batchID.String(), "err", err)
		}
		c.publishProgress(batchID)
		return &SubmitResult{
			BatchID:            batchID,
			Path:               path,
			TargetRate:         targetRate,
			TargetBits:         targetBits,
			TotalFiles:         0,
			AlreadyCovered:     alreadyCovered,
			ProjectedSizeBytes: totalProjected,
			AvailableBytes:     available,
			EnqueuedCount:      0,
		}, nil
	}

	// Enqueue. Pool's bounded channel makes Enqueue non-blocking
	// per-call; we transition the batch to `running` before the
	// first job lands so a slow ListTrackProjections couldn't have
	// the row sit in `pending` while jobs already started.
	if err := c.transitionStatus(batchID, "running", "", c.clock()); err != nil {
		c.logger.Warn("submit: transition to running",
			"batchID", batchID.String(),
			"err", err)
	}
	enqueued := 0
	deduped := 0
	for _, ca := range cands {
		spec := JobSpec{
			SourceAbsPath:    ca.absPath,
			SourceLibraryRel: ca.path,
			SourceMTimeNS:    ca.mtimeNS,
			SourceSize:       ca.size,
			SourceSampleRate: ca.sampleRate,
			SourceBits:       ca.bits,
			TargetSampleRate: targetRate,
			TargetBits:       targetBits,
			Quality:          QualityVeryHigh,
			OutputDir:        outputDir,
			BatchID:          batchID,
		}
		err := c.pool.Enqueue(spec)
		if errors.Is(err, ErrDuplicateInflight) {
			// Already queued/running from an overlapping batch. That job carries
			// the OTHER batch's ID and won't call back into this one, so drop the
			// path from remaining + total so allDone can still fire — the
			// in-flight job produces the variant. Without this, an overlapping
			// re-submit sat `running` forever.
			c.dropDedupedPath(batchID, ca.path)
			deduped++
			continue
		}
		if err != nil {
			// Queue full or pool closed — log the failure and stop
			// enqueueing further jobs, BUT keep the batch in
			// `liveBatches` so the already-enqueued jobs' callbacks
			// still attribute correctly. The remaining (never-
			// enqueued) tracks are dropped from RemainingIDs so the
			// terminal-status transition fires on the right count.
			// Per CodeRabbit security-high on PR #201: dropping the
			// batch on enqueue failure would leave in-flight workers
			// reporting back to a vanished slot — their progress
			// would never reach the row.
			c.logger.Warn("submit: enqueue failed; truncating batch",
				"batchID", batchID.String(),
				"path", ca.path,
				"err", err)
			c.mu.Lock()
			var (
				rowSnapshot    manifest.UpscaleBatchRow
				becameTerminal bool
				stateModified  bool
			)
			if st, ok := c.liveBatches[batchID]; ok {
				// Drop the never-enqueued tail from RemainingIDs.
				// (Includes the current `ca.path` since we didn't
				// successfully enqueue it.)
				dropping := false
				for _, c2 := range cands {
					if c2.path == ca.path {
						dropping = true
					}
					if dropping {
						delete(st.RemainingIDs, c2.path)
					}
				}
				// Reduce TotalFiles to the real-job count so the batch can
				// still reach terminal state via the enqueued-and-completed
				// count alone. Set directly (not `-= len(cands)-enqueued`) so
				// it stays correct after any earlier dedup drops.
				st.Row.TotalFiles = enqueued
				st.Row.Error = "partial enqueue: " + err.Error()
				st.Row.UpdatedAt = c.clock().UnixNano()
				// If nothing was enqueued, no callback will ever
				// arrive — transition to a terminal state now so
				// the row doesn't sit `running` forever. Per
				// CodeRabbit major on PR #204 round 2.
				if len(st.RemainingIDs) == 0 {
					st.Row.Status = "failed"
					becameTerminal = true
				}
				rowSnapshot = st.Row
				stateModified = true
				if becameTerminal {
					delete(c.liveBatches, batchID)
				}
			}
			c.mu.Unlock()
			// Persist the adjusted row OUTSIDE the lock — without
			// the write, the truncation lives only in
			// `liveBatches`. If the first enqueue fails (nothing
			// to drive a future callback), the DB row stays
			// `running` with the original totals forever.
			//
			// **Detached ctx** (CodeRabbit Major on PR #216): if
			// the caller cancels the request mid-truncate, the
			// in-memory totals get adjusted but the DB row stays
			// stale forever. Use a Background-rooted 5 s deadline
			// so a wedged DB doesn't park the goroutine while
			// still making the persist robust to caller-side
			// cancellation. The 5 s is generous for a single-
			// row UPDATE; orders of magnitude shorter than the
			// jobs the row is tracking.
			if stateModified {
				persistCtx, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
				if writeErr := c.store.UpdateUpscaleBatchProgress(persistCtx, rowSnapshot); writeErr != nil {
					c.logger.Warn("submit: persist truncated batch",
						"batchID", batchID.String(),
						"err", writeErr)
				}
				cancelPersist()
				c.publishProgressRow(rowSnapshot)
			}
			break
		}
		enqueued++
	}

	// The 202 response reports `enqueued` — the persisted row's TotalFiles
	// converges to exactly this in every path (it starts at len(cands), each
	// dedup decrements it via dropDedupedPath, and a queue-full break sets it to
	// `enqueued`). Reading it back from liveBatches would be both redundant AND
	// wrong when a first-enqueue failure already deleted the batch: the lookup
	// misses and falls back to len(cands), diverging from the row the admin Jobs
	// page shows (CodeRabbit PR #515).
	finalTotal := enqueued

	// Every candidate deduped against in-flight jobs (a fully-overlapping
	// re-submit) → no callback will fire for this batch; transition it now so it
	// doesn't sit `running` forever (the overlapping batch produces the variants).
	// Gate on `deduped == len(cands)`, NOT `enqueued == 0 && deduped > 0`: a
	// queue-full break can also leave enqueued == 0 with deduped > 0, and that
	// path already set a terminal (failed) status — completing it here would
	// clobber the failure (CodeRabbit).
	if deduped == len(cands) {
		if err := c.transitionStatus(batchID, "completed", "", c.clock()); err != nil {
			c.logger.Warn("submit: transition fully-deduped batch",
				"batchID", batchID.String(), "err", err)
		}
	}

	c.publishProgress(batchID)
	return &SubmitResult{
		BatchID:            batchID,
		Path:               path,
		TargetRate:         targetRate,
		TargetBits:         targetBits,
		TotalFiles:         finalTotal,
		AlreadyCovered:     alreadyCovered,
		ProjectedSizeBytes: totalProjected,
		AvailableBytes:     available,
		EnqueuedCount:      enqueued,
	}, nil
}

// dropDedupedPath removes a path that Enqueue reported as already in-flight
// (ErrDuplicateInflight) from the batch's remaining set + total, so allDone can
// still fire — the in-flight job (owned by an overlapping batch) produces the
// variant and never calls back into this batch.
func (c *Coordinator) dropDedupedPath(batchID uuid.UUID, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.liveBatches[batchID]; ok {
		delete(st.RemainingIDs, path)
		if st.Row.TotalFiles > 0 {
			st.Row.TotalFiles--
		}
	}
}

// SubmitOptimize is the CarPlay-targeted batch path: enrolls every
// eligible hi-res PCM track under `path` for downsampling to 16-bit /
// 44.1 or 48 kHz (family-preserving — see TargetRateForOptimize).
//
// Differs from Submit in three places:
//  1. Eligibility — per-track via OptimizeEligible (PCM-hi-res only,
//     legacy-codec fallback). NOT the upscale gate that rejects
//     downsampling targets.
//  2. Target rate / bits — resolved per-track from the source's
//     sample-rate family. TargetBits is uniformly 16.
//  3. JobSpec.Kind is JobKindOptimize so VariantID() mints the
//     `optimized-*` prefix.
//
// The batch row records TargetRate=0 to signal "per-track varies";
// the admin UI surfaces "Mobile optimization" instead of a fixed
// rate. TargetBits is 16 (uniform).
//
// Same infrastructure as Submit: Pool, ProjectedSize, disk-headroom
// preflight, batch row, RemainingIDs, enqueue truncation handling.
// Some duplication is accepted; a future refactor can consolidate
// once both paths are battle-tested.
// optimizeCandidate is the per-track work item produced by the
// optimize-batch selection pass. Same shape as upscale's internal
// candidate but carries `targetRate` per row (family-preserving).
type optimizeCandidate struct {
	path       string
	absPath    string
	size       int64
	mtimeNS    int64
	sampleRate int
	bits       int
	targetRate int
}

// optimizeCandidates is the aggregated result of `buildOptimizeCandidates`.
// Carries both the per-track candidates and the run-level counters the
// caller needs for the SubmitResult.
type optimizeCandidates struct {
	cands          []optimizeCandidate
	alreadyCovered int
	totalProjected int64
	// projectionsSeen is the pre-filter count of track projections
	// the helper walked. Lets the caller derive `skipped =
	// projectionsSeen − len(cands) − alreadyCovered` for persistence
	// on the upscale_batches.skipped_files column (v1.5 migration v9).
	// Distinct from len(cands) which is post-filter (only enqueue-
	// able tracks).
	projectionsSeen int
}

// SubmitOptimize is the CarPlay-targeted batch entry point.
// Pipeline: build candidates → disk-headroom preflight → insert batch
// row → enqueue jobs (with truncation handling). The pure-helpers
// split keeps cognitive complexity below the repo gate while
// preserving the structural pipeline upscale's `Submit` follows.
func (c *Coordinator) SubmitOptimize(ctx context.Context, path string, outputDir string) (*SubmitResult, error) {
	if c.resolver == nil {
		return nil, fmt.Errorf("submit optimize: no resolver wired — Coordinator can't build JobSpec absolute paths")
	}

	projections, err := c.store.ListTrackProjectionsUnderPrefix(ctx, path, manifest.VariantKindPrefixOptimized)
	if err != nil {
		return nil, fmt.Errorf("submit optimize: list projections: %w", err)
	}

	picked := c.buildOptimizeCandidates(path, projections)

	available, err := c.diskPreflight(outputDir, picked.totalProjected, "submit optimize")
	if err != nil {
		return nil, err
	}

	// Skip count = projections seen − enqueueable − already-covered.
	// Captures every `continue` arm of `buildOptimizeCandidates` in
	// one derivation rather than counting per-arm. Persisted on the
	// batch row for the Jobs page sub-line ("12 tracks skipped").
	skipped := picked.projectionsSeen - len(picked.cands) - picked.alreadyCovered
	if skipped < 0 {
		skipped = 0 // defensive — should be impossible
	}
	batchID, err := c.initOptimizeBatchState(ctx, path, picked.cands, skipped)
	if err != nil {
		return nil, err
	}

	// Empty-batch short-circuit: no candidates → no worker callback
	// will ever fire to transition the row out of `pending`. Mark
	// the batch completed synchronously so the admin row doesn't
	// sit `running` indefinitely. CodeRabbit bot review on PR #270.
	if len(picked.cands) == 0 {
		if err := c.transitionStatus(batchID, "completed", "", c.clock()); err != nil {
			c.logger.Warn("submit optimize: transition empty batch to completed",
				"batchID", batchID.String(), "err", err)
		}
		c.publishProgress(batchID)
		return &SubmitResult{
			BatchID:            batchID,
			Path:               path,
			TargetRate:         0,
			TargetBits:         16,
			TotalFiles:         0,
			AlreadyCovered:     picked.alreadyCovered,
			ProjectedSizeBytes: picked.totalProjected,
			AvailableBytes:     available,
			EnqueuedCount:      0,
		}, nil
	}

	if err := c.transitionStatus(batchID, "running", "", c.clock()); err != nil {
		c.logger.Warn("submit optimize: transition to running",
			"batchID", batchID.String(), "err", err)
	}
	enqueued := c.enqueueOptimizeJobs(batchID, picked.cands, outputDir)
	c.publishProgress(batchID)
	return &SubmitResult{
		BatchID:            batchID,
		Path:               path,
		TargetRate:         0, // per-track varies; admin surfaces "Mobile optimization"
		TargetBits:         16,
		TotalFiles:         enqueued,
		AlreadyCovered:     picked.alreadyCovered,
		ProjectedSizeBytes: picked.totalProjected,
		AvailableBytes:     available,
		EnqueuedCount:      enqueued,
	}, nil
}

// buildOptimizeCandidates filters projections through the optimize
// eligibility gate, resolves absolute paths, and accumulates the
// projected-size total. Resolver failures are logged-and-skipped
// (treated as "skip silently" by the caller). Pure helper — no
// side effects beyond logging.
func (c *Coordinator) buildOptimizeCandidates(batchPath string, projections []manifest.TrackProjection) optimizeCandidates {
	var (
		out            optimizeCandidates
		compressionFct = DefaultCompressionFactor(16)
		resolveErrors  int
	)
	// Capture pre-filter total so the caller can derive `skipped =
	// total − len(cands) − alreadyCovered` and persist it on the
	// batch row. Single source of truth — keeps the per-arm skip
	// classification out of the loop body.
	out.totalProjected = 0
	out.projectionsSeen = len(projections)
	for _, t := range projections {
		if t.HasVariant {
			out.alreadyCovered++
			continue
		}
		if t.SampleRate <= 0 || t.BitsPerSample <= 0 {
			continue
		}
		if t.IsDSD {
			// DSD is structurally excluded from CarPlay routing.
			continue
		}
		if !OptimizeEligible(t.Path, t.Codec, t.SampleRate, t.BitsPerSample) {
			continue
		}
		targetRate, terr := ResolveTargetRateForOptimize(t.SampleRate)
		if terr != nil {
			continue
		}
		absPath, err := c.resolver(t.Path)
		if err != nil {
			resolveErrors++
			c.logger.Warn("submit optimize: resolve failed; skipping track",
				"path", t.Path, "err", err)
			continue
		}
		out.cands = append(out.cands, optimizeCandidate{
			path:       t.Path,
			absPath:    absPath,
			size:       t.Size,
			mtimeNS:    t.MTimeNS,
			sampleRate: t.SampleRate,
			bits:       t.BitsPerSample,
			targetRate: targetRate,
		})
		out.totalProjected += ProjectedSize(t.Size, t.SampleRate, t.BitsPerSample,
			targetRate, 16, compressionFct)
	}
	if resolveErrors > 0 {
		c.logger.Info("submit optimize: filtered tracks with resolver failures",
			"batchPath", batchPath, "count", resolveErrors)
	}
	return out
}

// initOptimizeBatchState inserts the SQLite batch row and installs the
// in-memory `liveBatches` entry. Returns the freshly-minted batch ID.
// Caller transitions status separately.
func (c *Coordinator) initOptimizeBatchState(ctx context.Context, batchPath string, cands []optimizeCandidate, skipped int) (uuid.UUID, error) {
	// Propagate entropy failure instead of uuid.Must's panic (API
	// submission path; a transient CSPRNG error must not crash). Gemini r4.
	batchID, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate optimize batch uuid: %w", err)
	}
	now := c.clock().UnixNano()
	row := manifest.UpscaleBatchRow{
		ID:             batchID,
		Path:           batchPath,
		TargetRate:     0, // per-track varies
		TargetBits:     16,
		Status:         "pending",
		TotalFiles:     len(cands),
		ProcessedFiles: 0,
		FailedFiles:    0,
		SkippedFiles:   skipped,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := c.store.InsertUpscaleBatch(ctx, row); err != nil {
		return uuid.Nil, fmt.Errorf("submit optimize: insert batch row: %w", err)
	}
	state := &batchState{
		Row:          row,
		RemainingIDs: make(map[string]struct{}, len(cands)),
	}
	for _, ca := range cands {
		state.RemainingIDs[ca.path] = struct{}{}
	}
	c.mu.Lock()
	c.liveBatches[batchID] = state
	c.mu.Unlock()
	return batchID, nil
}

// enqueueOptimizeJobs drains the candidate list into the pool. On
// queue-full / pool-closed mid-batch, drops the never-enqueued tail
// from `RemainingIDs` and persists the truncated row so a partial
// enqueue still reaches a terminal status. Returns the count
// successfully enqueued.
func (c *Coordinator) enqueueOptimizeJobs(batchID uuid.UUID, cands []optimizeCandidate, outputDir string) int {
	enqueued := 0
	deduped := 0
	for _, ca := range cands {
		spec := JobSpec{
			SourceAbsPath:    ca.absPath,
			SourceLibraryRel: ca.path,
			SourceMTimeNS:    ca.mtimeNS,
			SourceSize:       ca.size,
			SourceSampleRate: ca.sampleRate,
			SourceBits:       ca.bits,
			TargetSampleRate: ca.targetRate,
			TargetBits:       16,
			Quality:          QualityVeryHigh,
			OutputDir:        outputDir,
			BatchID:          batchID,
			Kind:             JobKindOptimize,
		}
		err := c.pool.Enqueue(spec)
		if errors.Is(err, ErrDuplicateInflight) {
			c.dropDedupedPath(batchID, ca.path)
			deduped++
			continue
		}
		if err != nil {
			c.handleOptimizeEnqueueFailure(batchID, cands, ca.path, enqueued, err)
			break
		}
		enqueued++
	}
	// Fully deduped → complete now (mirrors Submit); gate on deduped == len(cands)
	// so a queue-full break doesn't spuriously complete a truncated batch.
	if deduped == len(cands) {
		if err := c.transitionStatus(batchID, "completed", "", c.clock()); err != nil {
			c.logger.Warn("optimize: transition fully-deduped batch",
				"batchID", batchID.String(), "err", err)
		}
	}
	return enqueued
}

// handleOptimizeEnqueueFailure drops the never-enqueued tail from the
// live batch state and persists the truncated row. Mirrors the
// truncation handling in `Submit` upscale path.
func (c *Coordinator) handleOptimizeEnqueueFailure(batchID uuid.UUID, cands []optimizeCandidate, failedPath string, enqueued int, failureErr error) {
	c.logger.Warn("submit optimize: enqueue failed; truncating batch",
		"batchID", batchID.String(), "path", failedPath, "err", failureErr)
	c.mu.Lock()
	var (
		rowSnapshot    manifest.UpscaleBatchRow
		stateModified  bool
		becameTerminal bool
	)
	if st, ok := c.liveBatches[batchID]; ok {
		dropping := false
		for _, c2 := range cands {
			if c2.path == failedPath {
				dropping = true
			}
			if dropping {
				delete(st.RemainingIDs, c2.path)
			}
		}
		// Set directly to the enqueued count (NOT `-= len(cands)-enqueued`) so it
		// stays correct after any earlier dedup drops — mirrors Submit's queue-full
		// path (line ~523). The `-=` form under-counts by the dedup total when a
		// dedup and a queue-full break co-occur in the same batch (CodeRabbit PR #515).
		st.Row.TotalFiles = enqueued
		st.Row.Error = "partial enqueue: " + failureErr.Error()
		st.Row.UpdatedAt = c.clock().UnixNano()
		if len(st.RemainingIDs) == 0 {
			st.Row.Status = "failed"
			becameTerminal = true
		}
		rowSnapshot = st.Row
		stateModified = true
		if becameTerminal {
			delete(c.liveBatches, batchID)
		}
	}
	c.mu.Unlock()
	if !stateModified {
		return
	}
	persistCtx, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
	if writeErr := c.store.UpdateUpscaleBatchProgress(persistCtx, rowSnapshot); writeErr != nil {
		c.logger.Warn("submit optimize: persist truncated batch",
			"batchID", batchID.String(), "err", writeErr)
	}
	cancelPersist()
	c.publishProgressRow(rowSnapshot)
}

// SetPublish wires (or rewires) the SSE broker emit closure.
// Construction-time callers may pass nil and call SetPublish later
// once the broker handle is in scope (the canonical wiring pattern
// in cmd/bridge/main.go — the Coordinator is built BEFORE the SSE
// broker so `RecoverInterruptedBatches` can run synchronously
// during startup). Concurrency-safe via the same coordinator mutex
// that guards the live-batch state.
func (c *Coordinator) SetPublish(publish CoordinatorPublishFunc) {
	c.mu.Lock()
	c.publish = publish
	c.mu.Unlock()
}

// Cancel transitions a batch to `cancelled` AND tries to drain the
// pool of its remaining jobs. The pool doesn't currently support
// targeted dequeue (jobs are FIFO; the next worker pulls whichever
// is at the head), so the practical effect is:
//
//   - any worker that started a job from this batch finishes it
//     (the variant lands on disk + in `track_variants`)
//   - any job still on the pool's pending channel runs when picked
//     up (variant also lands)
//   - the batch row is marked `cancelled` so the admin Jobs page
//     stops surfacing it as live
//
// "Cancel" therefore means "stop tracking; stop counting" rather
// than "kill in-flight." A future enhancement can add per-batch
// dequeue if operators need a harder stop.
func (c *Coordinator) Cancel(batchID uuid.UUID) error {
	return c.transitionStatus(batchID, "cancelled", "", c.clock())
}

// transitionStatus writes the new status + (optionally) the error
// message and updated_at into `upscale_batches`, drops the in-
// memory state when the new status is terminal, and emits an SSE
// progress event.
//
// Terminal statuses: completed / failed / cancelled / interrupted.
// `pending` and `running` keep the in-memory state alive for
// further pool callbacks.
func (c *Coordinator) transitionStatus(batchID uuid.UUID, status, errMsg string, at time.Time) error {
	c.mu.Lock()
	state, ok := c.liveBatches[batchID]
	if !ok {
		// Not live: already terminal, already cancelled, OR never
		// existed. Cancel is an idempotent no-op for all three — we don't
		// distinguish "unknown batch" (there's no cheap DB existence
		// check, and the HTTP handlers map every error to 500, not 404),
		// so cancelling a bogus/finished batchID succeeds silently.
		c.mu.Unlock()
		return nil
	}
	state.Row.Status = status
	if errMsg != "" {
		state.Row.Error = errMsg
	}
	state.Row.UpdatedAt = at.UnixNano()
	rowCopy := state.Row
	terminal := isTerminalStatus(status)
	if terminal {
		delete(c.liveBatches, batchID)
	}
	c.mu.Unlock()

	// No caller ctx for the internal callback-driven path, but this runs
	// on the single long-lived publisher goroutine — an unbounded SQLite
	// stall here would wedge every subsequent SSE state/complete event.
	// persistBounded applies the shared 5 s timeout. Gemini r4.
	if err := persistBounded(func(ctx context.Context) error {
		return c.store.UpdateUpscaleBatchStatus(ctx, rowCopy)
	}); err != nil {
		return fmt.Errorf("transition %s -> %s: %w", batchID, status, err)
	}
	c.publishProgressRow(rowCopy)
	return nil
}

// OnJobComplete is the Pool-side callback invoked when a sox
// invocation succeeded AND `UpsertVariant` committed. Signature
// matches the pool's `SetOnJobComplete` parameter exactly.
//
// `durationSeconds` is the wall-clock cost of the sox run (captured
// at worker startedAt → completedAt). Feeds the rolling throughput
// ring so ETAs reflect real bridge throughput; previously this
// path passed 0, leaving the ring empty even on a busy bridge
// (CodeRabbit high on PR #201).
//
// Bumps `processed_files` on the owning batch (no-op when batchID
// is zero — legacy single-track jobs that didn't go through Submit
// don't have a batch to attribute to). Transitions the batch to
// `completed` when all expected jobs have reported back.
func (c *Coordinator) OnJobComplete(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
	c.recordThroughputDuration(durationSeconds, completedAt)
	if batchID == uuid.Nil {
		return
	}
	c.mu.Lock()
	state, ok := c.liveBatches[batchID]
	if !ok {
		c.mu.Unlock()
		return
	}
	state.Row.ProcessedFiles++
	delete(state.RemainingIDs, path)
	state.Row.UpdatedAt = completedAt.UnixNano()
	allDone := len(state.RemainingIDs) == 0
	if allDone {
		// A completion event just incremented ProcessedFiles, so
		// FailedFiles < TotalFiles here — the batch is "completed" (some
		// tracks may have failed; FailedFiles carries that count and the
		// Jobs page renders it separately). A 100%-failure batch's last
		// event is a FAILURE, so it terminates in OnJobFailed, which sets
		// "failed" — this handler never observes an all-failed batch.
		state.Row.Status = "completed"
	}
	rowCopy := state.Row
	if allDone {
		delete(c.liveBatches, batchID)
	}
	c.mu.Unlock()

	// Bound the persist (publisher-goroutine path) via the shared
	// 5 s-timeout helper so a stalled SQLite write can't wedge later SSE
	// events. Gemini r4.
	if err := persistBounded(func(ctx context.Context) error {
		return c.store.UpdateUpscaleBatchProgress(ctx, rowCopy)
	}); err != nil {
		c.logger.Warn("OnJobComplete: update progress",
			"batchID", batchID.String(),
			"err", err)
	}
	c.publishProgressRow(rowCopy)
}

// OnJobFailed mirrors OnJobComplete for the failure side. Bumps
// `failed_files`; if every job in the batch has reported back and
// failed_files > 0, transitions the batch to `failed`.
func (c *Coordinator) OnJobFailed(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time) {
	c.recordThroughputDuration(durationSeconds, failedAt)
	if batchID == uuid.Nil {
		return
	}
	c.mu.Lock()
	state, ok := c.liveBatches[batchID]
	if !ok {
		c.mu.Unlock()
		return
	}
	state.Row.FailedFiles++
	delete(state.RemainingIDs, path)
	// Surface the first failure's error verbatim. The aggregate failure
	// count is carried separately in FailedFiles (the Jobs page renders
	// it), so Error stays the single first message rather than a
	// repetitive count-suffixed string.
	if state.Row.Error == "" {
		state.Row.Error = errMsg
	}
	state.Row.UpdatedAt = failedAt.UnixNano()
	allDone := len(state.RemainingIDs) == 0
	if allDone {
		if state.Row.FailedFiles == state.Row.TotalFiles {
			state.Row.Status = "failed"
		} else {
			state.Row.Status = "completed"
		}
	}
	rowCopy := state.Row
	if allDone {
		delete(c.liveBatches, batchID)
	}
	c.mu.Unlock()

	// Bound the persist (publisher-goroutine path) — see OnJobComplete.
	if err := persistBounded(func(ctx context.Context) error {
		return c.store.UpdateUpscaleBatchProgress(ctx, rowCopy)
	}); err != nil {
		c.logger.Warn("OnJobFailed: update progress",
			"batchID", batchID.String(),
			"err", err)
	}
	c.publishProgressRow(rowCopy)
}

// recordThroughputDuration is the primary entry; the per-job
// wall-clock seconds (from worker startedAt) is the authoritative
// per-sample value. A zero duration falls back to "fast-fail"
// semantics — the ring still advances so the window stays
// meaningful, but the sample contributes zero seconds.
func (c *Coordinator) recordThroughputDuration(durationSeconds float64, _ time.Time) {
	c.mu.Lock()
	c.throughputSamples[c.throughputCursor] = durationSeconds
	c.throughputCursor = (c.throughputCursor + 1) % throughputWindowSize
	c.throughputCount++
	c.mu.Unlock()
}

// Throughput returns the rolling average jobs-per-hour + a forward
// projection for the currently-live batches' remaining files. Both
// numbers are zero when fewer than `throughputMinSamples` samples
// have landed.
func (c *Coordinator) Throughput() ThroughputSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	samples := c.throughputCount
	if samples > throughputWindowSize {
		samples = throughputWindowSize
	}
	if samples < throughputMinSamples {
		return ThroughputSnapshot{Samples: samples}
	}
	var totalSec float64
	nonZero := 0
	for i := 0; i < samples; i++ {
		if c.throughputSamples[i] > 0 {
			totalSec += c.throughputSamples[i]
			nonZero++
		}
	}
	if nonZero == 0 {
		return ThroughputSnapshot{Samples: samples}
	}
	avgSeconds := totalSec / float64(nonZero)
	// Workers process in parallel — per-job seconds divided by worker
	// count gives the wall-clock time the operator actually waits
	// when the queue is full. JobsPerHour reflects total throughput
	// (jobs/hr across all workers), so it's already in the right
	// shape for an admin dashboard rate display.
	workers := 1
	if c.pool != nil {
		if n := c.pool.Stats().Workers; n > 0 {
			workers = n
		}
	}
	jobsPerHour := 3600.0 / avgSeconds * float64(workers)
	remaining := 0
	for _, st := range c.liveBatches {
		remaining += len(st.RemainingIDs)
	}
	etaSeconds := float64(remaining) * avgSeconds / float64(workers)
	return ThroughputSnapshot{
		JobsPerHour: jobsPerHour,
		EtaSeconds:  etaSeconds,
		Samples:     samples,
	}
}

// publishProgress emits a single SSE event for the given batchID.
// No-op when the row is no longer live in-memory (terminal status
// already published).
func (c *Coordinator) publishProgress(batchID uuid.UUID) {
	c.mu.Lock()
	state, ok := c.liveBatches[batchID]
	if !ok {
		c.mu.Unlock()
		return
	}
	row := state.Row
	c.mu.Unlock()
	c.publishProgressRow(row)
}

// publishProgressRow is the publish helper that takes a row copy
// (already snapshot under c.mu, released by the caller — every call
// site MUST drop the lock first). The publish func itself is
// re-snapshotted under c.mu here because SetPublish writes it under
// the same mutex; an unlocked read would race a post-construction
// rewire. The emit then runs outside the lock.
func (c *Coordinator) publishProgressRow(row manifest.UpscaleBatchRow) {
	c.mu.Lock()
	publish := c.publish
	c.mu.Unlock()
	if publish == nil {
		return
	}
	publish(BatchProgressEvent{
		BatchID:        row.ID.String(),
		Path:           row.Path,
		Status:         row.Status,
		TargetRate:     row.TargetRate,
		TargetBits:     row.TargetBits,
		TotalFiles:     row.TotalFiles,
		ProcessedFiles: row.ProcessedFiles,
		FailedFiles:    row.FailedFiles,
		Error:          row.Error,
		UpdatedAt:      time.Unix(0, row.UpdatedAt).UTC(),
	})
}

// persistBounded runs a callback-path DB write under a 5 s timeout.
// These writes fire on the long-lived publisher goroutine with no
// caller ctx, so an unbounded SQLite stall would wedge every later SSE
// event; the deferred cancel releases the timer the instant the write
// returns (no timer pile-up under a completion burst). Shared by
// transitionStatus / OnJobComplete / OnJobFailed so the timeout
// boilerplate isn't copy-pasted three times (Gemini r4 defer-cancel +
// Sonar new-code-duplication). The two Submit-path persists keep their
// inline form — different row snapshot + gating shape.
func persistBounded(write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return write(ctx)
}

// isTerminalStatus reports whether the given status indicates the
// batch has reached a final resting state — no further callbacks
// should attribute to it.
func isTerminalStatus(s string) bool {
	switch s {
	case "completed", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

// silence unused-context noise; `context.Context` is on Submit's
// signature for forward-compat (the future per-batch cancel-via-
// context surface) even though current Submit doesn't consult it.
var _ = context.TODO
