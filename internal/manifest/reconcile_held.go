package manifest

import (
	"context"
	"fmt"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// reconcileStep is one post-scan reconciliation pass: the pure decision over
// the library's rows (reconcile.go), the field its answer sets, and the store
// writer that persists it. The scan's tail runs the steps in order against the
// store (runReconcileStep), and settleHeldReconciles runs the same steps, in
// the same order, in memory: each reads what the one before it wrote.
type reconcileStep struct {
	// label names the pass in an error line (reportReconciliation).
	label string
	// fixed is the Info line of a pass that rewrote rows.
	fixed string
	// decide returns the targets the pass rewrites, each carrying its new
	// value. Pure; it reads only the fields its pass uses.
	decide func([]ReconcileTarget) []ReconcileTarget
	// take copies the field the pass writes from c onto a target.
	take func(dst *ReconcileTarget, c ReconcileTarget)
	// set copies the field the pass writes from c onto a Track.
	set func(t *Track, c ReconcileTarget)
	// apply is the store writer (applyReconciledTracks underneath).
	apply func(s *Store, ctx context.Context, changed []Track) (int, error)
}

// albumTitleStep rewrites tracks whose album tag is just the folder name to
// their folder's single clean-sibling title (reconcileAlbumTitles), so they
// don't split off into a separate album row on iOS. It runs FIRST so the
// album-artist pass then groups the now-unified folder.
var albumTitleStep = reconcileStep{
	label:  "album-title reconciliation",
	fixed:  "album-title reconciliation fixed folder-name album tags",
	decide: reconcileAlbumTitles,
	take:   func(dst *ReconcileTarget, c ReconcileTarget) { dst.Album = c.Album },
	set:    func(t *Track, c ReconcileTarget) { t.Album = c.Album },
	apply:  (*Store).ApplyAlbumTitleReconciliation,
}

// albumArtistStep reconciles AlbumArtist inconsistencies within each
// directory (reconcileAlbumArtists), so one physical album yields one
// AlbumArtist, and therefore one album identity on iOS. No MusicBrainz.
var albumArtistStep = reconcileStep{
	label:  "album-artist reconciliation",
	fixed:  "album-artist reconciliation unified split albums",
	decide: reconcileAlbumArtists,
	take:   func(dst *ReconcileTarget, c ReconcileTarget) { dst.AlbumArtist = c.AlbumArtist },
	set:    func(t *Track, c ReconcileTarget) { t.AlbumArtist = c.AlbumArtist },
	apply:  (*Store).ApplyAlbumArtistReconciliation,
}

// yearStep fills a MISSING album year from the album's dominant year
// (reconcileYears), so a single untagged track doesn't split off into its own
// album row on iOS. Fill-missing only: a present year is left alone.
var yearStep = reconcileStep{
	label:  "year reconciliation",
	fixed:  "year reconciliation filled missing album years",
	decide: reconcileYears,
	take:   func(dst *ReconcileTarget, c ReconcileTarget) { dst.Year = c.Year },
	set:    func(t *Track, c ReconcileTarget) { t.Year = c.Year },
	apply:  (*Store).ApplyYearReconciliation,
}

// yearByMBIDStep fills a year-0 stray (a few loose tracks in their own
// folder) from a same-MBID sibling (reconcileYearsByMBID), bounded to genuine
// strays so it cannot merge two full copies or editions. It complements the
// within-folder year pass before it.
var yearByMBIDStep = reconcileStep{
	label:  "year reconciliation (mbid)",
	fixed:  "year reconciliation (mbid) filled stray years",
	decide: reconcileYearsByMBID,
	take:   func(dst *ReconcileTarget, c ReconcileTarget) { dst.Year = c.Year },
	set:    func(t *Track, c ReconcileTarget) { t.Year = c.Year },
	apply:  (*Store).ApplyYearReconciliation,
}

// trackNumberStep fills a MISSING track number from the filename's leading
// "NN" (backfillTrackNumbersFromPath), so albums indexed before the
// extractor-level backfill (the scanner skips unchanged files, so they never
// re-extract) still order correctly on iOS.
var trackNumberStep = reconcileStep{
	label:  "track-number reconciliation",
	fixed:  "track-number reconciliation filled missing track numbers",
	decide: backfillTrackNumbersFromPath,
	take:   func(dst *ReconcileTarget, c ReconcileTarget) { dst.TrackNumber = c.TrackNumber },
	set:    func(t *Track, c ReconcileTarget) { t.TrackNumber = c.TrackNumber },
	apply:  (*Store).ApplyTrackNumberReconciliation,
}

// reconcileSteps are the reconciliation passes in the order the scan's tail
// runs them, and settleHeldReconciles replays them. One list, so the two
// cannot come to disagree about the order.
var reconcileSteps = []reconcileStep{albumTitleStep, albumArtistStep, yearStep, yearByMBIDStep, trackNumberStep}

// reconcileTargetOf is t's projection for the passes: every field any of
// them reads. The integers are copied, never shared: StreamTracks reuses one
// Track across rows, so a callback must not keep its pointers.
func reconcileTargetOf(t *Track) ReconcileTarget {
	return ReconcileTarget{
		Path:               t.Path,
		Album:              t.Album,
		AlbumArtist:        t.AlbumArtist,
		Year:               copyIntPtr(t.Year),
		TrackNumber:        copyIntPtr(t.TrackNumber),
		MusicBrainzAlbumID: t.MusicBrainzAlbumID,
	}
}

// copyIntPtr returns a pointer to a copy of *p, or nil.
func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// streamReconcileTargets streams the library into the passes' targets,
// leaving out the UPnP-routed rows (routedSet, computed once by the caller:
// reconciling them re-opens the enrich→walk→wipe loop on hybrid libraries),
// and never materializing every full Track (OOM discipline on low-memory
// hosts). A row in overlay is read from it in place of its stored values.
func (s *Scanner) streamReconcileTargets(ctx context.Context, routedSet map[string]struct{}, overlay map[string]*Track) ([]ReconcileTarget, error) {
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		if o, ok := overlay[t.Path]; ok {
			t = o
		}
		targets = append(targets, reconcileTargetOf(t))
		return nil
	}); err != nil {
		return nil, fmt.Errorf("stream tracks: %w", err)
	}
	return targets, nil
}

