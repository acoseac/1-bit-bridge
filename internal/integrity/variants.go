// Package integrity contains the bridge's proactive consistency
// watchers — long-lived goroutines that walk durable state on a
// schedule and reconcile drift between what the bridge thinks
// exists (SQLite manifest, track_variants table) and what
// actually exists on disk.
//
// The library-source-file watcher lives in internal/manifest's
// Scanner.RunPeriodic (6 h default, plus optional fsnotify). The
// upscale-variant watcher lives here. Both pair with the
// equivalent reactive paths in internal/api (download serving
// stat-on-open; the manifest scanner's per-walk diff) — the
// schedulers handle the "operator did something while the
// bridge wasn't looking" cases.
package integrity

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("integrity")

// VariantWatcher walks the track_variants table on a cadence
// configurable via cfg.Integrity.VariantSweepInterval (default
// 1 h) and reconciles rows whose sidecar file no longer exists
// on disk. Reasons a sidecar might disappear outside the
// bridge: operator `rm -rf <DataDir>/transcoded/`, backup
// software with eager retention, disk-image rebuild that
// preserved the SQLite DB but not the sidecar tree.
//
// On every miss, the row is removed via the supplied Deleter
// (bumps `tracks.indexed_at` so iOS delta-sync sees the
// disappearance) AND a single batched `upscale.deleted` SSE
// event is published per tick — iOS reconciles immediately
// without waiting for a manifest re-sync.
//
// Threading: one long-lived goroutine spun up by Start; stops
// on the supplied ctx's cancellation. Time.NewTicker is reset
// on every tick (we use a manual select loop) so the first
// sweep fires immediately at boot — closes the "operator
// deleted variant files while the bridge was down" case
// without waiting for the first interval to elapse.
// stopGrace bounds how long a stopFn waits for its run goroutine to return.
//
// A var, not a const, so the tests can shorten it — and shared by both
// long-lived loops in this package so there is one answer to "how long does
// shutdown wait for integrity work". Bounded rather than unconditional for the
// reason every other wait in this tree is: a wedged tick must degrade to a log
// line, never a hung process exit.
var stopGrace = 5 * time.Second

type VariantWatcher struct {
	lister      VariantLister
	deleter     VariantDeleter
	publish     PublishFunc
	variantsDir string
	interval    time.Duration

	// onTickComplete fires after every full sweep completes;
	// the test harness wires this to drive deterministic sync
	// without polling the watcher's internal state. nil in
	// production — Go's linker drops the call when unused.
	onTickComplete func(deletedCount int)

	// startOnce + stopOnce + done live on the struct (NOT as
	// locals inside Start) so a hypothetical second Start
	// returns a stopFn that closes the SAME channel the
	// original run goroutine selects on. Pre-fix `done` was
	// `Start`-local: a second Start's stopFn closed a fresh
	// channel and couldn't reach the active loop. CodeRabbit
	// Major on PR #209.
	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
	// exited is closed when the run goroutine returns, so stopFn can
	// JOIN it rather than merely signalling. See Start.
	exited chan struct{}
}

// VariantLister enumerates every row in the track_variants
// table. The watcher consumes the snapshot per tick and
// stats each sidecar path. Mirrors the manifest.Store
// method shape; cmd/bridge wires the adapter.
type VariantLister interface {
	AllVariants() ([]VariantSnapshot, error)
}

// VariantDeleter removes one variant row by (source_path,
// variant_id). The Store's DeleteVariant transactionally
// bumps `tracks.indexed_at` so iOS delta-sync observes the
// removal on the next manifest fetch. Per-row error tolerance:
// a Watcher tick logs and continues on per-row failure, but
// still publishes the events for the rows that DID delete.
type VariantDeleter interface {
	DeleteVariant(sourcePath, variantID string) error
}

// PublishFunc is the domain-specific publish callback fired
// once per sweep that observed at least one missing sidecar.
// The integrity package keeps the wire-shape construction at
// the cmd/bridge wiring layer so it can build the typed
// api.UpscaleDeletedEvent without an upward import cycle —
// the integrity package only knows "I observed these
// disappearances", not the broker's serialization concerns.
//
// `paths` and `variantIDs` are positional but NOT zipped 1:1:
// callers treat them as the set of paths affected AND the
// set of variantIDs that disappeared somewhere in those
// paths. Same semantic the api.UpscaleDeletedEvent struct
// already documents.
type PublishFunc func(paths []string, variantIDs []string)

