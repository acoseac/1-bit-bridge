package manifest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// scanLogger is the scanner-specific component logger. The store
// half of internal/manifest uses the package-level `logger` declared
// in store.go (component=manifest); scanner uses its own
// component=scanner so high-volume scan output filters cleanly from
// store/manifest reads.
var scanLogger = logging.Component("scanner")

// afterExtractHookForTests, when non-nil, runs inside runScanWorker's
// per-iteration recovery scope after ExtractWithContext returns. The
// only intended use is to trigger a deterministic panic from a unit
// test so the recover branch can be exercised without depending on a
// specific dhowden/tag failure mode (which varies by version + file
// shape). Production cost: one nil-check per file, negligible against
// the actual extract work. Strictly internal to the package.
var afterExtractHookForTests func(absPath string)

// scanBatchSize is the per-transaction track-upsert batch size used by
// the scanner's writer goroutine. SQLite's per-transaction fsync
// dominated initial-scan time on large libraries — a 50k-track library
// at one row per implicit-tx hit the disk 50k times. Batching to 500
// rows per BEGIN/COMMIT collapses that to ~100 transactions.
const scanBatchSize = 500

// scanChannelBuffer sizes both the path → worker and worker → writer
// channels. Large enough that the walker / extractors don't stall on
// transient slow stretches (worker doing a slow Extract on one file,
// writer mid-fsync); small enough that backpressure still applies and
// memory stays bounded under unusually slow downstream stages.
const scanChannelBuffer = 256

// pathInfo carries one walker-discovered file from the WalkDir
// callback to the worker pool. Fields are immutable post-send.
type pathInfo struct {
	abs  string
	rel  string
	info fs.FileInfo
}

// Scanner walks the library roots, calls Extract on every audio file it
// finds, and persists the result in a Store. It is safe to call Scan
// concurrently but the scanner enforces only one scan in flight at a time
// (subsequent calls block until the running scan finishes).
//
// Derivation: library-relative paths are forward-slash form regardless of
// the server OS, matching the wire shape. On Windows that means the
// scanner translates native separators → "/" for storage and back for
// native filesystem access. On Unix it's a no-op.
//
// Roots are hot-swappable via SetRoots so the admin console can add or
// remove a library root without restarting the server. Each Scan() takes a
// snapshot of the roots at entry so a mid-flight edit doesn't re-enter the
// walk with a different set — the new roots apply on the next scan.
type Scanner struct {
	roots atomic.Pointer[[]string]
	store *Store

	// artDir is the on-disk artwork cache directory the scanner writes
	// locally-extracted artwork (`local-<sha256>-500.jpg`) into. Empty
	// disables local-artwork extraction entirely (back-compat for the
	// 1-bit-bridge call sites that don't run the artwork pipeline). The
	// /v1/artwork handler reads from the same directory the enricher
	// writes to, so scanner-side files are served transparently.
	artDir string

	// folderArt single-flights `cover.jpg` / `folder.jpg` lookups on a
	// per-directory basis: the first worker to touch a given directory
	// installs a `*folderArtPromise`, runs the ReadDir + hash + atomic
	// write inside `once.Do`, and every subsequent worker on the same
	// directory parks inside Do until that work completes, then reads
	// the cached result. Reset at the top of each Scan / ScanSubtree
	// — cross-scan persistence would create stale "no folder.jpg" hits
	// when a user adds cover.jpg between scans.
	folderArt sync.Map // dir-path string -> *folderArtPromise

	mu       sync.Mutex
	scanning atomic.Bool
	lastFull atomic.Int64 // UnixNano of last successful full scan
	progress atomic.Int64 // tracks indexed so far during the current scan

	// panickedCnt counts files whose extraction panicked and was
	// recovered by the per-iteration `defer recover()` in
	// runScanWorker. Surfaced in the admin Library dashboard
	// alongside the existing scan counters so a malformed file the
	// dhowden/tag (or our own DSF/DFF parser) chokes on is visible
	// to operators instead of silently being skipped. Monotonic for
	// the process lifetime — rolls forward across scans.
	panickedCnt atomic.Int64

	// deleteThreshold is the N-consecutive-missing-scans grace period
	// before a row is reaped. Set via SetDeleteThreshold from cmd/bridge
	// at startup from cfg.Scanner.DeleteAfterMissingScans (default 3
	// per internal/config.DefaultDeleteAfterMissingScans). A value of
	// 1 preserves the pre-resilience immediate-delete behaviour for
	// operators on local-disk-only deployments where the silent-empty-
	// enumeration failure modes don't apply. Loaded via atomic.Int64
	// so a future admin-console live tune doesn't race the scanner's
	// deletion pass; today it's set once at boot.
	deleteThreshold atomic.Int64
}

// SetDeleteThreshold configures the missing-count grace period. Values
// <= 0 are normalised to 1 (immediate-delete behaviour). cmd/bridge
// calls this once after construction.
func (s *Scanner) SetDeleteThreshold(n int) {
	if n < 1 {
		n = 1
	}
	s.deleteThreshold.Store(int64(n))
}

// effectiveDeleteThreshold returns the configured threshold, falling
// back to 1 (immediate delete) when no SetDeleteThreshold call has been
// made — preserves the pre-resilience behaviour for any test harness
// or call path that constructs a Scanner without wiring the knob.
func (s *Scanner) effectiveDeleteThreshold() int {
	if t := int(s.deleteThreshold.Load()); t > 0 {
		return t
	}
	return 1
}

// folderArtResult records the per-directory outcome of a folder-art
// lookup. `found == true` means the directory had cover.jpg /
// folder.jpg (or a case-insensitive variant) and `mbid` carries the
// `local-<sha256>` sentinel ready to stamp on every track in that
// directory; `found == false` is a known-absent answer cached so
// sibling tracks short-circuit with no further filesystem work.
type folderArtResult struct {
	found bool
	mbid  string
}

// folderArtPromise serializes per-directory ReadDir + read + hash +
// atomic write so a 15-track album processed by 15 parallel workers
// does the I/O exactly once instead of 15 times. The first worker to
// LoadOrStore the pointer wins the once.Do; the rest retrieve the
// same pointer and park inside Do until the first worker's
// scanFolderArtwork returns. After Do unblocks every caller reads
// `res` directly — by that point it's settled.
type folderArtPromise struct {
	once sync.Once
	res  folderArtResult
}

// NewScanner constructs a Scanner. Caller owns the Store's lifecycle.
// artworkCacheDir is the directory under which the scanner writes
// `local-<sha256>-500.jpg` for embedded ID3 APIC art and folder-level
// cover.jpg / folder.jpg. Pass "" to disable local-artwork extraction
// (the rest of the pipeline still works — tracks just don't get a
// scanner-side `local-` ArtworkMBID and fall through to the enricher's
// MusicBrainz / iTunes path).
//
// Reads the on-disk `last_full_scan` scan-state key into `s.lastFull`
// so a fresh process keeps showing the most-recent successful scan
// timestamp on the dashboard rather than "never" until the next scan
// completes (which on a 50k-track library can be a long minute or
// two). Read failures and parse failures fall through silently —
// `LastFullScan()` returns the zero time and the dashboard shows
// "never", same as a fresh install.
//
// `OpenStore` already created the `scan_state` table (CREATE TABLE
// IF NOT EXISTS) before any caller can construct a Scanner against
// the returned `*Store`, so this lookup is safe at init time. A
// nil store is treated as "no persisted state" — the test harness
// occasionally constructs a Scanner without a store backing it.
func NewScanner(roots []string, store *Store, artworkCacheDir string) *Scanner {
	s := &Scanner{store: store, artDir: artworkCacheDir}
	rc := append([]string(nil), roots...)
	s.roots.Store(&rc)
	if store != nil {
		if v, err := store.GetScanState(context.Background(), "last_full_scan"); err == nil && v != "" {
			if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil && !t.IsZero() {
				s.lastFull.Store(t.UTC().UnixNano())
			}
		}
	}
	return s
}

// SetRoots atomically replaces the scanner's library roots. The change takes
// effect on the next Scan — an in-flight walk continues with its original
// snapshot. Caller should trigger a scan (or wait for the periodic tick) for
// the new roots to land in the manifest.
func (s *Scanner) SetRoots(roots []string) {
	rc := append([]string(nil), roots...)
	s.roots.Store(&rc)
}

// Roots returns a snapshot of the currently configured library roots.
func (s *Scanner) Roots() []string {
	p := s.roots.Load()
	if p == nil {
		return nil
	}
	return append([]string(nil), (*p)...)
}

// IsScanning reports whether a scan is currently running.
func (s *Scanner) IsScanning() bool { return s.scanning.Load() }

