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
	"log/slog"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("integrity")

// stopGrace bounds how long a stopFn waits for its run goroutine to return.
//
// A var, not a const, so the tests can shorten it — and shared by both
// long-lived loops in this package so there is one answer to "how long does
// shutdown wait for integrity work". Bounded rather than unconditional for the
// reason every other wait in this tree is: a wedged tick must degrade to a log
// line, never a hung process exit.
var stopGrace = 5 * time.Second

// VariantWatcher walks the track_variants table on a cadence
// configurable via cfg.Integrity.VariantSweepInterval (default
// 1 h) and reconciles rows whose sidecar file no longer exists
// on disk. Reasons a sidecar might disappear outside the
// bridge: operator `rm -rf <DataDir>/transcoded/`, backup
// software with eager retention, disk-image rebuild that
// preserved the SQLite DB but not the sidecar tree.
//
// A miss at the RECORDED path is not yet a disappearance. The
// sweep first asks LocateSidecar (locate.go) whether the file
// sits at its canonical place under the CURRENT variants dir
// with the recorded size — the state every row is in after the
// database and the tree move hosts together — and if so ADOPTS
// the row (rewrites `sidecar_path`; no `indexed_at` bump, nothing
// on the wire, because nothing changed for a client). Only a row
// whose file is at neither location is removed via the supplied
// reconciler (bumps `tracks.indexed_at` so iOS delta-sync sees
// the disappearance), and a single batched `upscale.deleted` SSE
// event is published per tick — iOS reconciles immediately
// without waiting for a manifest re-sync.
//
// Two guards sit between "missing" and "deleted". The mount-loss
// guard (VariantsDirSweepBlockReason) skips the whole tick when
// the directory is gone or empty. The relocation guard
// (MassDeleteRefusal) skips the DELETIONS of a tick that would
// reap more than cfg.Integrity.VariantSweepMaxDeletePercent of
// the catalog while the directory still holds sidecar files —
// the 2026-09-20 shape, where the directory was healthy and full
// and every row still pointed at the old host's path. Adoptions
// are applied either way; they are never the dangerous half.
//
// Every tick that saw rows logs ONE summary line (rows / present
// / adopted / deleted / mismatched / failed / refused) at
// Info, at Warn when it deleted or refused anything — the field
// report's first finding was that 10,248 deletions produced no
// line at all.
//
// Threading: one long-lived goroutine spun up by Start; stops
// on the supplied ctx's cancellation. Time.NewTicker is reset
// on every tick (we use a manual select loop) so the first
// sweep fires immediately at boot — closes the "operator
// deleted variant files while the bridge was down" case
// without waiting for the first interval to elapse.
type VariantWatcher struct {
	lister     VariantLister
	reconciler VariantReconciler
	publish    PublishFunc
	// variantsDir resolves the effective variants directory for the
	// mount-loss guard AND the relocation probe, and is asked PER
	// TICK — see NewVariantWatcher.
	variantsDir func() string
	interval    time.Duration
	// maxDeletePercent is the relocation guard's threshold
	// (cfg.Integrity.VariantSweepMaxDeletePercent); see MassDeleteRefusal.
	maxDeletePercent int

	// onTickComplete fires after every full sweep completes;
	// the test harness wires this to drive deterministic sync
	// without polling the watcher's internal state. nil in
	// production — Go's linker drops the call when unused.
	onTickComplete func(SweepReport)

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

// VariantReconciler is the write half the sweep needs, both arms
// keyed by (source_path, variant_id).
//
// DeleteVariant removes one row. The Store's DeleteVariant
// transactionally bumps `tracks.indexed_at` so iOS delta-sync
// observes the removal on the next manifest fetch. Per-row error
// tolerance: a tick logs and continues on per-row failure, but
// still publishes the events for the rows that DID delete.
//
// AdoptVariantSidecar rewrites one row's `sidecar_path` to the
// canonical location LocateSidecar found the file at. The Store's
// UpdateVariantSidecarPath deliberately does NOT bump `indexed_at`
// — a path-only change is invisible to a client — which is what
// makes adopting a whole relocated catalog free on the wire.
//
// One interface rather than an optional upgrade on the deleter: a
// `.(VariantAdopter)` type assertion that the production adapter
// forgot to satisfy would silently turn every relocation back into
// a deletion while a test fake that did satisfy it stayed green.
// The compiler enforces the wiring instead.
type VariantReconciler interface {
	DeleteVariant(sourcePath, variantID string) error
	AdoptVariantSidecar(sourcePath, variantID, newSidecarPath string) error
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
	// SizeBytes is the row's recorded sidecar size, which the
	// relocation probe compares against a file found at the canonical
	// location so a partial copy is never adopted (LocateSidecar).
	SizeBytes int64
}

// SweepReport is what one tick did, in rows. Rows is the catalog
// size the tick saw; the rest partition it (Refused counts the
// missing rows the relocation guard declined to delete). Handed to
// the test seam and folded into the per-tick summary log line.
type SweepReport struct {
	Rows       int
	Present    int
	Adopted    int
	Deleted    int
	Mismatched int
	// Failed counts rows a stat, an adoption UPDATE or a DELETE failed
	// on — each kept as it was, each logged, none a deletion.
	Failed  int
	Refused int
	// Skipped is true when the tick did not sweep at all — the
	// mount-loss guard fired, or the catalog query failed.
	Skipped bool
	// Cancelled is true when the context ended mid-tick, so the counts
	// describe a PARTIAL pass. Distinct from Skipped, which means
	// nothing was swept: a cancelled tick has real adoptions and real
	// deletions behind it, and a seam that could not tell the two apart
	// would read a shutdown as a no-op.
	Cancelled bool
}

// NewVariantWatcher constructs a watcher. interval ≤ 0 disables
// the watcher entirely — Start returns a no-op stopFn. Used by
// operators on minimal deploys who only run `--gc` manually.
//
// `variantsDir` resolves the effective variants output directory
// (`cfg.Upscale.EffectiveVariantsDir`) the sidecar paths live
// under. Before every sweep the watcher probes it via
// VariantsDirSweepBlockReason and skips the whole tick when the
// directory is missing or empty while rows exist — the signature
// of a cleanly-unmounted variants volume, where every per-row
// stat would report ENOENT and an unguarded sweep would
// mass-delete the catalog (2026-07-21 review H4). A nil provider,
// or one answering "", disables the guard (legacy unconditional
// sweep).
//
// A PROVIDER rather than a path, asked on every tick: the
// directory is a hot setting (POST /api/upscale/variants-dir), and
// a value captured at construction kept probing the volume the
// operator had moved AWAY from — so the guard that exists to
// notice an unmounted variants volume was watching the wrong one
// for the rest of the process. Every consumer of the field reads
// it live now, which is the rule for hot config: either every
// consumer reads a field live or every consumer takes it at boot,
// never a split. The same provider answers the relocation probe:
// "where should this row's file be NOW" has to be asked of the
// directory that is current on this tick.
//
// `maxDeletePercent` is the relocation guard's threshold
// (cfg.Integrity.VariantSweepMaxDeletePercent, already bounded to
// 0..100 by config validation); see MassDeleteRefusal.
func NewVariantWatcher(lister VariantLister, reconciler VariantReconciler, publish PublishFunc, variantsDir func() string, interval time.Duration, maxDeletePercent int) *VariantWatcher {
	return &VariantWatcher{
		lister:           lister,
		reconciler:       reconciler,
		publish:          publish,
		variantsDir:      variantsDir,
		interval:         interval,
		maxDeletePercent: maxDeletePercent,
	}
}

// SetOnTickComplete is a test-only seam. Production wires nil.
// Same convention as transcode.Pool's SetOnStateChange — the
// test harness can register a callback once at construction
// without exposing internal channels.
func (w *VariantWatcher) SetOnTickComplete(fn func(SweepReport)) {
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
	// waiting `interval` for the first sweep — and, since the
	// 2026-09-20 report, the "database and tree moved hosts
	// together" case, which the same boot sweep used to turn
	// into a whole-catalog deletion.
	report := w.tick(ctx)
	if w.onTickComplete != nil {
		w.onTickComplete(report)
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
			report := w.tick(ctx)
			if w.onTickComplete != nil {
				w.onTickComplete(report)
			}
		}
	}
}