// VariantSnapshot is the integrity-package-local projection of
// one track_variants row. Mirrors `api.VariantSummary` but
// stays internal to integrity so the package doesn't import
// internal/api (which would create an upward dependency cycle
// — api consumes integrity events via the broker, not the
// other way round).
type VariantSnapshot struct {
	SourcePath  string
	VariantID   string
	SidecarPath string
}

// NewVariantWatcher constructs a watcher. interval ≤ 0 disables
// the watcher entirely — Start returns a no-op stopFn. Used by
// operators on minimal deploys who only run `--gc` manually.
//
// `variantsDir` is the effective variants output directory
// (`cfg.Upscale.EffectiveVariantsDir`) the sidecar paths live
// under. Before every sweep the watcher probes it via
// VariantsDirSweepBlockReason and skips the whole tick when the
// directory is missing or empty while rows exist — the signature
// of a cleanly-unmounted variants volume, where every per-row
// stat would report ENOENT and an unguarded sweep would
// mass-delete the catalog (2026-07-21 review H4). An empty
// variantsDir disables the guard (legacy unconditional sweep).
func NewVariantWatcher(lister VariantLister, deleter VariantDeleter, publish PublishFunc, variantsDir string, interval time.Duration) *VariantWatcher {
	return &VariantWatcher{
		lister:      lister,
		deleter:     deleter,
		publish:     publish,
		variantsDir: variantsDir,
		interval:    interval,
	}
}

// SetOnTickComplete is a test-only seam. Production wires nil.
// Same convention as transcode.Pool's SetOnStateChange — the
// test harness can register a callback once at construction
// without exposing internal channels.
func (w *VariantWatcher) SetOnTickComplete(fn func(deletedCount int)) {
	w.onTickComplete = fn
}

// Start spins up the long-lived sweep goroutine and returns a
// stopFn the caller `defer`s on shutdown. The goroutine fires
// one immediate sweep at boot, then ticks every `interval`. A
// cancelled context AND the returned stopFn both cleanly stop
// the loop; either is sufficient (they're equivalent paths).
//
// Idempotent: a duplicate Start returns a stopFn that closes
// the SAME `w.done` channel the active run goroutine is
// selecting on. Both `startOnce` and `stopOnce` live on the
// struct so a second Start's stopFn doesn't pointlessly close
// a fresh per-call channel the run loop never sees. Calling
// Start with interval ≤ 0 returns a no-op stopFn — no
// goroutine is spawned at all (avoids the per-process resource
// cost on minimal deploys).
func (w *VariantWatcher) Start(ctx context.Context) (stopFn func()) {
	if w == nil || w.interval <= 0 {
		return func() {
			// No-op stopFn — Start was a no-op (interval ≤ 0
			// disables the watcher entirely; see docstring above),
			// so there's nothing to stop.
		}
	}
	w.startOnce.Do(func() {
		w.done = make(chan struct{})
		w.exited = make(chan struct{})
		go func() {
			defer close(w.exited)
			w.run(ctx, w.done)
		}()
	})
	return func() {
		w.stopOnce.Do(func() {
			// Cancel via the done channel — the run loop
			// selects on both ctx.Done() AND `done`, so the
			// caller can short-circuit even when the ctx
			// is the long-lived process-root context.
			if w.done != nil {
				close(w.done)
			}
			// JOIN, grace-bounded. Signalling alone left the caller free to
			// close the manifest store while a tick was mid-DeleteVariant —
			// the "database is closed" / SQLite-corruption class runServe's
			// bgWriters wait exists to prevent. cmd/bridge defers this stop
			// ahead of Store.Close, so waiting here is what makes that
			// ordering mean anything.
			//
			// Bounded rather than unconditional: a wedged tick degrades to a
			// log line, never a hung exit, matching the shutdown discipline
			// everywhere else in this tree.
			if w.exited != nil {
				t := time.NewTimer(stopGrace)
				defer t.Stop()
				select {
				case <-w.exited:
				case <-t.C:
				}
			}
		})
	}
}