// LastFullScan returns the timestamp of the most recent successful scan,
// or the zero time if none has completed yet.
func (s *Scanner) LastFullScan() time.Time {
	ns := s.lastFull.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// ScanProgress returns the number of tracks indexed during the current
// scan (resets at the start of each Scan).
func (s *Scanner) ScanProgress() int64 { return s.progress.Load() }

// PanickedCount returns the cumulative number of files whose extraction
// panicked and was recovered by the per-iteration `defer recover()` in
// runScanWorker. Monotonic for the process lifetime — does NOT reset
// between scans, so the admin "Library" dashboard can surface a steady
// "N files unreadable" hint that persists past the latest scan.
func (s *Scanner) PanickedCount() int64 { return s.panickedCnt.Load() }

// Scan runs a full walk of the library roots. Safe to cancel via ctx;
// any tracks whose batch flushed before cancellation are committed.
// Returns the count of tracks upserted (= committed by the writer).
//
// Pipeline: one walker goroutine drives `filepath.WalkDir`, fanning
// audio paths into a NumCPU-sized worker pool that does the CPU-bound
// tag extraction. Workers feed completed Tracks into a single writer
// goroutine that batches via `Store.UpsertTrackBatch` (one BEGIN/COMMIT
// per `scanBatchSize` rows). Folder upserts stay inline on the walker
// — they're 10× less common than tracks and serialising them avoids a
// second pipeline. The previous shape was fully serial: a 50k-track
// library left multi-core extraction unused AND paid a per-row SQLite
// fsync (50k transactions). The new shape is bound by walker+writer
// throughput, not single-core extract.
//
// **Progress semantics**: `s.progress` reflects rows committed (i.e.
// post-`UpsertTrackBatch`-flush), not rows extracted-but-pending. iOS
// reads this for its "scanning · X tracks" hint, and the older meaning
// would briefly over-report on a crash mid-batch.
func (s *Scanner) Scan(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanning.Store(true)
	s.progress.Store(0)
	// Per-Scan reset of the folder-art single-flight cache. Cross-scan
	// persistence would create stale "no folder.jpg" hits when a user
	// adds cover.jpg between scans (the scanner re-extracts the track
	// but the cache still says "absent").
	s.folderArt = sync.Map{}
	defer s.scanning.Store(false)

	// Snapshot of paths we knew about BEFORE this scan. At the end we drop
	// rows whose paths weren't touched during the walk — that's the
	// "deleted from disk" pass. Folders snapshot the same way so the
	// folder deletion pass can reap rows for directories the walker
	// no longer encounters (rename / removal upstream of any tracks
	// the user kept). Both snapshots are read-only post-walk.
	before, err := s.store.TrackPaths(ctx)
	if err != nil {
		return 0, fmt.Errorf("list existing: %w", err)
	}
	beforeSet := make(map[string]struct{}, len(before))
	for _, p := range before {
		beforeSet[p] = struct{}{}
	}
	beforeFolders, err := s.store.FolderPaths(ctx)
	if err != nil {
		return 0, fmt.Errorf("list existing folders: %w", err)
	}
	beforeFolderSet := make(map[string]struct{}, len(beforeFolders))
	for _, p := range beforeFolders {
		beforeFolderSet[p] = struct{}{}
	}

	// Walker writes `seen` (tracks) and `seenFolders` (directories)
	// from a single goroutine — workers don't touch either. Whether a
	// worker actually persists a row is independent of the deletion-
	// pass invariant ("we saw it on disk during this walk"); the
	// walker decides on visibility, not the worker.
	seen := make(map[string]struct{}, len(before))
	seenFolders := make(map[string]struct{}, len(beforeFolders))

	// Subtrees where the walker hit a transient I/O error (NAS drop,
	// antivirus lock, permission flap). The deletion pass below MUST
	// NOT delete tracks under these subtrees — pre-fix, a 200ms
	// network blip during a NAS scan caused every track in the
	// affected subtree to drop out of `seen` and get DeleteTrack'd
	// from the manifest. Files on disk were untouched but the
	// bridge served an empty/partial library until the next clean
	// scan repopulated. PR #N closes this hole.
	//
	// Keys are absolute directory paths (matching the form WalkDir
	// passes to the err callback); the deletion-pass guard checks
	// each candidate path against every entry as a hierarchical
	// prefix.
	errorSubtrees := make(map[string]struct{})

	// Snapshot roots once per scan so a mid-flight SetRoots doesn't re-enter
	// the walk with a different set.
	rootsPtr := s.roots.Load()
	var roots []string
	if rootsPtr != nil {
		roots = *rootsPtr
	}
	multiRoot := len(roots) > 1

	// Worker → writer pipeline. Both channels are buffered so a slow
	// stage doesn't stall the others over short blips, but bounded so
	// memory stays predictable under sustained slow downstream.
	paths := make(chan pathInfo, scanChannelBuffer)
	writes := make(chan *Track, scanChannelBuffer)

	nWorkers := runtime.NumCPU()
	var workersWG sync.WaitGroup
	for i := 0; i < nWorkers; i++ {
		workersWG.Add(1)
		go s.runScanWorker(ctx, paths, writes, multiRoot, &workersWG)
	}

	committed := new(atomic.Int64)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go s.runScanWriter(ctx, writes, committed, &writerWG)

	// Walker drives `filepath.WalkDir` for each configured root,
	// enqueuing audio files for the workers. Folder upserts stay inline
	// — single-goroutine writes, no contention with the batched
	// track-upsert path (which holds s.mu).
	//
	// FUSE drop mode (b) — clean-empty mount: immediately after each
	// per-root walk, if zero entries were observed AND the DB carries
	// history for this root AND no `.bridge-allow-empty` sentinel
	// file is present, sentinel the whole root so the deletion pass
	// spares its rows. Without this guard a cleanly-unmounted FUSE
	// mount (host directory still exists, contents vanished) is
	// indistinguishable from "operator legitimately wiped the root"
	// — `os.Stat(root)` and `WalkDir(root)` both succeed silently,
	// `errorSubtrees` stays empty, and the deletion pass nukes every
	// row under the dropped root. Per-root interrogation (not a
	// global post-loop check) isolates the protection to the failing
	// mount.
	var walkErr error
	for _, root := range roots {
		rootSentinel := relPath(root, root, multiRoot)
		observed, err := s.walkRoot(ctx, root, multiRoot, seen, seenFolders, errorSubtrees, paths)
		if err != nil {
			walkErr = err
			break
		}
		// Mode (a) already sentinel'd via walkRoot's upfront os.Stat;
		// skip the (b) interrogation for roots already errored.
		if _, errored := errorSubtrees[rootSentinel]; errored {
			// The root itself failed to walk → it contributed 0 files and
			// the deletion pass is spared. Surface ONE actionable line for
			// the silent-empty-library case (common in Docker when the
			// container user can't read a bind-mount). Re-probe with ReadDir:
			// os.Stat succeeds on a mounted-but-unreadable dir, so only the
			// directory-READ error distinguishes "unreadable by this user"
			// from "not mounted" (the latter is already logged as "root
			// unreachable" by walkRoot — don't double-log it here).
			if observed == 0 {
				if _, readErr := os.ReadDir(root); errors.Is(readErr, fs.ErrPermission) {
					scanLogger.Error("library root unreadable",
						"root", root, "uid", os.Getuid(),
						"hint", "indexed 0 files here — the mount isn't readable by this user; on Docker chown the bind-mount to the container UID or run with --user/PUID. See docs/docker.md")
				}
			}
			continue
		}
		if observed == 0 && !hasAllowEmptySentinel(root) {
			n, countErr := s.store.CountTracksUnderRoot(ctx, root, multiRoot)
			if countErr != nil {
				// Fail closed: we can't audit, so sentinel the root
				// rather than letting the deletion pass run on
				// untrusted state. CodeRabbit Major + Gemini medium
				// on PR #289 — pre-fix the .warn+continue silently
				// disabled the safety gate.
				scanLogger.Warn("count tracks under root; conservatively sparing deletion for root",
					"root", root, "err", countErr)
				errorSubtrees[rootSentinel] = struct{}{}
				continue
			}
			if n > 0 {
				scanLogger.Error("suspected clean-empty mount failure",
					"root", root, "rows_in_db", n,
					"hint", "place .bridge-allow-empty at the root to confirm intent")
				errorSubtrees[rootSentinel] = struct{}{}
			}
		}
	}

	// Drain order matters: close `paths` first so workers can exit
	// their range loop; wait for all workers; close `writes` so the
	// writer can exit its range loop; wait for the writer to flush its
	// final batch.
	close(paths)
	workersWG.Wait()
	close(writes)
	writerWG.Wait()

	count := int(committed.Load())
	if walkErr != nil {
		return count, walkErr
	}

	// Deletion pass: anything in the "before" snapshot that we didn't
	// see in this walk gets its missing_count bumped; rows whose
	// missing_count hits the configured threshold get reaped.
	// errorSubtrees-spared paths are skipped entirely (their counter
	// is NOT incremented, so a transient flap doesn't burn one of
	// their N grace scans). The threshold defaults to 3 — see
	// internal/config.DefaultDeleteAfterMissingScans + the
	// `missing_count` migration doc for the silent-empty-enumeration
	// rationale.
	threshold := s.effectiveDeleteThreshold()
	// UPnP-routed rows are NOT filesystem-owned — they never appear in
	// a disk walk, so without this exclusion every scan would bump
	// their missing_count and the threshold pass would mass-delete the
	// routed catalog (15k rows for a Chord 2Go upstream) after
	// `threshold` scans. Their lifecycle belongs to the upstream
	// ingest's last_seen_at reconcile. A fetch failure degrades to an
	// empty set — the store-side NOT-IN guard on the threshold DELETE
	// is the backstop that keeps routed rows undeletable regardless.
	routedSet := make(map[string]struct{})
	if routed, err := s.store.UPnPRoutedSourcePaths(ctx); err != nil {
		scanLogger.Warn("routed-paths fetch for missing pass failed", "err", err)
	} else {
		for _, p := range routed {
			routedSet[p] = struct{}{}
		}
	}
	missingTracks := make([]string, 0)
	spared := 0
	for p := range beforeSet {
		if _, ok := seen[p]; ok {
			continue
		}
		if _, routed := routedSet[p]; routed {
			continue
		}
		if isUnderErroredSubtree(p, errorSubtrees) {
			spared++
			continue
		}
		missingTracks = append(missingTracks, p)
	}
	deletedTracks, err := s.store.IncrementMissingTracksAndDeleteAtThreshold(ctx, missingTracks, threshold)
	if err != nil {
		scanLogger.Error("missing-count tracks pass", "err", err, "missing", len(missingTracks))
	}
	if spared > 0 {
		scanLogger.Warn("spared tracks from deletion pass (parent walk error)",
			"spared", spared, "subtrees", len(errorSubtrees))
	}
	if len(missingTracks) > 0 {
		scanLogger.Info("tracks missing this scan",
			"missing", len(missingTracks), "deleted", deletedTracks, "threshold", threshold)
	}

	// Folder orphan-cleanup pass: a directory the walker no longer
	// encounters (renamed or removed upstream of every track that was
	// originally indexed under it) leaves a folders-table row behind
	// otherwise. Without this pass the table grows unboundedly and
	// `/v1/manifest`-derived "Folders" surfaces would ship phantom
	// directories. Same threshold-based grace period as tracks — a
	// transient walk error AND a brief flap that escapes the
	// errorSubtrees catch both get one missed-scan increment without
	// data loss.
	missingFolders := make([]string, 0)
	sparedFolders := 0
	for p := range beforeFolderSet {
		if _, ok := seenFolders[p]; ok {
			continue
		}
		if isUnderErroredSubtree(p, errorSubtrees) {
			sparedFolders++
			continue
		}
		missingFolders = append(missingFolders, p)
	}
	deletedFolders, err := s.store.IncrementMissingFoldersAndDeleteAtThreshold(ctx, missingFolders, threshold)
	if err != nil {
		scanLogger.Error("missing-count folders pass", "err", err, "missing", len(missingFolders))
	}
	if sparedFolders > 0 {
		scanLogger.Warn("spared folders from deletion pass (parent walk error)",
			"spared", sparedFolders, "subtrees", len(errorSubtrees))
	}
	if len(missingFolders) > 0 {
		scanLogger.Info("folders missing this scan",
			"missing", len(missingFolders), "deleted", deletedFolders, "threshold", threshold)
	}

	s.lastFull.Store(time.Now().UTC().UnixNano())
	_ = s.store.SetScanState(ctx, "last_full_scan", time.Now().UTC().Format(time.RFC3339Nano))

	// Album-title reconciliation: rewrite tracks whose album tag is just the
	// folder name (a mis-tag / scan fallback, e.g. the dub folder convention)
	// to their folder's single clean-sibling title, so they don't split off
	// into a separate album row on iOS. Runs FIRST so the AlbumArtist pass
	// below then groups the now-unified folder. DB-only, enriched_at-untouched.
	// Non-fatal.
	if n, rErr := s.runAlbumTitleReconciliation(ctx); rErr != nil {
		scanLogger.Error("album-title reconciliation", "err", rErr)
	} else if n > 0 {
		scanLogger.Info("album-title reconciliation fixed folder-name album tags", "tracks", n)
	}
	// Reconcile AlbumArtist inconsistencies within each directory so one
	// physical album yields one consistent AlbumArtist (and therefore one
	// album identity on iOS). DB-only — no MusicBrainz; leaves
	// enriched_at untouched. Non-fatal: a reconciliation error must not
	// fail an otherwise-successful scan.
	if n, rErr := s.runAlbumArtistReconciliation(ctx); rErr != nil {
		scanLogger.Error("album-artist reconciliation", "err", rErr)
	} else if n > 0 {
		scanLogger.Info("album-artist reconciliation unified split albums", "tracks", n)
	}
	// Year reconciliation: fill a MISSING album year from the album's
	// dominant year, so a single untagged track doesn't split off into its
	// own album row on iOS. Same DB-only, enriched_at-untouched contract as
	// the AlbumArtist pass. Non-fatal.
	if n, rErr := s.runYearReconciliation(ctx); rErr != nil {
		scanLogger.Error("year reconciliation", "err", rErr)
	} else if n > 0 {
		scanLogger.Info("year reconciliation filled missing album years", "tracks", n)
	}
	// Cross-folder year fill by MusicBrainz release id: fills a year-0 stray
	// (a few loose tracks in their own folder) from a same-MBID sibling —
	// bounded to genuine strays so it can't merge two full copies / editions.
	// Complements the within-folder pass above. Same DB-only,
	// enriched_at-untouched contract. Non-fatal.
	if n, rErr := s.runYearReconciliationByMBID(ctx); rErr != nil {
		scanLogger.Error("year reconciliation (mbid)", "err", rErr)
	} else if n > 0 {
		scanLogger.Info("year reconciliation (mbid) filled stray years", "tracks", n)
	}
	// Track-number backfill: fill a MISSING track number from the filename's
	// leading "NN" so albums indexed before the extractor-level backfill (the
	// scanner skips unchanged files by mtime, so they never re-extract) still
	// order correctly on iOS. Same DB-only, enriched_at-untouched contract;
	// routed UPnP rows excluded. Non-fatal.
	if n, rErr := s.runTrackNumberReconciliation(ctx); rErr != nil {
		scanLogger.Error("track-number reconciliation", "err", rErr)
	} else if n > 0 {
		scanLogger.Info("track-number reconciliation filled missing track numbers", "tracks", n)
	}

	return count, nil
}

// routedExclusionSet returns the set of UPnP-routed source paths that the
// reconciliation passes MUST skip. Routed rows are owned by the upstream
// ingest's skip-if-unchanged reconcile; the bridge-side reconcilers write the
// exact fields walkFieldsEqual diffs (AlbumArtist / Album / Year /
// TrackNumber), so reconciling a routed row makes the next UPnP walk see a
// mismatch and re-upsert it — resetting enriched_at and re-bumping indexed_at
// on every cycle (the PR #369 enrich→walk→wipe loop, otherwise re-opened via
// the reconciliation vector; mirrors the missing-pass's routed sparing, #370).
//
// FAIL CLOSED on a fetch error: these passes have no store-side NOT-IN backstop
// (unlike the missing-pass DELETE), so continuing with an empty exclusion set
// would reconcile routed rows. Aborting is safe — the Scan tail logs + continues
// (non-fatal) and the next scan retries. A filesystem-only library returns an
// empty set with no error and proceeds normally.
func (s *Scanner) routedExclusionSet(ctx context.Context) (map[string]struct{}, error) {
	routed, err := s.store.UPnPRoutedSourcePaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("routed source paths: %w", err)
	}
	set := make(map[string]struct{}, len(routed))
	for _, p := range routed {
		set[p] = struct{}{}
	}
	return set, nil
}

