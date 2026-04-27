package manifest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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

	mu       sync.Mutex
	scanning atomic.Bool
	lastFull atomic.Int64 // UnixNano of last successful full scan
	progress atomic.Int64 // tracks indexed so far during the current scan
}

// NewScanner constructs a Scanner. Caller owns the Store's lifecycle.
func NewScanner(roots []string, store *Store) *Scanner {
	s := &Scanner{store: store}
	rc := append([]string(nil), roots...)
	s.roots.Store(&rc)
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
	defer s.scanning.Store(false)

	// Snapshot of paths we knew about BEFORE this scan. At the end we drop
	// rows whose paths weren't touched during the walk — that's the
	// "deleted from disk" pass.
	before, err := s.store.TrackPaths()
	if err != nil {
		return 0, fmt.Errorf("list existing: %w", err)
	}
	beforeSet := make(map[string]struct{}, len(before))
	for _, p := range before {
		beforeSet[p] = struct{}{}
	}

	// Walker writes `seen` from a single goroutine — workers don't
	// touch it. Whether a worker actually persists a row is independent
	// of the deletion-pass invariant ("we saw it on disk during this
	// walk"); the walker decides on visibility, not the worker.
	seen := make(map[string]struct{}, len(before))

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
		go s.runScanWorker(ctx, paths, writes, &workersWG)
	}

	committed := new(atomic.Int64)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go s.runScanWriter(ctx, writes, committed, &writerWG)

	// Walker drives `filepath.WalkDir` for each configured root,
	// enqueuing audio files for the workers. Folder upserts stay inline
	// — single-goroutine writes, no contention with the batched
	// track-upsert path (which holds s.mu).
	var walkErr error
	for _, root := range roots {
		if err := s.walkRoot(ctx, root, multiRoot, seen, paths); err != nil {
			walkErr = err
			break
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

	// Deletion pass: anything in the "before" snapshot that we didn't see
	// in this walk is gone from disk.
	for p := range beforeSet {
		if _, ok := seen[p]; !ok {
			if err := s.store.DeleteTrack(p); err != nil {
				log.Printf("scanner: delete track %q: %v", p, err)
			}
		}
	}

	s.lastFull.Store(time.Now().UTC().UnixNano())
	_ = s.store.SetScanState("last_full_scan", time.Now().UTC().Format(time.RFC3339Nano))
	return count, nil
}

// runScanWorker is one of NumCPU workers reading walker-supplied paths
// off `paths`, doing the early-skip GetTrack check + the CPU-bound
// Extract, and feeding completed Tracks into the writer's `writes`
// channel. Errors from GetTrack/Extract are logged-and-skipped (matches
// the legacy walker's "log + continue" semantics — a single corrupt
// FLAC must not abort the whole scan).
func (s *Scanner) runScanWorker(ctx context.Context, paths <-chan pathInfo, writes chan<- *Track, wg *sync.WaitGroup) {
	defer wg.Done()
	for pi := range paths {
		if ctx.Err() != nil {
			// Drain remaining paths without doing work so the walker
			// can close the channel and we exit cleanly.
			continue
		}
		// Early-skip on unchanged-since-last-scan: matches the legacy
		// walker. Concurrent reads from N workers against the same
		// SQLite handle are fine (modernc.org/sqlite WAL mode allows
		// concurrent readers).
		existing, _ := s.store.GetTrack(pi.rel)
		if existing != nil && existing.Size == pi.info.Size() && !existing.ModTime.Before(pi.info.ModTime()) {
			continue
		}
		t := &Track{
			Path:    pi.rel,
			Size:    pi.info.Size(),
			ModTime: pi.info.ModTime().UTC(),
		}
		fillFromPath(t, pi.rel) // last-resort heuristics for files with no tags
		if err := Extract(pi.abs, t); err != nil {
			log.Printf("scanner: extract %q: %v", pi.abs, err)
		}
		select {
		case writes <- t:
		case <-ctx.Done():
			return
		}
	}
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
		if err := s.store.UpsertTrackBatch(batch); err != nil {
			log.Printf("scanner: upsert batch (%d rows): %v", len(batch), err)
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
				// Honour cancellation between batches; don't drop
				// the un-flushed remainder (next iteration handles).
				// Drain remaining channel to let workers exit.
				for range writes {
				}
				return
			}
		}
	}
	flush()
}

