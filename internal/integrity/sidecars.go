package integrity

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// gcChunkSize bounds the number of filesystem entries the orphan
// sidecar sweeper processes per tick. Operators on libraries with
// hundreds of thousands of variant files get a chunked sweep
// (multiple ticks to fully cover the tree) rather than one
// long-running pass that could compete with library-scan I/O.
//
// 5000 trades off two concerns: each tick processes a meaningful
// slice in well under 100 ms wall-clock on a warm-cache SSD walk
// (~5-10 µs per entry once the OS dirent cache is hot), AND a
// 100k-variant library completes one full sweep in ~20 ticks
// rather than the ~1000 ticks the original 100-floor required.
// Pre-fix the chunk-size-100 + per-tick AllSidecarPaths SELECT
// produced O(N × N/chunk) total DB read on every full sweep — for
// a 100k-variant library, ~100M rows per cycle. Gemini medium on
// PR #282 caught the quadratic blow-up.
//
// `filepath.WalkDir` is single-threaded so the per-tick budget is
// strictly bounded by chunk × per-entry-cost — at 5000 entries +
// 50 µs/entry (cold cache, NTFS / exFAT on USB-attached spinning
// rust, the pathological tier) the per-tick wall-clock tops out
// around 250 ms. That still leaves the next ticker fire (interval
// is operator-configured, typically minutes to hours) with full
// headroom for library scans + serving.
//
// Pure constant, not configurable — the operator-facing knob is
// the SWEEP CADENCE (`cfg.Integrity.OrphanSidecarSweepIntervalSec`),
// not the chunk size. A future review that wants per-deploy
// chunk tuning should add it as a sibling config field rather
// than ripping this out, so the default-shape semantics stay
// stable for existing operators.
const gcChunkSize = 5000

// gcGracePeriod gates orphan detection on file modification time:
// files newer than this threshold are skipped during the sweep so a
// concurrent `UpsertVariant` writer that lands the sidecar on disk
// BEFORE its row commits to the manifest store doesn't get treated
// as orphan and unlinked behind its in-flight transaction. Gemini
// HIGH on PR #282 caught the race — SQLite WAL gives the SELECT a
// consistent snapshot, but the snapshot reflects state AT THE MOMENT
// the SELECT runs, while the filesystem walk happens AFTER. The
// window between `INSERT INTO track_variants` (file already on
// disk) and the COMMIT (snapshot now includes the row) is bounded
// by the transaction duration — typically <100 ms even on slow
// SQLite hosts, but a contending writer could push it to seconds.
//
// 10 minutes is chosen as the safe-by-construction overshoot:
// orders of magnitude longer than any plausible transaction
// window, short enough that an actually-orphan file lingers for
// at most one extra sweep cycle (operator-tolerable for the opt-in
// feature), and uniform across deploys regardless of disk speed.
//
// **Test seam**: production reads the constant; the
// `gracePeriodForTest` field on OrphanSidecarSweeper overrides it
// per-instance so the regression test can use a millisecond-scale
// grace without sleeping 10 minutes. Same DI shape `Pool.runner`
// uses for the sox subprocess.
const gcGracePeriod = 10 * time.Minute

