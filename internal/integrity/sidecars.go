package integrity

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// gcChunkSize bounds how many orphans ONE tick of the background sweep
// unlinks. It no longer bounds the walk.
//
// Every tick takes the whole tree's inventory (TakeSidecarInventory,
// read-only), because the mass-orphan refusal has to see the whole tree's
// count: a 5,000-entry slice of it cannot tell a lost index from a crop.
// Measured on the dev Mac (Apple silicon, APFS, warm cache, 2026-09-28):
// 128 ms median over 100,001 files in 111 directories against a
// 100,001-row known set, 117 ms over the same tree against 200 rows (the
// lost-index shape); building that known set is another 50 ms, a cost the
// tick already paid. This docblock has long carried ~50 µs per entry for
// the pathological tier (cold cache, NTFS or exFAT on USB-attached
// spinning rust) — an estimate, not a measurement — which puts the same
// tree at ~5 s a tick. The cadence (`cfg.Integrity.
// OrphanSidecarSweepIntervalSec`, typically minutes to hours) is what
// spaces that from library scans and serving.
//
// What the chunk still bounds: how many files a tick unlinks, so a
// legitimate backlog drains over several ticks rather than in one burst,
// and how many orphan paths the inventory retains (MaxOrphanPaths: ~750 KB
// at 150 bytes a path). `SidecarInventory.Orphans` is counted in full
// either way, and the refusal reads that count, never the retained list —
// a count capped at the chunk would read 5,000 orphans where there are
// 10,048, and against a catalog of 10,000 rows it would proceed.
//
// A failed unlink keeps its slot, so the retained list is the tree's first
// `gcChunkSize` orphans in walk order on every tick: fewer than that many
// files this user cannot remove only shrink each tick's share, while that
// many or more at the head of the walk stall the rest until the operator
// fixes them — each tick's sampled WARN names them.
//
// 5000 was chosen when the chunk bounded the WALK (Gemini on PR #282: the
// 100 it replaced made a full sweep O(N × N/chunk) in AllVariants reads).
// Pure constant, not configurable — the operator-facing knob is the SWEEP
// CADENCE, not the chunk size.
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
// Measured against the TICK's start (sampled after the catalog listing,
// before the walk), not against the moment of the unlink: the walk
// between them makes every file look older, and the earlier instant is
// the conservative one.
//
// **Test seam**: production reads the constant; the
// `gracePeriodForTest` field on OrphanSidecarSweeper overrides it
// per-instance so the regression test can use a millisecond-scale
// grace without sleeping 10 minutes. Same DI shape `Pool.runner`
// uses for the sox subprocess.
const gcGracePeriod = 10 * time.Minute

// orphanRefusalRepeat is how often a refusal that goes on is logged
// again: once when a streak of refused ticks starts, then at most once
// per this interval while it lasts.
const orphanRefusalRepeat = 24 * time.Hour

// orphanRefusalExamples bounds how many orphan paths the refusal line
// names — enough to recognise the files as renditions, few enough to keep
// the numbers readable.
const orphanRefusalExamples = 3