// walkRoot drives `filepath.WalkDir` for one root, recording folder
// mtimes inline (single-writer; no contention with the workers'
// track-upsert pipeline) and handing off audio files to the worker
// pool via `paths`. The deletion-pass `seen` map is written here so
// workers don't need a mutex around it — visibility-during-walk is a
// walker-domain concern, independent of whether the worker actually
// re-extracts (early-skip case) or persists.
func (s *Scanner) walkRoot(ctx context.Context, root string, multiRoot bool, seen map[string]struct{}, paths chan<- pathInfo) error {
	return filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission error on one dir shouldn't kill the whole scan.
			log.Printf("scanner: walk %q: %v", abs, err)
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
			if info, err := d.Info(); err == nil {
				rel := relPath(root, abs, multiRoot)
				_ = s.store.UpsertFolder(&Folder{Path: rel, ModTime: info.ModTime().UTC()})
			}
			return nil
		}
		// Skip dot-files and unsupported extensions.
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(abs))
		if !Ext[ext] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			log.Printf("scanner: stat %q: %v", abs, err)
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
func fillFromPath(t *Track, rel string) {
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
			log.Printf("scanner: initial scan: %v", err)
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
				log.Printf("scanner: periodic scan: %v", err)
			}
		}
	}
}

// BuildManifest returns a Manifest built from the current Store contents.
// since, if non-zero, filters tracks by mtime (for incremental iOS
// updates).
func BuildManifest(store *Store, roots []string, since time.Time) (*Manifest, error) {
	var sp *time.Time
	if !since.IsZero() {
		sp = &since
	}
	tracks, err := store.ListTracks(sp)
	if err != nil {
		return nil, err
	}
	folders, err := store.ListFolders()
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
	total, terr := store.CountTracks()
	if terr != nil {
		log.Printf("manifest: CountTracks for enrichment-progress: %v", terr)
	} else if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(); perr == nil {
		m.EnrichmentProgress = &EnrichmentProgress{
			TracksTotal:    total,
			TracksEnriched: enriched,
			LastEnrichedAt: lastEnrichedAt,
		}
	} else {
		log.Printf("manifest: EnrichmentCounts: %v", perr)
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
func WriteManifest(w io.Writer, store *Store, roots []string, since time.Time) (err error) {
	folders, err := store.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	// EnrichmentProgress block: same best-effort behaviour as
	// BuildManifest — counter failures log but don't fail the request.
	// `tracksTotal` and the iOS-side enrichment hint cohabit a single
	// CountTracks() call so the protocol invariant `manifest.total ==
	// EnrichmentProgress.TracksTotal` holds (Qodo #2 carry-over).
	var ep *EnrichmentProgress
	if total, terr := store.CountTracks(); terr != nil {
		log.Printf("manifest: CountTracks for enrichment-progress: %v", terr)
	} else if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(); perr == nil {
		ep = &EnrichmentProgress{
			TracksTotal:    total,
			TracksEnriched: enriched,
			LastEnrichedAt: lastEnrichedAt,
		}
	} else {
		log.Printf("manifest: EnrichmentCounts: %v", perr)
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
	first := true
	streamErr := store.StreamTracks(sp, func(t *Track) error {
		if !first {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		_, err = bw.Write(b)
		return err
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
func BuildManifestPage(store *Store, roots []string, cursor string, limit int) (*Manifest, error) {
	if limit <= 0 {
		limit = 1000
	}
	// Over-fetch by one so the last row of the current query tells us
	// "is there another page" definitively.
	tracks, err := store.ListTracksPage(cursor, limit+1)
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
		folders, ferr := store.ListFolders()
		if ferr != nil {
			return nil, ferr
		}
		total, terr := store.CountTracks()
		if terr != nil {
			return nil, terr
		}
		m.Folders = folders
		m.Total = &total
		if enriched, lastEnrichedAt, perr := store.EnrichmentCounts(); perr == nil {
			m.EnrichmentProgress = &EnrichmentProgress{
				TracksTotal:    total,
				TracksEnriched: enriched,
				LastEnrichedAt: lastEnrichedAt,
			}
		} else {
			log.Printf("manifest: EnrichmentCounts (paginated): %v", perr)
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
	return m, nil
}

// DefaultDBPath returns the SQLite path used when the user doesn't
// override. Lives under dataDir so the same config logic applies.
func DefaultDBPath(dataDir string) string { return filepath.Join(dataDir, "bridge.db") }

// ensurePathExists is a small test helper (exported so api_test can use).
var _ = os.Stat