// runReconcileStep runs one pass over the whole library: it streams the
// targets, decides the rows the pass rewrites, and persists them
// (loadAndApplyReconciled: indexed_at bumped, enriched_at untouched, a row
// changed since its read left for the next scan). It returns the number of
// rows written. DB-only, no network.
func (s *Scanner) runReconcileStep(ctx context.Context, routedSet map[string]struct{}, step reconcileStep) (int, error) {
	targets, err := s.streamReconcileTargets(ctx, routedSet, nil)
	if err != nil {
		return 0, err
	}
	return s.loadAndApplyReconciled(ctx, step.decide(targets), step.set,
		func(ctx context.Context, changed []Track) (int, error) { return step.apply(s.store, ctx, changed) })
}

// runReconcileStepsInMemory runs every pass over targets, in the tail's order,
// each step's answer written into targets before the next step reads them:
// what the tail's passes leave in the store, computed without writing it.
func runReconcileStepsInMemory(targets []ReconcileTarget) {
	at := make(map[string]int, len(targets))
	for i := range targets {
		at[targets[i].Path] = i
	}
	for _, step := range reconcileSteps {
		for _, c := range step.decide(targets) {
			if i, ok := at[c.Path]; ok {
				step.take(&targets[i], c)
			}
		}
	}
}

// reconciledFieldsDiffer reports whether a and b differ in a field a
// reconciliation pass writes: the album, the album artist, the year, the
// track number.
func reconciledFieldsDiffer(a, b *Track) bool {
	return a.Album != b.Album || a.AlbumArtist != b.AlbumArtist ||
		!sameIntPtr(a.Year, b.Year) || !sameIntPtr(a.TrackNumber, b.TrackNumber)
}

// sameIntPtr reports whether two optional integers are equal: both absent,
// or both present with one value.
func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// maxHeldReconciles bounds the re-reads a scan holds for its tail, each a
// whole Track in memory until the tail writes it. An extractor change that
// reads an album field differently across a whole library would otherwise
// hold every row at once; past the bound a re-read is decided as it was
// before backlog B188, and churns as it did.
const maxHeldReconciles = 10000

// mayHoldAnother counts one more re-read the scan would hold, and reports
// whether the scan's limit (heldLimit, else maxHeldReconciles) admits it.
func (s *Scanner) mayHoldAnother() bool {
	limit := s.heldLimit
	if limit <= 0 {
		limit = maxHeldReconciles
	}
	return s.heldCount.Add(1) <= limit
}

// takeHeldReconciles returns the scan's held re-reads and forgets them.
func (s *Scanner) takeHeldReconciles() []*Track {
	held := s.heldReconciles
	s.heldReconciles = nil
	s.heldCount.Store(0)
	return held
}