// OrphanSidecarSweeper walks the variants directory on a cadence
// (configured via `cfg.Integrity.OrphanSidecarSweepIntervalSec`) and
// unlinks `.flac` files whose path is NOT present in the current
// `track_variants` snapshot — neither as a row's recorded `sidecar_path`
// nor as its canonical path under the tree being walked
// (KnownSidecarSet). The forward half of the operator-triggered
// `bridge upscale --gc` sweep, which `VariantWatcher` (in variants.go)
// does NOT cover — that type handles the REVERSE direction (rows whose
// sidecar file disappeared on disk).
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
// SELECT a consistent snapshot natively, so `AllVariants` is
// safe to call without an explicit transaction wrapper.
//
// **Each tick decides alone, as `bridge upscale --gc` does**
// (2026-09-28). A tick lists the catalog, takes ONE read-only inventory
// of the whole tree (TakeSidecarInventory — the walker `upscale --gc`,
// `analyze --gc` and `bridge doctor`'s variants-index share), asks
// MassOrphanRefusal of the whole tree's counts, and only then unlinks: at
// most gcChunkSize files, each re-checked first (reclaimOrphan). Until
// then this sweep unlinked INSIDE a walk chunked at 5,000 entries with a
// cursor across ticks, and its only guard was "is the known set EMPTY?" —
// so #940's shape, a catalog of 200 rows over a stranded tree of 10,048
// files after a lost index, passed it, and the sweep unlinked the whole
// tree in three ticks (4,800, 5,000 and 248, measured on the old code)
// while `--gc` refused the same tree. A verdict tallied
// ACROSS ticks was considered and rejected: it can come from a partial
// walk (an unmount, a cancel, a pruned root) or from a catalog that
// changed mid-pass, and a budget carried between passes can be spent on
// files it never counted. So no verdict crosses a tick; the one state
// that does is the refusal's log latch.
//
// **No override.** A background sweeper has nobody in the loop to express
// intent — the reason `--allow-empty` and `--allow-mass-orphans` exist on
// the CLI and not here — so a refused tick unlinks nothing and says why:
// one WARN when a streak of refused ticks starts, repeated at most once a
// day while it lasts (the M-SEARCH rule: an identical line every tick
// makes every other line unfindable), and one Info line when a tick
// proceeds again. `bridge upscale --gc --allow-mass-orphans` is the way
// past it. The threshold is `cfg.Integrity.VariantSweepMaxDeletePercent`,
// the reverse sweep's knob with the same meaning at this end (100
// disables both guards).
//
// **What the refusal does NOT cover.** MassOrphanRefusal refuses only when
// its floor of ten, `orphans > rows` AND the ratio all hold, so a stranded
// tree NO LARGER than the catalog is reaped here exactly as `--gc` reaps
// it. `orphans > rows` is the term that knows a lost index; a tree the
// catalog could still describe does not trip it.
//
// **Parity with `--gc` by construction**, because the walker is shared:
// the root is resolved before the walk and paths are reported under the
// configured one (#959) — so a SYMLINKED variants directory is walked
// now, where this sweep's own WalkDir used to Lstat the link, see one
// non-directory entry and sweep nothing; dot-directories are pruned at
// the walk; a link to a directory or a Windows junction is never a
// candidate ("not a REGULAR file", #969); and a walk error fails closed.
// One difference is deliberate: this sweep considers `.flac` files only
// (shouldConsiderSidecarFile) where `upscale --gc` passes a nil Consider
// and counts and removes every file, so the ratio here is over the files
// this sweep would remove, as `analyze --gc`'s is over waveform files.
//
// Threading: one long-lived goroutine spun up by Start; stops on
// ctx cancellation OR the stopFn closing the done channel.
// Mirrors `VariantWatcher` exactly so cmd/bridge's wiring +
// shutdown ordering treats both watchers symmetrically.
type OrphanSidecarSweeper struct {
	lister SidecarLister
	// outputDir resolves the variant tree to walk, and is asked PER
	// TICK — see NewOrphanSidecarSweeper.
	outputDir func() string
	interval  time.Duration
	// maxOrphanPercent is the mass-orphan refusal's threshold
	// (cfg.Integrity.VariantSweepMaxDeletePercent); see MassOrphanRefusal.
	maxOrphanPercent int

	// refusing and lastRefusalLog are the refusal's log latch, the only
	// state that crosses ticks. refusing is true from a refused tick until
	// a tick whose walk finished proceeds; lastRefusalLog is when the
	// refusal was last logged. A tick that stops before a verdict (a
	// listing or a walk that failed or was stopped, an empty catalog)
	// leaves both alone: it is evidence of nothing, so it neither ends a
	// streak nor says the catalog recovered. Owned by the run goroutine;
	// the tests drive tick directly, never beside a running loop.
	refusing       bool
	lastRefusalLog time.Time

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

	// chunkSizeForTest overrides gcChunkSize when positive, so the tests
	// can exercise the per-tick unlink cap and the full-count refusal with
	// a chunk of ~100 instead of seeding 5,000 files. Production leaves it
	// zero.
	chunkSizeForTest int

	// beforeUnlinksForTest, when set, runs between a tick's walk and its
	// first unlink: a test repoints a symlinked variants directory there,
	// the window TestAnOrphanSweepUnlinksInTheTreeItWalked holds. Nil in
	// production.
	beforeUnlinksForTest func()

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
	// exited is closed when the run goroutine returns, so stopFn can
	// JOIN it rather than merely signalling. See Start.
	exited chan struct{}
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
// the `track_variants` rows the forward sweep builds its known set
// from. `manifest.Store` is wired via a thin adapter in cmd/bridge;
// the explicit interface lets tests inject fakes without spinning a
// real SQLite store.
//
// Rows, not a bare set of paths (which is what this returned until
// the 2026-09-20 report): the known set must hold each row's
// CANONICAL path under the tree being walked beside its recorded
// one, and the canonical path is computed from (source_path,
// variant_id). With the recorded paths alone, a database copied to
// a host where the variants dir has a new path knows NOTHING under
// that dir — every file of a byte-identical 259.7 GiB tree reads as
// an orphan and the walk unlinks it, chunk by chunk, older-than-
// grace first. The sweep projects the rows into a
// `map[string]struct{}` itself (O(1) lookup against thousands of
// filesystem entries; a slice would O(n²)-walk on every tick).
// The ROW COUNT matters too: it is the `rows` MassOrphanRefusal weighs
// the orphans against.
type SidecarLister interface {
	AllVariants(ctx context.Context) ([]VariantSnapshot, error)
}

// NewOrphanSidecarSweeper constructs a sweeper. interval ≤ 0
// disables the sweeper entirely — Start returns a no-op stopFn.
// Used by operators on minimal deploys who run `bridge upscale --gc`
// manually.
//
// `outputDir` resolves the absolute path of the variant tree to
// walk — typically `<cfg.DataDir>/transcoded/` via
// `cfg.Upscale.EffectiveVariantsDir`. It is asked on EVERY tick:
// the directory is a hot setting (POST /api/upscale/variants-dir),
// and a path captured at construction kept this sweeper walking
// the tree the operator had moved away from, for the rest of the
// process, while new sidecars landed somewhere it never looked. (An
// earlier docblock here recorded that a restart was required, which
// was true, and was the defect.) An empty answer is a REFUSAL (tick).
// WalkDir("") only reports ENOENT, but a root resolved first is "."
// (filepath.EvalSymlinks("") and filepath.Clean("") both answer it), the
// working directory, and this sweep unlinks — and it DOES resolve its
// root first, through TakeSidecarInventory.
//
// `maxOrphanPercent` is the mass-orphan refusal's threshold
// (cfg.Integrity.VariantSweepMaxDeletePercent, already bounded to
// 0..100 by config validation) — the same number, taken the same way, as
// NewVariantWatcher's `maxDeletePercent`; see MassOrphanRefusal.
func NewOrphanSidecarSweeper(lister SidecarLister, outputDir func() string, interval time.Duration, maxOrphanPercent int) *OrphanSidecarSweeper {
	return &OrphanSidecarSweeper{
		lister:           lister,
		outputDir:        outputDir,
		interval:         interval,
		maxOrphanPercent: maxOrphanPercent,
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
		s.exited = make(chan struct{})
		go func() {
			defer close(s.exited)
			s.run(ctx, s.done)
		}()
	})
	return func() {
		s.stopOnce.Do(func() {
			if s.done != nil {
				close(s.done)
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
			if s.exited != nil {
				t := time.NewTimer(stopGrace)
				defer t.Stop()
				select {
				case <-s.exited:
				case <-t.C:
				}
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

// tick performs one sweep and returns the count of files it unlinked (NOT
// the count of orphans observed: an orphan it left, for any reason,
// counts 0). In order:
//
//  1. Resolve the tree and refuse an empty answer, before anything could
//     resolve "" to the working directory.
//  2. List the catalog (AllVariants) BEFORE the walk — snapshot-then-walk,
//     see the type's docblock — and refuse an EMPTY known set over a
//     directory that holds files, every tick, as it always has.
//  3. Take the whole tree's inventory, with no MaxEntries: a sweep that
//     deletes on a truncated inventory would be deleting on a ratio
//     measured from part of the tree. A walk that fails or is stopped
//     unlinks nothing.
//  4. Ask MassOrphanRefusal of the whole-tree counts — `inv.Orphans`, the
//     full count, never the retained list — and unlink nothing on a
//     refusal.
//  5. Unlink at most the chunk's worth of the retained orphans, each
//     re-checked first (reclaimOrphan).
//
// Every tick that reaches the walk logs one summary line (orphanTick.log);
// a refused one logs its refusal through the latch (noteRefusal).
func (s *OrphanSidecarSweeper) tick(ctx context.Context) int {
	root := ""
	if s.outputDir != nil {
		root = s.outputDir()
	}
	if root == "" {
		// Nothing resolved: refuse, before anything could resolve "" to
		// "." (the working directory; WalkDir("") itself only errors).
		logger.Warn("orphan sidecar sweep: refusing — no variants directory resolved")
		return 0
	}
	rows, err := s.lister.AllVariants(ctx)
	if err != nil {
		// A listing the shutdown stopped is not a failed sweep.
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			logger.Error("orphan sidecar sweep: AllVariants failed",
				slog.Any("err", failure),
			)
		}
		return 0
	}
	// Case-fold + clean the known-set keys so a casing delta between the
	// DB SidecarPath and the on-disk WalkDir path can't misclassify a live
	// sidecar as orphan (and unlink it) on a case-insensitive FS — the same
	// hazard fixed in `bridge upscale --gc` (CodeRabbit on PR #477).
	//
	// Both spellings of every row go in: the recorded path AND the canonical
	// one under the tree being walked (KnownSidecarSet). A relocated catalog
	// — rows still naming the old host's directory, files at their
	// source-mirrored places under this one — is then fully known, and the
	// reverse sweep (VariantWatcher) adopts the rows at its own pace while
	// this walk leaves the files alone. `bridge upscale --gc`'s forward
	// sweep builds its set the same way.
	known := KnownSidecarSet(root, rows)

	// FAIL CLOSED on an empty known-set. Every file under the variants dir
	// misses an empty `known`, so the whole rendition tree would read as
	// orphaned — hours of sox/ffmpeg on a real library, and nothing
	// regenerates it until the operator asks again.
	//
	// The listing's error arm above fails closed already; a query that
	// SUCCEEDS and returns no rows did not, and the routes to it are
	// ordinary. The reset procedure this repo's own CLAUDE.md documents is
	// `rm -f bridge.db*` + restart, and `run` takes a tick at boot. A
	// single<->multi root flip runs WipeFilesystemTracks, and
	// `track_variants` CASCADEs on `tracks`, so the window between the wipe
	// and the re-transcode reads zero rows too.
	//
	// Kept, and AHEAD of the mass-orphan refusal below. That one refuses
	// most of the same trees (with no rows, every orphan is more than the
	// catalog holds) but not one under its floor of ten, which it would
	// unlink whole; and this one costs no walk, says what is actually
	// wrong (the catalog is empty), and WARNs every tick as it always has.
	// It is not a verdict about the tree, so it leaves the refusal's latch
	// alone.
	//
	// An empty set over an EMPTY directory is not an error — there is nothing
	// to protect and nothing to do — so that returns quietly, as before.
	// Deliberately no operator override here: a background sweeper has
	// nobody in the loop to express intent, which is what the two CLI GCs'
	// explicit --allow-empty is for.
	if len(known) == 0 {
		empty, emptyErr := dirIsEmpty(root)
		if emptyErr == nil && !empty {
			logger.Warn("orphan sidecar sweep: refusing — no variant row references any "+
				"sidecar, but the variants directory holds files",
				slog.String("variants_dir", root),
			)
		}
		return 0
	}

	tickStart := time.Now()
	chunk := s.effectiveChunkSize()
	inv, err := TakeSidecarInventory(ctx, root, known, SidecarInventoryOptions{
		Consider:       shouldConsiderSidecarFile,
		MaxOrphanPaths: chunk,
	})
	if err != nil {
		// The walk stops only for its context or for a tree it could not
		// read. A shutdown's cancellation is not reported; a deadline or a
		// read failure is, at WARN. Either way nothing is unlinked, and the
		// latch is untouched: a walk that did not finish is evidence of
		// nothing about the tree.
		failure := ctxerr.WithoutCancellation(ctx, err)
		if failure != nil {
			logger.Warn("orphan sidecar sweep: walk aborted",
				slog.String("outputDir", root),
				slog.Any("err", failure),
			)
		}
		orphanTick{root: root, cutShort: true, cancelled: failure == nil}.log()
		return 0
	}

	// An entry the walk could not read (inv.Unreadable) is missing from
	// every count, and so from the orphans this tick could unlink: the
	// verdict is taken over what the walk could see, as `upscale --gc`'s is,
	// and the summary line carries the count.
	if reason := MassOrphanRefusal(inv.Orphans, inv.Files, len(rows), s.maxOrphanPercent); reason != "" {
		s.noteRefusal(tickStart, root, reason, inv.OrphanPaths)
		orphanTick{root: root, walked: true, inv: inv, refused: true}.log()
		return 0
	}
	s.noteProceeding(root, inv, len(rows))

	paths, walked := inv.OrphanPaths, inv.OrphanWalkedPaths
	if len(paths) > chunk {
		// MaxOrphanPaths already capped the list; the cap is restated where
		// the unlinks happen so it cannot quietly depend on an option.
		paths, walked = paths[:chunk], walked[:chunk]
	}
	if s.beforeUnlinksForTest != nil {
		s.beforeUnlinksForTest()
	}
	tally, stopErr := s.reclaimOrphans(ctx, paths, walked, tickStart)
	t := orphanTick{root: root, walked: true, inv: inv, tally: tally}
	if stopErr != nil {
		// Only the context stops the unlinks. As for the walk: a shutdown is
		// not reported, a deadline is; the files unlinked before it stay
		// counted.
		failure := ctxerr.WithoutCancellation(ctx, stopErr)
		if failure != nil {
			logger.Warn("orphan sidecar sweep: unlinking aborted",
				slog.String("outputDir", root),
				slog.Any("err", failure),
			)
		}
		t.cutShort, t.cancelled = true, failure == nil
	}
	t.log()
	return tally.unlinked
}

// reclaimOrphans hands each orphan's WALKED path to reclaimOrphan, counting
// and logging what it did under the configured spelling in paths, and stops
// at a cancelled context with the context's error. walked[i] is the file
// paths[i] names, as the inventory's walk visited it (see
// SidecarInventory.OrphanWalkedPaths): the unlink goes to the tree the
// verdict was taken over, whatever the configured root points at by now.
// Per-path lines are sampled at logSampleCap per message per tick, the rest
// at Debug: a legitimate backlog is a chunk of 5,000 unlinks a tick, and
// the summary line carries the totals.
func (s *OrphanSidecarSweeper) reclaimOrphans(ctx context.Context, paths, walked []string, tickStart time.Time) (orphanTally, error) {
	grace := s.effectiveGracePeriod()
	var (
		tally  orphanTally
		sample logSampler
	)
	for i, p := range paths {
		if err := ctx.Err(); err != nil {
			return tally, err
		}
		outcome, err := reclaimOrphan(walked[i], tickStart, grace, os.Lstat, os.Stat)
		switch outcome {
		case orphanUnlinked:
			tally.unlinked++
			sample.log(slog.LevelInfo, "orphan sidecar sweep: unlinked orphan",
				slog.String("path", p),
			)
		case orphanGone:
			tally.gone++
		case orphanInGrace:
			tally.inGrace++
		case orphanNotAFile:
			tally.notAFile++
			sample.log(slog.LevelInfo, "orphan sidecar sweep: left an orphan that is no longer a file",
				slog.String("path", p),
			)
		case orphanUnreadable:
			tally.failed++
			sample.log(slog.LevelWarn, "orphan sidecar sweep: stat failed",
				slog.String("path", p),
				slog.Any("err", err),
			)
		case orphanUnlinkFailed:
			tally.failed++
			sample.log(slog.LevelWarn, "orphan sidecar sweep: unlink failed",
				slog.String("path", p),
				slog.Any("err", err),
			)
		}
	}
	return tally, nil
}

// orphanOutcome is what reclaimOrphan did with one path the inventory
// classified as an orphan.
type orphanOutcome uint8

const (
	// orphanUnlinked — removed.
	orphanUnlinked orphanOutcome = iota
	// orphanGone — not there when it was re-checked or removed: something
	// else took it between the walk and the unlink, which is the outcome
	// the sweep wanted, so it is done rather than failed (`upscale --gc`
	// reads ENOENT the same way). Not counted as unlinked: this sweep did
	// not remove it.
	orphanGone
	// orphanInGrace — modified inside the grace period of the tick's
	// start: a writer may have put the file down before its row committed
	// (gcGracePeriod). Left for a later tick.
	orphanInGrace
	// orphanNotAFile — no longer something this sweep may unlink: a link
	// to a directory, or a Windows junction, stands at the path now (the
	// #969 rule, asked again). Left alone.
	orphanNotAFile
	// orphanUnreadable — the re-check could not tell what is at the path.
	// Left alone: "could not find out" is not "junk".
	orphanUnreadable
	// orphanUnlinkFailed — the re-check passed and os.Remove failed.
	orphanUnlinkFailed
)

// reclaimOrphan re-checks one path the tick's inventory classified as an
// orphan, and removes it only if it is still a file this sweep may reclaim.
//
// The inventory and the unlink are separate steps, as in `upscale --gc`,
// because the refusal has to see the whole tree before anything goes; so
// the path is asked again, freshly, before os.Remove. An Lstat first (and
// the tick needs one anyway: the inventory keeps no mtimes), then the SAME
// classifyWalkEntry the inventory used, so the two cannot disagree about
// what a candidate is: an entry that is not a REGULAR file is stat'd, and
// a link to a directory or a Windows junction (ModeIrregular without
// ModeDir, since Go 1.23) is never unlinked, because it may be the only
// reference to an album parked on another volume. A dangling link still
// classifies, as in the walk: it is junk in this tree. Then the grace
// check against the tick's start (gcGracePeriod), then os.Remove.
//
// lstat and stat are parameters so the Windows junction shape, which no
// other platform can construct, is drivable on every CI leg; production
// passes os.Lstat and os.Stat.
func reclaimOrphan(path string, tickStart time.Time, grace time.Duration, lstat, stat func(string) (fs.FileInfo, error)) (orphanOutcome, error) {
	info, err := lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return orphanGone, nil
	case err != nil:
		return orphanUnreadable, err
	}
	var statErr error
	switch classifyWalkEntry(info.Mode(), func() (fs.FileInfo, error) {
		target, err := stat(path)
		statErr = err
		return target, err
	}) {
	case walkEntrySkip:
		return orphanNotAFile, nil
	case walkEntryUnreadable:
		return orphanUnreadable, statErr
	}
	if tickStart.Sub(info.ModTime()) < grace {
		return orphanInGrace, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return orphanGone, nil
		}
		return orphanUnlinkFailed, err
	}
	return orphanUnlinked, nil
}

// orphanTally counts what reclaimOrphans did with a tick's orphans, one
// field per outcome; failed covers orphanUnreadable and orphanUnlinkFailed.
type orphanTally struct {
	unlinked, gone, inGrace, notAFile, failed int
}

// noteRefusal logs a refused tick through the latch: one WARN when a streak
// of refused ticks starts, then at most one per orphanRefusalRepeat while
// it lasts. Measured between tick starts on the monotonic clock, so a
// stepped wall clock neither repeats it early nor holds it back.
//
// The reason carries the numbers (MassOrphanRefusal); the examples are
// relative to the variants directory, the form the doctor names them in.
// The hint never names `bridge variants move`: that command needs the
// ROWS, and in the lost-index shape there are none to move (#940).
func (s *OrphanSidecarSweeper) noteRefusal(now time.Time, root, reason string, orphans []string) {
	if s.refusing && now.Sub(s.lastRefusalLog) < orphanRefusalRepeat {
		return
	}
	s.refusing = true
	s.lastRefusalLog = now
	examples := make([]string, 0, orphanRefusalExamples)
	for _, p := range orphans {
		if len(examples) == orphanRefusalExamples {
			break
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			p = rel
		}
		examples = append(examples, p)
	}
	logger.Warn(msgOrphanRefusal,
		slog.String("reason", reason),
		slog.String("variants_dir", root),
		slog.Any("examples", examples),
		slog.String("hint", orphanRefusalHint),
	)
}

// noteProceeding ends a refusal streak: the first tick whose walk finished
// and whose verdict proceeds says so, once, with the counts it proceeded
// on. Outside a streak it says nothing.
//
// The line claims only that the check passed, because the counts it passed
// on need not be a recovered catalog: a variants volume unmounted during a
// streak leaves a missing or empty directory, an inventory of nothing, and
// a verdict that proceeds (MassOrphanRefusal has no files to weigh). The
// counts say which it was, and a tree that comes back still stranded
// starts a new streak with a WARN of its own rather than waiting out a day.
func (s *OrphanSidecarSweeper) noteProceeding(root string, inv SidecarInventory, rows int) {
	if !s.refusing {
		return
	}
	s.refusing = false
	logger.Info(msgOrphanRefusalLifted,
		slog.Int("files", inv.Files),
		slog.Int("orphans", inv.Orphans),
		slog.Int("rows", rows),
		slog.String("variants_dir", root),
	)
}

// orphanRefusalHint is the refusal line's advice, the CLI refusal's
// (`gcRefuseMassOrphans`) in one line.
const orphanRefusalHint = "nothing was unlinked. A catalog this much smaller than the tree it describes usually " +
	"means the INDEX was lost (a bridge.db restored from an older snapshot, or reset, or rows reaped after a host " +
	"move), not that the files are junk, and they cannot be re-derived from disk. Check `bridge doctor` " +
	"(variants-index) and restore the rows if they are recoverable. Only if the files really are junk: " +
	"`bridge upscale --gc --allow-mass-orphans`. This sweep has no override; it logs this when it starts " +
	"refusing and once a day while it keeps refusing."

// The orphan sweep's refusal lines: the latched WARN, and the Info line a
// tick logs when it proceeds after a streak of refusals.
const (
	msgOrphanRefusal       = "orphan sidecar sweep: refusing to unlink — the catalog is far smaller than the tree it describes"
	msgOrphanRefusalLifted = "orphan sidecar sweep: no longer refusing — this tick's counts pass the mass-orphan check"
)

// orphanTick is one tick's account, for its summary line.
type orphanTick struct {
	root string
	// walked is true once the inventory finished, so inv holds the whole
	// tree's counts. A walk that did not finish has none worth printing:
	// TakeSidecarInventory returns none, and zeros would read as an empty
	// tree.
	walked bool
	inv    SidecarInventory
	// refused is true when MassOrphanRefusal refused the unlinks.
	refused bool
	tally   orphanTally
	// cutShort is true when a stop or a failure ended the walk or the
	// unlinks before they finished; cancelled, when that was the shutdown.
	cutShort, cancelled bool
}

// log writes the tick's one summary line, at Info: complete when its walk
// and its unlinks finished (a refused tick included: it finished
// deciding), cut short when a shutdown or a failure ended either first. It
// still carries what the tick unlinked before it ended, and says whether
// the shutdown stopped it (Gemini API review, #1004).
func (t orphanTick) log() {
	msg := msgOrphanTickComplete
	if t.cutShort {
		msg = msgOrphanTickCutShort
	}
	attrs := make([]slog.Attr, 0, 12)
	if t.walked {
		attrs = append(attrs,
			slog.Int("files", t.inv.Files),
			slog.Int("known", t.inv.Known),
			slog.Int("orphans", t.inv.Orphans),
			slog.Int("unreadable", t.inv.Unreadable),
			slog.Bool("refused", t.refused),
		)
	}
	attrs = append(attrs,
		slog.Int("unlinked", t.tally.unlinked),
		slog.Int("gone", t.tally.gone),
		slog.Int("in_grace", t.tally.inGrace),
		slog.Int("not_a_file", t.tally.notAFile),
		slog.Int("failed", t.tally.failed),
		slog.Bool("cancelled", t.cancelled),
		slog.String("variants_dir", t.root),
	)
	logger.LogAttrs(context.Background(), slog.LevelInfo, msg, attrs...)
}

// The orphan sweep's summary line, one per tick that reached the walk:
// complete when its walk and its unlinks finished, cut short when a
// shutdown or a failure ended one of them first.
const (
	msgOrphanTickComplete = "orphan sidecar sweep: tick complete"
	msgOrphanTickCutShort = "orphan sidecar sweep: tick cut short"
)

// KnownSidecarSet is the forward sweeps' "this file has a row" set:
// every row's recorded `sidecar_path` AND its canonical path under
// `variantsDir` (CanonicalSidecarPath — empty, and skipped, for a row
// with no source identity), each case-folded and cleaned to match the
// walk's on-disk spelling on a case-insensitive filesystem. Shared by
// OrphanSidecarSweeper and `bridge upscale --gc` so the two forward
// sweeps cannot disagree about which files a relocated catalog owns.
func KnownSidecarSet(variantsDir string, rows []VariantSnapshot) map[string]struct{} {
	known := make(map[string]struct{}, 2*len(rows))
	for _, r := range rows {
		if r.SidecarPath != "" {
			known[strings.ToLower(filepath.Clean(r.SidecarPath))] = struct{}{}
		}
		if c := CanonicalSidecarPath(variantsDir, r); c != "" {
			known[strings.ToLower(filepath.Clean(c))] = struct{}{}
		}
	}
	return known
}

// shouldConsiderSidecarFile is the background sweep's Consider: whether
// an entry is a candidate for the orphan check at all. Today the answer
// is "any `.flac` extension", asked of the entry's BASENAME by
// TakeSidecarInventory (a basename and its full path have the same
// filepath.Ext, so it answers for either). Extracted as a pure function
// for unit testing without a real walk.
//
// Narrower than `upscale --gc` on purpose, and this docblock said the
// opposite until 2026-09-28 ("the operator-triggered `--gc` uses the same
// shape"), which was never true — at #284, where this predicate came in,
// `--gc`'s forward sweep already removed every unreferenced file. `--gc`
// passes a nil Consider and counts and removes EVERY file in the variants
// directory, while this sweep never unlinks anything but a `.flac`, and so
// measures its mass-orphan ratio over `.flac` files only.
//
// Future variant formats (FLAC-only today; opus / wavpack are
// hypothetical follow-ups) would extend the predicate rather than
// adding a parallel function — keeps the policy decision in one
// place.
func shouldConsiderSidecarFile(path string) bool {
	return filepath.Ext(path) == ".flac"
}
