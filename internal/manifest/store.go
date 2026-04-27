package manifest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver (pure-Go, no cgo)
)

// Store persists Tracks and Folders in a single SQLite file.
// The store is safe for concurrent Open/Close/Read/Write within one
// process; SQLite's own locking serializes writes across goroutines.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes multi-statement transactions
}

// OpenStore opens (or creates) a SQLite DB at path and applies the schema.
// The file and its parent directory are created if missing.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection for writes keeps WAL + busy_timeout friendly.
	// Reads are fine via the default pool; modernc.org/sqlite uses the
	// standard database/sql connection pool.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS tracks (
		path        TEXT PRIMARY KEY,
		size        INTEGER NOT NULL,
		mtime_ns    INTEGER NOT NULL,
		tags_json   BLOB    NOT NULL,
		indexed_at  INTEGER NOT NULL,
		enriched_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_tracks_mtime ON tracks(mtime_ns);
	CREATE INDEX IF NOT EXISTS idx_tracks_indexed ON tracks(indexed_at);
	CREATE INDEX IF NOT EXISTS idx_tracks_enriched ON tracks(enriched_at);
	-- Functional indexes on the JSON-extracted MBID fields drive the
	-- 202-vs-404 probe on /v1/artwork + /v1/artist-image. Without
	-- these, every cache miss triggers a full tags_json scan -- O(n)
	-- on a 50k-track library. The expression here must match the
	-- hasTrackWithJSONField query exactly (same json_extract path,
	-- same BLOB column) for SQLite to use the index.
	CREATE INDEX IF NOT EXISTS idx_tracks_artwork_mbid
		ON tracks(json_extract(tags_json, '$.artworkMBID'));
	CREATE INDEX IF NOT EXISTS idx_tracks_artist_mbid
		ON tracks(json_extract(tags_json, '$.artistMBID'));

	CREATE TABLE IF NOT EXISTS folders (
		path     TEXT PRIMARY KEY,
		mtime_ns INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS scan_state (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Idempotent fallback for DBs created before enriched_at existed.
	// "duplicate column" is expected and ignored.
	_, _ = s.db.Exec(`ALTER TABLE tracks ADD COLUMN enriched_at INTEGER NOT NULL DEFAULT 0`)
	return nil
}

// UnenrichedTracks returns up to limit tracks that haven't been through the
// MusicBrainz/CoverArt pass. Used by internal/enrich.
func (s *Store) UnenrichedTracks(limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT tags_json FROM tracks
		WHERE enriched_at = 0
		ORDER BY path ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// marshalForStorage encodes a Track into the JSON blob written to
// `tags_json`. **Strips `Enriched` before marshalling** so the field
// is never persisted in the blob — the column-truth invariant is that
// `Track.Enriched` is column-derived (`enriched_at != 0`) at read
// time and must not exist in `tags_json`.
//
// Without this, a caller that takes a `Track` from `ListTracks` /
// `ListTracksPage` (which DO splice `Enriched` from the column) and
// passes it back into `UpsertTrack` or `MarkEnriched` would persist
// the spliced value into `tags_json`. Then `GetTrack` /
// `UnenrichedTracks` (which read only the JSON, not the column)
// would deserialize a stale `Enriched` flag — and an `UpsertTrack`
// that resets `enriched_at = 0` would leave the column saying "not
// enriched" while the JSON says "enriched: true". CodeRabbit caught
// the latent risk on PR #68 even though no caller exercises it
// today; this defensive shim makes the invariant structural rather
// than relying on every future caller to remember.
func marshalForStorage(t *Track) ([]byte, error) {
	clone := *t
	clone.Enriched = nil
	return json.Marshal(&clone)
}

// MarkEnriched updates a Track's stored tags (with enricher additions) and
// stamps enriched_at so the worker won't re-process it.
func (s *Store) MarkEnriched(t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		UPDATE tracks
		SET tags_json = ?, enriched_at = ?
		WHERE path = ?
	`, raw, time.Now().UnixNano(), t.Path)
	return err
}

// ----- tracks -----

// UpsertTrack writes or replaces the row for t.Path. The tags are encoded
// as JSON so the schema can evolve without column migrations during v0.
func (s *Store) UpsertTrack(t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size        = excluded.size,
			mtime_ns    = excluded.mtime_ns,
			tags_json   = excluded.tags_json,
			indexed_at  = excluded.indexed_at,
			enriched_at = 0
	`, t.Path, t.Size, t.ModTime.UnixNano(), raw, time.Now().UnixNano())
	return err
}

// UpsertTrackBatch writes (or replaces) many tracks inside a single
// transaction with one prepared statement reused across rows. The
// scanner's writer goroutine calls this with up-to-`scanBatchSize`
// rows per flush — collapsing the N×fsync of per-row autocommit (50k
// transactions on a 50k-track library) into ~N/500 transactions.
//
// Empty input is a no-op. On any per-row error the transaction rolls
// back via the deferred Rollback (Commit-after-Rollback is harmless),
// so partial-batch writes never leak. The returned error is the first
// failure; callers should log and continue (the scanner's writer does).
//
// Holds `s.mu` for the duration of the transaction so concurrent
// `MarkEnriched` / `WipeAllTracks` / `DeleteTracksByPrefix` don't
// interleave their multi-statement sections with our writes — matches
// the existing convention from those callers.
func (s *Store) UpsertTrackBatch(ts []*Track) error {
	if len(ts) == 0 {
		return nil
	}
	// Pre-marshal every row OUTSIDE the lock so the critical section
	// only covers the actual SQLite writes. JSON marshalling 500 rows
	// can dwarf the BEGIN/COMMIT cost; keeping it out of the locked
	// region lets concurrent enrichment / wipe paths progress (Gemini
	// on PR #71). marshalForStorage failures abort the whole batch
	// before any SQL touches the DB.
	type row struct {
		path    string
		size    int64
		mtime   int64
		tagsRaw []byte
	}
	rows := make([]row, len(ts))
	for i, t := range ts {
		raw, err := marshalForStorage(t)
		if err != nil {
			return err
		}
		rows[i] = row{
			path:    t.Path,
			size:    t.Size,
			mtime:   t.ModTime.UnixNano(),
			tagsRaw: raw,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size        = excluded.size,
			mtime_ns    = excluded.mtime_ns,
			tags_json   = excluded.tags_json,
			indexed_at  = excluded.indexed_at,
			enriched_at = 0
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixNano()
	for _, r := range rows {
		if _, err := stmt.Exec(r.path, r.size, r.mtime, r.tagsRaw, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteTrack removes a track by path. Missing rows are not an error.
func (s *Store) DeleteTrack(path string) error {
	_, err := s.db.Exec(`DELETE FROM tracks WHERE path = ?`, path)
	return err
}

// GetTrack fetches a single track by path. Returns (nil, nil) if absent.
func (s *Store) GetTrack(path string) (*Track, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT tags_json FROM tracks WHERE path = ?`, path).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Track
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Shared sentinels for `Track.Enriched`'s pointer assignments. The
// field is `*bool` for wire-shape reasons (nil distinguishes
// "pre-v1.1 server, field absent" from explicit `false`), but the
// server-side scan only ever needs two values. Without these,
// `t.Enriched = &enriched` where `enriched` is a loop-local would
// force one heap allocation per track — Qodo flagged this on a
// 50k-track library as a real GC-pressure issue. Two package-level
// vars let every row share the same two pointers and the loop
// allocates nothing extra.
//
// Safe to share: the value at `*enrichedTrue` / `*enrichedFalse` is
// never mutated (the `Track.Enriched` consumers only ever read,
// and the JSON encoder only reads as well).
var (
	enrichedTrue  = true
	enrichedFalse = false
)

// ListTracks returns all tracks, or (if since != nil) only tracks that
// were written/updated in the index after since. Filtered by
// indexed_at (when we last wrote the row) rather than mtime_ns (the
// on-disk file time) so that files copied into the library with an
// old mtime still surface in incremental deltas — otherwise the iOS
// client has to do a full sync to see ripped-years-ago albums that
// were just added.
//
// `Track.Enriched` is spliced in from the row's `enriched_at` column
// (true iff != 0). The JSON-encoded `tags_json` blob doesn't carry
// the enriched bit because enrichment status is column-tracked
// separately from tag content — embedding it in `tags_json` would
// require re-marshalling every track on each `MarkEnriched` write
// just to flip a bool, which is what the column is for.
func (s *Store) ListTracks(since *time.Time) ([]Track, error) {
	q := `SELECT tags_json, enriched_at FROM tracks`
	args := []any{}
	if since != nil {
		q += ` WHERE indexed_at > ?`
		args = append(args, since.UnixNano())
	}
	q += ` ORDER BY path ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Track{}
	for rows.Next() {
		var raw []byte
		var enrichedAt int64
		if err := rows.Scan(&raw, &enrichedAt); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		// Share package-level pointers — see the `enrichedTrue` /
		// `enrichedFalse` declaration above for the rationale.
		if enrichedAt != 0 {
			t.Enriched = &enrichedTrue
		} else {
			t.Enriched = &enrichedFalse
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StreamTracks calls fn for every row matching the same predicate as
// ListTracks (since-filtered when sp != nil). Used by the legacy
// /v1/manifest endpoint to stream JSON to the response writer without
// materialising a 50k-row []Track in memory — a Pi-class host with a
// large library would OOM otherwise (review item).
//
// fn receives a *Track that is REUSED across iterations — the same
// allocation is reset and re-populated each row. fn MUST NOT retain
// the pointer past return; callers that need to hold on must copy
// the Track value. The reuse cuts ~50k struct allocs out of a
// large-library scan compared to a per-row var.
//
// Iteration stops on the first non-nil error fn returns; that error
// is propagated. rows.Err() (post-iteration) is also returned if fn
// finished cleanly.
func (s *Store) StreamTracks(sp *time.Time, fn func(*Track) error) error {
	if fn == nil {
		// Defensive guard: invoking the callback later would panic with
		// a nil-deref. CodeRabbit on PR #70 — surface a clear error
		// before doing any DB work so misuse is obvious instead of a
		// production crash deep in the streaming-manifest path.
		return errors.New("StreamTracks: nil callback")
	}
	q := `SELECT tags_json, enriched_at FROM tracks`
	args := []any{}
	if sp != nil {
		q += ` WHERE indexed_at > ?`
		args = append(args, sp.UnixNano())
	}
	q += ` ORDER BY path ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	// Hoisted outside the loop: the same Track is reused each
	// iteration to honour the contract documented above. `t = Track{}`
	// resets every field (including the spliced Enriched pointer) so
	// stale data from row N never leaks into row N+1.
	var t Track
	for rows.Next() {
		var raw []byte
		var enrichedAt int64
		if err := rows.Scan(&raw, &enrichedAt); err != nil {
			return err
		}
		t = Track{}
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		if enrichedAt != 0 {
			t.Enriched = &enrichedTrue
		} else {
			t.Enriched = &enrichedFalse
		}
		if err := fn(&t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ListTracksPage returns up to `limit` tracks with `path > afterPath`,
// ordered by path ASC. The path ordering is stable (it's the primary
// key) so a paginated iteration — start with `afterPath=""` and keep
// passing the last returned path back in — walks every track exactly
// once without duplication or gaps, even across arbitrary reorders
// of the underlying data.
//
// `limit <= 0` falls back to a sensible default (1000) so an errant
// caller can't wedge the server on a 0-row response forever.
//
// Intentionally does NOT accept a `since` parameter — since-delta
// responses are small by construction and the iOS side pulls them
// in a single request. Mixing pagination + since would require a
// composite cursor (timestamp + path) for consistent ordering,
// which isn't worth the complexity for a code path that already
// returns bounded output.
func (s *Store) ListTracksPage(afterPath string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(`
		SELECT tags_json, enriched_at FROM tracks
		WHERE path > ?
		ORDER BY path ASC
		LIMIT ?
	`, afterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Track{}
	for rows.Next() {
		var raw []byte
		var enrichedAt int64
		if err := rows.Scan(&raw, &enrichedAt); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		// Same enriched-from-column splice as `ListTracks` — see
		// the comment there for the "why a column, not the JSON" detail
		// and the `enrichedTrue` / `enrichedFalse` declaration for the
		// pointer-sharing rationale.
		if enrichedAt != 0 {
			t.Enriched = &enrichedTrue
		} else {
			t.Enriched = &enrichedFalse
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasTrackWithArtworkMBID reports whether at least one indexed track
// carries the given value in its `artworkMBID` tag. Used by the
// `/v1/artwork/{mbid}` handler to tell a genuinely-unknown MBID
// (return 404) apart from one the server's seen but hasn't cached yet
// (return 202 + Retry-After so iOS retries with backoff instead of
// treating the miss as terminal).
//
// Backed by a functional index on `json_extract(tags_json,
// '$.artworkMBID')` (added in `migrate`) so the lookup is O(log n)
// instead of a full table scan on a 50k-track library. The `LIMIT 1`
// lets SQLite stop at first match.
func (s *Store) HasTrackWithArtworkMBID(mbid string) bool {
	if mbid == "" {
		return false
	}
	return s.hasTrackWithJSONField(artworkMBIDField, mbid)
}

// HasTrackWithArtistMBID mirrors HasTrackWithArtworkMBID for the
// `/v1/artist-image/{mbid}` handler. Same 202-vs-404 distinction.
// Also indexed (see `migrate`).
func (s *Store) HasTrackWithArtistMBID(mbid string) bool {
	if mbid == "" {
		return false
	}
	return s.hasTrackWithJSONField(artistMBIDField, mbid)
}

// Field names for the JSON-extract lookup. Declared as constants
// (not parameters passed by callers) so `hasTrackWithJSONField` can
// enforce a whitelist — the function used to take an arbitrary
// string and splice it into the SQL, which is fine today (both call
// sites are in-package) but a fragile pattern to leave for future
// extensions. The whitelist switch inside keeps each supported
// field's query as a pre-built string literal, eliminating any
// path where user input could influence the SQL.
type jsonField string

const (
	artworkMBIDField jsonField = "artworkMBID"
	artistMBIDField  jsonField = "artistMBID"
)

func (s *Store) hasTrackWithJSONField(field jsonField, value string) bool {
	var q string
	switch field {
	case artworkMBIDField:
		q = `SELECT 1 FROM tracks WHERE json_extract(tags_json, '$.artworkMBID') = ? LIMIT 1`
	case artistMBIDField:
		q = `SELECT 1 FROM tracks WHERE json_extract(tags_json, '$.artistMBID') = ? LIMIT 1`
	default:
		// Unknown field — by construction unreachable, but refusing
		// quietly is safer than compiling a bogus query.
		return false
	}
	var found int
	err := s.db.QueryRow(q, value).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Genuine database errors (disk I/O, connection closed,
		// migration mid-flight) get logged. `sql.ErrNoRows` is the
		// expected "no such MBID" outcome and stays quiet.
		log.Printf("store: hasTrackWithJSONField %s: %v", field, err)
		return false
	}
	return found == 1
}

// CountTracks returns the total number of track rows. /v1/health polls
// this frequently, so it's backed by a SELECT COUNT(*) instead of a
// full path-materialization + len().
func (s *Store) CountTracks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tracks`).Scan(&n)
	return n, err
}

// EnrichmentCounts returns the library-wide enrichment counters used
// by the manifest's `enrichmentProgress` block: number of tracks past
// the enrich pass (`enriched_at != 0`) and the wall-clock of the most
// recent successful enrichment. Single SQL trip via aggregate
// expressions so a 50k-track library doesn't allocate per-row.
//
// **Deliberately does NOT return total** — Qodo flagged the original
// signature: combining `total` here with the `CountTracks()` call in
// `BuildManifestPage`'s first-page branch produced two separate
// COUNT(*) queries against `tracks`, and a concurrent
// `UpsertTrack`/`DeleteTrack` between the two could let
// `manifest.total` and `enrichmentProgress.tracksTotal` disagree
// inside the same response — directly contradicting the protocol's
// guarantee that they match. Callers now compute total once via
// `CountTracks()` and reuse it for both fields, eliminating both the
// divergence window and the redundant query.
//
// **Pointer return on `lastEnrichedAt`** so the JSON serialization
// path can drop the field cleanly when no track has ever been
// enriched. A zero `time.Time` value would slip past `omitempty` (Go's
// `encoding/json` doesn't treat the time-struct's IsZero as "empty"),
// emit `"0001-01-01T00:00:00Z"` on the wire, and the iOS decoder would
// parse that as a real date — breaking the 24 h freshness gate.
//
// Backed implicitly by `idx_tracks_enriched` (already present from
// `migrate`) — the `enriched_at != 0` and `MAX(enriched_at)` clauses
// are both index-friendly.
func (s *Store) EnrichmentCounts() (enriched int, lastEnrichedAt *time.Time, err error) {
	var lastNs sql.NullInt64
	err = s.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN enriched_at != 0 THEN 1 ELSE 0 END), 0),
			MAX(enriched_at)
		FROM tracks
	`).Scan(&enriched, &lastNs)
	if err != nil {
		return 0, nil, err
	}
	if lastNs.Valid && lastNs.Int64 != 0 {
		t := time.Unix(0, lastNs.Int64).UTC()
		lastEnrichedAt = &t
	}
	return enriched, lastEnrichedAt, nil
}

// CountTracksByPrefix returns the number of track rows whose path begins
// with prefix. In multi-root mode the admin console passes
// "<rootBasename>/" to get a per-root count. prefix is matched literally —
// "_" and "%" are escaped via the ESCAPE clause so a root named "foo_bar"
// isn't treated as a LIKE wildcard.
func (s *Store) CountTracksByPrefix(prefix string) (int, error) {
	escaped := likeEscape(prefix)
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tracks WHERE path LIKE ? ESCAPE '\'`,
		escaped+"%",
	).Scan(&n)
	return n, err
}

// DeleteTracksByPrefix removes all track rows whose path begins with
// prefix. Returns the number of rows deleted. Used by the admin console
// after removing a library root so /v1/manifest stops returning tracks
// that will never resolve. See CountTracksByPrefix for the escaping note.
//
// Wrapped in a transaction for consistency with WipeAllTracks — a single
// DELETE is already atomic in SQLite, but this keeps the store's mutation
// surface uniformly transactional and lets a follow-up add a companion
// folders-cleanup without churn in the commit boundary.
func (s *Store) DeleteTracksByPrefix(prefix string) (int64, error) {
	escaped := likeEscape(prefix)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`DELETE FROM tracks WHERE path LIKE ? ESCAPE '\'`,
		escaped+"%",
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// WipeAllTracks drops every track and folder row. Used by the admin
// console on a single-root ↔ multi-root transition, where stored paths
// change form (bare "Artist/…" vs "RootBasename/Artist/…") and the cheap
// fix is to let the next scan re-populate from zero.
//
// Wrapped in a transaction so a failure between the two DELETEs can't
// leave the DB with folders that outlive their tracks (or the reverse)
// — next startup would see half-cleared state that the scanner has no
// logic to reconcile cleanly.
func (s *Store) WipeAllTracks() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM tracks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM folders`); err != nil {
		return err
	}
	return tx.Commit()
}

// likeEscape prepares a literal string for LIKE pattern matching. Escapes
// "%", "_", and "\" with a leading backslash. Caller must use
// `ESCAPE '\'` in the SQL.
func likeEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// TrackPaths returns every known track path (sorted). Used by the scanner's
// "remove tracks deleted from disk" pass.
func (s *Store) TrackPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM tracks ORDER BY path ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ----- folders -----

// UpsertFolder records a folder's mtime so the scanner can skip unchanged
// subtrees.
func (s *Store) UpsertFolder(f *Folder) error {
	_, err := s.db.Exec(`
		INSERT INTO folders(path, mtime_ns) VALUES (?, ?)
		ON CONFLICT(path) DO UPDATE SET mtime_ns = excluded.mtime_ns
	`, f.Path, f.ModTime.UnixNano())
	return err
}

// FolderMTime returns the stored mtime for a folder path, or the zero time
// if absent.
func (s *Store) FolderMTime(path string) (time.Time, error) {
	var ns int64
	err := s.db.QueryRow(`SELECT mtime_ns FROM folders WHERE path = ?`, path).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, ns), nil
}

// ListFolders returns every folder record (sorted).
func (s *Store) ListFolders() ([]Folder, error) {
	rows, err := s.db.Query(`SELECT path, mtime_ns FROM folders ORDER BY path ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		var p string
		var ns int64
		if err := rows.Scan(&p, &ns); err != nil {
			return nil, err
		}
		out = append(out, Folder{Path: p, ModTime: time.Unix(0, ns).UTC()})
	}
	return out, rows.Err()
}

// ----- scan_state -----

// SetScanState writes a key/value pair to the scan_state table.
func (s *Store) SetScanState(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO scan_state(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v
	`, key, value)
	return err
}

// GetScanState returns the value for key, or "" if missing.
func (s *Store) GetScanState(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM scan_state WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