// currentVariantsDir asks the provider, treating a nil provider as
// "no guard" — the same answer an empty path gives.
func (w *VariantWatcher) currentVariantsDir() string {
	if w.variantsDir == nil {
		return ""
	}
	return w.variantsDir()
}

// tick performs one full sweep and reports what it did. Logs ERROR
// only on the outer AllVariants query failure (the only path where
// we can't even start); WARN (sampled per tick, see logSample) on
// per-row stat / adopt / delete failures; and ONE summary line per
// tick that saw rows. Skips wholesale (WARN, nothing touched) when
// the variants dir probe reports missing/empty with rows in the
// catalog — see NewVariantWatcher and VariantsDirSweepBlockReason.
//
// Two passes over the snapshot. The first classifies every row with
// LocateSidecar and applies the ADOPTIONS as it goes (a relocated
// row's file is right there; nothing about adopting it depends on
// the rest of the catalog). The second applies the DELETIONS — but
// only after MassDeleteRefusal has looked at how many there are
// against how many rows the tick saw, which is a question that can
// only be asked once the whole snapshot is classified. That ordering
// is the point: the guard needs the count, and the count is not known
// until every row has been asked.
func (w *VariantWatcher) tick(ctx context.Context) SweepReport {
	rows, err := w.lister.AllVariants()
	if err != nil {
		// A listing the shutdown stopped is not a failed sweep. The
		// lister's adapter runs on the same scanCtx this tick does.
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("integrity variant sweep: AllVariants failed",
				slog.Any("err", failure),
			)
		}
		return SweepReport{Skipped: true}
	}
	if len(rows) == 0 {
		return SweepReport{}
	}
	dir := w.currentVariantsDir()
	// Mount-loss guard: rows exist but the whole variants dir is
	// missing or empty → the volume is almost certainly unmounted
	// (a clean unmount reverts the mountpoint to an empty local
	// dir), NOT a library whose every sidecar was individually
	// deleted. Skip the sweep rather than mass-deleting the
	// catalog on per-row ENOENTs. Probed per tick so a later
	// unmount is caught even after healthy ticks. Shares the
	// helper with `bridge upscale --gc`'s reverse-sweep guard.
	// The directory is RESOLVED per tick too, so a hot move of
	// the variants dir moves the probe with it.
	if dir != "" {
		if reason := VariantsDirSweepBlockReason(dir); reason != "" {
			logger.Warn("integrity variant sweep: skipping sweep, variants dir unhealthy with rows in catalog",
				slog.String("variants_dir", dir),
				slog.String("reason", reason),
				slog.Int("rows", len(rows)),
			)
			return SweepReport{Rows: len(rows), Skipped: true}
		}
	}

	report := SweepReport{Rows: len(rows)}
	var (
		missing []VariantSnapshot
		sample  logSampler
	)
	// Pass one: classify, adopting as we go.
	for _, r := range rows {
		// Honour cancellation between rows so a shutdown
		// during a long sweep on a large library doesn't
		// hold the process up for minutes.
		select {
		case <-ctx.Done():
			// Still one summary line. The docblock's promise is
			// "every tick that saw rows logs ONE summary line",
			// and a cancelled pass one has already applied its
			// adoptions — returning bare left a shutdown mid-sweep
			// with per-row lines (sampled at ten) and no totals,
			// which is a smaller copy of the silence the field
			// report's first finding was about.
			report.Cancelled = true
			w.logSummary(dir, report)
			return report
		default:
		}
		loc := LocateSidecar(dir, r)
		switch loc.Verdict {
		case SidecarPresent:
			report.Present++
		case SidecarRelocated:
			if err := w.reconciler.AdoptVariantSidecar(r.SourcePath, r.VariantID, loc.Canonical); err != nil {
				// An adoption the shutdown stopped ends the tick, as the
				// check at the top of this loop would, rather than counting
				// as a failure.
				if ctxerr.WithoutCancellation(ctx, err) == nil {
					report.Cancelled = true
					w.logSummary(dir, report)
					return report
				}
				// The file is there and the row still points at the old
				// path; nothing is lost and the next tick asks again. Not
				// a deletion candidate under any reading.
				report.Failed++
				sample.log(slog.LevelWarn, "integrity variant sweep: adopt failed",
					slog.String("source_path", r.SourcePath),
					slog.String("variant_id", r.VariantID),
					slog.String("canonical", loc.Canonical),
					slog.Any("err", err),
				)
				continue
			}
			report.Adopted++
			sample.log(slog.LevelInfo, "integrity variant sweep: adopted relocated sidecar",
				slog.String("source_path", r.SourcePath),
				slog.String("variant_id", r.VariantID),
				slog.String("from", r.SidecarPath),
				slog.String("to", loc.Canonical),
			)
		case SidecarMismatched:
			report.Mismatched++
			sample.log(slog.LevelWarn, "integrity variant sweep: sidecar at canonical path has a different size; keeping the row",
				slog.String("source_path", r.SourcePath),
				slog.String("variant_id", r.VariantID),
				slog.String("canonical", loc.Canonical),
				slog.Int64("recorded_size", r.SizeBytes),
			)
		case SidecarUnknown:
			// Permission errors, I/O faults, etc. — log and skip
			// rather than treating as "missing". `--gc`'s reverse
			// pass behaves the same way.
			report.Failed++
			sample.log(slog.LevelWarn, "integrity variant sweep: stat failed",
				slog.String("sidecar", r.SidecarPath),
				slog.String("variant_id", r.VariantID),
				slog.Any("err", loc.Err),
			)
		case SidecarMissing:
			missing = append(missing, r)
		}
	}

	// Relocation guard, asked of the whole tick. A refusal leaves the
	// rows in place for the operator to look at; the summary line and
	// the reason say exactly what was seen.
	if reason := MassDeleteRefusal(dir, len(missing), len(rows), w.maxDeletePercent); reason != "" {
		report.Refused = len(missing)
		logger.Warn("integrity variant sweep: refusing to delete rows — this looks like a relocation, not a deletion",
			slog.String("reason", reason),
			slog.String("variants_dir", dir),
			slog.String("hint", "if the sidecars really are gone: `bridge upscale --gc --allow-mass-delete`; if they were moved: put them at their source-mirrored paths under the variants directory, or `bridge variants move --to <dir> --confirm MOVE`"),
		)
		w.logSummary(dir, report)
		return report
	}

	// Pass two: delete. `paths` is the deduplicated set of affected
	// source paths; `variantIDs` is the (potentially repeating) set
	// of deleted variantIDs. Per the upscale.deleted contract
	// documented in internal/api/upscale_deleted_event.go: `Paths`
	// and `VariantIDs` are NOT zipped 1:1, just the union of what
	// disappeared. Dedup paths so a track with multiple missing
	// variants (rare but legitimate — e.g. 96k + 192k variants for
	// the same source both wiped by an external rm) doesn't emit the
	// same path twice in the SSE payload. CodeRabbit Minor on PR #209.
	var (
		paths      []string
		variantIDs []string
		pathsSeen  = make(map[string]struct{})
	)
	for _, r := range missing {
		select {
		case <-ctx.Done():
			// Same as pass one: the rows deleted before the
			// cancellation are real and the count is what says so.
			report.Cancelled = true
			w.publishDeleted(paths, variantIDs)
			w.logSummary(dir, report)
			return report
		default:
		}
		if delErr := w.reconciler.DeleteVariant(r.SourcePath, r.VariantID); delErr != nil {
			// A delete the shutdown stopped ends the tick, as above.
			if ctxerr.WithoutCancellation(ctx, delErr) == nil {
				report.Cancelled = true
				w.publishDeleted(paths, variantIDs)
				w.logSummary(dir, report)
				return report
			}
			report.Failed++
			sample.log(slog.LevelWarn, "integrity variant sweep: DB delete failed",
				slog.String("source_path", r.SourcePath),
				slog.String("variant_id", r.VariantID),
				slog.Any("err", delErr),
			)
			continue
		}
		report.Deleted++
		sample.log(slog.LevelInfo, "integrity variant sweep: deleted row whose sidecar is missing at both locations",
			slog.String("source_path", r.SourcePath),
			slog.String("variant_id", r.VariantID),
			slog.String("recorded", r.SidecarPath),
		)
		variantIDs = append(variantIDs, r.VariantID)
		if _, seen := pathsSeen[r.SourcePath]; !seen {
			pathsSeen[r.SourcePath] = struct{}{}
			paths = append(paths, r.SourcePath)
		}
	}
	w.publishDeleted(paths, variantIDs)
	w.logSummary(dir, report)
	return report
}