// run is the watcher goroutine body. One tick at boot, then
// `interval`-spaced ticks until ctx cancels OR stopFn closes
// `done`. Per-tick errors log but never abort the loop —
// transient SQLite hiccups shouldn't permanently disable the
// watcher.
func (w *VariantWatcher) run(ctx context.Context, done chan struct{}) {
	// Immediate sweep at boot covers the "operator deleted
	// variants while the bridge was down" case without
	// waiting `interval` for the first sweep.
	deleted := w.tick(ctx)
	if w.onTickComplete != nil {
		w.onTickComplete(deleted)
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			deleted := w.tick(ctx)
			if w.onTickComplete != nil {
				w.onTickComplete(deleted)
			}
		}
	}
}

// tick performs one full sweep. Returns the count of rows
// removed (NOT the count of misses observed — a stat-but-
// delete-failed row counts 0). Logs WARN on per-row stat /
// delete failures; logs ERROR only on the outer AllVariants
// query failure (the only path where we can't even start).
// Skips wholesale (WARN, 0 deletions) when the variants dir
// probe reports missing/empty with rows in the catalog — see
// NewVariantWatcher and VariantsDirSweepBlockReason.
func (w *VariantWatcher) tick(ctx context.Context) int {
	rows, err := w.lister.AllVariants()
	if err != nil {
		logger.Error("integrity variant sweep: AllVariants failed",
			slog.Any("err", err),
		)
		return 0
	}
	if len(rows) == 0 {
		return 0
	}
	// Mount-loss guard: rows exist but the whole variants dir is
	// missing or empty → the volume is almost certainly unmounted
	// (a clean unmount reverts the mountpoint to an empty local
	// dir), NOT a library whose every sidecar was individually
	// deleted. Skip the sweep rather than mass-deleting the
	// catalog on per-row ENOENTs. Probed per tick so a later
	// unmount is caught even after healthy ticks. Shares the
	// helper with `bridge upscale --gc`'s reverse-sweep guard.
	if w.variantsDir != "" {
		if reason := VariantsDirSweepBlockReason(w.variantsDir); reason != "" {
			logger.Warn("integrity variant sweep: skipping sweep, variants dir unhealthy with rows in catalog",
				slog.String("variants_dir", w.variantsDir),
				slog.String("reason", reason),
				slog.Int("rows", len(rows)),
			)
			return 0
		}
	}
	// `paths` is the deduplicated set of affected source paths;
	// `variantIDs` is the (potentially repeating) set of deleted
	// variantIDs. Per the upscale.deleted contract documented in
	// internal/api/upscale_deleted_event.go: `Paths` and
	// `VariantIDs` are NOT zipped 1:1, just the union of what
	// disappeared. Dedup paths so a track with multiple missing
	// variants (rare but legitimate — e.g. 96k + 192k variants
	// for the same source both wiped by an external rm) doesn't
	// emit the same path twice in the SSE payload. CodeRabbit
	// Minor on PR #209.
	var (
		paths       []string
		variantIDs  []string
		pathsSeen   = make(map[string]struct{})
		deletedRows int
	)
	for _, r := range rows {
		// Honour cancellation between rows so a shutdown
		// during a long sweep on a large library doesn't
		// hold the process up for minutes.
		select {
		case <-ctx.Done():
			return deletedRows
		default:
		}
		_, statErr := os.Stat(r.SidecarPath)
		if statErr == nil {
			continue
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			// Permission errors, I/O faults, etc. — log
			// and skip rather than treating as "missing".
			// `--gc` reverse-pass behaves the same way.
			logger.Warn("integrity variant sweep: stat failed",
				slog.String("sidecar", r.SidecarPath),
				slog.String("variant_id", r.VariantID),
				slog.Any("err", statErr),
			)
			continue
		}
		if delErr := w.deleter.DeleteVariant(r.SourcePath, r.VariantID); delErr != nil {
			logger.Warn("integrity variant sweep: DB delete failed",
				slog.String("source_path", r.SourcePath),
				slog.String("variant_id", r.VariantID),
				slog.Any("err", delErr),
			)
			continue
		}
		deletedRows++
		variantIDs = append(variantIDs, r.VariantID)
		if _, seen := pathsSeen[r.SourcePath]; !seen {
			pathsSeen[r.SourcePath] = struct{}{}
			paths = append(paths, r.SourcePath)
		}
	}
	if len(paths) > 0 && w.publish != nil {
		// Single batched callback per sweep — iOS
		// reconciles all affected tracks in one pass
		// rather than fielding N separate event hops.
		w.publish(paths, variantIDs)
	}
	return deletedRows
}