// runAlbumArtistReconciliation runs the post-scan AlbumArtist
// consistency pass over the whole library: load all tracks, compute the
// directory-scoped dominant-value fixes (see reconcileAlbumArtists), and
// persist them (indexed_at bumped, enriched_at untouched). Returns the
// number of tracks unified. DB-only — no network.
func (s *Scanner) runAlbumArtistReconciliation(ctx context.Context) (int, error) {
	// Skip UPnP-routed rows (see routedExclusionSet): reconciling them
	// re-opens the enrich→walk→wipe loop on hybrid libraries.
	routedSet, err := s.routedExclusionSet(ctx)
	if err != nil {
		return 0, err
	}
	// Stream the whole library into lightweight targets — never
	// materialize every full Track (OOM risk on low-memory hosts; the
	// codebase streams everywhere else for the same reason).
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		targets = append(targets, ReconcileTarget{Path: t.Path, Album: t.Album, AlbumArtist: t.AlbumArtist})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("stream tracks: %w", err)
	}
	changed := reconcileAlbumArtists(targets)
	return s.loadAndApplyReconciled(ctx, changed,
		func(t *Track, c ReconcileTarget) { t.AlbumArtist = c.AlbumArtist },
		s.store.ApplyAlbumArtistReconciliation)
}

// runYearReconciliation runs the post-scan year fill-missing pass: it
// streams the library into lightweight targets (never materializing every
// full Track — OOM discipline, same as the AlbumArtist pass), fills a
// MISSING album year from the album's dominant year (see reconcileYears),
// loads the full Track only for the changed rows, and persists via
// ApplyYearReconciliation (bumps indexed_at, leaves enriched_at untouched).
// A row deleted between the stream and the get is SKIPPED, not fatal.
func (s *Scanner) runYearReconciliation(ctx context.Context) (int, error) {
	routedSet, err := s.routedExclusionSet(ctx) // skip routed rows (see routedExclusionSet)
	if err != nil {
		return 0, err
	}
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		// Deep-copy the year value: StreamTracks reuses one Track
		// allocation across rows, so the callback must not retain its
		// pointers. A plain value copy keeps the target independent.
		var yr *int
		if t.Year != nil {
			v := *t.Year
			yr = &v
		}
		targets = append(targets, ReconcileTarget{Path: t.Path, Album: t.Album, Year: yr})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("stream tracks: %w", err)
	}
	changed := reconcileYears(targets)
	return s.loadAndApplyReconciled(ctx, changed,
		func(t *Track, c ReconcileTarget) { t.Year = c.Year },
		s.store.ApplyYearReconciliation)
}

// runAlbumTitleReconciliation runs the post-scan album-title fix: it streams
// the library into lightweight targets, rewrites tracks whose album tag is just
// the folder name to their folder's single clean-sibling title (see
// reconcileAlbumTitles), loads the full Track only for the changed rows, and
// persists via ApplyAlbumTitleReconciliation (bumps indexed_at, leaves
// enriched_at untouched). Runs BEFORE the AlbumArtist pass so the two compose
// in one scan (unified titles let the AlbumArtist pass then group the folder).
func (s *Scanner) runAlbumTitleReconciliation(ctx context.Context) (int, error) {
	routedSet, err := s.routedExclusionSet(ctx) // skip routed rows (see routedExclusionSet)
	if err != nil {
		return 0, err
	}
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		targets = append(targets, ReconcileTarget{Path: t.Path, Album: t.Album})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("stream tracks: %w", err)
	}
	changed := reconcileAlbumTitles(targets)
	return s.loadAndApplyReconciled(ctx, changed,
		func(t *Track, c ReconcileTarget) { t.Album = c.Album },
		s.store.ApplyAlbumTitleReconciliation)
}

// runYearReconciliationByMBID runs the post-scan CROSS-folder year fill: it
// streams the library into lightweight targets carrying the MusicBrainz release
// id, fills a year-0 stray's year from a same-MBID sibling (see
// reconcileYearsByMBID — bounded to genuine strays), loads the full Track only
// for the changed rows, and persists via ApplyYearReconciliation (bumps
// indexed_at, leaves enriched_at untouched). Complements the within-folder
// reconcileYears for strays that live in their own single-track folder.
func (s *Scanner) runYearReconciliationByMBID(ctx context.Context) (int, error) {
	routedSet, err := s.routedExclusionSet(ctx) // skip routed rows (see routedExclusionSet)
	if err != nil {
		return 0, err
	}
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		// Deep-copy the year pointer (StreamTracks reuses one Track alloc);
		// MusicBrainzAlbumID is a string value, copied by the struct assignment.
		var yr *int
		if t.Year != nil {
			v := *t.Year
			yr = &v
		}
		targets = append(targets, ReconcileTarget{Path: t.Path, Year: yr, MusicBrainzAlbumID: t.MusicBrainzAlbumID})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("stream tracks: %w", err)
	}
	changed := reconcileYearsByMBID(targets)
	return s.loadAndApplyReconciled(ctx, changed,
		func(t *Track, c ReconcileTarget) { t.Year = c.Year },
		s.store.ApplyYearReconciliation)
}