// OrphanSidecarSweeper walks `outputDir/transcoded/` on a cadence
// (configured via `cfg.Integrity.OrphanSidecarSweepIntervalSec`)
// and unlinks `.flac` files whose absolute path is NOT present in
// the current `track_variants.sidecar_path` snapshot. The forward
// half of the operator-triggered `bridge upscale --gc` sweep,
// which `VariantWatcher` (in variants.go) does NOT cover — that
// type handles the REVERSE direction (rows whose sidecar file
// disappeared on disk).
//
// **Disabled by default** — opt-in via a non-zero interval. The
// existing operator workflow of "run `--gc` manually when storage
// gets tight" stays correct; this knob exists for the
// hands-off-operator profile.
//
// **Snapshot-then-walk** (NOT walk-then-snapshot): the sweeper
// takes the `track_variants.sidecar_path` projection BEFORE the
// filesystem walk so a concurrent `UpsertVariant` writer cannot
// produce a false-positive orphan (under the reverse order, the
// new sidecar lands on disk BEFORE the new row is in the snapshot
// — and the sweeper would unlink the file behind a row that
// hasn't yet rolled into its view). SQLite WAL mode gives every
// SELECT a consistent snapshot natively, so `AllSidecarPaths` is
// safe to call without an explicit transaction wrapper.
//
// **Chunked walking**: at most `gcChunkSize` files are stat'd /
// unlinked per tick. Operators on libraries with hundreds of
// thousands of variants get steady progress across multiple ticks
// rather than one long-running pass that competes with library
// scanning. Per-tick wall-clock stays bounded.
//
// **Walk pointer survives across ticks**: when a tick hits the
// chunk cap, the next tick's walk picks up from where the prior
// tick stopped via filename-relative-ordering — `filepath.Walk`
// is deterministic in lexical order, so we can track the
// `lastProcessedPath` cursor and skip-until on the next pass.
// Pre-cursor the sweeper would re-walk the same first 100 files
// every tick forever on libraries with >100 sidecars.
//
// Threading: one long-lived goroutine spun up by Start; stops on
// ctx cancellation OR the stopFn closing the done channel.
// Mirrors `VariantWatcher` exactly so cmd/bridge's wiring +
// shutdown ordering treats both watchers symmetrically.
type OrphanSidecarSweeper struct {
	lister    SidecarLister
	outputDir string
	interval  time.Duration

	// lastProcessedPath is the cursor across chunked ticks. The
	// next tick starts its walk at the first path > this value.
	// Empty string at boot → start from the beginning.
	//
	// Owned by the run goroutine; no concurrent reads (the test
	// seam below is the only out-of-run accessor and it's
	// gated through SetOnTickComplete which fires from the run
	// goroutine itself).
	lastProcessedPath string

	// onTickComplete is a test-only seam — same convention as
	// VariantWatcher.SetOnTickComplete. Fires AFTER the per-tick
	// stats are computed; tests use it to drive deterministic
	// sync without polling internal state.
	onTickComplete func(unlinked int)

	// gracePeriodForTest overrides gcGracePeriod when positive. The
	// regression test for the race-condition contract injects a
	// millisecond-scale grace; the unrelated tests that just need
	// "no grace floor" set it to `1 * time.Nanosecond`. Same DI
	// shape as onTickComplete + the `Pool.runner` precedent.
	//
	// Zero (default) → production constant. Negative → also production
	// constant (defensive against accidental negative).
	gracePeriodForTest time.Duration

	// chunkSizeForTest overrides gcChunkSize when positive. Lets the
	// chunk-cap regression test exercise the cursor / split-tick
	// behaviour with a small chunk (~100) instead of seeding the
	// production chunk size (5000) of files. Production leaves it
	// zero.
	chunkSizeForTest int

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
}

// effectiveGracePeriod returns the per-instance grace override when
// the test seam is set, otherwise the production constant. Pure
// helper for the inline use inside `tick`.
func (s *OrphanSidecarSweeper) effectiveGracePeriod() time.Duration {
	if s.gracePeriodForTest > 0 {
		return s.gracePeriodForTest
	}
	return gcGracePeriod
}

// effectiveChunkSize returns the per-instance chunk size override
// when the test seam is set, otherwise the production constant.
// Pure helper for the inline use inside `tick`.
func (s *OrphanSidecarSweeper) effectiveChunkSize() int {
	if s.chunkSizeForTest > 0 {
		return s.chunkSizeForTest
	}
	return gcChunkSize
}

// SidecarLister is the integrity-package-local read surface for
// `track_variants.sidecar_path` projection. `manifest.Store` will
// be wired via a thin adapter in cmd/bridge; the explicit interface
// lets tests inject fakes without spinning a real SQLite store.
//
// Returns a SET of sidecar paths (`map[string]struct{}` for O(1)
// lookup against thousands of filesystem entries during a walk).
// Bare `[]string` was rejected: per-file lookup against a slice is
// O(n) and a 50k-variant library would O(n²)-walk on every tick.
type SidecarLister interface {
	AllSidecarPaths(ctx context.Context) (map[string]struct{}, error)
}

// NewOrphanSidecarSweeper constructs a sweeper. interval ≤ 0
// disables the sweeper entirely — Start returns a no-op stopFn.
// Used by operators on minimal deploys who run `bridge upscale --gc`
// manually.
//
// `outputDir` is the absolute path of the variant tree to walk.
// Typically `<cfg.DataDir>/transcoded/` resolved via
// `cfg.Upscale.EffectiveVariantsDir`. The sweeper does NOT
// re-resolve this per tick — a config edit that moves the variants
// directory at runtime requires a bridge restart for the change to
// take effect (same operational shape as VariantWatcher's interval).
func NewOrphanSidecarSweeper(lister SidecarLister, outputDir string, interval time.Duration) *OrphanSidecarSweeper {
	return &OrphanSidecarSweeper{
		lister:    lister,
		outputDir: outputDir,
		interval:  interval,
	}
}

