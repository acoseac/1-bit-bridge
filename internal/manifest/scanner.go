package manifest

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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

// Scan runs a full walk of the library roots. It's safe to cancel via ctx;
// the partial work is committed (each UpsertTrack is its own transaction).
// Returns the count of tracks upserted.
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

	seen := make(map[string]struct{}, len(before))
	count := 0

	// Snapshot roots once per scan so a mid-flight SetRoots doesn't re-enter
	// the walk with a different set. multiRoot is derived from the same
	// snapshot — passing it explicitly to walkRoot avoids a second Load.
	rootsPtr := s.roots.Load()
	var roots []string
	if rootsPtr != nil {
		roots = *rootsPtr
	}
	multiRoot := len(roots) > 1
	for _, root := range roots {
		if err := s.walkRoot(ctx, root, multiRoot, seen, &count); err != nil {
			return count, err
		}
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

func (s *Scanner) walkRoot(ctx context.Context, root string, multiRoot bool, seen map[string]struct{}, count *int) error {
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

		existing, _ := s.store.GetTrack(rel)
		if existing != nil && existing.Size == info.Size() && !existing.ModTime.Before(info.ModTime()) {
			// Unchanged since last index — skip re-extracting tags.
			return nil
		}

		t := &Track{
			Path:    rel,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		}
		fillFromPath(t, rel) // last-resort heuristics for files with no tags
		if err := Extract(abs, t); err != nil {
			log.Printf("scanner: extract %q: %v", abs, err)
		}
		if err := s.store.UpsertTrack(t); err != nil {
			log.Printf("scanner: upsert %q: %v", rel, err)
		}
		*count++
		s.progress.Store(int64(*count))
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