// runTrackNumberReconciliation runs the post-scan track-number backfill pass:
// it streams the library into lightweight targets (never materializing every
// full Track — OOM discipline), fills a MISSING track number from the filename
// (see backfillTrackNumbersFromPath), loads the full Track only for the changed
// rows, and persists via ApplyTrackNumberReconciliation (bumps indexed_at,
// leaves enriched_at untouched). This is the migration path for tracks indexed
// before the extractor-level backfill — the scanner skips unchanged files, so
// they never re-extract. A row deleted between the stream and the get is
// SKIPPED, not fatal.
func (s *Scanner) runTrackNumberReconciliation(ctx context.Context) (int, error) {
	// Exclude UPnP-routed rows (their track numbers belong to the upstream
	// DIDL metadata, not bridge-side filename parsing) — see routedExclusionSet
	// for the full rationale and the fail-closed contract, shared by all five
	// reconciliation passes.
	routedSet, err := s.routedExclusionSet(ctx)
	if err != nil {
		return 0, err
	}
	var targets []ReconcileTarget
	if err := s.store.StreamTracks(ctx, nil, func(t *Track) error {
		if _, isRouted := routedSet[t.Path]; isRouted {
			return nil
		}
		// Deep-copy the pointer value: StreamTracks reuses one Track
		// allocation across rows, so the callback must not retain its
		// pointers. A plain value copy keeps the target independent.
		var tn *int
		if t.TrackNumber != nil {
			v := *t.TrackNumber
			tn = &v
		}
		targets = append(targets, ReconcileTarget{Path: t.Path, TrackNumber: tn})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("stream tracks: %w", err)
	}
	changed := backfillTrackNumbersFromPath(targets)
	return s.loadAndApplyReconciled(ctx, changed,
		func(t *Track, c ReconcileTarget) { t.TrackNumber = c.TrackNumber },
		s.store.ApplyTrackNumberReconciliation)
}

// loadAndApplyReconciled loads the full Track for each changed target, stamps
// the reconciled value via set, skips rows deleted since the stream (not
// fatal), and persists the batch via apply. Shared by the three post-scan
// reconciliation passes (AlbumArtist / Year / TrackNumber); apply bumps
// indexed_at and leaves enriched_at untouched (see applyReconciledTracks).
func (s *Scanner) loadAndApplyReconciled(
	ctx context.Context,
	changed []ReconcileTarget,
	set func(t *Track, c ReconcileTarget),
	apply func(context.Context, []Track) (int, error),
) (int, error) {
	if len(changed) == 0 {
		return 0, nil
	}
	tracks := make([]Track, 0, len(changed))
	for _, c := range changed {
		t, err := s.store.GetTrack(ctx, c.Path)
		if err != nil {
			return 0, fmt.Errorf("get track %q: %w", c.Path, err)
		}
		if t == nil {
			continue // deleted between the stream and the get — skip, not fatal
		}
		set(t, c)
		tracks = append(tracks, *t)
	}
	if len(tracks) == 0 {
		return 0, nil
	}
	return apply(ctx, tracks)
}

// runScanWorker is one of NumCPU workers reading walker-supplied paths
// off `paths`, doing the early-skip GetTrack check + the CPU-bound
// Extract, and feeding completed Tracks into the writer's `writes`
// channel. Errors from GetTrack/Extract are logged-and-skipped (matches
// the legacy walker's "log + continue" semantics — a single corrupt
// FLAC must not abort the whole scan).
func (s *Scanner) runScanWorker(ctx context.Context, paths <-chan pathInfo, writes chan<- *Track, multiRoot bool, wg *sync.WaitGroup) {
	defer wg.Done()
	// One ExtractContext per worker, reused across every track this
	// worker pulls. The pointer to s.folderArt is stable for the
	// lifetime of the Scan (we replaced the value at the top of Scan,
	// and nothing else mutates the field during the scan), so all
	// workers share the same single-flight map. Empty s.artDir
	// disables local-artwork extraction inside ExtractWithContext.
	ec := &ExtractContext{
		ArtworkCacheDir: s.artDir,
		FolderArtCache:  &s.folderArt,
	}
	for pi := range paths {
		if ctx.Err() != nil {
			// Drain remaining paths without doing work so the walker
			// can close the channel and we exit cleanly.
			continue
		}
		// Per-iteration panic recovery: dhowden/tag and our own DSF/
		// DFF parsers can panic on malformed files (truncated ID3v2
		// headers, bad MP4 atom trees, FLAC blocks lying about their
		// length). Without recovery a single bad file would crash the
		// worker goroutine and reduce pool capacity for the rest of
		// the scan; with recovery the file is logged + skipped and
		// the worker proceeds to the next path on the channel.
		// Mirrors the same pattern processJob uses in
		// internal/transcode/pool.go.
		var trackToWrite *Track
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.panickedCnt.Add(1)
					scanLogger.Error("panic extracting track",
						"path", pi.abs,
						"panic", r,
						"stack", string(debug.Stack()))
					trackToWrite = nil
				}
			}()
			// Early-skip on unchanged-since-last-scan: matches the legacy
			// walker. Concurrent reads from N workers against the same
			// SQLite handle are fine (modernc.org/sqlite WAL mode allows
			// concurrent readers).
			//
			// Recovery exception (PR #98 follow-up): if the existing row
			// carries a `local-<hash>` ArtworkMBID but the matching cache
			// file is missing on disk (operator wiped <dataDir>/artwork
			// after a scan, or copied the data dir without the artwork
			// subdir), fall through to re-extract so the cache is
			// rebuilt. Without this, the API serves 202+Retry-After
			// indefinitely for the missing local- mbid because the
			// enricher won't refetch a local- value. Pure UUID-bearing
			// rows AND empty-ArtworkMBID rows still take the fast skip —
			// the recovery cost is one os.Stat per `local-` track per
			// scan, scoped narrowly to the one case the cache might
			// genuinely need rebuilding.
			existing, _ := s.store.GetTrack(ctx, pi.rel)
			if existing != nil && existing.Size == pi.info.Size() && !existing.ModTime.Before(pi.info.ModTime()) {
				if !s.needsLocalArtworkRecovery(existing) {
					// Even on the early-skip path we MUST reset the
					// missing_count for this row, otherwise a flap-
					// then-restore on a mtime-equal file (the exact
					// production case: silent partial enumeration came
					// back and the file never changed) leaves the
					// counter stuck and the row eventually gets reaped
					// even though it's right there on disk. The
					// UpsertTrack reset only fires on the slow extract
					// path; without this targeted reset the skip
					// optimization defeats the resilience contract.
					// Cheap PRIMARY-KEY UPDATE, no-op when already 0.
					if err := s.store.ResetTrackMissingCount(ctx, pi.rel); err != nil {
						scanLogger.Warn("reset missing_count on skip", "path", pi.rel, "err", err)
					}
					return
				}
			}
			t := &Track{
				Path:    pi.rel,
				Size:    pi.info.Size(),
				ModTime: pi.info.ModTime().UTC(),
			}
			fillFromPath(t, pi.rel, multiRoot) // last-resort heuristics for files with no tags
			if err := ExtractWithContext(pi.abs, t, ec); err != nil {
				scanLogger.Error("extract", "path", pi.abs, "err", err)
			}
			// Capture-then-call so a concurrent test that nils the
			// hook between the check and invocation can't trigger a
			// nil-deref panic. The hook is set/cleared from test
			// goroutines while production code reads it from worker
			// goroutines — the local copy makes the read+call atomic
			// from this caller's view (Gemini medium on PR #188).
			if hook := afterExtractHookForTests; hook != nil {
				hook(pi.abs)
			}
			trackToWrite = t
		}()
		if trackToWrite == nil {
			// Either an early-skip-unchanged hit, or a panic during
			// extraction. Both paths skip the writer hand-off.
			continue
		}
		select {
		case writes <- trackToWrite:
		case <-ctx.Done():
			return
		}
	}
}

// needsLocalArtworkRecovery reports whether an unchanged-eligible
// track must still be re-extracted because its locally-curated
// artwork cache file went missing. Returns true only when the row
// carries a `local-<hash>` ArtworkMBID AND the matching
// `<artDir>/<mbid>-500.jpg` is absent. Pure UUID-bearing or empty-
// MBID rows always return false — those don't have a scanner-side
// cache to rebuild. Empty `s.artDir` also returns false (no local-
// artwork pipeline configured).
//
// The os.Stat cost is paid only on tracks with the `local-` prefix,
// not on the whole library — for a typical install this is 0% of
// rows on first scan and at most a fraction once local-artwork
// extraction has run.
func (s *Scanner) needsLocalArtworkRecovery(t *Track) bool {
	if s.artDir == "" {
		return false
	}
	if !strings.HasPrefix(t.ArtworkMBID, "local-") {
		return false
	}
	cachePath := filepath.Join(s.artDir, t.ArtworkMBID+"-500.jpg")
	_, err := os.Stat(cachePath)
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	// Transient I/O error (NAS drop, permission flap, antivirus
	// lock). Recovery would force an audio-file reopen + tag re-
	// parse for every affected track on every flaky scan — much
	// worse than the alternative of leaving the existing local-
	// MBID in place until the underlying I/O issue resolves.
	// Pre-fix this branch fell through to `return true` and wasted
	// scan time on every NAS hiccup; CodeRabbit Major review on
	// the PR #98 fix-up commit caught it.
	scanLogger.Warn("local-art recovery probe failed",
		"path", cachePath, "err", err)
	return false
}

// runScanWriter is the single writer goroutine that consumes Tracks
// from `writes`, batches them into `scanBatchSize`-row chunks, and
// flushes via `Store.UpsertTrackBatch` (one BEGIN/COMMIT per chunk).
// `committed` is incremented post-flush so `ScanProgress()` always
// reflects rows actually persisted to disk.
//
// On a flush error we log and clear the batch — partial rows are lost
// but the scan continues. The legacy walker had the same behaviour
// (bare `log.Printf` on UpsertTrack failure); per-batch failure is
// rarer because a single transaction wraps many rows.
func (s *Scanner) runScanWriter(ctx context.Context, writes <-chan *Track, committed *atomic.Int64, wg *sync.WaitGroup) {
	defer wg.Done()
	batch := make([]*Track, 0, scanBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.store.UpsertTrackBatch(ctx, batch); err != nil {
			scanLogger.Error("upsert batch", "rows", len(batch), "err", err)
		} else {
			n := committed.Add(int64(len(batch)))
			s.progress.Store(n)
		}
		batch = batch[:0]
	}
	for t := range writes {
		batch = append(batch, t)
		if len(batch) >= scanBatchSize {
			flush()
			if ctx.Err() != nil {
				// Honour cancellation between batches. The current
				// batch was just flushed; anything still inbound is
				// drained without flushing so workers can exit and
				// the writer can return promptly.
				for range writes {
				}
				return
			}
		}
	}
	flush()
}