// SetOnTickComplete is a test-only seam mirroring
// VariantWatcher.SetOnTickComplete. Production wires nil; Go's
// linker drops the call when unused.
func (s *OrphanSidecarSweeper) SetOnTickComplete(fn func(unlinked int)) {
	s.onTickComplete = fn
}

// Start spins up the long-lived sweep goroutine and returns a
// stopFn the caller `defer`s on shutdown. The goroutine fires one
// immediate sweep at boot, then ticks every `interval`. A cancelled
// ctx AND the returned stopFn both cleanly stop the loop.
//
// Idempotent: a duplicate Start returns a stopFn that closes the
// SAME `s.done` channel the active run goroutine selects on.
// `startOnce` + `stopOnce` mirror the VariantWatcher fix from
// CodeRabbit Major on PR #209.
//
// Interval ≤ 0 returns a no-op stopFn — no goroutine is spawned at
// all. This is the production "disabled by default" path; explicit
// zero in the YAML config opts out.
func (s *OrphanSidecarSweeper) Start(ctx context.Context) (stopFn func()) {
	if s == nil || s.interval <= 0 {
		return func() {
			// No-op stopFn — see Start docstring.
		}
	}
	s.startOnce.Do(func() {
		s.done = make(chan struct{})
		go s.run(ctx, s.done)
	})
	return func() {
		s.stopOnce.Do(func() {
			if s.done != nil {
				close(s.done)
			}
		})
	}
}

// run is the sweeper goroutine body. One tick at boot, then
// `interval`-spaced ticks until ctx cancels OR stopFn closes
// `done`. Per-tick errors log at WARN/ERROR but never abort the
// loop — transient SQLite hiccups or filesystem unmounts shouldn't
// permanently disable the sweeper.
func (s *OrphanSidecarSweeper) run(ctx context.Context, done chan struct{}) {
	unlinked := s.tick(ctx)
	if s.onTickComplete != nil {
		s.onTickComplete(unlinked)
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			unlinked := s.tick(ctx)
			if s.onTickComplete != nil {
				s.onTickComplete(unlinked)
			}
		}
	}
}