// publishDeleted fires the single batched callback per sweep that
// observed at least one deletion — iOS reconciles all affected
// tracks in one pass rather than fielding N separate event hops.
func (w *VariantWatcher) publishDeleted(paths, variantIDs []string) {
	if len(paths) > 0 && w.publish != nil {
		w.publish(paths, variantIDs)
	}
}

// logSummary writes the one line per tick that the 2026-09-20 sweep
// never wrote. Warn when the tick deleted or refused anything —
// those are the ticks an operator scrolling a journal is looking
// for — Info otherwise, so a healthy hourly tick is one findable
// line rather than silence.
//
// Reached from every exit that saw rows, INCLUDING the two
// cancellation arms. They used to return bare, so a shutdown partway
// through left ten sampled per-row lines and no totals — the same
// silence at a smaller scale, and the counts carry `cancelled` so the
// line is not read as a complete tick.
func (w *VariantWatcher) logSummary(dir string, r SweepReport) {
	level := slog.LevelInfo
	if r.Deleted > 0 || r.Refused > 0 {
		level = slog.LevelWarn
	}
	logger.Log(context.Background(), level, "integrity variant sweep: summary",
		slog.Int("rows", r.Rows),
		slog.Int("present", r.Present),
		slog.Int("adopted", r.Adopted),
		slog.Int("deleted", r.Deleted),
		slog.Int("mismatched", r.Mismatched),
		slog.Int("failed", r.Failed),
		slog.Int("refused", r.Refused),
		slog.Bool("cancelled", r.Cancelled),
		slog.String("variants_dir", dir),
	)
}

// logSampleCap is how many per-row lines of each message a tick emits
// at the message's own level before the rest drop to Debug. A relocated
// catalog adopts thousands of rows in one tick and a copy in flight
// mismatches thousands more every hour until it lands; the summary line
// carries the totals, and the M-SEARCH lesson (199,078 of 200,000 log
// lines) is that an unbounded per-row log makes every other line
// unfindable.
const logSampleCap = 10

// logSampler counts per-message emissions within one tick.
type logSampler struct {
	seen map[string]int
}

func (s *logSampler) log(level slog.Level, msg string, attrs ...slog.Attr) {
	if s.seen == nil {
		s.seen = make(map[string]int)
	}
	s.seen[msg]++
	if s.seen[msg] > logSampleCap {
		level = slog.LevelDebug
	}
	logger.LogAttrs(context.Background(), level, msg, attrs...)
}