// ScanSubtree walks just `dir` (which must be at or under one of
// the configured library roots) and feeds its audio files into the
// same worker → writer pipeline as `Scan`. Used by the fsnotify
// watcher (PR-4) for incremental updates after a debounced
// directory event — a 50k-track library doesn't need to walk every
// root just because one folder gained a file.
//
// Runs a deletion pass **scoped to the resolved subtree**: tracks
// and folders that were under `dir` per the previous DB state but
// are NOT seen this pass get deleted. The original cross-root
// concern ("could this be a moved file?") is closed by scope: a file
// that moved from /A/foo to /B/bar fires fsnotify on both, and each
// event's ScanSubtree only looks at rows under its own scope —
// /A/foo's scan deletes the source-side row, /B/bar's scan adds the
// destination-side row. No phantom duplicates, no ~6h wait for the
// next periodic full Scan.
//
// Honours the same errored-subtree sparing as Scan: a transient
// WalkDir error must not wipe rows for paths still on disk.
//
// Cancelable via ctx. Returns the count of rows committed by the
// writer goroutine. A subtree scan that resolves to no audio
// files is a fast no-op.
func (s *Scanner) ScanSubtree(ctx context.Context, dir string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Same per-scan reset rationale as Scan(): each subtree scan starts
	// with a fresh folder-art single-flight cache.
	s.folderArt = sync.Map{}

	rootsPtr := s.roots.Load()
	var roots []string
	if rootsPtr != nil {
		roots = *rootsPtr
	}
	if len(roots) == 0 {
		return 0, fmt.Errorf("no library roots configured")
	}
	multiRoot := len(roots) > 1

	// Resolve `dir` to its parent root so relPath produces the
	// same library-relative form the full scan uses. Refuse when
	// the dir doesn't live under any configured root — silently
	// scanning unrelated paths would let an arbitrary fsnotify
	// event poison the manifest with garbage paths.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return 0, fmt.Errorf("resolve %q: %w", dir, err)
	}
	var owningRoot string
	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		// `filepath.Rel(absRoot, absDir)` returns a relative path
		// from absRoot to absDir (e.g. "Genre/Artist/Album" or
		// "."). A path starting with ".." means absDir climbs out
		// of absRoot — i.e. NOT under it. This is the idiomatic
		// cross-platform containment check — pre-fix
		// strings.HasPrefix was byte-exact, which broke on
		// case-insensitive filesystems where an event for
		// `/Music/Album/track.flac` could surface against a
		// configured root path `/music` and false-negative
		// (Gemini High on PR #79). filepath.Rel routes through
		// the OS path APIs so platform-native casing semantics
		// are preserved.
		rel, err := filepath.Rel(absRoot, absDir)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			owningRoot = absRoot
			break
		}
	}
	if owningRoot == "" {
		return 0, fmt.Errorf("dir %q is not under any configured library root", dir)
	}

	// `relScope` is the library-relative path of the subtree being
	// scanned, used as the predicate for the bounded deletion pass.
	// Same form `relPath` produces for everything the walker upserts,
	// so set membership in `seen`/`seenFolders` is directly
	// comparable against the snapshot keys.
	relScope := relPath(owningRoot, absDir, multiRoot)

	// Subtree-scoped "before" snapshots. Smaller than Scan's whole-
	// library snapshot — one query per scope — and the deletion pass
	// only considers rows that were already inside the scope.
	beforeTracks, err := s.store.TrackPathsUnder(ctx, relScope)
	if err != nil {
		return 0, fmt.Errorf("list existing tracks under %q: %w", relScope, err)
	}
	beforeTrackSet := make(map[string]struct{}, len(beforeTracks))
	for _, p := range beforeTracks {
		beforeTrackSet[p] = struct{}{}
	}
	beforeFolders, err := s.store.FolderPathsUnder(ctx, relScope)
	if err != nil {
		return 0, fmt.Errorf("list existing folders under %q: %w", relScope, err)
	}
	beforeFolderSet := make(map[string]struct{}, len(beforeFolders))
	for _, p := range beforeFolders {
		beforeFolderSet[p] = struct{}{}
	}

	seen := make(map[string]struct{}, len(beforeTrackSet))
	seenFolders := make(map[string]struct{}, len(beforeFolderSet))
	errorSubtrees := make(map[string]struct{})

	paths := make(chan pathInfo, scanChannelBuffer)
	writes := make(chan *Track, scanChannelBuffer)

	nWorkers := runtime.NumCPU()
	var workersWG sync.WaitGroup
	for i := 0; i < nWorkers; i++ {
		workersWG.Add(1)
		go s.runScanWorker(ctx, paths, writes, multiRoot, &workersWG)
	}

	committed := new(atomic.Int64)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go s.runScanWriter(ctx, writes, committed, &writerWG)

	// Subtree walker: same shape as walkRoot, including the err-
	// callback's errored-subtree recording so the deletion pass
	// below skips rows under transiently-unreachable directories.
	walkErr := filepath.WalkDir(absDir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// `fs.ErrNotExist` on the subtree root (or any descendant)
			// is the SIGNAL we're here for — fsnotify fired because the
			// directory was deleted/renamed, and the bounded deletion
			// pass below is what reaps the now-stale rows. Recording
			// errorSubtrees in this case would spare exactly the rows
			// we came to clean (CodeRabbit Major review on PR #160's
			// first commit).
			//
			// FUSE drop mode (c) — host-dir-vs-mount-state distinction:
			// a cleanly-unmounted FUSE binds the host directory back to
			// an empty inode, so the owning root *succeeds* `os.Stat`
			// while nested paths fail with `fs.ErrNotExist`
			// indistinguishable from "operator rm -rf'd this folder."
			// Naive trust-on-fs.ErrNotExist would let the bounded
			// deletion pass nuke every row under the dropped mount.
			// Audit the owning root before proceeding:
			//   (i) os.Stat root — if missing/unreadable, hard error.
			//   (ii) os.ReadDir root — any error is a hard stop (we
			//        can't audit the root, so the state is untrusted).
			//   (iii) only if ReadDir succeeds AND zero entries AND no
			//        .bridge-allow-empty sentinel AND DB has tracks →
			//        hard error.
			// Otherwise (root alive AND non-empty, or sentinel present)
			// fs.ErrNotExist on the subtree is a legitimate operator
			// delete and the bounded deletion pass runs as before.
			if errors.Is(err, fs.ErrNotExist) {
				if auditErr := auditOwningRootOnSubtreeMiss(ctx, s.store, owningRoot, multiRoot); auditErr != nil {
					scanLogger.Error("subtree absent but owning root audit failed",
						"path", abs, "root", owningRoot, "err", auditErr)
					return auditErr
				}
				scanLogger.Info("subtree removed", "path", abs)
				return nil
			}
			// Genuine transient failures (permission flap, EACCES, NAS
			// drop) still record the subtree so the spare kicks in.
			scanLogger.Warn("subtree walk", "path", abs, "err", err)
			key := abs
			if d != nil && !d.IsDir() {
				key = filepath.Dir(abs)
			}
			errorSubtrees[relPath(owningRoot, key, multiRoot)] = struct{}{}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				// Stat failure on a directory must record the subtree
				// as errored so the deletion pass spares its folder
				// row AND any tracks under it. Without this, a
				// transient stat failure (NAS drop mid-walk) on a
				// directory whose row already existed would silently
				// leak through and the row would be reaped — same
				// regression class the file-stat branch below
				// addresses.
				scanLogger.Warn("subtree dir stat", "path", abs, "err", err)
				errorSubtrees[relPath(owningRoot, abs, multiRoot)] = struct{}{}
				return nil
			}
			rel := relPath(owningRoot, abs, multiRoot)
			if err := s.store.UpsertFolder(ctx, &Folder{Path: rel, ModTime: info.ModTime().UTC()}); err != nil {
				// Surface the failure but treat the row as errored-
				// subtree so the bounded deletion pass spares it AND
				// its descendants — without this guard, an UpsertFolder
				// failure on a pre-existing row would leave seenFolders
				// unmarked and the deletion pass would reap a still-
				// valid row (CodeRabbit Major review on PR #160's
				// second round).
				scanLogger.Error("subtree upsert folder", "path", rel, "err", err)
				errorSubtrees[rel] = struct{}{}
				return nil
			}
			seenFolders[rel] = struct{}{}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if !enqueueableAudioFile(abs, d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			scanLogger.Warn("subtree stat", "path", abs, "err", err)
			errorSubtrees[relPath(owningRoot, filepath.Dir(abs), multiRoot)] = struct{}{}
			return nil
		}
		rel := relPath(owningRoot, abs, multiRoot)
		seen[rel] = struct{}{}
		select {
		case paths <- pathInfo{abs: abs, rel: rel, info: info}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})

	close(paths)
	workersWG.Wait()
	close(writes)
	writerWG.Wait()

	// Surface cancellation so a partial subtree update doesn't
	// look like a clean completion (CodeRabbit Major post-merge
	// on PR #83). `Scan` does the same; converting ctx.Err() to
	// nil here would let the watcher's debounce loop log a
	// success line for a scan that actually only committed a
	// prefix of the work. Skip the deletion pass on cancel for
	// the same reason — half-walked state is a poor input.
	if ctx.Err() != nil {
		return int(committed.Load()), ctx.Err()
	}
	if walkErr != nil {
		return int(committed.Load()), walkErr
	}

	// Bounded deletion pass: only rows that were under `relScope`
	// to begin with are candidates, so a cross-root move (the
	// original concern) cleans the source-side row from /A/foo's
	// scan and the destination-side row already came in via
	// /B/bar's scan. Same missing_count threshold model as the
	// full-scan path — see Scan's docblock for the rationale.
	threshold := s.effectiveDeleteThreshold()
	missingTracks := make([]string, 0)
	sparedTracks := 0
	for p := range beforeTrackSet {
		if _, ok := seen[p]; ok {
			continue
		}
		if isUnderErroredSubtree(p, errorSubtrees) {
			sparedTracks++
			continue
		}
		missingTracks = append(missingTracks, p)
	}
	deletedTracks, derr := s.store.IncrementMissingTracksAndDeleteAtThreshold(ctx, missingTracks, threshold)
	if derr != nil {
		scanLogger.Error("subtree missing-count tracks pass", "err", derr, "missing", len(missingTracks))
	}
	missingFolders := make([]string, 0)
	sparedFolders := 0
	for p := range beforeFolderSet {
		if _, ok := seenFolders[p]; ok {
			continue
		}
		if isUnderErroredSubtree(p, errorSubtrees) {
			sparedFolders++
			continue
		}
		missingFolders = append(missingFolders, p)
	}
	deletedFolders, derr := s.store.IncrementMissingFoldersAndDeleteAtThreshold(ctx, missingFolders, threshold)
	if derr != nil {
		scanLogger.Error("subtree missing-count folders pass", "err", derr, "missing", len(missingFolders))
	}
	if sparedTracks > 0 || sparedFolders > 0 {
		scanLogger.Warn("subtree scan spared rows from deletion pass (parent walk error)",
			"tracks", sparedTracks, "folders", sparedFolders, "subtrees", len(errorSubtrees))
	}
	if len(missingTracks) > 0 || len(missingFolders) > 0 {
		scanLogger.Info("subtree scan missing rows",
			"tracks_missing", len(missingTracks), "tracks_deleted", deletedTracks,
			"folders_missing", len(missingFolders), "folders_deleted", deletedFolders,
			"threshold", threshold)
	}

	return int(committed.Load()), nil
}

