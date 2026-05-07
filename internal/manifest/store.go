package manifest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	_ "modernc.org/sqlite" // register "sqlite" driver (pure-Go, no cgo)
)

var logger = logging.Component("manifest")

// Store persists Tracks and Folders in a single SQLite file.
// The store is safe for concurrent Open/Close/Read/Write within one
// process. WAL mode lets readers proceed concurrently with at most one
// active writer; the Go-side `mu` enforces "at most one writer" so
// SQLite's `busy_timeout` retry is never reached under our workload.
//
// **Writer contract**: every method that issues `INSERT` / `UPDATE` /
// `DELETE` SQL MUST hold `s.mu` (UpsertTrack, UpsertTrackBatch,
// DeleteTrack, DeleteTracksByPrefix, MarkEnriched, WipeAllTracks,
// PutScanState, etc.). Readers DO NOT hold `s.mu` so concurrent
// `/v1/manifest` streaming and `ListTracks` queries can fan out
// during a long scan or enrichment pass — this is the whole point
// of running WAL.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes ALL writers (see contract above)
	// now returns the timestamp used by `indexed_at` writes from the
	// variant write paths (UpsertVariant, DeleteVariant). Defaults to
	// time.Now in production; tests override with a deterministic
	// monotonically-incrementing fake so the
	// `delta-sync-surfaces-new-variant` regression assertions don't
	// depend on wall-clock sleeps. Same DI shape we'd reach for the
	// next time a write path needs a controllable clock; UpsertTrack
	// stays on direct `time.Now()` for now (not in this PR's scope).
	now func() time.Time
}