// tick performs one chunked sweep. Returns the count of files
// unlinked (NOT the count of orphans observed — a stat-but-unlink-
// failed file counts 0). Logs WARN on per-file unlink failures;
// logs ERROR only on the outer snapshot fetch failure.
//
// **Uses `filepath.WalkDir`** (NOT `filepath.Walk`) so the per-entry
// callback receives a cheap `fs.DirEntry` instead of `fs.FileInfo`
// — `os.Stat`-per-entry is deferred until we actually need ModTime
// for the grace-period check, AND `WalkDir` natively handles
// `filepath.SkipAll` as the "stop the walk cleanly" sentinel.
// `filepath.Walk` (legacy form) treats SkipAll as a generic error
// and logs the misleading "walk aborted" warning — caught by Gemini
// HIGH on PR #282.
//
// **Grace-period gate**: files modified within `effectiveGracePeriod()`
// of the tick start are skipped. Closes the race between
// `UpsertVariant` writers (file on disk before row commit) and the
// sweeper — without it, a sweep that takes its snapshot DURING the
// writer's transaction window would treat the brand-new sidecar as
// orphan and unlink it.
//
// Walk order is lexical via filepath.WalkDir; the `lastProcessedPath`
// cursor lets successive ticks pick up where the prior tick
// stopped. The cursor resets to empty when the walk completes
// without hitting the chunk cap — that's the "we've covered the
// whole tree this tick, next tick should start over" signal.
func (s *OrphanSidecarSweeper) tick(ctx context.Context) int {
	known, err := s.lister.AllSidecarPaths(ctx)
	if err != nil {
		logger.Error("orphan sidecar sweep: AllSidecarPaths failed",
			slog.Any("err", err),
		)
		return 0
	}

	grace := s.effectiveGracePeriod()
	chunkSize := s.effectiveChunkSize()
	tickStart := time.Now()
	walkStartCursor := s.lastProcessedPath
	var (
		examined    int
		unlinked    int
		hitChunkCap bool
		newCursor   string
	)

	err = filepath.WalkDir(s.outputDir, func(path string, d fs.DirEntry, walkErr error) error {
		// Honour cancellation between entries — a shutdown
		// during a long walk on a multi-TB variant tree should
		// return promptly.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if walkErr != nil {
			// Per-entry I/O fault (permission denied, transient
			// unmount, etc.) — log + continue. Caller's overall
			// loop tolerates this.
			logger.Warn("orphan sidecar sweep: walk entry error",
				slog.String("path", path),
				slog.Any("err", walkErr),
			)
			return nil
		}
		// Cheap pre-check via DirEntry (no stat syscall) — directories
		// + non-FLAC entries skip without touching the filesystem.
		// This is the load-bearing reason WalkDir wins over Walk on a
		// 100k-variant library: most entries don't need ModTime.
		if d.IsDir() || !shouldConsiderSidecarFile(path) {
			return nil
		}
		// Skip past the cursor: only process entries whose lexical
		// order is STRICTLY GREATER than the prior tick's last
		// processed path. Empty cursor = start of tree.
		if walkStartCursor != "" && path <= walkStartCursor {
			return nil
		}

		examined++
		// Every entry past the cursor advances the next-tick
		// cursor, even when the entry was a known sidecar
		// (otherwise the cursor would stall on the first known
		// file and we'd re-walk every previous tick's set).
		newCursor = path

		if _, isKnown := known[path]; isKnown {
			// Sidecar is in the DB — nothing to do. No ModTime read
			// needed; the known-set check already proved consistency.
			if examined >= chunkSize {
				hitChunkCap = true
				return filepath.SkipAll
			}
			return nil
		}

		// Candidate orphan: present on disk, NOT in track_variants.
		// Before unlinking, gate on file ModTime — a concurrent
		// UpsertVariant writer could have landed this file in the
		// gap between the SELECT snapshot above and the walk below,
		// and unlinking it would corrupt the writer's in-flight
		// transaction. Files newer than the grace period stay put;
		// the next sweep cycle re-evaluates them once the writer's
		// row has had time to commit.
		info, infoErr := d.Info()
		if infoErr != nil {
			logger.Warn("orphan sidecar sweep: stat failed",
				slog.String("path", path),
				slog.Any("err", infoErr),
			)
			// Don't unlink a file we can't stat — same defensive
			// shape the existing VariantWatcher uses for stat
			// failures (skip and continue).
			if examined >= chunkSize {
				hitChunkCap = true
				return filepath.SkipAll
			}
			return nil
		}
		if tickStart.Sub(info.ModTime()) < grace {
			// Too new to risk unlinking — UpsertVariant writer may
			// still be in flight. Skip; next sweep re-checks.
			if examined >= chunkSize {
				hitChunkCap = true
				return filepath.SkipAll
			}
			return nil
		}

		// Confirmed orphan: old enough to rule out the writer race.
		if rmErr := os.Remove(path); rmErr != nil {
			logger.Warn("orphan sidecar sweep: unlink failed",
				slog.String("path", path),
				slog.Any("err", rmErr),
			)
		} else {
			unlinked++
			logger.Info("orphan sidecar sweep: unlinked orphan",
				slog.String("path", path),
			)
		}
		if examined >= chunkSize {
			hitChunkCap = true
			return filepath.SkipAll
		}
		return nil
	})

	if err != nil {
		// Walk-level error (root missing, ctx cancellation that
		// propagated up). Log at WARN — the sweeper can resume
		// on the next tick.
		logger.Warn("orphan sidecar sweep: walk aborted",
			slog.String("outputDir", s.outputDir),
			slog.Any("err", err),
		)
	}

	// Cursor update: if we hit the chunk cap, the next tick
	// resumes after `newCursor`. Otherwise we've walked the
	// entire tree past the prior cursor — reset to empty so
	// the next tick starts fresh from the top.
	if hitChunkCap {
		s.lastProcessedPath = newCursor
	} else {
		s.lastProcessedPath = ""
	}

	logger.Info("orphan sidecar sweep: tick complete",
		slog.Int("examined", examined),
		slog.Int("unlinked", unlinked),
		slog.Bool("hit_chunk_cap", hitChunkCap),
		slog.String("next_cursor", s.lastProcessedPath),
	)
	return unlinked
}

// shouldConsiderSidecarFile is the pure-helper predicate that
// decides whether a filesystem entry observed during the walk is a
// candidate for orphan-check. Today the answer is "any `.flac`
// extension"; the operator-triggered `--gc` uses the same shape
// (see cmd/bridge/upscale.go::runGCForwardSweep). Extracted as a
// pure function for unit testing without a real walk.
//
// Future variant formats (FLAC-only today; opus / wavpack are
// hypothetical follow-ups) would extend the predicate rather than
// adding a parallel function — keeps the policy decision in one
// place.
func shouldConsiderSidecarFile(path string) bool {
	return filepath.Ext(path) == ".flac"
}