// walkRoot drives `filepath.WalkDir` for one root, recording folder
// mtimes inline (single-writer; no contention with the workers'
// track-upsert pipeline) and handing off audio files to the worker
// pool via `paths`. The deletion-pass `seen` (tracks) and
// `seenFolders` maps are written here so workers don't need a mutex
// around them — visibility-during-walk is a walker-domain concern,
// independent of whether the worker actually re-extracts (early-skip
// case) or persists. `errorSubtrees` is also written here when the
// WalkDir err callback fires — the deletion pass uses it to spare
// tracks AND folders under transiently-unreachable subtrees from
// being wiped from the manifest.
//
// Returns the count of entries observed beneath the root (file or
// dir DirEntries excluding the root itself) and any walk error. The
// count drives the caller's "clean-empty mount" detection — zero
// entries beneath a root that the DB carries history for is a strong
// FUSE-drop signal, since `os.Stat(root)` and `WalkDir(root)` both
// succeed silently in that scenario.
//
// FUSE drop mode (a) — unreadable / nonexistent root: explicit
// upfront `os.Stat(root)` so the operator gets a clear .error log
// keyed on the root rather than a generic mid-walk warning. Sentinel
// the whole root into errorSubtrees and return (0, nil) so the
// outer Scan loop continues to the next root rather than fatal-
// aborting. In a multi-root deployment (local SSD + remote FUSE
// archive), a cloud outage on the archive must NOT block cleanup
// of legitimately-deleted files on the SSD.
func (s *Scanner) walkRoot(ctx context.Context, root string, multiRoot bool, seen, seenFolders, errorSubtrees map[string]struct{}, paths chan<- pathInfo) (int, error) {
	if _, err := os.Stat(root); err != nil {
		scanLogger.Error("root unreachable", "root", root, "err", err,
			"hint", "the library root can't be reached — is the volume/mount present? On Docker check the -v / compose volumes mapping. See docs/docker.md")
		errorSubtrees[relPath(root, root, multiRoot)] = struct{}{}
		return 0, nil
	}
	var observed int
	walkErr := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if abs != root {
			observed++
		}
		if err != nil {
			// Permission error on one dir shouldn't kill the whole scan.
			scanLogger.Warn("walk", "path", abs, "err", err)
			// Record the affected subtree so the deletion pass
			// won't wipe its tracks. If `d` is non-nil and a
			// directory, key on `abs` itself; otherwise key on
			// the parent (the closest known directory at the
			// error point). The walker will not descend into
			// the failed dir, so any tracks in `beforeSet`
			// under this prefix won't reach `seen` this pass.
			key := abs
			if d != nil && !d.IsDir() {
				key = filepath.Dir(abs)
			}
			rel := relPath(root, key, multiRoot)
			errorSubtrees[rel] = struct{}{}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			// Check skip *before* upserting — otherwise .Trash,
			// .Spotlight-V100, $RECYCLE.BIN, etc. land in the folders
			// table and the iOS client sees them in the manifest.
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			// Record folder mtimes for the manifest / future skip logic.
			info, err := d.Info()
			if err != nil {
				// Same containment as the file-stat branch below: a
				// transient I/O hiccup on a directory's stat must NOT
				// leave the folder row eligible for deletion-pass
				// reaping. Record the subtree as errored so the spare
				// kicks in for the row AND every track / folder under it.
				scanLogger.Warn("dir stat", "path", abs, "err", err)
				errorSubtrees[relPath(root, abs, multiRoot)] = struct{}{}
				return nil
			}
			rel := relPath(root, abs, multiRoot)
			if err := s.store.UpsertFolder(ctx, &Folder{Path: rel, ModTime: info.ModTime().UTC()}); err != nil {
				// Surface the failure but treat the row as errored-
				// subtree so the deletion pass spares it AND its
				// descendants — without this guard, an UpsertFolder
				// failure on a pre-existing row would leave seenFolders
				// unmarked and the deletion pass would reap a still-
				// valid row (CodeRabbit Major review on PR #160's
				// second round).
				scanLogger.Error("upsert folder", "path", rel, "err", err)
				errorSubtrees[rel] = struct{}{}
				return nil
			}
			seenFolders[rel] = struct{}{}
			return nil
		}
		// Skip dot-files and unsupported extensions.
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if !enqueueableAudioFile(abs, d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			scanLogger.Warn("stat", "path", abs, "err", err)
			// Stat failure on a known audio extension — record the
			// parent so the file isn't wiped from the manifest.
			// Same containment rationale as the err-callback branch
			// above: a transient I/O hiccup shouldn't trigger a
			// delete.
			rel := relPath(root, filepath.Dir(abs), multiRoot)
			errorSubtrees[rel] = struct{}{}
			return nil
		}

		rel := relPath(root, abs, multiRoot)
		seen[rel] = struct{}{}

		select {
		case paths <- pathInfo{abs: abs, rel: rel, info: info}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	return observed, walkErr
}

// allowEmptySentinelFilename is the marker file an operator places
// at a library root to confirm "yes, this root is legitimately empty
// now — proceed with the deletion pass." Without the sentinel, the
// scanner's FUSE drop-mode (b) guard treats an empty root with DB
// history as a suspected mount failure and spares the rows.
const allowEmptySentinelFilename = ".bridge-allow-empty"

// hasAllowEmptySentinel reports whether the operator has placed the
// `.bridge-allow-empty` marker file at the given library root.
// Returning true bypasses the clean-empty-mount guard for this root,
// so a deliberate "I really did wipe this library" operation can
// complete without manual sentinel cleanup.
//
// MUST be a regular file — an accidental directory or symlink of
// that name would otherwise authorise deletion silently
// (CodeRabbit Major review on PR #289). Any os.Stat error or
// non-regular shape returns false; only an explicit, successfully-
// stat'd regular-file sentinel authorises the deletion pass to
// proceed.
func hasAllowEmptySentinel(root string) bool {
	info, err := os.Stat(filepath.Join(root, allowEmptySentinelFilename))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// auditOwningRootOnSubtreeMiss validates that a subtree walker's
// `fs.ErrNotExist` reflects a legitimate operator-deleted directory
// rather than a FUSE mount that has dropped underneath the bridge.
// Returns nil when the subtree absence is trustworthy and the
// caller's deletion pass should proceed; returns a non-nil error
// when the state is untrusted and the deletion pass MUST be aborted.
//
// Decision matrix:
//   - root missing/unreadable: untrusted — return wrapped os.Stat err.
//   - os.ReadDir on root fails: untrusted — return wrapped err. Any
//     ReadDir failure (permission drop, transient FUSE disruption)
//     means we can't audit the root state, so the safe default is
//     to refuse the deletion.
//   - root exists AND has entries (including the `.bridge-allow-empty`
//     sentinel if present, since os.ReadDir surfaces it as a
//     directory entry): trustworthy — fs.ErrNotExist on the subtree
//     is a legitimate operator delete, return nil. Gemini medium
//     review on PR #289 caught the redundant explicit-sentinel
//     check that this branch already subsumes.
//   - root exists AND is empty AND CountTracksUnderRoot > 0:
//     untrusted — DB carries history but the root has nothing, this
//     looks like a mount drop. Return a suspected-mount-drop error
//     to abort the deletion pass.
//   - root exists AND is empty AND CountTracksUnderRoot == 0:
//     trustworthy — fresh install or post-wipe state with no rows
//     to protect.
func auditOwningRootOnSubtreeMiss(ctx context.Context, store *Store, owningRoot string, multiRoot bool) error {
	if _, err := os.Stat(owningRoot); err != nil {
		return fmt.Errorf("audit owning root: stat: %w", err)
	}
	entries, err := os.ReadDir(owningRoot)
	if err != nil {
		return fmt.Errorf("audit owning root: read dir: %w", err)
	}
	if len(entries) > 0 {
		return nil
	}
	n, err := store.CountTracksUnderRoot(ctx, owningRoot, multiRoot)
	if err != nil {
		return fmt.Errorf("audit owning root: count tracks: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("audit owning root %q: empty on disk but %d tracks in DB (suspected mount drop; place .bridge-allow-empty to confirm intent)", owningRoot, n)
	}
	return nil
}

// isUnderErroredSubtree reports whether `path` is at or under any of
// the directory paths the walker recorded in `errorSubtrees`. Used
// by the deletion pass to spare tracks whose parent dir hit a
// transient I/O error this pass — without this guard a single
// network blip during a NAS scan wipes the affected subtree from
// the manifest. Both `path` and the keys are in the
// library-relative forward-slash form `relPath` produces.
//
// **Root-level error sentinels** (qodo + gemini bot review on
// PR #74, plus coderabbit follow-up CRITICAL): the walker keys an
// errored root via `relPath(root, root, multiRoot)`, which produces:
//   - single-root mode: "."
//   - multi-root mode:  "<rootBase>/." (because relPath in multi-
//     root mode prepends the root's basename + "/" then appends the
//     `filepath.Rel(root, root)` result, which is ".")
//
// Without normalising both forms, a whole-library outage in either
// mode would spare zero tracks. We treat "." / "" as "every track"
// (single-root) AND any path ending in "/." as "everything under
// that root prefix" (multi-root). The latter handles the multi-
// root case exactly without false-positive matching.
func isUnderErroredSubtree(path string, errorSubtrees map[string]struct{}) bool {
	if len(errorSubtrees) == 0 {
		return false
	}
	for sub := range errorSubtrees {
		if sub == "." || sub == "" {
			// Whole-root outage in single-root mode — every track
			// under this root counts as under the errored subtree
			// by definition.
			return true
		}
		if strings.HasSuffix(sub, "/.") {
			// Multi-root whole-root sentinel of the form
			// "<rootBase>/." — every path under "<rootBase>/" is
			// unreachable this pass.
			rootPrefix := strings.TrimSuffix(sub, ".")
			if strings.HasPrefix(path, rootPrefix) {
				return true
			}
			continue
		}
		if path == sub {
			return true
		}
		// Append a trailing slash so a sibling like "foo-other" can't
		// match "foo" — only paths actually under `sub/` qualify.
		prefix := sub
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// relPath converts an absolute on-disk path to the library-relative,
// forward-slash form used in storage. In multi-root mode the first segment
// is the root's basename so the resolver can route back.
func relPath(root, abs string, multiRoot bool) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	rel = filepath.ToSlash(rel)
	if multiRoot {
		return filepath.Base(root) + "/" + rel
	}
	return rel
}

// fillFromPath populates Title / Album / Artist fields from the library-
// relative path as a last-resort heuristic for files with no embedded
// tags. The iOS scanner does the same thing on its local walk path.
//
// In multi-root mode `relPath` prefixes every track's library-relative
// form with the root's basename (e.g. `Music/Pink Floyd/Dark Side/
// Money.flac` for root `/Music`). The album/artist heuristics here
// derive Album/Artist from the trailing directory segments, so the
// prefix has to be stripped FIRST — otherwise an untagged file directly
// under a root named "Alphaville" was deriving Artist="Alphaville"
// from the root basename instead of falling through to "no artist
// guessable". (Bug ROOT.) Single-root scans don't carry the prefix
// and pass through unchanged.
//
// Title is computed BEFORE the strip because the filename always lives
// at the leaf regardless of prefix mode — stripping wouldn't change
// `parts[len-1]`.
func fillFromPath(t *Track, rel string, multiRoot bool) {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return
	}
	// Title = filename without extension.
	base := parts[len(parts)-1]
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if t.Title == "" {
		t.Title = base
	}
	// Strip the multi-root prefix before the album/artist heuristics so
	// they evaluate the legitimate `Artist/Album/Track.ext` shape.
	if multiRoot && len(parts) > 1 {
		parts = parts[1:]
	}
	// Album = immediate parent dir.
	if len(parts) >= 2 && t.Album == "" {
		t.Album = parts[len(parts)-2]
	}
	// Artist = grandparent dir.
	if len(parts) >= 3 && t.Artist == "" {
		t.Artist = parts[len(parts)-3]
	}
}

// shouldSkipDir returns true for directories we never want to traverse.
// Classic metadata / trash / hidden dirs.
func shouldSkipDir(name string) bool {
	switch name {
	case ".Trash", ".Trashes", "$RECYCLE.BIN", "System Volume Information",
		".AppleDouble", ".AppleDesktop", ".DocumentRevisions-V100",
		".Spotlight-V100", ".TemporaryItems", ".fseventsd":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// variantIDInfixRe matches a bridge variant ID EXACTLY:
// `<kind>-v<schemaVersion>-<targetRate>-<targetBits>` (kind ∈ upscaled/optimized), e.g.
// `optimized-v2-44100-16`, `upscaled-v2-176400-24`. Anchored to the whole segment so a
// coincidental `.optimized-…` substring in a real filename doesn't match. Version-agnostic
// (`v[0-9]+`) — a future schema bump must still be excluded. Mirrors the
// `VariantKindPrefix*` LIKE-discriminator rationale (kept in `manifest` to avoid the
// `transcode` import cycle).
var variantIDInfixRe = regexp.MustCompile(
	`^(?:` + VariantKindPrefixUpscaled + `|` + VariantKindPrefixOptimized + `)-v[0-9]+-[0-9]+-[0-9]+$`)

// isVariantSidecarName reports whether a file basename is one of the bridge's own
// optimize/upscale transcode artifacts and therefore must NOT be indexed as a library
// track. Sidecars are `<srcBase>.<variantID>.flac` (the transcoder always emits FLAC) —
// e.g. `01 Love Letters.flac.upscaled-v2-176400-24.flac`. They live in the on-demand
// variant pool (`upscale.variantsDir`), served via the variant path + aggregated onto
// their PARENT track's manifest entry — never standalone library content. But if a
// `variantsDir` resolves inside a `libraryRoot`, or variants get synced into a scanned
// mount (the field case: a `variants/` folder inside a read-only B2 library bucket —
// ~26% of a 24k-track library indexed as phantom downscaled "tracks", doubling every
// affected album), the extension-only walk gate would otherwise enqueue them; skipping
// them drops the phantom rows on the next scan via the bounded deletion pass.
//
// The match is ANCHORED (not a loose `.optimized-` substring): the trailing dot-segment
// before `.flac` must be a well-formed variant ID AND the part before it must itself be a
// supported audio file (the source), so a legitimately-named single-extension track like
// `Song.optimized-Mix.flac` or `01.upscaled-mix.flac` is never mistaken for a sidecar.
func isVariantSidecarName(name string) bool {
	const variantExt = ".flac"
	if !strings.HasSuffix(name, variantExt) {
		return false
	}
	base := name[:len(name)-len(variantExt)]
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return false
	}
	return variantIDInfixRe.MatchString(base[dot+1:]) &&
		Ext[strings.ToLower(filepath.Ext(base[:dot]))]
}

// enqueueableAudioFile reports whether a walked file should be enqueued for extraction:
// a supported audio extension AND not one of our own variant sidecars. Shared by both
// WalkDir callbacks (full Scan + incremental subtree walk) so the file-inclusion gate
// lives in ONE place.
func enqueueableAudioFile(abs, name string) bool {
	return Ext[strings.ToLower(filepath.Ext(abs))] && !isVariantSidecarName(name)
}

// RunPeriodic runs an initial scan, then rescans every interval until ctx
// is done. Logs errors but never exits early on a scan failure. Waits for
// the initial scan goroutine before returning, so callers can safely Close
// the Store after ctx cancellation without racing in-flight SQL.
func (s *Scanner) RunPeriodic(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	var initial sync.WaitGroup
	initial.Add(1)
	go func() {
		defer initial.Done()
		if _, err := s.Scan(ctx); err != nil && ctx.Err() == nil {
			scanLogger.Error("initial scan", "err", err)
		}
	}()
	defer initial.Wait()
	// Periodic rescans.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Scan(ctx); err != nil && ctx.Err() == nil {
				scanLogger.Error("periodic scan", "err", err)
			}
		}
	}
}

// BuildManifest returns a Manifest built from the current Store contents.
// since, if non-zero, filters tracks by mtime (for incremental iOS
// updates).
func BuildManifest(ctx context.Context, store *Store, roots []string, since time.Time) (*Manifest, error) {
	var sp *time.Time
	if !since.IsZero() {
		sp = &since
	}
	tracks, err := store.ListTracks(ctx, sp)
	if err != nil {
		return nil, err
	}
	folders, err := store.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	basenames := make([]string, len(roots))
	for i, r := range roots {
		basenames[i] = filepath.Base(r)
	}
	m := &Manifest{
		Version:      1,
		GeneratedAt:  time.Now().UTC(),
		LibraryRoots: basenames,
		Folders:      folders,
		Tracks:       tracks,
	}
	// Library-wide enrichment counters land on every non-paginated
	// response. `EnrichmentCounts` query failure is non-fatal — the
	// manifest stays useful even if the counter rollup hits a
	// transient error; older clients ignore the field anyway. We DO
	// log on failure (Qodo #3) so an operational issue doesn't slip by
	// silently — the same functions return hard errors on
	// `ListTracks` / `ListFolders`, so a quiet failure here is an
	// observability gap specific to this code path.
	//
	// `tracksTotal` is sourced from a single `CountTracks()` (Qodo #2)
	// to guarantee the protocol invariant that the manifest's top-
	// level `total` and `EnrichmentProgress.TracksTotal` cannot
	// diverge in the same response under concurrent writes. For the
	// non-paginated `BuildManifest` path the manifest doesn't carry a
	// top-level `total`, so we still need this count — but we now do
	// it once, here, instead of letting both the manifest builder and
	// EnrichmentCounts each issue their own `COUNT(*)`.
	total, terr := store.CountTracks(ctx)
	if terr != nil {
		logger.Error("CountTracks for enrichment-progress", "err", terr)
	} else if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(ctx); perr == nil {
		m.EnrichmentProgress = &EnrichmentProgress{
			TracksTotal:    total,
			TracksEnriched: enriched,
			LastEnrichedAt: lastEnrichedAt,
		}
	} else {
		logger.Error("EnrichmentCounts", "err", perr)
	}
	return m, nil
}

// WriteManifest streams the legacy non-paginated manifest as JSON
// directly to w, bounding peak memory regardless of library size.
//
// The original BuildManifest materialised the full []Track in RAM via
// ListTracks + per-row json.Unmarshal — a 50k-track library at ~3-5 KB
// per row pushes well over 200 MB during a single legacy /v1/manifest
// request, which OOM-kills Pi-class hosts (review item). This streams
// each track straight from rows.Next() into the writer so peak alloc
// stays bounded by the envelope (folders + counts) plus one Track at
// a time.
//
// **Wire shape parity with BuildManifest** is enforced by emitting the
// same JSON keys in the same order, with the same omitempty rules:
// version → generatedAt → libraryRoots → folders (omit when empty) →
// tracks (always emitted as `[]` for empty libraries) →
// enrichmentProgress (omit on counter failure).
//
// **Mid-stream errors are unrecoverable** — the headers and prefix are
// already on the wire, so we can't switch to a 5xx. The error is
// returned for the caller to log; the truncated JSON will fail to parse
// on iOS, surfacing as a sync error which iOS handles by retrying.
//
// since, if non-zero, filters tracks by indexed_at (matches the
// BuildManifest semantics).
func WriteManifest(ctx context.Context, w io.Writer, store *Store, roots []string, since time.Time) error {
	return writeManifestGated(ctx, w, store, roots, since, false)
}

// writeManifestGated is the gated worker behind the public
// WriteManifest. `withVariants=false` (the test-friendly default)
// strips Track.Variants before emitting each row; `true` (set by
// the runtime Provider when `cfg.Upscale.Enabled`) lets variants
// flow through unchanged. Existing tests continue to call the
// public WriteManifest unchanged.
//
// `ctx` is checked inside the per-row callback so a client disconnect
// mid-stream terminates the scan within at most one row instead of
// running to EOF. The check returns ctx.Err() which propagates up
// through `streamErr` and surfaces in the caller's log line.
func writeManifestGated(ctx context.Context, w io.Writer, store *Store, roots []string, since time.Time, withVariants bool) (err error) {
	folders, err := store.ListFolders(ctx)
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	// EnrichmentProgress block: same best-effort behaviour as
	// BuildManifest — counter failures log but don't fail the request.
	// `tracksTotal` and the iOS-side enrichment hint cohabit a single
	// CountTracks() call so the protocol invariant `manifest.total ==
	// EnrichmentProgress.TracksTotal` holds (Qodo #2 carry-over).
	var ep *EnrichmentProgress
	if total, terr := store.CountTracks(ctx); terr != nil {
		logger.Error("CountTracks for enrichment-progress", "err", terr)
	} else if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(ctx); perr == nil {
		ep = &EnrichmentProgress{
			TracksTotal:    total,
			TracksEnriched: enriched,
			LastEnrichedAt: lastEnrichedAt,
		}
	} else {
		logger.Error("EnrichmentCounts", "err", perr)
	}

	basenames := make([]string, len(roots))
	for i, r := range roots {
		basenames[i] = filepath.Base(r)
	}

	// bufio.Writer keeps the per-track Write calls from each becoming
	// a syscall — large libraries would otherwise pay one write(2) per
	// row, dominating the streaming win.
	bw := bufio.NewWriter(w)
	// Flush via defer so an early return on stream error still ships
	// the bytes already buffered (CodeRabbit on PR #70). Without this,
	// a mid-stream failure could leave the client with no body at all
	// — the deferred status writer in the handler still emits 200 once
	// the first byte lands, so an unflushed prefix produces a
	// 200-with-empty-body. The first error wins (named return shadows
	// the flush error if streaming already failed).
	defer func() {
		if flushErr := bw.Flush(); err == nil && flushErr != nil {
			err = flushErr
		}
	}()

	writeField := func(prefix string, v any) error {
		if _, err := bw.WriteString(prefix); err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = bw.Write(b)
		return err
	}

	if _, err := bw.WriteString(`{"version":1`); err != nil {
		return err
	}
	if err := writeField(`,"generatedAt":`, time.Now().UTC()); err != nil {
		return err
	}
	if err := writeField(`,"libraryRoots":`, basenames); err != nil {
		return err
	}
	// Match Manifest's `omitempty` on Folders — empty slice → no key.
	if len(folders) > 0 {
		if err := writeField(`,"folders":`, folders); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString(`,"tracks":[`); err != nil {
		return err
	}

	var sp *time.Time
	if !since.IsZero() {
		sp = &since
	}
	// Per-track encoder reuses bw's buffer directly — no intermediate
	// []byte allocation per track. For a 50k-track library the prior
	// `json.Marshal(t) + bw.Write(b)` pattern allocated 50k separate
	// byte slices and made the streaming path GC-bound rather than
	// I/O-bound. `Encoder.Encode` appends `\n` after each value;
	// JSON spec treats `\n` as ignorable whitespace inside an array,
	// so the wire shape `{...}\n,{...}\n,{...}\n` stays valid for
	// any spec-compliant parser (iOS JSONDecoder included). Trailing
	// `]` lands after the last track's `\n` — also valid whitespace.
	enc := json.NewEncoder(bw)
	first := true
	streamErr := store.StreamTracks(ctx, sp, func(t *Track) error {
		// Cheap per-row cancel check. SQLite's row iteration is
		// synchronous so this is the natural pulse to honour the
		// client's deadline / disconnect. Returning the ctx error
		// terminates the scan; the caller's log line surfaces it
		// (CancellationError vs DeadlineExceeded is preserved).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !first {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		// Variant gate: the store always populates Variants from
		// `track_variants` (cheap, one column per page). When the
		// runtime feature flag is off, strip them here so the wire
		// shape matches a pre-v1.2 bridge — operator can disable
		// the feature without losing the cached sidecars on disk.
		if !withVariants {
			t.Variants = nil
		}
		return enc.Encode(t)
	})
	if streamErr != nil {
		return fmt.Errorf("stream tracks: %w", streamErr)
	}
	if _, err := bw.WriteString(`]`); err != nil {
		return err
	}
	if ep != nil {
		if err := writeField(`,"enrichmentProgress":`, ep); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString(`}`); err != nil {
		return err
	}
	// Final flush handled by the deferred call above so error paths
	// also get one.
	return nil
}

// BuildManifestPage returns one page of a paginated full-manifest
// iteration. The caller walks the whole library by calling with
// `cursor=""` on the first page and feeding each response's
// `NextCursor` back in until it comes back nil.
//
// **First-page-only fields.** `Folders` and `Total` are populated
// only when `cursor == ""`. For a 50k-track library with 5k folders,
// repeating them on every page would add ~250k rows of redundant
// JSON across a pagination run. iOS snapshots them from the first
// page and ignores later pages' values (they're absent on the wire
// via omitempty).
//
// **limit+1 query trick.** We ask the store for `limit+1` rows.
// When we get back exactly `limit+1`, we know for certain there's
// another page and set `NextCursor` to the last in-page row. When
// we get back ≤ `limit`, we've hit the tail and `NextCursor` stays
// nil. Old behaviour ("request limit; if exactly limit, assume more")
// caused an extra round-trip that returned zero rows whenever the
// track count was an exact multiple of limit.
func BuildManifestPage(ctx context.Context, store *Store, roots []string, cursor string, limit int) (*Manifest, error) {
	return buildManifestPageGated(ctx, store, roots, cursor, limit, false)
}

// buildManifestPageGated is the gated worker behind the public
// BuildManifestPage. Strips Track.Variants when the feature flag
// is off — see writeManifestGated for the symmetrical contract.
func buildManifestPageGated(ctx context.Context, store *Store, roots []string, cursor string, limit int, withVariants bool) (*Manifest, error) {
	if limit <= 0 {
		limit = 1000
	}
	// Over-fetch by one so the last row of the current query tells us
	// "is there another page" definitively.
	tracks, err := store.ListTracksPage(ctx, cursor, limit+1)
	if err != nil {
		return nil, err
	}
	basenames := make([]string, len(roots))
	for i, r := range roots {
		basenames[i] = filepath.Base(r)
	}
	m := &Manifest{
		Version:      1,
		GeneratedAt:  time.Now().UTC(),
		LibraryRoots: basenames,
	}
	// Only the first page pays the folders + total + enrichment-counts
	// lookups. A `COUNT(*)` on a 50k-track sqlite table is cheap but not
	// free; ListFolders walks the folders table fully; EnrichmentCounts
	// runs a CASE/SUM aggregate over enriched_at. iOS snapshots all three
	// from the first page and ignores them on subsequent pages, so paying
	// the queries once per pagination run instead of once per page is the
	// meaningful win for large libs.
	//
	// `tracksTotal` reuses the local `total` from CountTracks above (Qodo
	// #2) — the protocol guarantees `manifest.total` and
	// `EnrichmentProgress.TracksTotal` match in paginated mode, and that
	// invariant only holds if both fields read from the same query. The
	// previous shape called `EnrichmentProgress()` separately and could
	// disagree under concurrent `UpsertTrack` / `DeleteTrack`.
	//
	// EnrichmentCounts failure is non-fatal — older clients ignore the
	// field, newer clients fall back to "no progress hint" — but we
	// log so the failure isn't invisible (Qodo #3).
	if cursor == "" {
		folders, ferr := store.ListFolders(ctx)
		if ferr != nil {
			return nil, ferr
		}
		total, terr := store.CountTracks(ctx)
		if terr != nil {
			return nil, terr
		}
		m.Folders = folders
		m.Total = &total
		if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(ctx); perr == nil {
			m.EnrichmentProgress = &EnrichmentProgress{
				TracksTotal:    total,
				TracksEnriched: enriched,
				LastEnrichedAt: lastEnrichedAt,
			}
		} else {
			logger.Error("EnrichmentCounts (paginated)", "err", perr)
		}
	}
	if len(tracks) > limit {
		// Trim the over-fetched row — it becomes the cursor for the
		// next page. The remaining `limit` rows are what we ship.
		last := tracks[limit-1].Path
		m.NextCursor = &last
		m.Tracks = tracks[:limit]
	} else {
		// Short read — this is the last page. `NextCursor` stays nil.
		m.Tracks = tracks
	}
	// Variant gate (mirror of the streaming path in
	// writeManifestGated). Strip Variants when the feature flag is
	// off so a disabled bridge advertises the same shape as a pre-
	// v1.2 server, preserving operator intent without losing the
	// cached sidecars on disk.
	if !withVariants {
		for i := range m.Tracks {
			m.Tracks[i].Variants = nil
		}
	}
	return m, nil
}

// DefaultDBPath returns the SQLite path used when the user doesn't
// override. Lives under dataDir so the same config logic applies.
func DefaultDBPath(dataDir string) string { return filepath.Join(dataDir, "bridge.db") }

// ensurePathExists is a small test helper (exported so api_test can use).
var _ = os.Stat