// OpenStore opens (or creates) a SQLite DB at path and applies the schema.
// The file and its parent directory are created if missing.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store dir: %w", err)
	}
	uri := dsn.File(path, "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection for writes keeps WAL + busy_timeout friendly.
	// Reads are fine via the default pool; modernc.org/sqlite uses the
	// standard database/sql connection pool.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// migration is one step in the schema ladder. `sql` is executed as a
// single multi-statement Exec; `post` runs afterwards for cases where
// a later refactor needs a Go-side fixup (e.g. the swallowed
// ALTER-TABLE for back-compat with pre-v1.1 DBs that lack a column
// the baseline `CREATE TABLE` now declares).
//
// Always make `sql` idempotent (`IF NOT EXISTS`, `OR REPLACE`, etc.).
// A crash mid-migration and restart should re-run cleanly.
type migration struct {
	version int
	name    string
	sql     string
	post    func(*sql.DB) error
}

// migrations defines the schema ladder. New entries append to this
// slice; never reorder or rewrite existing entries (they may have
// already run on deployed DBs at the bumped `user_version`).
var migrations = []migration{
	{
		version: 1,
		name:    "baseline (v1.0 → v1.1 schema)",
		sql: `
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
		`,
		post: func(db *sql.DB) error {
			// Idempotent fallback for DBs created before enriched_at
			// existed. "duplicate column" is expected and ignored —
			// CREATE TABLE above already declares the column for fresh
			// DBs; this path covers v1.0 → v1.1 in-place upgrades that
			// were running before the column was introduced.
			_, _ = db.Exec(`ALTER TABLE tracks ADD COLUMN enriched_at INTEGER NOT NULL DEFAULT 0`)
			return nil
		},
	},
	{
		version: 2,
		name:    "track_variants (v1.2 — PCM upscaling)",
		// Why this table is created unconditionally even when
		// upscale.enabled = false: predictable round-trip. An operator
		// can enable → run `bridge upscale` → variants populate →
		// disable (manifest stops advertising them) → re-enable
		// (variants reappear without re-conversion). The runtime
		// feature gate lives in `provider.go`'s manifest splice, not
		// here.
		//
		// Schema notes:
		//   - PRIMARY KEY (source_path, variant_id) lets multiple
		//     variants of the same source coexist (different target
		//     rates, future DSD synthesis). The `variant_id` is opaque
		//     to the table; only the `bridge upscale` producer knows
		//     the naming convention.
		//   - sidecar_path stores the absolute on-disk path. We do
		//     NOT recompute it from a hash + variant_id at read time
		//     — operators may relocate `<dataDir>/transcoded/` and we
		//     want the DB to be authoritative for "where this file
		//     lives right now".
		//   - source_mtime_ns + source_size belt-and-braces freshness
		//     check. A sidecar whose source has drifted is considered
		//     stale; the variant-resolve path returns 410 Gone so iOS
		//     falls back to the original until `bridge upscale` re-
		//     converts.
		//   - sox_settings is opaque JSON — forensic record of the
		//     resampler args used. Only consumed by ad-hoc operator
		//     debugging.
		//   - ON DELETE CASCADE removes the row when the parent
		//     `tracks` row is deleted, but DOES NOT remove the
		//     on-disk sidecar file. Store.DeleteTrack handles the
		//     filesystem cleanup explicitly before issuing the
		//     parent DELETE — see the function for the rationale.
		sql: `
		CREATE TABLE IF NOT EXISTS track_variants (
			source_path     TEXT    NOT NULL,
			variant_id      TEXT    NOT NULL,
			sidecar_path    TEXT    NOT NULL,
			format          TEXT    NOT NULL,
			sample_rate     INTEGER NOT NULL,
			bits_per_sample INTEGER NOT NULL,
			size_bytes      INTEGER NOT NULL,
			source_mtime_ns INTEGER NOT NULL,
			source_size     INTEGER NOT NULL,
			sox_settings    TEXT    NOT NULL,
			created_at      INTEGER NOT NULL,
			PRIMARY KEY (source_path, variant_id),
			FOREIGN KEY (source_path) REFERENCES tracks(path) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_track_variants_source_path
			ON track_variants(source_path);
		`,
	},
	{
		version: 3,
		name:    "case-insensitive path lookup indexes",
		// iOS normalises a track's path via `share.normalize(path:)`
		// before storing it in SwiftData (NFC + lowercase) and sends
		// THAT shape on every endpoint that accepts a path —
		// including POST /v1/upscale, which then hands the value
		// to `Store.GetTrack` for the eligibility gate. The manifest
		// stores paths in their original case (the FS scan emits
		// "Abdullah Ibrahim/..." while iOS sends "/abdullah
		// ibrahim/..."), so the equality compare missed and every
		// lookup returned nil → ErrUpscaleIneligible. Operator
		// reproducer: long-press a FLAC, "Generate upscaled
		// version", bridge log shows `enqueued=0 queueFull=false`,
		// no work happens.
		//
		// Two functional indexes mirror the lookups:
		//   - tracks(LOWER(path)) for GetTrack
		//   - track_variants(LOWER(source_path), variant_id) for
		//     GetVariant
		// Functional indexes capture the LOWER expression at
		// definition time; SQLite uses them when the query's WHERE
		// clause matches the index expression byte-for-byte.
		// Indexes are populated lazily on first query; for an
		// existing 50k-track DB the first lookup pays the index
		// build (a few hundred ms typical) once.
		//
		// SQLite's built-in LOWER() is ASCII-only — for libraries
		// with accented characters (most non-English) iOS's full
		// Unicode lowercase via `String.lowercased()` will land on
		// a different byte sequence. Future fix: precompute the
		// lowercased form Go-side and store as a separate column.
		// For now this covers the 90% case (Western libraries) and
		// the manifest's existing PRIMARY KEY remains authoritative
		// for the canonical case-preserved form. See LookupTrack /
		// LookupVariant in store.go.
		sql: `
		CREATE INDEX IF NOT EXISTS idx_tracks_path_lower
			ON tracks(LOWER(path));
		CREATE INDEX IF NOT EXISTS idx_track_variants_source_path_lower
			ON track_variants(LOWER(source_path), variant_id);
		`,
	},
}

// normalizePathForLookup folds an iOS-shaped track path back toward
// the form Store.GetTrack / GetVariant can compare with manifest's
// canonical (case-preserved) PRIMARY KEY. Three transformations:
//   - collapse `//`, `.`, and `..` segments via path.Clean. Without
//     this, a request shaped `Artist//Album/01.flac` would resolve
//     correctly on disk (filesystem treats `//` as a single
//     separator) but miss the manifest row keyed at the canonical
//     `Artist/Album/01.flac` form, surfacing as
//     ErrUpscaleIneligible / variant_not_found / track_not_found
//     across the API surface (Gemini on PR #147).
//   - strip a single leading "/" (iOS's `share.normalize` adds one
//     to bridge-source paths so SMB and bridge paths share the
//     same anchor; the manifest stores the bridge form without).
//   - the rest of the case fold happens at the SQL layer via
//     `LOWER(path) = LOWER(?)` against the v3 functional index.
//
// Pure / nil-safe / cheap; called from the two lookup helpers
// only, not from write paths (the manifest stays authoritative
// for the original case). Centralizing the cleaning here means
// /v1/download, /v1/download?variant=, and /v1/upscale all benefit
// from the same fix — none of those endpoints have to repeat the
// normalization logic.
func normalizePathForLookup(p string) string {
	if p == "" {
		return ""
	}
	// Root the input before path.Clean to keep the result well-defined
	// (path.Clean("") returns ".", path.Clean("/") returns "/"); strip
	// the leading slash afterwards so the form matches the PRIMARY KEY
	// shape the scanner stores (manifest paths are slash-free).
	cleaned := path.Clean("/" + p)
	cleaned = strings.TrimPrefix(cleaned, "/")
	return cleaned
}

// migrate walks the migration ladder, applying any whose `version`
// exceeds the database's current `PRAGMA user_version`. After each
// step succeeds the version is bumped so a partial-batch rerun
// continues where it left off.
//
// **Pre-ladder DBs** (created before this PR) carry `user_version = 0`
// AND have the v1.1 schema applied via the legacy implicit path.
// Migration 1 is idempotent (`IF NOT EXISTS` everywhere + swallowed
// ALTER), so re-running it on those DBs is a no-op that just bumps
// the version stamp.
func (s *Store) migrate() error {
	var current int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if _, err := s.db.Exec(m.sql); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(s.db); err != nil {
				return fmt.Errorf("migration %d (%s) post-DDL: %w", m.version, m.name, err)
			}
		}
		// PRAGMA user_version doesn't accept parameter binding (it's a
		// directive, not DML), so format the int into the literal SQL.
		// The version comes from a hardcoded slice — never user input —
		// so SQL-injection risk is zero.
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			return fmt.Errorf("set user_version to %d: %w", m.version, err)
		}
	}
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
//
// Holds `s.mu` for the SQL exec so an in-flight enrichment update never
// races a `UpsertTrackBatch` from the scanner — both are writers and
// the contract documented on the Store type forbids them from
// overlapping in SQLite. JSON marshalling stays outside the lock so
// the critical section is one statement long.
func (s *Store) MarkEnriched(t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) UpsertTrack(t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
//
// Holds `s.mu` per the writer contract on Store. Called from the
// scanner's deletion-pass at end of scan; could otherwise race
// concurrent enricher MarkEnriched and surface as
// `database is locked` after the SQLite busy_timeout.
//
// Sidecar-cleanup contract (PR feat/upscale-phase1): the table
// `track_variants` cascades on the parent track delete, but SQLite
// CASCADE only removes the row — it does NOT remove the on-disk
// `.flac` sidecar file. Without intervention a `--gc` would later
// have no DB record to find the orphan by, so 100MB+ files would
// leak per deleted source. Sequence here is:
//  1. SELECT every sidecar_path for the track.
//  2. os.Remove each one (log-and-continue on error — a missing or
//     already-deleted sidecar shouldn't block the DB delete).
//  3. DELETE FROM tracks (cascade fires after files are gone).
//
// `bridge upscale --gc` is the recovery path for sidecars that
// escape this — interrupted DeleteTrack, manual SQL tampering,
// restored-from-backup mismatch.
func (s *Store) DeleteTrack(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Step 1: enumerate sidecars BEFORE the DB delete. Shared
	// helper keeps the per-policy details (rows.Err handling,
	// log-and-continue per-row) aligned with the bulk-delete
	// paths.
	rows, err := s.db.Query(`SELECT sidecar_path FROM track_variants WHERE source_path = ?`, path)
	var sidecars []string
	if err == nil {
		for rows.Next() {
			var sp string
			if scanErr := rows.Scan(&sp); scanErr != nil {
				logger.Warn("delete-track: scan sidecar_path", "track", path, "err", scanErr)
				continue
			}
			sidecars = append(sidecars, sp)
		}
		if iterErr := rows.Err(); iterErr != nil {
			logger.Warn("delete-track: iter sidecars", "track", path, "err", iterErr)
		}
		rows.Close()
	} else {
		// Couldn't enumerate sidecars — log and proceed with
		// the parent delete anyway. The orphan file becomes
		// `--gc`'s problem on the next pass, NOT a reason to
		// leave a stale row in the manifest.
		logger.Warn("delete-track: list sidecars", "track", path, "err", err)
	}
	// Step 2: parent delete. CASCADE clears `track_variants`
	// rows; sidecar files we just enumerated will be unlinked
	// next.
	if _, err = s.db.Exec(`DELETE FROM tracks WHERE path = ?`, path); err != nil {
		return err
	}
	// Step 3: best-effort filesystem cleanup, shared with the
	// bulk-delete paths.
	removeSidecarFiles(sidecars)
	return nil
}

// GetTrack fetches a single track by EXACT path match. Returns
// (nil, nil) if absent.
//
// Case-sensitive by design: `tracks.path` is the SQL PRIMARY KEY
// and on case-sensitive filesystems (most Linux deployments) two
// files can legitimately coexist whose paths differ only by case.
// `Scanner.runScanWorker`'s unchanged-file fast-path calls this
// with the exact path it just walked; any case-folding here would
// risk returning an arbitrary sibling and silently skipping the
// real file from indexing.
//
// External callers that hand in iOS-shaped paths (lowercase +
// leading slash from `share.normalize(path:)`) should call
// `LookupTrack` instead — that path tolerates the iOS normalisation
// at the cost of a slower index scan, which is fine for the
// once-per-request /v1/upscale eligibility gate but wrong for the
// scanner's hot inner loop. (Qodo on PR #126.)
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

// LookupTrack fetches a single track by an iOS-shaped path —
// lowercase + leading slash from `share.normalize(path:)` — and
// resolves it against the manifest's case-preserved canonical
// PRIMARY KEY. Returns (nil, nil) if absent.
//
// Two-stage lookup:
//
//  1. Exact match (back-compat fast path; covers the case where
//     iOS sends a path that already matches the canonical case,
//     e.g. on case-sensitive filesystems where paths weren't
//     normalised to lowercase).
//  2. Case-insensitive fallback via `LOWER(path) = LOWER(?)`. The
//     v3 migration's `idx_tracks_path_lower` functional index
//     makes this O(1).
//
// Use this for any caller that takes a path from iOS — the upscale
// eligibility gate is the canonical example. The two-stage shape
// preserves correctness on case-sensitive filesystems where two
// distinct files may legitimately differ only by case: the exact
// match wins first, and only when no exact match exists does the
// fallback reach. Multiple distinct case-colliding rows can still
// be sources of ambiguity in the fallback (`LIMIT 1` returns one
// arbitrarily) — that's a manifest-data anomaly, not a lookup
// concern, and the SAME-CASE upsert path keeps it impossible
// under normal scanner operation. (Qodo on PR #126: scanner
// hot-loop callers must NOT use this — they need GetTrack's
// exact-match contract for correctness.)
//
// Limitation: SQLite's built-in LOWER is ASCII-only. Libraries
// with accented characters will see iOS's full-Unicode
// `String.lowercased()` produce a different byte sequence than
// SQLite's LOWER, and lookups will still miss. Documented in the
// v3 migration; future fix is a Go-side-precomputed
// `path_lower` column populated from `golang.org/x/text/cases`.
func (s *Store) LookupTrack(path string) (*Track, error) {
	if t, err := s.GetTrack(path); err != nil || t != nil {
		return t, err
	}
	cleaned := normalizePathForLookup(path)
	if cleaned == path {
		// Exact already missed and the cleaned form is identical
		// — no further fallback to attempt that wouldn't repeat
		// the same query.
		return s.lookupTrackByLowerCase(cleaned)
	}
	// Try the leading-slash-stripped form as a back-compat
	// exact match before falling through to the case-folded
	// scan; some iOS code paths only strip the slash without
	// lowercasing, and that exact form should land cheaply.
	if t, err := s.GetTrack(cleaned); err != nil || t != nil {
		return t, err
	}
	return s.lookupTrackByLowerCase(cleaned)
}

func (s *Store) lookupTrackByLowerCase(cleaned string) (*Track, error) {
	// Fail closed on ambiguity: fetch LIMIT 2, and if a second row
	// exists, refuse to pick one. On case-sensitive filesystems
	// (most Linux deployments) two distinct files can legitimately
	// coexist whose paths differ only by case — the case-folded
	// fallback would otherwise return whichever row SQLite happens
	// to visit first, silently re-introducing the aliasing problem
	// `GetTrack` was kept exact to avoid. (CodeRabbit on PR #126.)
	//
	// Conservative: nil is a slightly worse answer than "guess at
	// random" for the rare both-rows-distinct case, but the caller
	// already treats nil as "track not found" → the upscale
	// eligibility gate returns ErrUpscaleIneligible. The exact-
	// match fast paths in `LookupTrack` cover the common case where
	// iOS sends a path that already matches one row's case, so
	// this only reaches the fallback when the iOS-shape genuinely
	// can't be distinguished.
	rows, err := s.db.Query(
		`SELECT tags_json FROM tracks WHERE LOWER(path) = LOWER(?) LIMIT 2`,
		cleaned,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	if rows.Next() {
		logger.Warn("LookupTrack: case-folded fallback is ambiguous, refusing to pick a row", "path", cleaned)
		return nil, nil
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

// variantsAggSQL is the correlated subquery suffix appended to every
// `SELECT ... FROM tracks` that wants the variants column. Returns
// a JSON array string per row — `[]` (literal two chars) for tracks
// with no variants, otherwise `[{...}, {...}]`. Single SQL round
// trip per page regardless of variant cardinality (load-bearing
// against N+1 on a 50k-track library).
//
// Constant string concatenation in the caller (not parameter
// binding) is safe here: the subquery has no user input. Track
// path lookups use the existing prepared-statement parameter on
// the outer SELECT.
const variantsAggSQL = `
	(SELECT json_group_array(json_object(
	            'id',            v.variant_id,
	            'format',        v.format,
	            'sampleRate',    v.sample_rate,
	            'bitsPerSample', v.bits_per_sample,
	            'sizeBytes',     v.size_bytes,
	            'label',         v.variant_id))
	 FROM track_variants v
	 WHERE v.source_path = tracks.path) AS variants_json`

// scanTrackVariants decodes the JSON aggregation column produced
// by variantsAggSQL into Track.Variants. Empty / `null` / `[]`
// payloads land as nil so `omitempty` drops the field on the
// wire. Defensive against partially-corrupt JSON: malformed input
// is logged and the variants slice stays nil — a track is still
// playable from its source.
func scanTrackVariants(t *Track, raw []byte) {
	if len(raw) == 0 {
		return
	}
	// Cheap fast-path: SQLite's json_group_array returns the
	// literal two-byte string `[]` for tracks with no variants.
	// Skip the unmarshal in the common case.
	if len(raw) == 2 && raw[0] == '[' && raw[1] == ']' {
		return
	}
	var vs []Variant
	if err := json.Unmarshal(raw, &vs); err != nil {
		logger.Warn("scan-track-variants: unmarshal", "err", err)
		return
	}
	if len(vs) > 0 {
		// Server-side label finalisation: the SQL aggregation
		// emits `label = variant_id` as a placeholder so the
		// label is non-null on the wire even if the producer
		// didn't compute one. Replace with a human-friendly
		// rendering here so iOS clients don't have to.
		for i := range vs {
			vs[i].Label = humanLabelForVariant(vs[i])
		}
		t.Variants = vs
	}
}

// humanLabelForVariant renders an iOS-friendly description for
// the picker UI. Today's only producer is `bridge upscale`, which
// mints `upscaled-v1-<rate>-<bits>` IDs.
func humanLabelForVariant(v Variant) string {
	rateLabel := formatSampleRateLabel(v.SampleRate)
	switch {
	case v.Format == "flac":
		return fmt.Sprintf("Upscaled FLAC %d/%s", v.BitsPerSample, rateLabel)
	default:
		return fmt.Sprintf("%s %d/%s", v.Format, v.BitsPerSample, rateLabel)
	}
}

// formatSampleRateLabel mirrors iOS's TrackQualityChip rendering:
// 44.1 family → "44.1", "88.2", "176.4", "352.8"; 48 family →
// "48", "96", "192", "384". The integer-Hz wire form gets a more
// compact display in either family.
func formatSampleRateLabel(hz float64) string {
	switch int(hz) {
	case 44100:
		return "44.1"
	case 88200:
		return "88.2"
	case 176400:
		return "176.4"
	case 352800:
		return "352.8"
	case 48000, 96000, 192000, 384000:
		return strconv.Itoa(int(hz / 1000))
	default:
		// Fallback: kHz with up to one decimal.
		return strconv.FormatFloat(hz/1000, 'f', -1, 64)
	}
}

// ListTracks returns all tracks, or (if since != nil) only tracks that
// were written/updated in the index after since. Filtered by
// indexed_at (when we last wrote the row OR when a variant write/delete
// touched the row's variant set, see UpsertVariant / DeleteVariant)
// rather than mtime_ns (the on-disk file time) so that files copied
// into the library with an old mtime still surface in incremental
// deltas — otherwise the iOS client has to do a full sync to see
// ripped-years-ago albums that were just added.
//
// Variant writes bump `indexed_at` for the parent row so iOS clients
// running an incremental sync after submitting an upscale request see
// the new variant land within the next delta window — without that
// bump, the wand on iOS sat at `.inFlight` then `.stalled` because
// the delta-filtered manifest never returned the parent row at all.
//
// `Track.Enriched` is spliced in from the row's `enriched_at` column
// (true iff != 0). The JSON-encoded `tags_json` blob doesn't carry
// the enriched bit because enrichment status is column-tracked
// separately from tag content — embedding it in `tags_json` would
// require re-marshalling every track on each `MarkEnriched` write
// just to flip a bool, which is what the column is for.
func (s *Store) ListTracks(since *time.Time) ([]Track, error) {
	q := `SELECT tags_json, enriched_at, ` + variantsAggSQL + ` FROM tracks`
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
		var variantsRaw []byte
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		scanTrackVariants(&t, variantsRaw)
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
	q := `SELECT tags_json, enriched_at, ` + variantsAggSQL + ` FROM tracks`
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
		var variantsRaw []byte
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw); err != nil {
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
		scanTrackVariants(&t, variantsRaw)
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
		SELECT tags_json, enriched_at, `+variantsAggSQL+` FROM tracks
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
		var variantsRaw []byte
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		scanTrackVariants(&t, variantsRaw)
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
		logger.Error("hasTrackWithJSONField", "field", field, "err", err)
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

// ArtworkMBIDsInUse returns the distinct artworkMBID values currently
// referenced by at least one track row. Used by `bridge artwork --gc`
// to identify cached files in <dataDir>/artwork/ that are no longer
// referenced and can be removed (orphan recovery, see Gemini A10 /
// iOS bug review #10 — pre-fix the artwork directory grew unbounded
// over months/years of curation since there was no cleanup path).
//
// Backed by the same functional index used by `HasTrackWithArtworkMBID`
// (`idx_tracks_artwork_mbid`); the DISTINCT + WHERE-NOT-NULL filter
// runs as an index scan. Scales to any library size.
//
// Returns NULL-filtered values: tracks without artworkMBID set
// (`json_extract` returns NULL) are skipped at the SQL layer so the
// caller's set logic doesn't have to handle empty strings explicitly.
func (s *Store) ArtworkMBIDsInUse() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT json_extract(tags_json, '$.artworkMBID')
		FROM tracks
		WHERE json_extract(tags_json, '$.artworkMBID') IS NOT NULL
		  AND json_extract(tags_json, '$.artworkMBID') != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v.Valid && v.String != "" {
			out = append(out, v.String)
		}
	}
	return out, rows.Err()
}

// EnrichmentCounts returns the library-wide enrichment counters used
// by the manifest's `enrichmentProgress` block: number of tracks past
// the enrich pass (`enriched_at != 0`) and the wall-clock of the most
// recent successful enrichment. Two scalar subqueries so the SQLite
// query planner can use `idx_tracks_enriched` for BOTH:
//   - `COUNT(*) WHERE enriched_at != 0` — index range scan over the
//     non-zero partition, often optimised to an index-only count.
//   - `MAX(enriched_at)` — O(log n) B-tree tail lookup.
//
// **Pre-fix**: the query was a single `SELECT SUM(CASE WHEN
// enriched_at != 0 THEN 1 ELSE 0 END), MAX(enriched_at) FROM tracks`.
// The doc-comment claimed both clauses were index-friendly — that was
// only true for the MAX clause; SQLite's planner cannot use an index
// for `SUM(CASE WHEN ...)` because the CASE expression is opaque,
// forcing a full table scan over `tracks` on every `/v1/health` poll.
// On a 100k-track library on a low-power host (Pi 4/5, low-IOPS SD
// cards), that's a measurable CPU spike every ~15 s. Per Gemini A9 /
// iOS bug review #9.
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
func (s *Store) EnrichmentCounts() (enriched int, lastEnrichedAt *time.Time, err error) {
	var lastNs sql.NullInt64
	// `enriched_at > 0` is sargeable; `enriched_at != 0` is not. Even
	// though both predicates describe the same row set (the column is
	// only ever 0 or a positive Unix timestamp set by `MarkEnriched`),
	// SQLite's planner converts `> 0` into a bounded index range scan
	// against `idx_tracks_enriched`, while `!= 0` falls back to a
	// covering-index full walk. Verified via EXPLAIN QUERY PLAN — the
	// `> 0` form emits `SEARCH tracks USING COVERING INDEX
	// idx_tracks_enriched (enriched_at>?)` while `!= 0` emits a bare
	// `SCAN tracks` (CodeRabbit Major round-1 on PR #164).
	err = s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM tracks WHERE enriched_at > 0),
			(SELECT MAX(enriched_at) FROM tracks)
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
//
// Holds `s.mu` per the writer contract on Store so an admin "remove
// library root" never interleaves with a concurrent scanner
// `UpsertTrackBatch` — pre-fix the two writers could collide in
// SQLite and surface as `database is locked` after `busy_timeout`.
// Now they queue in Go.
//
// Sidecar-cleanup: same orphan-prevention contract as
// `DeleteTrack`. CASCADE drops the `track_variants` rows but
// leaves on-disk `.flac` sidecars; without explicit cleanup
// here, a "remove library root" admin action would leak every
// sidecar belonging to that root with no DB row left for `--gc`
// to find by source-path lookup. CodeRabbit second-pass on PR
// #108. Single-track DeleteTrack and the wipe-all paths share
// the same `removeSidecarsForPaths` helper so all three deletion
// entry points stay aligned.
func (s *Store) DeleteTracksByPrefix(prefix string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	escaped := likeEscape(prefix)
	// Step 1: enumerate doomed sidecars BEFORE the cascade drops
	// the rows. Reuses the proactive-cleanup contract documented
	// on DeleteTrack.
	doomedSidecars, _ := s.listSidecarsByPathPrefix(escaped)
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
	// Step 2: filesystem cleanup AFTER the commit. Best-effort —
	// per-file errors logged but never propagated; the row delete
	// already committed and the operator's intent ("get rid of
	// this prefix") has been honored at the DB layer.
	removeSidecarFiles(doomedSidecars)
	return n, nil
}

// listSidecarsByPathPrefix returns every sidecar_path whose
// source_path matches the LIKE-escaped prefix. Used by
// DeleteTracksByPrefix's pre-cascade enumeration. Caller MUST
// hold s.mu (writer-serialization contract).
func (s *Store) listSidecarsByPathPrefix(escapedPrefix string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT sidecar_path FROM track_variants WHERE source_path LIKE ? ESCAPE '\'`,
		escapedPrefix+"%",
	)
	if err != nil {
		logger.Warn("list sidecars by prefix", "prefix", escapedPrefix, "err", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if scanErr := rows.Scan(&sp); scanErr != nil {
			logger.Warn("scan sidecar by prefix", "err", scanErr)
			continue
		}
		out = append(out, sp)
	}
	if iterErr := rows.Err(); iterErr != nil {
		logger.Warn("iter sidecars by prefix", "err", iterErr)
	}
	return out, nil
}

// listAllSidecars returns every sidecar_path in the table. Used
// by WipeAllTracks. Same writer-lock contract.
func (s *Store) listAllSidecars() ([]string, error) {
	rows, err := s.db.Query(`SELECT sidecar_path FROM track_variants`)
	if err != nil {
		logger.Warn("list all sidecars", "err", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if scanErr := rows.Scan(&sp); scanErr != nil {
			logger.Warn("scan sidecar (wipe)", "err", scanErr)
			continue
		}
		out = append(out, sp)
	}
	if iterErr := rows.Err(); iterErr != nil {
		logger.Warn("iter all sidecars", "err", iterErr)
	}
	return out, nil
}

// removeSidecarFiles unlinks every path in the slice, logging
// (but not propagating) per-file errors. Missing files are
// silent — they're already in the desired state. Shared by all
// three deletion entry points so the "log but don't block on
// per-file failure" policy lives in one place.
func removeSidecarFiles(paths []string) {
	for _, sp := range paths {
		if rmErr := os.Remove(sp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			logger.Warn("remove sidecar", "path", sp, "err", rmErr)
		}
	}
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
//
// Sidecar-cleanup: same orphan-prevention contract as
// `DeleteTrack`/`DeleteTracksByPrefix`. CodeRabbit second-pass on
// PR #108. CASCADE alone would leak every cached `.flac` sidecar.
func (s *Store) WipeAllTracks() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doomedSidecars, _ := s.listAllSidecars()
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
	if err := tx.Commit(); err != nil {
		return err
	}
	removeSidecarFiles(doomedSidecars)
	return nil
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

// TrackPathsUnder returns every track path at or under relDir (sorted).
// relDir is in the library-relative forward-slash form used in storage.
//
// Three normalizations on relDir:
//   - "" or "." (single-root whole-library scope) returns every track,
//     matching TrackPaths.
//   - "<base>/." (multi-root whole-root sentinel; relPath form for an
//     fsnotify event on the root itself) returns every track under
//     "<base>/".
//   - otherwise the match is "<relDir>/%" with LIKE-special characters
//     escaped via the ESCAPE clause so a directory whose name happens to
//     contain "%" or "_" doesn't widen the match.
//
// Tracks are files, never directories, so a track path can never equal
// relDir itself — only the descendant pattern is needed.
//
// Used by ScanSubtree's bounded deletion pass.
func (s *Store) TrackPathsUnder(relDir string) ([]string, error) {
	if relDir == "" || relDir == "." {
		return s.TrackPaths()
	}
	var pattern string
	if strings.HasSuffix(relDir, "/.") {
		pattern = likeEscape(strings.TrimSuffix(relDir, ".")) + "%"
	} else {
		pattern = likeEscape(relDir) + "/%"
	}
	rows, err := s.db.Query(
		`SELECT path FROM tracks WHERE path LIKE ? ESCAPE '\' ORDER BY path ASC`,
		pattern,
	)
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
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) UpsertFolder(f *Folder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// FolderPaths returns every known folder path (sorted). Folder
// analogue of TrackPaths — used by the scanner's orphan-folder
// deletion pass to enumerate the "before" snapshot. Distinct from
// ListFolders, which projects (path, mtime) tuples and is the right
// shape for callers that need both fields.
func (s *Store) FolderPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM folders ORDER BY path ASC`)
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

// FolderPathsUnder returns every folder path at or under relDir
// (sorted), INCLUDING relDir itself when a row for it exists.
// relDir is in the library-relative forward-slash form used in storage.
//
// Same three normalizations as TrackPathsUnder. Unlike tracks (which
// are always files), the row for relDir itself is a real folder row
// the scanner upserted on its previous walk, so the match must
// include it — otherwise a "directory was renamed in place" event
// would leave the original folder row behind.
//
// Used by ScanSubtree's bounded deletion pass.
func (s *Store) FolderPathsUnder(relDir string) ([]string, error) {
	if relDir == "" || relDir == "." {
		return s.FolderPaths()
	}
	if strings.HasSuffix(relDir, "/.") {
		// Multi-root whole-root sentinel ("<base>/."): match every
		// folder under "<base>/". The "<base>/." row IS upserted by
		// the walker (relPath(root, root, true) returns this form),
		// so include it via an exact-match alongside the LIKE.
		stripped := strings.TrimSuffix(relDir, ".")
		pattern := likeEscape(stripped) + "%"
		rows, err := s.db.Query(
			`SELECT path FROM folders WHERE path = ? OR path LIKE ? ESCAPE '\' ORDER BY path ASC`,
			relDir, pattern,
		)
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
	pattern := likeEscape(relDir) + "/%"
	rows, err := s.db.Query(
		`SELECT path FROM folders WHERE path = ? OR path LIKE ? ESCAPE '\' ORDER BY path ASC`,
		relDir, pattern,
	)
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

// DeleteFolder removes a single folder row by exact path. Missing
// rows are not an error — same idempotent contract as DeleteTrack
// for the scanner's deletion pass, which iterates over a snapshot
// that may have raced with an out-of-process delete (admin "remove
// library root" path).
//
// Folders carry no children in the schema (track_variants references
// tracks, not folders), so this is a single DELETE with no cascade.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) DeleteFolder(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM folders WHERE path = ?`, path)
	return err
}

// ----- scan_state -----

// SetScanState writes a key/value pair to the scan_state table.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) SetScanState(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// VariantRow is the on-disk record for one cached transcoded
// rendering of a track. Mirrors `track_variants` schema column-for-
// column. Constructed by the transcode package when a sox run
// completes, queried by the variant-resolve path on
// `/v1/download?variant=`, and by `bridge upscale --gc`.
type VariantRow struct {
	SourcePath    string
	VariantID     string
	SidecarPath   string
	Format        string
	SampleRate    int
	BitsPerSample int
	SizeBytes     int64
	SourceMTimeNS int64
	SourceSize    int64
	SoxSettings   string
	CreatedAt     int64
}

// UpsertVariant writes (or replaces) one row in `track_variants` AND
// bumps the parent track row's `indexed_at` so iOS delta-sync (which
// filters tracks via `WHERE indexed_at > ?`, see ListTracks) surfaces
// the new variant on the next manifest fetch. Both writes happen in
// a single transaction; if the INSERT succeeds but the parent UPDATE
// fails (e.g. driver error), `defer tx.Rollback()` unwinds cleanly so
// the variant doesn't appear without its parent row's freshness signal.
//
// Without the bump, an iOS client that submitted an upscale request,
// got `enqueued=0` (variant cached) plus a successful `scanShare`, would
// still NOT see the variant — the parent row's `indexed_at` predates
// `share.lastScanFinishedAt`, so it falls outside the delta window.
// The wand stays at `.inFlight` for 10 min, then flips to `.stalled`.
//
// Holds `s.mu` per the writer contract. Replacement semantics
// mirror UpsertTrack — re-running `bridge upscale --force` re-
// converts and overwrites the prior row's metadata cleanly.
func (s *Store) UpsertVariant(v VariantRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit; structural rollback guarantee.
	if _, err := tx.Exec(`
		INSERT INTO track_variants
			(source_path, variant_id, sidecar_path, format,
			 sample_rate, bits_per_sample, size_bytes,
			 source_mtime_ns, source_size, sox_settings, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (source_path, variant_id) DO UPDATE SET
			sidecar_path    = excluded.sidecar_path,
			format          = excluded.format,
			sample_rate     = excluded.sample_rate,
			bits_per_sample = excluded.bits_per_sample,
			size_bytes      = excluded.size_bytes,
			source_mtime_ns = excluded.source_mtime_ns,
			source_size     = excluded.source_size,
			sox_settings    = excluded.sox_settings,
			created_at      = excluded.created_at
	`, v.SourcePath, v.VariantID, v.SidecarPath, v.Format,
		v.SampleRate, v.BitsPerSample, v.SizeBytes,
		v.SourceMTimeNS, v.SourceSize, v.SoxSettings, v.CreatedAt); err != nil {
		return err
	}
	// Parent indexed_at bump. UPDATE on a missing parent is a no-op
	// (RowsAffected=0) but the FK on track_variants.source_path with
	// ON DELETE CASCADE means a variant insert with no parent FK-fails
	// at the INSERT above before we ever reach here — so a no-op UPDATE
	// implies an out-of-band parent delete in another transaction
	// (impossible under our `s.mu` contract). Safe to ignore.
	//
	// Strictly-advancing indexed_at update via CASE WHEN form. Three
	// guarantees in one expression:
	//   1. Monotonic — indexed_at can only advance, never regress
	//      (defense against past-clock injection / NTP rewind).
	//      (Qodo on PR #156, round 1.)
	//   2. STRICTLY advancing — when the new clock value equals the
	//      stored indexed_at (rapid back-to-back variant writes,
	//      injected-clock test scenarios, low-resolution wall clocks),
	//      we increment by 1 ns instead of leaving the value
	//      unchanged. Without this, a real variant change can be
	//      invisible to clients that already synced at the equal
	//      timestamp (delta-sync filter is `indexed_at > since`).
	//      (CodeRabbit on PR #156, round 2.)
	//   3. Single-statement / single round-trip — no read-then-write
	//      pattern that would race with a concurrent variant write
	//      under our `s.mu` writer-serialization contract anyway, but
	//      keeping it atomic is also marginally faster.
	now := s.now().UnixNano()
	if _, err := tx.Exec(`
		UPDATE tracks SET indexed_at = CASE
			WHEN indexed_at >= ? THEN indexed_at + 1
			ELSE ?
		END
		WHERE path = ?
	`, now, now, v.SourcePath); err != nil {
		return err
	}
	return tx.Commit()
}

// GetVariant fetches one row by (source_path, variant_id). Returns
// (nil, nil) if absent — same convention as GetTrack.
func (s *Store) GetVariant(sourcePath, variantID string) (*VariantRow, error) {
	var v VariantRow
	// Exact match by design — `track_variants.source_path` is part
	// of the SQL PRIMARY KEY and case-insensitive lookups would risk
	// returning an arbitrary case-colliding row's sidecar path on
	// case-sensitive filesystems. Use `LookupVariant` for callers
	// that hand in iOS-shaped paths from `share.normalize`. (Qodo
	// on PR #126: variant lookup non-determinism could stream the
	// wrong sidecar from /v1/download.)
	err := s.db.QueryRow(`
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		WHERE source_path = ? AND variant_id = ?
	`, sourcePath, variantID).Scan(
		&v.SourcePath, &v.VariantID, &v.SidecarPath, &v.Format,
		&v.SampleRate, &v.BitsPerSample, &v.SizeBytes,
		&v.SourceMTimeNS, &v.SourceSize, &v.SoxSettings, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// LookupVariant fetches a variant by an iOS-shaped sourcePath
// (lowercase + leading slash from `share.normalize(path:)`) and
// resolves it against the manifest's case-preserved
// `track_variants.source_path`. Returns (nil, nil) if absent.
//
// Same two-stage lookup as `LookupTrack`: exact first
// (cheap + correct on case-sensitive filesystems where two
// case-colliding distinct rows could otherwise alias), then
// `LOWER(source_path) = LOWER(?)` falling back via the v3
// migration's `idx_track_variants_source_path_lower` functional
// index. Use this when the caller hands in iOS-shaped paths;
// internal callers that walk the canonical PRIMARY KEY should
// stay on `GetVariant`. (Qodo on PR #126: the upscale freshness
// check is the canonical caller — it follows a `LookupTrack` and
// must agree with it on which row is being inspected.)
func (s *Store) LookupVariant(sourcePath, variantID string) (*VariantRow, error) {
	if v, err := s.GetVariant(sourcePath, variantID); err != nil || v != nil {
		return v, err
	}
	cleaned := normalizePathForLookup(sourcePath)
	if cleaned == sourcePath {
		return s.lookupVariantByLowerCase(cleaned, variantID)
	}
	if v, err := s.GetVariant(cleaned, variantID); err != nil || v != nil {
		return v, err
	}
	return s.lookupVariantByLowerCase(cleaned, variantID)
}

func (s *Store) lookupVariantByLowerCase(cleanedSourcePath, variantID string) (*VariantRow, error) {
	// Same fail-closed-on-ambiguity contract as
	// `lookupTrackByLowerCase` — see that function's comment for
	// the case-collision rationale. Two distinct case-colliding
	// `track_variants.source_path` rows under the same
	// `variant_id` would otherwise let LIMIT 1 stream the wrong
	// sidecar from /v1/download. (CodeRabbit on PR #126.)
	rows, err := s.db.Query(`
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		WHERE LOWER(source_path) = LOWER(?) AND variant_id = ?
		LIMIT 2
	`, cleanedSourcePath, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var v VariantRow
	if err := rows.Scan(
		&v.SourcePath, &v.VariantID, &v.SidecarPath, &v.Format,
		&v.SampleRate, &v.BitsPerSample, &v.SizeBytes,
		&v.SourceMTimeNS, &v.SourceSize, &v.SoxSettings, &v.CreatedAt,
	); err != nil {
		return nil, err
	}
	if rows.Next() {
		logger.Warn("LookupVariant: case-folded fallback is ambiguous, refusing to pick a row",
			"sourcePath", cleanedSourcePath, "variantID", variantID)
		return nil, nil
	}
	return &v, nil
}

// AllVariants returns every row in track_variants. Used by `bridge
// upscale --gc` to drive the mark-and-sweep against the on-disk
// `<dataDir>/transcoded/` directory.
func (s *Store) AllVariants() ([]VariantRow, error) {
	rows, err := s.db.Query(`
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		ORDER BY source_path ASC, variant_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VariantRow{}
	for rows.Next() {
		var v VariantRow
		if err := rows.Scan(
			&v.SourcePath, &v.VariantID, &v.SidecarPath, &v.Format,
			&v.SampleRate, &v.BitsPerSample, &v.SizeBytes,
			&v.SourceMTimeNS, &v.SourceSize, &v.SoxSettings, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVariant removes one row by (source_path, variant_id) AND bumps
// the parent track row's `indexed_at` so iOS delta-sync sees the
// removal on the next manifest fetch. Both writes happen in a single
// transaction; `defer tx.Rollback()` is the structural unwind guarantee.
//
// **Skips the bump on a no-op delete** (RowsAffected==0): when the
// requested (source_path, variant_id) didn't exist, the variant set
// is unchanged and a manifest-churn-inducing indexed_at bump would be
// false signal to iOS clients (CodeRabbit + Gemini on PR #156).
//
// Currently has no production callers (the `bridge upscale --gc` path
// in cmd/bridge/upscale.go walks the filesystem and removes orphan
// sidecar files; it does not touch DB rows). Defensive plumbing for the
// case a future caller does delete a variant — the bump symmetry
// matches UpsertVariant so iOS doesn't miss the disappearance.
//
// Holds `s.mu`. Caller is responsible for removing the on-disk sidecar
// file — same separation-of-concerns as DeleteTrack pre-cleanup.
func (s *Store) DeleteVariant(sourcePath, variantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM track_variants WHERE source_path = ? AND variant_id = ?`,
		sourcePath, variantID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		// Strictly-advancing indexed_at update — see UpsertVariant
		// for the full rationale. Same CASE WHEN form so a clock
		// equality (test injection, low-resolution wall clock,
		// rapid back-to-back variant writes) still produces a
		// strictly-greater indexed_at, keeping
		// `delta-sync WHERE indexed_at > since` reliable.
		now := s.now().UnixNano()
		if _, err := tx.Exec(`
			UPDATE tracks SET indexed_at = CASE
				WHEN indexed_at >= ? THEN indexed_at + 1
				ELSE ?
			END
			WHERE path = ?
		`, now, now, sourcePath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountVariants returns (rowCount, totalSizeBytes) across the
// whole `track_variants` table. Used by the admin console's
// upscale stats card. Single SQL aggregate — cheap even on
// large tables.
//
// Returns (0, 0, nil) when the table is empty (or the upscale
// feature has never been used). Errors propagate; the admin
// handler degrades to "stats unavailable" on failure.
func (s *Store) CountVariants() (int, int64, error) {
	var (
		count int
		bytes sql.NullInt64
	)
	row := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM track_variants`)
	if err := row.Scan(&count, &bytes); err != nil {
		return 0, 0, err
	}
	return count, bytes.Int64, nil
}