// settleHeldReconciles writes the scan's held re-reads (Track.awaitsReconcile)
// as the reconciliation passes will leave them, and returns how many it
// wrote. It runs at the scan's reconciliation head, after the deletion pass
// and before the passes, with the routed set the passes use.
//
// Each held re-read is merged with its row as stored now
// (mergePostScanFields), and the passes run in memory (runReconcileStepsInMemory)
// over the library's rows with EVERY held re-read's values in place of its
// row's: the inputs the tail's passes would read had the scan written those
// rows as they stand. A re-read then takes the values the passes give it: one
// that marshals as its stored row is stamped (versionStampOnly), any other is
// written whole, with those values, so the passes after it find nothing left
// to change. The held values stand in for all the held rows at once, and
// that is what keeps a bump's extractor change from being voted down: a
// change that reads every track of an album differently leaves no outlier,
// while one judged against its siblings' stored rows would be voted back to
// the old reading, row by row, and never applied.
//
// A re-read whose row is gone (reaped since its worker read it) writes
// nothing. A failed stream writes the held re-reads as they stand
// (settleHeldUnreconciled's answer); a shutdown writes nothing, and the next
// scan re-reads them, since their rows keep their stale version.
func (s *Scanner) settleHeldReconciles(ctx context.Context, routedSet map[string]struct{}) int {
	held := s.takeHeldReconciles()
	if len(held) == 0 || ctx.Err() != nil {
		return 0
	}
	live, stored := s.mergeHeldWithStored(ctx, held)
	if len(live) == 0 {
		return 0
	}
	overlay := make(map[string]*Track, len(live))
	for _, t := range live {
		overlay[t.Path] = t
	}
	targets, err := s.streamReconcileTargets(ctx, routedSet, overlay)
	if err != nil {
		failure := ctxerr.WithoutCancellation(ctx, err)
		if failure == nil {
			return 0 // a shutdown stopped the stream: write nothing
		}
		scanLogger.Warn("held re-reads written unreconciled", "rows", len(live), "err", failure)
		written, _, _ := s.writeHeld(ctx, live, stored)
		return written
	}
	runReconcileStepsInMemory(targets)
	for i := range targets {
		if t, ok := overlay[targets[i].Path]; ok {
			t.Album, t.AlbumArtist = targets[i].Album, targets[i].AlbumArtist
			t.Year, t.TrackNumber = targets[i].Year, targets[i].TrackNumber
		}
	}
	written, stamped, rewritten := s.writeHeld(ctx, live, stored)
	scanLogger.Info("version-stale re-reads written as reconciliation leaves them",
		"stamped", stamped, "rewritten", rewritten)
	return written
}

// settleHeldUnreconciled writes the scan's held re-reads as they stand, each
// merged with its stored row and stamped when unchanged: what the version-stale
// leg did before backlog B188. It is the answer of a scan that runs no
// reconciliation (its routed set could not be read, or it ended before its
// reconciliation head); a shutdown writes nothing.
func (s *Scanner) settleHeldUnreconciled(ctx context.Context) int {
	held := s.takeHeldReconciles()
	if len(held) == 0 || ctx.Err() != nil {
		return 0
	}
	live, stored := s.mergeHeldWithStored(ctx, held)
	written, _, _ := s.writeHeld(ctx, live, stored)
	return written
}

// mergeHeldWithStored merges each held re-read with its row as stored now,
// and returns the re-reads that still have a row, with those rows by path. A
// row the lookup cannot read is left out: its stale version sends the next
// scan back to it.
func (s *Scanner) mergeHeldWithStored(ctx context.Context, held []*Track) ([]*Track, map[string]*Track) {
	live := make([]*Track, 0, len(held))
	stored := make(map[string]*Track, len(held))
	for _, t := range held {
		cur, err := s.store.GetTrack(ctx, t.Path)
		if err != nil {
			if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
				scanLogger.Warn("held re-read lookup", "path", t.Path, "err", failure)
			}
			continue
		}
		if cur == nil {
			continue
		}
		t.awaitsReconcile = false
		mergePostScanFields(t, cur)
		live = append(live, t)
		stored[t.Path] = cur
	}
	return live, stored
}

// writeHeld writes merged re-reads, each stamped when it marshals as its
// stored row (markStampIfUnchanged) and written whole otherwise, in the
// writer's batches. It returns how many rows it wrote, and how many of the
// re-reads it stamped and wrote whole.
func (s *Scanner) writeHeld(ctx context.Context, live []*Track, stored map[string]*Track) (written, stamped, rewritten int) {
	var full, stamps []*Track
	for _, t := range live {
		markStampIfUnchanged(t, stored[t.Path])
		if t.versionStampOnly {
			stamps = append(stamps, t)
		} else {
			full = append(full, t)
		}
	}
	for start := 0; start < len(full); start += scanBatchSize {
		chunk := full[start:min(start+scanBatchSize, len(full))]
		if err := s.store.UpsertTrackBatch(ctx, chunk); err != nil {
			if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
				scanLogger.Error("upsert batch", "rows", len(chunk), "err", failure)
			}
			continue
		}
		written += len(chunk)
	}
	for start := 0; start < len(stamps); start += scanBatchSize {
		chunk := stamps[start:min(start+scanBatchSize, len(stamps))]
		if err := s.store.StampExtractorVersionBatch(ctx, chunk); err != nil {
			if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
				scanLogger.Error("stamp extractor-version batch", "rows", len(chunk), "err", failure)
			}
			continue
		}
		written += len(chunk)
	}
	if written > 0 {
		// Committed rows, as the scan's writer counts them (runScanWriter).
		s.progress.Add(int64(written))
		s.noteScanProgress(time.Now())
	}
	return written, len(stamps), len(full)
}
