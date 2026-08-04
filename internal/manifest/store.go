package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
	"github.com/google/uuid"
	_ "modernc.org/sqlite" // register "sqlite" driver (pure-Go, no cgo)
)

// observeLockWait records SQLite transaction lock-wait timing into
// both the Prometheus histogram (for /metrics scrapers) AND the
// sliding-window backbone (for /v1/diagnostics's p50/p99 read).
// Centralized helper so the dual-publish contract can't drift between
// the BeginTx and ExecContext call sites.
func observeLockWait(op string, start time.Time) {
	dur := time.Since(start).Seconds()
	metrics.SQLiteLockWaitHist.WithLabelValues(op).Observe(dur)
	metrics.SQLiteLockWaitWindow.Observe(dur)
}

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
	// now returns the timestamp used by every Store write path that
	// writes a timestamp into a row (indexed_at on variants, enriched_at
	// on tracks, etc.). Defaults to time.Now in production; tests
	// override with a deterministic monotonically-incrementing fake
	// so delta-sync regression assertions don't depend on wall-clock
	// sleeps.
	//
	// Senior-audit follow-up: previously only the variant write paths
	// routed through `s.now()`; UpsertTrack / MarkEnriched and
	// DeleteVariant's `now := time.Now().UnixNano()` line used direct
	// `time.Now()`. The sweep through `time.Now() → s.now()` lands
	// every timestamped write inside Store on the injectable clock
	// surface, so a future test that wants to pin a strictly-advancing
	// indexed_at across UpsertTrack / UpsertVariant in one transaction
	// can do so without racing the wall clock.
	now func() time.Time

	// ftsAvailable caches whether the `tracks_fts` virtual table exists,
	// probed once in OpenStore after migrate(). The FTS5 module is either
	// compiled into the driver or not for the process lifetime, and the
	// schema is fixed after migrate, so the answer can't change — yet
	// SearchAvailable was re-querying sqlite_master on every SearchTracks
	// call (once per admin-search keystroke). Set once before the Store
	// is published to any caller, so it's safe to read without s.mu.
	ftsAvailable bool
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
		db.Close() // release the handle + background goroutines; mirrors the migrate() error path below
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// Probe FTS5 availability once. migrate() has created tracks_fts iff
	// FTS5 is compiled in; the result is fixed for the process lifetime,
	// so SearchAvailable reads this cached bool instead of hitting
	// sqlite_master on every search. A probe failure degrades to
	// "search unavailable" (503 at the handler) rather than failing
	// startup over an optional feature.
	if err := s.probeFTSAvailable(); err != nil {
		logger.Warn("fts availability probe failed; library search disabled", "err", err)
		s.ftsAvailable = false
	}
	return s, nil
}

// probeFTSAvailable resolves whether the tracks_fts virtual table exists
// and caches it on the Store. Called once from OpenStore after migrate.
func (s *Store) probeFTSAvailable() error {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tracks_fts'`,
	).Scan(&n); err != nil {
		return fmt.Errorf("probe tracks_fts existence: %w", err)
	}
	s.ftsAvailable = n > 0
	return nil
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
		// a different byte sequence.
		//
		// **Resolved in v4**: a Go-registered `unicode_lower()`
		// scalar function (see internal/manifest/sqlfunc.go)
		// replaces both these indexes with Unicode-aware variants
		// keyed on `unicode_lower(path)` / `unicode_lower(source_path)`.
		// The v3 indexes are dropped in v4. Don't rewrite v3 itself
		// — the migration ladder convention forbids touching shipped
		// entries; v3 ran on operator DBs in v1.2 and the v4 step
		// must be the one that drops + recreates.
		sql: `
		CREATE INDEX IF NOT EXISTS idx_tracks_path_lower
			ON tracks(LOWER(path));
		CREATE INDEX IF NOT EXISTS idx_track_variants_source_path_lower
			ON track_variants(LOWER(source_path), variant_id);
		`,
	},
	{
		version: 4,
		name:    "unicode-aware path lookup indexes",
		// The Go-registered `unicode_lower(text)` scalar (see
		// internal/manifest/sqlfunc.go) folds the input via
		// `cases.Lower(language.Und).String(...)`, matching iOS's
		// `String.lowercased()` semantics byte-for-byte. The
		// determinism flag at registration is required for use in
		// functional indexes — without it SQLite would refuse to use
		// the index and fall back to a full table scan.
		//
		// DROP-then-CREATE because functional indexes encode the
		// expression at definition time; SQLite has no `ALTER INDEX`
		// for the expression. The DROPs are `IF EXISTS` so a fresh
		// DB without the v3 indexes still applies the migration
		// cleanly.
		//
		// **Index-build cost**: rebuilding two functional indexes on
		// a 50k-track DB takes a few hundred ms on a Pi-class host
		// (the underlying scan is the dominant cost; the per-row
		// `unicode_lower` call is sub-microsecond). First launch
		// after upgrade pays this once.
		//
		// **Operator note**: a plain `sqlite3` CLI session that
		// opens this DB outside of a Go process can't query
		// `unicode_lower(...)` — the function is registered at the
		// driver level and only exists inside processes that import
		// internal/manifest. The TEXT data is preserved case in the
		// column either way; ad-hoc inspection via the CLI should
		// fall back to `lower(path)` (ASCII-only, same semantics
		// the v3 indexes had).
		sql: `
		DROP INDEX IF EXISTS idx_tracks_path_lower;
		DROP INDEX IF EXISTS idx_track_variants_source_path_lower;
		CREATE INDEX IF NOT EXISTS idx_tracks_path_unicode_lower
			ON tracks(unicode_lower(path));
		CREATE INDEX IF NOT EXISTS idx_track_variants_source_path_unicode_lower
			ON track_variants(unicode_lower(source_path), variant_id);
		`,
	},
	{
		version: 5,
		name:    "missing_count columns on tracks and folders",
		// Adds `missing_count INTEGER` to both `tracks` and `folders`.
		// The scanner increments it on each pass where a row is NOT
		// in the seen-set AND its subtree did NOT hit an errorSubtree
		// guard; it resets to 0 on each confirm via UpsertTrack /
		// UpsertTrackBatch / UpsertFolder. Rows reach `>= threshold`
		// only after N consecutive scans that successfully completed
		// without seeing the row — defending against silent partial
		// enumeration on flaky network mounts (SMB re-auth flap, NFS
		// brownout, libsmb2 timeout returning an empty Readdir).
		//
		// **Idempotency: BOTH ALTERs live in post(), not in `sql`.**
		// `migrate()` short-circuits on the first error from
		// `s.db.ExecContext(ctx, m.sql)`, so a partial-apply scenario (first
		// ALTER committed, second failed mid-migration, restart)
		// would otherwise hit "duplicate column" on the first ALTER
		// of the retry and never reach the post() that's meant to
		// swallow it — leaving the DB stuck. Doing both ALTERs in
		// post() (which the migrate caller invokes with error-tolerant
		// `_, _ = db.Exec(...)` semantics by convention) makes each
		// step independently idempotent and re-runnable after any
		// partial failure. The `sql` payload is a harmless SQL
		// comment so `s.db.ExecContext(ctx, m.sql)` always succeeds cleanly
		// before post() does the real work. Caught by Gemini HIGH
		// bot review on PR #193.
		sql: `-- columns added in post() for idempotency; see migration v5 docblock`,
		post: func(db *sql.DB) error {
			// Idempotent ALTERs — "duplicate column" is the expected
			// no-op when the prior run committed the column already.
			// Errors are swallowed deliberately: the next migrate()
			// call retries, and any persistent failure (disk full,
			// permissions) will manifest at the next real write the
			// scanner attempts, with a clearer error context.
			_, _ = db.Exec(`ALTER TABLE tracks ADD COLUMN missing_count INTEGER NOT NULL DEFAULT 0`)
			_, _ = db.Exec(`ALTER TABLE folders ADD COLUMN missing_count INTEGER NOT NULL DEFAULT 0`)
			return nil
		},
	},
	{
		version: 6,
		name:    "upscale_batches (v1.3 — operator-driven upscaling)",
		// Tracks operator-initiated upscale runs at folder / root /
		// library granularity. Replaces the per-track-tap model in
		// v1.2 — the iOS wand UI submitted one job at a time with no
		// queue position or ETA visibility; this table is the bridge-
		// side ledger that admin Library Inspector + Jobs page render.
		//
		// Schema notes:
		//   - `id BLOB PRIMARY KEY` carries a UUID v4 emitted by the
		//     coordinator at Submit time. BLOB chosen over TEXT to
		//     store the raw 16-byte form; saves ~36 bytes per row vs
		//     hex-encoded TEXT at the cost of needing
		//     `uuid.FromBytes(...)` on read. Negligible at expected
		//     scale (hundreds of rows over a library's lifetime).
		//   - `path` is the operator-selected scope: library-relative
		//     folder, library-relative root, or empty string for
		//     "whole library." Same normalization rules as
		//     `normalizePathForLookup` (NFC, slash-collapsed, no
		//     leading slash).
		//   - `target_rate INTEGER` in Hz (e.g. 192000), `target_bits
		//     INTEGER` (16 / 24). Each batch carries the resolved
		//     target at Submit time so a mid-run change to the global
		//     setting doesn't retroactively shift in-flight work.
		//   - `status` lifecycle: `pending` → `running` → `completed`
		//     | `failed` | `cancelled` | `interrupted`. `interrupted`
		//     is the bridge-restart recovery state — set by
		//     `RecoverInterruptedBatches` at boot, distinct from
		//     `cancelled` (operator action) and `failed` (every job
		//     errored). Pre-CHECK constraint enforces the enum at the
		//     DB layer; future statuses require a CHECK rewrite +
		//     migration bump.
		//   - `total_files` / `processed_files` / `failed_files`
		//     counters are bumped by the coordinator on each pool
		//     callback (PR 3 wires this).
		//   - `error TEXT` accommodates redacted sox stderr which can
		//     run multi-line for codec-mismatch / corrupt-header
		//     failures. SQLite TEXT is unbounded — DON'T add a
		//     `CHECK (length(error) < N)` constraint; truncation
		//     belongs in the coordinator's writer if needed (~4 KiB
		//     cap is a reasonable upper bound but not enforced here).
		//   - `created_at` / `updated_at` are nanosecond Unix epochs,
		//     matching the existing `indexed_at` / `enriched_at`
		//     convention.
		//
		// Index `idx_upscale_batches_status_created` powers the Jobs
		// page's filter chips (All / Running / Completed / Failed)
		// without a full table scan.
		sql: `
		CREATE TABLE IF NOT EXISTS upscale_batches (
			id              BLOB    PRIMARY KEY,
			path            TEXT    NOT NULL,
			target_rate     INTEGER NOT NULL,
			target_bits     INTEGER NOT NULL,
			status          TEXT    NOT NULL
				CHECK (status IN ('pending','running','completed','failed','cancelled','interrupted')),
			total_files     INTEGER NOT NULL DEFAULT 0,
			processed_files INTEGER NOT NULL DEFAULT 0,
			failed_files    INTEGER NOT NULL DEFAULT 0,
			error           TEXT,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_upscale_batches_status_created
			ON upscale_batches(status, created_at DESC);
		`,
	},
	{
		version: 7,
		name:    "tracks_fts (v1.4 — FTS5 library search)",
		// Standalone FTS5 virtual table indexing path / title / artist /
		// album so the admin Library Inspector can search across a 50k
		// track library in O(log N) rather than the O(N) LIKE scan that
		// a 2026-vintage Pi 4 would visibly stall on. Standalone
		// (not external-content) because title/artist/album live inside
		// the tags_json BLOB on `tracks` — FTS5 cannot index JSON content
		// directly, so triggers + backfill use json_extract to materialise
		// the indexed columns. ~10–15% storage overhead vs an
		// external-content shape; in exchange we avoid rowid coupling and
		// a tags_json deserialize per hit on read.
		//
		// `unicode61 remove_diacritics 2` lets "dvorak" match "Dvořák" —
		// audiophile-collection friendly. Don't pre-lowercase user input
		// before passing to MATCH; the tokenizer normalises both index
		// and query.
		//
		// Probe-and-skip-on-failure: the migration tries an FTS5 TEMP
		// table first. On environments where modernc.org/sqlite was
		// compiled without FTS5 (highly unusual but possible on minimal
		// builds), the probe errors, the migration logs a warning, and
		// bumps the version stamp without creating the real table. The
		// search API checks `tracks_fts` existence at request time and
		// returns 503 if absent, so library search degrades gracefully
		// rather than the whole bridge failing to start.
		sql: `-- table + triggers + backfill run in post() so we can probe FTS5 availability`,
		post: func(db *sql.DB) error {
			// FTS5 capability probe via a TEMP table on a DEDICATED
			// connection — TEMP tables are connection-scoped, so the
			// CREATE and its DROP MUST hit the SAME session. Issuing both
			// via the pooled db.Exec could draw different connections,
			// leaving the (empty) probe table lingering on the first for
			// the process lifetime. No schema side effects if the probe
			// fails; cheap if it succeeds; failure swallows and returns
			// nil (graceful degradation — the search API 503s if
			// tracks_fts is absent).
			// Bound the whole probe (conn checkout + CREATE/DROP) so a
			// pathologically locked/unresponsive DB fails gracefully
			// (FTS5 disabled) instead of wedging startup. The probe is an
			// in-memory temp-table op — microseconds normally — so 5s only
			// fires on a genuine hang.
			probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, connErr := db.Conn(probeCtx)
			if connErr != nil {
				logger.Warn("FTS5 probe: checkout connection", "err", connErr.Error())
				return nil
			}
			// `defer conn.Close()` inside a scoped func: panic-safe (a
			// future ExecContext that panics still releases the connection)
			// AND returns it to the pool BEFORE the O(N) tracks_fts backfill
			// below runs on the pool, rather than holding it idle.
			probeErr := func() error {
				defer conn.Close()
				if _, err := conn.ExecContext(probeCtx, `CREATE VIRTUAL TABLE temp.__fts5_probe USING fts5(x)`); err != nil {
					return err
				}
				_, _ = conn.ExecContext(probeCtx, `DROP TABLE temp.__fts5_probe`)
				return nil
			}()
			if probeErr != nil {
				logger.Warn("FTS5 unavailable; library search will be disabled",
					"err", probeErr.Error())
				return nil
			}

			// FTS5 confirmed. Create the real table + triggers.
			// IF NOT EXISTS on all DDL — defends against partial-apply
			// resumes (migration aborted mid-step, version stamp not
			// bumped, retry sees existing artefacts).
			ddl := `
			CREATE VIRTUAL TABLE IF NOT EXISTS tracks_fts USING fts5(
				path, title, artist, album,
				tokenize='unicode61 remove_diacritics 2'
			);
			CREATE TRIGGER IF NOT EXISTS tracks_fts_ai AFTER INSERT ON tracks BEGIN
				INSERT INTO tracks_fts(path, title, artist, album) VALUES (
					new.path,
					COALESCE(json_extract(new.tags_json, '$.title'), ''),
					COALESCE(json_extract(new.tags_json, '$.artist'), ''),
					COALESCE(json_extract(new.tags_json, '$.album'), '')
				);
			END;
			CREATE TRIGGER IF NOT EXISTS tracks_fts_au AFTER UPDATE ON tracks BEGIN
				DELETE FROM tracks_fts WHERE path = old.path;
				INSERT INTO tracks_fts(path, title, artist, album) VALUES (
					new.path,
					COALESCE(json_extract(new.tags_json, '$.title'), ''),
					COALESCE(json_extract(new.tags_json, '$.artist'), ''),
					COALESCE(json_extract(new.tags_json, '$.album'), '')
				);
			END;
			CREATE TRIGGER IF NOT EXISTS tracks_fts_ad AFTER DELETE ON tracks BEGIN
				DELETE FROM tracks_fts WHERE path = old.path;
			END;
			`
			if _, err := db.Exec(ddl); err != nil {
				return fmt.Errorf("create tracks_fts table+triggers: %w", err)
			}

			// Backfill — only if FTS table is empty. The version stamp
			// only bumps after this post() succeeds, so a retry after
			// partial-success would see a non-empty FTS table here and
			// skip; that's correct (every prior INSERT was atomic at
			// the SQL level).
			var ftsCount int
			if err := db.QueryRow(`SELECT COUNT(*) FROM tracks_fts`).Scan(&ftsCount); err != nil {
				return fmt.Errorf("probe tracks_fts count: %w", err)
			}
			if ftsCount == 0 {
				if _, err := db.Exec(`
					INSERT INTO tracks_fts(path, title, artist, album)
					SELECT path,
					       COALESCE(json_extract(tags_json, '$.title'), ''),
					       COALESCE(json_extract(tags_json, '$.artist'), ''),
					       COALESCE(json_extract(tags_json, '$.album'), '')
					FROM tracks
				`); err != nil {
					return fmt.Errorf("backfill tracks_fts: %w", err)
				}
			}
			return nil
		},
	},
	{
		version: 8,
		name:    "scope tracks_fts_au trigger to indexed columns (v1.4 followup)",
		// The v7 tracks_fts_au trigger fired on EVERY UPDATE to
		// `tracks` — including hot-path counter writes
		// (`missing_count`, `indexed_at`) that don't change any
		// FTS-indexed column. Each write triggered a redundant
		// DELETE-then-INSERT pair on `tracks_fts`, which on a 50k-row
		// library compounds during scan / enrich loops. CodeRabbit
		// caught it post-merge on PR #243.
		//
		// Fix: drop the broad trigger + recreate with `AFTER UPDATE
		// OF path, tags_json` so only column changes that actually
		// affect FTS content fire the trigger.
		//
		// Probe-gated like v7: if FTS5 isn't available, this is a
		// no-op (the trigger doesn't exist to drop, the table
		// doesn't exist to scope; the search API stays disabled).
		sql: `-- probe-gated; runs in post()`,
		post: func(db *sql.DB) error {
			// If tracks_fts wasn't created in v7 (FTS5 unavailable),
			// the trigger doesn't exist either. The DROP is harmless
			// (IF EXISTS), and the CREATE is skipped via the
			// existence probe below.
			var ftsTableCount int
			if err := db.QueryRow(`
				SELECT COUNT(*) FROM sqlite_master
				 WHERE type='table' AND name='tracks_fts'
			`).Scan(&ftsTableCount); err != nil {
				return fmt.Errorf("probe tracks_fts existence: %w", err)
			}
			if ftsTableCount == 0 {
				return nil // FTS5 unavailable, nothing to scope.
			}
			ddl := `
			DROP TRIGGER IF EXISTS tracks_fts_au;
			CREATE TRIGGER IF NOT EXISTS tracks_fts_au
			  AFTER UPDATE OF path, tags_json ON tracks BEGIN
				DELETE FROM tracks_fts WHERE path = old.path;
				INSERT INTO tracks_fts(path, title, artist, album) VALUES (
					new.path,
					COALESCE(json_extract(new.tags_json, '$.title'), ''),
					COALESCE(json_extract(new.tags_json, '$.artist'), ''),
					COALESCE(json_extract(new.tags_json, '$.album'), '')
				);
			END;
			`
			if _, err := db.Exec(ddl); err != nil {
				return fmt.Errorf("re-scope tracks_fts_au trigger: %w", err)
			}
			return nil
		},
	},
	{
		version: 9,
		name:    "upscale_batches.skipped_files (v1.5 inspector skip-reason)",
		// Additive column: persists the number of tracks that
		// `Coordinator.Submit` / `SubmitOptimize` saw in the
		// projection but did NOT enqueue (failed `OptimizeEligible`,
		// were DSD, had zero / missing rate-or-bits, source format
		// already at target, etc.). Distinct from `total_files`
		// (which equals the EnqueuedCount on the live row) and
		// from `failed_files` (per-job SoX failures during the run).
		//
		// Operators looking at the Jobs page row for a batch that
		// "completed 0/0" previously had no way to distinguish
		// "empty folder" from "every track ineligible" from "every
		// track already had a variant". The skip-count closes that
		// gap — the admin Jobs page surfaces "X tracks skipped" as
		// a sub-line on the row whenever this column is non-zero.
		//
		// SQLite `ALTER TABLE ADD COLUMN ... DEFAULT 0` is constant-
		// time (doesn't rewrite existing rows). Backfill is implicit:
		// pre-migration rows get 0, which is harmless — the UI only
		// renders the sub-line when `SkippedFiles > 0`, so legacy
		// rows look identical to the pre-feature shape.
		//
		// **Idempotency: ALTER lives in post(), not in `sql`.** Same
		// rationale as the v5 docblock: if a partial-apply scenario
		// commits the column but crashes before `user_version` is
		// bumped, the next boot retries — and `ALTER TABLE ADD COLUMN`
		// errors "duplicate column name" on a column that already
		// exists. Doing it in post() with error-tolerant
		// `_, _ = db.Exec(...)` makes the step re-runnable.
		sql: `-- column added in post() for idempotency; see migration v9 docblock`,
		post: func(db *sql.DB) error {
			// Idempotent ALTER — "duplicate column" is the expected
			// no-op when the prior run committed the column already.
			_, _ = db.Exec(`ALTER TABLE upscale_batches ADD COLUMN skipped_files INTEGER NOT NULL DEFAULT 0`)
			return nil
		},
	},
	{
		version: 10,
		name:    "device_registrations (cross-bridge backup/telemetry foundation)",
		// Per-device identity table backing playlist backup + playback
		// telemetry (both keyed on the client's durable, device-local
		// recovery token — NOT the ephemeral auth.Token.ID, which is
		// re-minted on every re-pairing). `device_token` is the iOS
		// Keychain recovery token (high-entropy hex, kSecAttrSynchronizable
		// =false). `token_id` is the auth token currently presenting that
		// device_token — updated on every authed request (self-healing
		// rebind) so a re-pair re-attaches prior backups without operator
		// intervention. `device_name` is best-effort: empty on the
		// header-path upsert (regular requests carry no name), populated
		// at pairing-approval time from the join request's deviceName.
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS device_registrations (
			device_token  TEXT PRIMARY KEY,
			token_id      TEXT NOT NULL,
			device_name   TEXT NOT NULL DEFAULT '',
			first_seen_at INTEGER NOT NULL,
			last_seen_at  INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_device_reg_token_id ON device_registrations(token_id);
		`,
	},
	{
		version: 11,
		name:    "playlists + playlist_items (cross-bridge backup safe)",
		// Per-device playlist backups. The bridge is a safe, NOT a player:
		// a playlist may mix tracks from several bridges + local/SMB, and
		// foreign items are stored as OPAQUE references the bridge never
		// resolves or serves — iOS re-resolves them locally on restore.
		//
		// `playlists.id` is the client's own stable UUID (lowercased),
		// scoped per device_token. `last_modified_at` is the client's
		// wall-clock UnixNano, used only as a backup-hygiene LWW guard (a
		// strictly-older PUT is bounced 409 with the server copy).
		// `deleted` is a tombstone so a delete propagates instead of a
		// silent reappear on the next backup sweep.
		//
		// playlist_items: each row is EITHER local (`path` set, resolvable
		// on this bridge) OR foreign (`origin_fingerprint`+`origin_path`,
		// where origin_fingerprint is the owning bridge's cert fp or a
		// 'local'/'smb' sentinel). `title`/`artist` are render fallback for
		// the admin surface. `position` is the authoritative 0-based order.
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS playlists (
			id               TEXT PRIMARY KEY,
			device_token     TEXT NOT NULL,
			name             TEXT NOT NULL,
			last_modified_at INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL,
			deleted          INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_playlists_device ON playlists(device_token);

		CREATE TABLE IF NOT EXISTS playlist_items (
			playlist_id        TEXT NOT NULL,
			position           INTEGER NOT NULL,
			path               TEXT,
			origin_fingerprint TEXT,
			origin_path        TEXT,
			title              TEXT,
			artist             TEXT,
			PRIMARY KEY (playlist_id, position),
			FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE
		);
		`,
	},
	{
		version: 12,
		name:    "playback_history (opt-in, owner-visible telemetry)",
		// Per-device playback telemetry, scoped to the device_token. Owner-
		// visible only (loopback admin) — never exposed off-host. Opt-in on
		// the iOS side; events queue offline-first there and drain in
		// batches to POST /v1/history/batch.
		//
		// `duration_used` is REAL (seconds actually listened) — scanned into
		// a Go float64, never an int, so fractional skip seconds survive.
		// `started_at` / `received_at` are UnixNano. `is_dop` is 0/1.
		// AUTOINCREMENT id keeps a stable cursor for paginated admin reads.
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS playback_history (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			device_token   TEXT NOT NULL,
			path           TEXT NOT NULL,
			started_at     INTEGER NOT NULL,
			duration_used  REAL    NOT NULL,
			codec          TEXT,
			variant_id     TEXT,
			iface_type     TEXT,
			device_name    TEXT,
			output_rate    INTEGER,
			is_dop         INTEGER NOT NULL DEFAULT 0,
			received_at    INTEGER NOT NULL
		);
		-- Index matches ListHistory's WHERE device_token = ? ORDER BY id DESC
		-- cursor paging (id is monotonic with insert order); a started_at
		-- index would force a filesort there (Gemini on PR #336).
		CREATE INDEX IF NOT EXISTS idx_history_device_id ON playback_history(device_token, id DESC);
		CREATE INDEX IF NOT EXISTS idx_history_path ON playback_history(path);
		`,
	},
	{
		version: 13,
		name:    "upnp_track_routing (upstream MediaServer ingestion sidecar)",
		// Routing sidecar for tracks ingested from an upstream UPnP
		// MediaServer (e.g. the Chord 2Go's MiniDLNA). The wire `Track`
		// row in `tracks` is unchanged — its `path` is the LOAD-BEARING
		// STABLE identity derived from the upstream's filesystem-tree
		// view (Browse Folders), and trackID hashes on that. This
		// sidecar carries the VOLATILE locators the file proxy uses to
		// re-resolve bytes at fetch time: the server UDN, the
		// ContentDirectory ObjectID, and the last-known <res> URL.
		//
		// `source_path` PRIMARY KEY matches `tracks.path` exactly. The
		// FK ON DELETE CASCADE lets DeleteTrack / DeleteTracksByPrefix
		// reap the routing row alongside the track without an explicit
		// orphan-sweep. (SQLite enforces FKs only when
		// `PRAGMA foreign_keys = ON` — the bridge already runs with
		// foreign_keys enabled in store.go's OpenStore.)
		//
		// Index on (server_udn, last_seen_at) powers the per-server
		// reconcile sweep (tracks no longer seen in the current walk
		// generation drop from the manifest).
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS upnp_track_routing (
			source_path       TEXT PRIMARY KEY,
			server_udn        TEXT NOT NULL,
			object_id         TEXT NOT NULL,
			parent_object_id  TEXT NOT NULL DEFAULT '',
			res_url           TEXT NOT NULL,
			protocol_info     TEXT NOT NULL DEFAULT '',
			last_seen_at      INTEGER NOT NULL,
			FOREIGN KEY (source_path) REFERENCES tracks(path) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_upnp_routing_server_seen
			ON upnp_track_routing(server_udn, last_seen_at);
		`,
	},
	{
		version: 14,
		name:    "track_analysis (offline audio analysis sidecars)",
		// Per-track offline audio-analysis results produced by
		// `bridge analyze`. Phase 1 carries only the waveform sidecar
		// pointer + content tag; a later phase ALTERs this table to add
		// signal-derived scalars (integrated LUFS / ReplayGain / key /
		// tempo) — keep new columns additive with DEFAULTs so the ladder
		// stays append-only and idempotent.
		//
		// `source_path` PRIMARY KEY matches `tracks.path` exactly. FK ON
		// DELETE CASCADE reaps the row when the parent track is deleted;
		// the on-disk waveform FILE is unlinked explicitly by DeleteTrack
		// / DeleteTracksByPrefix / WipeAllTracks (CASCADE drops the row
		// but not the sidecar — same contract as track_variants).
		//
		// `waveform_path` is the absolute on-disk path (authoritative;
		// never recomputed at read time, so operators can relocate
		// <dataDir>/waveforms/). `waveform_tag` is 8 hex of the sidecar
		// bytes' SHA-256 — spliced onto Track.WaveformTag and used by iOS
		// as an immutable-cache key. `source_mtime_ns` + `source_size`
		// drive the scan-skip gate and the serving-time freshness check.
		// `schema_version` lets a future waveform-format change
		// invalidate prior sidecars without a migration.
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS track_analysis (
			source_path      TEXT PRIMARY KEY,
			waveform_path    TEXT NOT NULL DEFAULT '',
			waveform_tag     TEXT NOT NULL DEFAULT '',
			waveform_size    INTEGER NOT NULL DEFAULT 0,
			source_mtime_ns  INTEGER NOT NULL,
			source_size      INTEGER NOT NULL,
			schema_version   TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			FOREIGN KEY (source_path) REFERENCES tracks(path) ON DELETE CASCADE
		);
		`,
	},
	{
		version: 15,
		name:    "track_analysis unicode-lower lookup index",
		// LookupAnalysis falls back to
		// `unicode_lower(source_path) = unicode_lower(?)` for iOS-shaped
		// lowercase paths; v14 only has the PRIMARY KEY on source_path, so
		// that fold degrades to a table scan as analysis rows grow (25k+
		// on a large library). This functional index mirrors the v4
		// `idx_track_variants_source_path_unicode_lower` exactly — same
		// Go-registered deterministic `unicode_lower` scalar, same
		// build-once-on-first-query cost. Appended per the ladder
		// convention (v14 already shipped). (CodeRabbit on #395.)
		sql: `
		CREATE INDEX IF NOT EXISTS idx_track_analysis_source_path_unicode_lower
			ON track_analysis(unicode_lower(source_path));
		`,
	},
	{
		version: 16,
		name:    "track_analysis.replaygain_track_db (signal-derived loudness)",
		// First of the v14-anticipated signal-derived scalars: the EBU
		// R128 / ReplayGain 2.0 track gain (dB to the -18 LUFS reference)
		// computed by `bridge analyze` from a channel-aware decode.
		//
		// **NULLABLE on purpose** — NULL means "loudness not yet computed
		// for this row", DISTINCT from a real 0.0 dB gain. That sentinel
		// is load-bearing: the scan-skip gate re-analyzes a waveform-fresh
		// row whose loudness IS NULL (rows that predate this column), so
		// the existing 25k+ analyzed library backfills loudness on the
		// next pass without invalidating its waveforms. A NOT NULL DEFAULT
		// would erase that distinction and the backfill would never fire.
		//
		// Spliced onto Track.ReplayGainTrackDB at read time ONLY when the
		// source carries no ReplayGain tag (curated tags always win).
		//
		// **Idempotency: ALTER lives in post(), not in `sql`.** Same
		// rationale as the v9 docblock — a crash after the column commits
		// but before the user_version bump retries on next boot, and a
		// bare `ALTER ... ADD COLUMN` errors "duplicate column name".
		//
		// Swallow ONLY that specific error (the idempotent re-run) and
		// surface everything else: a lock / I/O / disk-full during the
		// ALTER must NOT be masked behind a bumped user_version + a
		// missing column (which would break every later query referencing
		// the column). This is the precise form of the bare
		// `_, _ = db.Exec(...)` used by v5/v9 — prefer it for new
		// migrations. (Gemini on #396.)
		sql: `-- column added in post() for idempotency; see migration v9 docblock`,
		post: func(db *sql.DB) error {
			if _, err := db.Exec(`ALTER TABLE track_analysis ADD COLUMN replaygain_track_db REAL`); err != nil &&
				!strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		version: 17,
		name:    "track_analysis.key_root/key_mode/bpm (signal-derived key + tempo)",
		// Estimated musical key (Krumhansl-Schmuckler) + tempo (onset
		// autocorrelation), the second batch of v14-anticipated signal-
		// derived scalars. All three NULLABLE — NULL = "not estimated"
		// (too little signal / pre-column row), the same backfill sentinel
		// as replaygain_track_db. key_root is 0..11 (C=0), key_mode is
		// "major"/"minor", bpm is integer. Spliced onto Track at read
		// time: key always (no tag source today), bpm only when the source
		// has no BPM tag.
		//
		// **Idempotency: ALTERs live in post()** with the same precise
		// "duplicate column name"-only swallow as v16 — each column is
		// independent, so a crash partway re-applies cleanly and a real
		// lock / I/O error surfaces instead of being masked.
		sql: `-- columns added in post() for idempotency; see migration v9/v16 docblock`,
		post: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE track_analysis ADD COLUMN key_root INTEGER`,
				`ALTER TABLE track_analysis ADD COLUMN key_mode TEXT`,
				`ALTER TABLE track_analysis ADD COLUMN bpm INTEGER`,
			} {
				if _, err := db.Exec(stmt); err != nil &&
					!strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		version: 18,
		name:    "smart_playlists (generated dynamic-feed cache)",
		// Precomputed cache of server-generated smart/dynamic playlists
		// (Heavy Rotation, Auto Mix, Forgotten Favorites, …), rebuilt by
		// the daily runSmartPlaylistRegenerator and served verbatim by
		// GET /v1/smart-playlists. **DISTINCT from the `playlists` table**:
		// that one is the LWW user-backup store (device-authored,
		// restore-on-reinstall, last-write-wins guard); this one is
		// ephemeral, derived, and REPLACED WHOLESALE each regeneration —
		// never device-authored, no LWW. Keeping them separate avoids
		// polluting the backup store with generated feeds.
		//
		// `slug` is the stable per-family id the homepage binds rows to.
		// `items_json` is the ordered item list — or, for the time-of-day
		// family, the per-UTC-hour pools the handler shifts to the
		// device's local hour — as a JSON blob: read whole, replaced
		// whole, never queried by item, so a blob (not a child table) is
		// the right shape (same rationale as tags_json). The table holds
		// only a handful of families so no index is needed.
		//
		// Append-only / idempotent per the ladder contract.
		sql: `
		CREATE TABLE IF NOT EXISTS smart_playlists (
			slug         TEXT    PRIMARY KEY,
			kind         TEXT    NOT NULL,
			title        TEXT    NOT NULL,
			subtitle     TEXT    NOT NULL DEFAULT '',
			position     INTEGER NOT NULL DEFAULT 0,
			refreshed_at INTEGER NOT NULL,
			items_json   BLOB    NOT NULL
		);
		`,
	},
	{
		version: 19,
		name:    "playlist_covers (operator-uploaded custom cover art)",
		// Operator-uploaded cover images for smart-mix families
		// (scope 'smartmix', key = family slug) and backed-up user
		// playlists (scope 'playlist', key = playlist id). The resized
		// JPEG bytes live on disk under <DataDir>/playlist-covers/; this
		// table maps a (scope,key) to the stored image's content hash +
		// extension so the wire DTO can advertise `imageHash` and iOS can
		// cache-bust on re-upload. Pruned per-row on playlist delete /
		// family retirement (no orphaned mappings — the on-disk JPEG is
		// removed alongside). Append-only / idempotent per the ladder.
		sql: `
		CREATE TABLE IF NOT EXISTS playlist_covers (
			scope      TEXT    NOT NULL,
			key        TEXT    NOT NULL,
			image_hash TEXT    NOT NULL,
			ext        TEXT    NOT NULL DEFAULT 'jpg',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (scope, key)
		);
		`,
	},
	{
		version: 20,
		name:    "smart_playlists.energy_json/modal_rate_hz (waveform-signed-cover halo data)",
		// Per-family energy envelope (a normalized 0..1 loudness contour
		// across member tracks) + the mix's modal sample rate, derived by the
		// regenerator and served on GET /v1/smart-playlists so iOS renders the
		// "waveform-signed cover" halo — a spline drawn from the mix's own
		// audio energy, glowing in its modal-rate Hugo-2 LED color — without a
		// second round-trip. energy_json is the JSON-encoded []float64 (read
		// whole, replaced whole, like items_json); an absent/empty value tells
		// the wire handler to fall back to the iOS seeded waveform.
		// modal_rate_hz 0 = "unknown rate" (fixed family color).
		//
		// **Idempotency: ALTERs live in post()** with the same precise
		// "duplicate column name"-only swallow as v16/v17.
		sql: `-- columns added in post() for idempotency; see migration v9/v16 docblock`,
		post: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE smart_playlists ADD COLUMN energy_json BLOB`,
				`ALTER TABLE smart_playlists ADD COLUMN modal_rate_hz INTEGER NOT NULL DEFAULT 0`,
			} {
				if _, err := db.Exec(stmt); err != nil &&
					!strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		version: 21,
		name:    "release_atlas + artist_atlas (Phase 2 rich-tier metadata cache)",
		// MBID-keyed caches for Atlas-sourced rich metadata pushed by the
		// closed-source iOS app via POST /v1/atlas-ingest and served back via
		// GET /v1/atlas-meta/{release,artist}/{mbid}. Standalone (NOT spliced
		// into tracks/tags_json) so a re-scan never wipes them and the streamed
		// manifest stays lean. found=0 is a TOMBSTONE (the app checked Atlas and
		// it had nothing) so obscure releases aren't re-queried every view.
		// genres_json is a JSON []string. ingested_at is UnixNano, bridge-
		// stamped on every upsert.
		sql: `
		CREATE TABLE IF NOT EXISTS release_atlas (
			release_mbid TEXT PRIMARY KEY,
			description  TEXT NOT NULL DEFAULT '',
			record_label TEXT NOT NULL DEFAULT '',
			genres_json  TEXT NOT NULL DEFAULT '[]',
			found        INTEGER NOT NULL DEFAULT 0,
			atlas_etag   TEXT NOT NULL DEFAULT '',
			ingested_at  INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS artist_atlas (
			artist_mbid TEXT PRIMARY KEY,
			bio         TEXT NOT NULL DEFAULT '',
			bio_summary TEXT NOT NULL DEFAULT '',
			genres_json TEXT NOT NULL DEFAULT '[]',
			found       INTEGER NOT NULL DEFAULT 0,
			atlas_etag  TEXT NOT NULL DEFAULT '',
			ingested_at INTEGER NOT NULL
		);
		`,
	},
	{
		version: 22,
		name:    "attribution columns on release_atlas + artist_atlas (Phase A4)",
		// Per-field attribution for the multi-source Atlas convergence: the
		// winning album description / artist bio carries the SOURCE it came from
		// (wiki / bandcamp / lastfm / tadb / qobuz) + that source's canonical URL,
		// so iOS can render "Read more on <source>" for CC-BY-SA / ToS compliance.
		// release: description_source(_url); artist: bio_source(_url). Both the
		// harvest sink (atlasHarvestSink → UpsertArtistAtlasMeta) and the ferry
		// ingest (POST /v1/atlas-ingest) populate them.
		//
		// **Idempotency: ALL ALTERs live in post(), not `sql`** — same contract as
		// migration v5: migrate() short-circuits on the first `sql` ExecContext
		// error, so a partial-apply (some columns committed, restart) must not
		// hit a non-swallowed "duplicate column". `sql` is a harmless comment;
		// post() does the real ALTERs with error-tolerant `_, _ = db.Exec(...)`.
		sql: `-- columns added in post() for idempotency; see migration v22 docblock`,
		post: func(db *sql.DB) error {
			// Idempotent AND error-surfacing — unlike the older swallow-all v5
			// form, add each column only when PRAGMA table_info shows it absent,
			// and propagate a real ALTER failure so migrate() does NOT advance
			// user_version over a partial schema (Gemini HIGH + CodeRabbit on PR
			// #410). The column-check form matters MORE here than for v5: the v5
			// scanner columns are written every pass (a missing one surfaces at
			// once), but these atlas columns are written only on a (possibly rare)
			// Atlas ingest, so a partial migration could sit silent for a long
			// time then break the first ingest with "no such column".
			for _, a := range []struct{ table, col string }{
				{"release_atlas", "description_source"},
				{"release_atlas", "description_source_url"},
				{"artist_atlas", "bio_source"},
				{"artist_atlas", "bio_source_url"},
			} {
				exists, err := atlasColumnExists(db, a.table, a.col)
				if err != nil {
					return fmt.Errorf("inspect %s.%s: %w", a.table, a.col, err)
				}
				if exists {
					continue
				}
				// Table/column names are compile-time constants — PRAGMA / ALTER
				// don't accept a bound identifier, so they're formatted in; no
				// user input reaches this path.
				if _, err := db.Exec("ALTER TABLE " + a.table + " ADD COLUMN " + a.col + " TEXT NOT NULL DEFAULT ''"); err != nil {
					return fmt.Errorf("add %s.%s: %w", a.table, a.col, err)
				}
			}
			return nil
		},
	},
	{
		version: 23,
		name:    "artwork_version column on tracks (cover cache-bust)",
		// A content marker for the cover served at /v1/artwork/{artworkMBID},
		// set when a premium cover is (re)fetched for a UUID MBID — whose URL
		// is stable while its bytes change (CAA → premium). The manifest
		// surfaces it (Track.ArtworkVersion) so iOS can invalidate its
		// albumID-keyed artwork cache when a cover upgrades; local-<sha256>
		// MBIDs already encode content so they leave it NULL. Column-only
		// (spliced at read, never in tags_json). Idempotent via post() + the
		// table_info check — same no-swallowed-ALTER contract as v22.
		sql: `-- column added in post() for idempotency; see migration v23 docblock`,
		post: func(db *sql.DB) error {
			exists, err := atlasColumnExists(db, "tracks", "artwork_version")
			if err != nil {
				return fmt.Errorf("inspect tracks.artwork_version: %w", err)
			}
			if exists {
				return nil
			}
			if _, err := db.Exec("ALTER TABLE tracks ADD COLUMN artwork_version TEXT"); err != nil {
				return fmt.Errorf("add tracks.artwork_version: %w", err)
			}
			return nil
		},
	},
	{
		version: 24,
		name:    "booklets table + release-mbid functional index + tracks.booklet_tag",
		// PDF album booklets (v1.8): per-release availability + fetch state
		// learned from Atlas via the harvest credential (booklets table), a
		// functional index on $.musicBrainzAlbumID so the whole-album
		// booklet_tag bump (SetBookletTagAndBumpIndex) is index-backed
		// instead of a table scan, and the column-only tracks.booklet_tag
		// wire marker (spliced at read like artwork_version, never in
		// tags_json). Index FIRST, all DDL idempotent (IF NOT EXISTS) per
		// the migration-ladder contract; the ADD COLUMN rides post() with
		// the same no-swallowed-ALTER shape as v22/v23.
		sql: `
			CREATE INDEX IF NOT EXISTS idx_tracks_release_mbid
				ON tracks(json_extract(tags_json, '$.musicBrainzAlbumID'));
			CREATE TABLE IF NOT EXISTS booklets (
				release_mbid   TEXT PRIMARY KEY,
				available      INTEGER NOT NULL DEFAULT 0,
				etag           TEXT NOT NULL DEFAULT '',
				bytes          INTEGER NOT NULL DEFAULT 0,
				check_attempts INTEGER NOT NULL DEFAULT 0,
				checked_at     INTEGER NOT NULL,
				fetched_at     INTEGER
			);
		`,
		post: func(db *sql.DB) error {
			exists, err := atlasColumnExists(db, "tracks", "booklet_tag")
			if err != nil {
				return fmt.Errorf("inspect tracks.booklet_tag: %w", err)
			}
			if exists {
				return nil
			}
			if _, err := db.Exec("ALTER TABLE tracks ADD COLUMN booklet_tag TEXT"); err != nil {
				return fmt.Errorf("add tracks.booklet_tag: %w", err)
			}
			return nil
		},
	},
	{
		// v25: format-fact columns on tracks (sample_rate /
		// bits_per_sample / is_dsd / codec), stamped at every upsert
		// and backfilled once from tags_json here.
		//
		// COLUMN-TRUTH CONTRACT: these are query ACCELERATORS only —
		// they let the admin inspector's coverage rollups evaluate
		// transcode-eligibility predicates with plain-column SQL
		// instead of json_extract on the browse hot path. Go readers
		// (GetTrack / ListTracks / every wire path) keep reading
		// tags_json; the columns are never spliced onto wire output
		// and MUST NOT gain json tags anywhere (same class as the
		// mtime_ns column gotcha — tags_json remains read-truth).
		//
		// The backfill touches ONLY the four new columns — never
		// enriched_at (the enricher's queue driver) or indexed_at
		// (the iOS delta-sync clock). One full-table UPDATE, one-time
		// at boot (seconds on a 50k-row library; transient WAL growth
		// of ~rowcount × tens of bytes).
		version: 25,
		name:    "tracks format-fact columns + backfill",
		sql:     `-- columns added idempotently in post(); see v25 docblock`,
		post: func(db *sql.DB) error {
			for _, a := range []struct{ col, typ string }{
				{"sample_rate", "INTEGER"},
				{"bits_per_sample", "INTEGER"},
				{"is_dsd", "INTEGER"},
				{"codec", "TEXT"},
			} {
				exists, err := atlasColumnExists(db, "tracks", a.col)
				if err != nil {
					return fmt.Errorf("inspect tracks.%s: %w", a.col, err)
				}
				if exists {
					continue
				}
				if _, err := db.Exec("ALTER TABLE tracks ADD COLUMN " + a.col + " " + a.typ); err != nil {
					return fmt.Errorf("add tracks.%s: %w", a.col, err)
				}
			}
			return backfillFormatColumns(db)
		},
	},
	{
		// v26: rebuild every functional index embedding unicode_lower.
		// unicode_lower() now NFC-composes its output (see
		// internal/manifest/sqlfunc.go; 2026-07-21 review, M9) so iOS's
		// NFC-normalised lookup shape matches paths the scanner stored
		// in NFD (files migrated from HFS+ or synced from Linux/NAS
		// onto a Mac). Functional indexes persist the function's output
		// at build time, so an index built by the pre-NFC function
		// keeps NFD-keyed entries for those rows and the new function
		// alone can't fix the lookup — the indexes must be rebuilt.
		//
		// All THREE indexes that embed unicode_lower are rebuilt: the
		// two v4 ones the review called out (tracks.path,
		// track_variants.source_path) plus the v15
		// track_analysis.source_path index, which shares the miss
		// class (LookupAnalysis on an accented NFD path).
		//
		// DROP-then-CREATE (same shape as v4) rather than REINDEX:
		// both rebuild from current data, but the ladder's established
		// idiom re-applies cleanly if a crash lands between statements.
		// **Index-build cost**: same order as v4/v15 — a few hundred ms
		// on a 50k-track library, paid once at first launch after
		// upgrade.
		version: 26,
		name:    "rebuild unicode_lower indexes for NFC composition",
		sql: `
		DROP INDEX IF EXISTS idx_tracks_path_unicode_lower;
		DROP INDEX IF EXISTS idx_track_variants_source_path_unicode_lower;
		DROP INDEX IF EXISTS idx_track_analysis_source_path_unicode_lower;
		CREATE INDEX IF NOT EXISTS idx_tracks_path_unicode_lower
			ON tracks(unicode_lower(path));
		CREATE INDEX IF NOT EXISTS idx_track_variants_source_path_unicode_lower
			ON track_variants(unicode_lower(source_path), variant_id);
		CREATE INDEX IF NOT EXISTS idx_track_analysis_source_path_unicode_lower
			ON track_analysis(unicode_lower(source_path));
		`,
	},
	{
		// v27: per-track extractor-version stamp for self-healing tag
		// re-extraction. `extractor_version` records the
		// manifest.ExtractorVersion that produced the row's tags; the
		// scan-skip gate (scanner.go) re-extracts any row whose stamp is
		// < the current constant, so a tag-extraction fix (e.g. the MP4 ©
		// atom canonicalization that recovers M4A year / composer /
		// multi-value artist) self-applies on the next scan with no
		// explicit backfill. NOT NULL DEFAULT 0 is a constant-time ADD
		// COLUMN; every existing row reads 0, which is < ExtractorVersion
		// (>= 1), so the first post-upgrade scan re-extracts the whole
		// library once, then size+mtime skips resume. Mirrors the
		// analyze.WaveformSchemaVersion stamp idiom.
		version: 27,
		name:    "tracks extractor_version stamp",
		sql:     `-- column added idempotently in post(); see v27 docblock`,
		post: func(db *sql.DB) error {
			exists, err := atlasColumnExists(db, "tracks", "extractor_version")
			if err != nil {
				return fmt.Errorf("inspect tracks.extractor_version: %w", err)
			}
			if exists {
				return nil
			}
			if _, err := db.Exec("ALTER TABLE tracks ADD COLUMN extractor_version INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("add tracks.extractor_version: %w", err)
			}
			return nil
		},
	},
	{
		// v28: provenance for MBIDs written by the acoustic-fingerprinting
		// fallback. Holds the AcoustID cluster ID that produced the write.
		//
		// COLUMN ONLY. It must never gain a `json:` tag and must never be
		// spliced onto wire output — same rule as the v25 format-fact
		// columns. That keeps it off the protocol entirely: no
		// ProtocolVersion bump, no PROTOCOL.md change, no iOS mirror.
		//
		// It exists because a fingerprint match has a residual error rate
		// that text matching does not. Without provenance, an MBID written
		// from audio is indistinguishable from one written from tags,
		// forever — so there is no way to audit the feature's output or undo
		// it selectively. Presence alone gives a one-statement undo; the
		// value lets a later pass re-check a link against AcoustID once
		// upstream corrections land.
		version: 28,
		name:    "tracks acoustid_match provenance",
		sql:     `-- column added idempotently in post(); see v28 docblock`,
		post: func(db *sql.DB) error {
			exists, err := atlasColumnExists(db, "tracks", "acoustid_match")
			if err != nil {
				return fmt.Errorf("inspect tracks.acoustid_match: %w", err)
			}
			if exists {
				return nil
			}
			if _, err := db.Exec("ALTER TABLE tracks ADD COLUMN acoustid_match TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("add tracks.acoustid_match: %w", err)
			}
			return nil
		},
	},
	{
		// v29: the wf4 track-quality scalars — true peak (dB, of the
		// 48 kHz analysis rendering, BS.1770-style 4x oversampled), the
		// community DR score, and the FLAC audio-MD5 verification state
		// ("verified"/"mismatch", NULL = not verifiable / not FLAC).
		// Additive with NULL defaults per the v14 docblock's contract;
		// backfill is driven by the WaveformSchemaVersion wf3→wf4 bump
		// (the scan-skip gate re-enqueues every stale-stamped row), not
		// by a NULL sentinel. Uses the v28 atlasColumnExists pre-check
		// form rather than the older duplicate-column error swallow.
		version: 29,
		name:    "track_analysis quality scalars (true peak, DR, audio MD5)",
		sql:     `-- columns added idempotently in post(); see v29 docblock`,
		post: func(db *sql.DB) error {
			for _, col := range []struct{ name, ddl string }{
				{"true_peak_db", "ALTER TABLE track_analysis ADD COLUMN true_peak_db REAL"},
				{"dr_score", "ALTER TABLE track_analysis ADD COLUMN dr_score INTEGER"},
				{"audio_md5_state", "ALTER TABLE track_analysis ADD COLUMN audio_md5_state TEXT"},
			} {
				exists, err := atlasColumnExists(db, "track_analysis", col.name)
				if err != nil {
					return fmt.Errorf("inspect track_analysis.%s: %w", col.name, err)
				}
				if exists {
					continue
				}
				if _, err := db.Exec(col.ddl); err != nil {
					return fmt.Errorf("add track_analysis.%s: %w", col.name, err)
				}
			}
			return nil
		},
	},
	{
		version: 30,
		name:    "track_analysis audio_md5_attempts (bounded transient retry)",
		sql:     `-- column added idempotently in post(); see the comment below`,
		// v30 records how many times the FLAC audio-MD5 pass has failed
		// for a reason that says nothing about the file — a pipe or
		// spawn failure under load, a faulted read on a network mount,
		// a killed child.
		//
		// It exists because a "" verdict is committed WITH the schema
		// stamp, so the scan-skip gate never revisits the row: pre-v30
		// a one-second I/O blip permanently recorded a healthy file as
		// unverifiable, indistinguishable from one that genuinely
		// carries no checksum.
		//
		// A counter rather than a plain retry flag so the retry is
		// BOUNDED. Each retry costs a full re-analysis (the pipeline is
		// one decode, not a resumable stage), and an unbounded one
		// would re-decode every FLAC in the library on every sweep for
		// as long as the condition lasted — trading a wrong scalar for
		// an hourly treadmill.
		//
		// The one-time backfill is the load-bearing part, and DEFAULT 0
		// alone is WRONG for it. The verification pass is FLAC-only, so
		// every MP3 / M4A / WAV / AIFF row also carries an empty
		// verdict — at 0 they all read as "worth another attempt", and
		// since each attempt is a full re-analysis the migration would
		// quietly re-decode most of the library. That is a schema-
		// version bump wearing a different hat.
		//
		// So non-FLAC rows are written straight to the cap: the pass
		// cannot run on them, and asking again would never change that.
		// FLAC rows keep 0 and get their one bounded round, which is
		// the population this feature is actually for. It self-limits —
		// a row that verifies clears to 0, one that cannot be verified
		// is written straight to the cap — so the cost is one extra
		// decode per FLAC currently carrying an empty verdict, once.
		//
		// `lower(...) LIKE` rather than a bare LIKE for the extension
		// match: SQLite's LIKE already folds ASCII case here, but the
		// explicit lower() matches AnalysisCoverage's neighbouring
		// predicates and does not rely on `case_sensitive_like` staying
		// at its default. Getting this backwards is safe in the
		// expensive direction only — a missed row retries once more
		// than needed; a wrongly-capped row silently loses its heal.
		post: func(db *sql.DB) error {
			exists, err := atlasColumnExists(db, "track_analysis", "audio_md5_attempts")
			if err != nil {
				return fmt.Errorf("inspect track_analysis.audio_md5_attempts: %w", err)
			}
			if exists {
				return nil
			}
			if _, err := db.Exec(
				`ALTER TABLE track_analysis ADD COLUMN audio_md5_attempts INTEGER NOT NULL DEFAULT 0`,
			); err != nil {
				return fmt.Errorf("add track_analysis.audio_md5_attempts: %w", err)
			}
			if _, err := db.Exec(
				`UPDATE track_analysis SET audio_md5_attempts = ?
				  WHERE lower(source_path) NOT LIKE '%.flac'`,
				AudioMD5MaxAttempts,
			); err != nil {
				return fmt.Errorf("backfill track_analysis.audio_md5_attempts: %w", err)
			}
			return nil
		},
	},
}

// backfillFormatColumns derives the v25 format-fact columns from
// tags_json for every existing row. Idempotent (re-running recomputes
// the same values); runs inside the bridge's own connection so the
// v4 expression-index function (unicode_lower) is registered. CAST
// keeps SQLite's dynamic typing honest for the numeric columns —
// tags_json carries sampleRate as a JSON number (Go *float64); a
// missing key extracts as NULL and CAST(NULL) stays NULL, preserving
// "unknown". JSON booleans extract as integer 1/0. Keys are
// camelCase and case-sensitive (matches the existing $.artworkMBID
// json_extract usage across the store).
func backfillFormatColumns(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE tracks SET
			sample_rate     = CAST(json_extract(tags_json, '$.sampleRate')    AS INTEGER),
			bits_per_sample = CAST(json_extract(tags_json, '$.bitsPerSample') AS INTEGER),
			is_dsd          = json_extract(tags_json, '$.isDSD'),
			codec           = json_extract(tags_json, '$.codec')`)
	if err != nil {
		return fmt.Errorf("backfill format columns: %w", err)
	}
	return nil
}

// atlasColumnExists reports whether `table` already has a column named `col`,
// via PRAGMA table_info. The v22 migration uses it to make its ADD COLUMNs
// idempotent WITHOUT swallowing non-duplicate ALTER errors (a real failure must
// not let migrate() advance user_version over a partial schema). `table` is a
// compile-time constant — PRAGMA doesn't accept a bound identifier.
func atlasColumnExists(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// NOTE (r1 review #49, dropped): a covering index on
// track_variants(variant_id, source_path, size_bytes) was considered to
// speed up RollupByPrefix("")'s full-table aggregate. It was dropped
// because SQLite's planner then prefers it for the variant-LOOKUP hot
// path (/v1/download?variant=, which filters variant_id=?) over the
// more-selective unicode_lower(source_path) index — a net regression on
// the media-serving path for a marginal gain on a loopback-admin
// dashboard query. Pinned by TestUnicodeLowerVariantIndexIsSelected.

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
	ctx := context.Background() // migrations run during OpenStore — no caller ctx.
	var current int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if _, err := s.db.ExecContext(ctx, m.sql); err != nil {
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
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			return fmt.Errorf("set user_version to %d: %w", m.version, err)
		}
	}
	return nil
}

// UnenrichedTracks returns up to limit tracks that haven't been through the
// MusicBrainz/CoverArt pass. Used by internal/enrich.
//
// Ordering is newest-`indexed_at`-first (LIFO), so an album dropped into a
// steady-state library enriches ahead of the older backlog instead of waiting
// behind it alphabetically — the single-goroutine worker's real ceiling is the
// upstream rate limits, not CPU, so which rows drain first is the only lever on
// perceived responsiveness. `path ASC` is the deterministic tie-break: an
// UpsertTrackBatch flush stamps one `indexed_at` for the whole batch, so ties
// are common and would otherwise permute nondeterministically. On a *fresh*
// full scan every row shares ~one `indexed_at`, so the order is a near-no-op
// there (you're waiting for everything anyway) — the win is precisely the
// add-one-album-to-a-big-library case.
//
// The sort+LIMIT is isolated into a subquery that selects only `path`, so
// SQLite's sorter materializes the lightweight (path, indexed_at) keys of the
// unenriched pool — NOT the heavy `tags_json` BLOB living in each row's B-tree
// leaf — and the outer query then reads `tags_json` for just the ~limit winning
// paths via primary-key lookups. On a heavy fresh backlog (every row
// `enriched_at = 0`) that's a 50k-lightweight-key sort + ~100 blob reads instead
// of pulling 50k blobs through the sorter, which matters on slow flash
// (Pi/SD-card) during the 15s poll. The sort itself is unavoidable (finding the
// newest-N needs it); a composite `(enriched_at, indexed_at)` index would make
// it a bounded index-range scan, but it costs write-amplification on every track
// write for a gain the path-only subquery already largely captures, so it's left
// off. (Gemini review on PR #490.)
func (s *Store) UnenrichedTracks(ctx context.Context, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tags_json FROM tracks
		WHERE path IN (
			SELECT path FROM tracks
			WHERE enriched_at = 0
			ORDER BY indexed_at DESC, path ASC
			LIMIT ?
		)
		ORDER BY indexed_at DESC, path ASC
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Pre-size to the LIMIT (normalized above): the loop appends at most
	// `limit` rows, so this avoids the append-growth reallocations. Cap the
	// hint so a caller passing a huge limit can't pre-allocate a giant
	// backing array when few rows are actually unenriched (BatchLimit is
	// caller-controlled, only floored at <=0); append still grows past the
	// cap in the rare large-batch case.
	capHint := limit
	if capHint > 1000 {
		capHint = 1000
	}
	out := make([]Track, 0, capHint)
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
// formatColumnBinds returns SQL-nullable binds for the v25 format-fact
// columns (sample_rate / bits_per_sample / is_dsd / codec), derived
// from the Track's own fields. Untouched values stay nil → SQL NULL,
// preserving "unknown" for rows whose extractor couldn't determine
// geometry. Pointer fields are nil-checked BEFORE dereference —
// Track.SampleRate is *float64 and a blind deref would panic on
// geometry-less rows (dhowden fallback paths leave them populated,
// but the WAV/AIFF walkers and hand-built test fixtures may not).
// Shared by UpsertTrack and UpsertTrackBatch so the two write paths
// can't drift.
func formatColumnBinds(t *Track) (rate, bits, isDSD, codec any) {
	if t.SampleRate != nil {
		rate = int64(*t.SampleRate)
	}
	if t.BitsPerSample != nil {
		bits = int64(*t.BitsPerSample)
	}
	if t.IsDSD != nil {
		isDSD = boolToInt(*t.IsDSD)
	}
	if t.Codec != "" {
		codec = t.Codec
	}
	return rate, bits, isDSD, codec
}

func marshalForStorage(t *Track) ([]byte, error) {
	clone := *t
	clone.Enriched = nil
	// Variants are column-derived (spliced from the variants table at
	// read time via scanTrackVariants, same as Enriched). Zero them so a
	// caller that round-trips a ListTracks Track back through a write
	// path — e.g. ApplyAlbumArtistReconciliation — can't freeze stale
	// variants into tags_json, where the JSON-only readers (GetTrack /
	// UnenrichedTracks) would then return them.
	clone.Variants = nil
	// WaveformTag is column-derived (spliced from track_analysis at
	// read time, same as Enriched / Variants). Zero it before the
	// blob marshal so a caller that round-trips a read Track back
	// through a write path can't leak the spliced value into
	// tags_json, where the JSON-only readers (GetTrack /
	// UnenrichedTracks) would then return a stale tag contradicting
	// the track_analysis column.
	clone.WaveformTag = ""
	// ArtworkVersion is column-derived (spliced from the artwork_version column
	// at read time, same as Enriched / WaveformTag) and set ONLY by the
	// premium-cover refetch path. Zero it before the blob marshal so a caller
	// that round-trips a read Track back through a write path can't freeze the
	// spliced value into tags_json (where the JSON-only readers would surface a
	// stale version) AND can't have UpsertTrack's tags_json clobber the column.
	clone.ArtworkVersion = ""
	// BookletTag is column-derived exactly like ArtworkVersion (spliced from
	// the booklet_tag column at read time, set only by the booklet
	// availability loop) — same zero-before-marshal contract.
	clone.BookletTag = ""
	// ReplayGainTrackDB is DUAL-source: a curated tag (the scanner
	// extracted it — must persist) OR an analysis splice (must NOT
	// persist, else a round-tripped read Track freezes the analysis value
	// into tags_json as a faux curated tag that wins over future analysis
	// recomputes/deletes). Scrub ONLY the analysis-derived case, flagged
	// by spliceAnalysisReplayGain. Unlike Enriched / WaveformTag (always
	// column-derived, zeroed unconditionally), this one is conditional.
	if clone.replayGainFromAnalysis {
		clone.ReplayGainTrackDB = nil
	}
	// KeyRoot/KeyMode are analysis-only (no curated tag source today), so
	// — like WaveformTag — zero them unconditionally so a round-tripped
	// read Track never freezes them into tags_json. BPM is dual-source
	// like ReplayGain: scrub ONLY the analysis-derived case.
	clone.KeyRoot = nil
	clone.KeyMode = ""
	if clone.bpmFromAnalysis {
		clone.BPM = nil
	}
	// The wf4 quality scalars are analysis-only like KeyRoot/KeyMode —
	// zero unconditionally so a round-tripped read Track never freezes
	// them into tags_json.
	clone.TruePeakDB = nil
	clone.DRScore = nil
	clone.AudioMD5State = ""
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
func (s *Store) MarkEnriched(ctx context.Context, t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// `indexed_at` MUST bump on every enrichment write — without it,
	// iOS delta-sync (`WHERE indexed_at > since`) silently drops
	// enriched rows from incremental manifest fetches. The track was
	// already in the manifest at its UpsertTrack-time indexed_at, so
	// iOS's last sync `since` is later than that, and the freshly-
	// enriched tags_json would otherwise never surface until a full
	// manifest re-pull. Gemini Medium on PR #215 caught this.
	//
	// CASE-WHEN strict-advance pattern mirrors UpsertVariant /
	// DeleteVariant: a same-nanosecond clock (test-injected fakes,
	// low-res wall clocks, rapid back-to-back enrichment writes) still
	// produces a strictly-greater indexed_at, keeping delta-sync's
	// `> since` boundary semantically correct.
	//
	// The v25 format-fact columns are deliberately NOT re-stamped here:
	// enrichment only adds MBIDs / artwork refs to tags_json — it never
	// changes sampleRate / bitsPerSample / isDSD / codec, so the columns
	// stamped at Upsert time can't drift.
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx, `
		UPDATE tracks
		SET tags_json = ?,
		    enriched_at = ?,
		    indexed_at = CASE
		        WHEN indexed_at >= ? THEN indexed_at + 1
		        ELSE ?
		    END
		WHERE path = ?
	`, raw, now, now, now, t.Path)
	return err
}

// enrichmentMissPredicateSQL is the "this row is missing something the enricher
// was supposed to fill" test, shared by ResetEnrichedMisses and its
// folder-scoped twin ResetEnrichedMissesUnderPrefix so the two cannot drift.
//
// COALESCE-to-” folds JSON-null/absent and explicit-empty into one predicate —
// Track MBID fields are omitempty so absent is the normal shape, but the
// Distinct*MBIDs enumerators guard != ” for the same defensive reason
// (CodeRabbit + Gemini on PR #495).
//
// The musicBrainzAlbumID arm is NOT redundant with artworkMBID. artworkMBID
// also carries the scanner's `local-<sha256>` sentinel for embedded APIC /
// folder.jpg art, so a track whose album never resolved on MusicBrainz but
// which HAS local cover art reads as "not missing" on the artwork arm while
// still having no release MBID at all — and therefore no Atlas description,
// label, genres, booklet or premium cover, all of which key on it.
//
// Measured on the production bridge when the arm was added (2026-07-29): 8,945
// of 19,482 tracks had no album MBID, and 6,801 of those — every one via a
// local- sentinel — were invisible to the two-arm predicate. The operator
// pressed "Retry missing" and 76% of the affected rows silently stayed put.
//
// Still scoped to rows that actually miss data, so a full-library MB/CAA
// re-crawl is never triggered: on that same library this selects 46% of rows,
// and the enricher's album/artist LRU caches collapse them to a few hundred
// distinct upstream queries.
// It is spelled out VERBATIM inside each statement below rather than
// concatenated into them. A `const stmt = "…" + predicate` form is folded at
// compile time and is exactly as safe, but it still reads as an assembled query
// — to a reviewer and to SonarCloud's go:S2077 alike — and the whole statement
// is easier to read in one piece anyway. TestEnrichmentMissPredicateIsShared
// asserts both statements embed this text byte-for-byte, so the copies cannot
// drift; that test is the thing keeping them honest, not the concatenation.
const enrichmentMissPredicateSQL = `(COALESCE(json_extract(tags_json, '$.artworkMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.artistMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.musicBrainzAlbumID'), '') = '')`

// resetEnrichedMissesSQL is the library-wide "Retry missing" statement.
const resetEnrichedMissesSQL = `
		UPDATE tracks SET enriched_at = 0
		 WHERE enriched_at > 0
		   AND (COALESCE(json_extract(tags_json, '$.artworkMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.artistMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.musicBrainzAlbumID'), '') = '')`

// resetEnrichedMissesUnderPrefixSQL is its folder-scoped twin. Takes the
// subtree base (slash-free) as both bind parameters.
//
// Byte-range, not LIKE: SQLite's default LIKE folds ASCII case, so the
// folder-scoped "Retry metadata" button also re-queued a case-twin
// sibling folder's tracks. This is one of the few sanctioned
// `enriched_at` writers and its whole justification is that it is
// tightly scoped — a predicate that silently covers a folder the
// operator did not select sends real MB/CAA/Deezer traffic for it.
const resetEnrichedMissesUnderPrefixSQL = `
		UPDATE tracks SET enriched_at = 0
		 WHERE enriched_at > 0
		   AND path COLLATE BINARY >= ? || '/'
		   AND path COLLATE BINARY < ? || '0'
		   AND (COALESCE(json_extract(tags_json, '$.artworkMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.artistMBID'), '') = ''
		     OR COALESCE(json_extract(tags_json, '$.musicBrainzAlbumID'), '') = '')`

// enrichmentBreakdownSQL backs the dashboard's enrichment card. `matched` is
// the NEGATION of the same miss-predicate the two statements above use, so the
// card's "missing" count is exactly what the "Retry missing" button re-queues —
// see EnrichmentBreakdown for why that equality is load-bearing.
const enrichmentBreakdownSQL = `
		SELECT
			(SELECT COUNT(*) FROM tracks WHERE enriched_at = 0),
			(SELECT COUNT(*) FROM tracks WHERE enriched_at > 0
				AND NOT (COALESCE(json_extract(tags_json, '$.artworkMBID'), '') = ''
				      OR COALESCE(json_extract(tags_json, '$.artistMBID'), '') = ''
				      OR COALESCE(json_extract(tags_json, '$.musicBrainzAlbumID'), '') = '')),
			(SELECT COUNT(*) FROM tracks),
			(SELECT MAX(enriched_at) FROM tracks)`

// ResetEnrichedMisses re-queues every track the enricher finished WITHOUT a
// full result — enriched (enriched_at > 0) but still missing its release
// artwork MBID, its artist MBID, or its release MBID — by resetting
// enriched_at to 0 so the worker's `WHERE enriched_at = 0` query picks it up
// again. This is the operator-triggered "Retry missing" reset behind
// POST /api/enrichment/retry: the third sanctioned enriched_at writer alongside
// the upsert reset and MarkEnriched (see CLAUDE.md "enriched_at monotonicity").
// It is semantically the documented manual `UPDATE tracks SET enriched_at = 0`
// recipe, scoped to the rows that actually miss data (see
// enrichmentMissPredicateSQL).
//
// indexed_at is deliberately NOT bumped — nothing about the row's content
// changed yet; MarkEnriched bumps it when the retry actually lands new data.
// UPnP-routed rows flow through the enricher like filesystem rows (the worker
// query has no routing anti-join), so no exclusion is needed here either.
// Holds s.mu (writer contract). Returns the number of rows re-queued.
func (s *Store) ResetEnrichedMisses(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, resetEnrichedMissesSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetEnrichedByArtistMBIDs re-queues every enriched track whose artistMBID
// is in the given set. The admin "Retry missing" handler uses it for the
// facet ResetEnrichedMisses can't express in SQL: artistMBID resolved but the
// cached artist image file is missing on disk (the have/missing set is
// computed in Go from the artwork cache dir). Re-running the enricher on
// those rows re-fetches the Deezer artist image; the album-level caches make
// the MB half of the re-run cheap. Same enriched_at-writer sanction and
// no-indexed_at-bump contract as ResetEnrichedMisses.
//
// The set is passed as ONE bound JSON-array parameter consumed via
// json_each — a single static statement with no placeholder construction
// (no S2077 surface) and no bind-variable-ceiling chunking. Holds s.mu.
func (s *Store) ResetEnrichedByArtistMBIDs(ctx context.Context, mbids []string) (int64, error) {
	if len(mbids) == 0 {
		return 0, nil
	}
	blob, err := json.Marshal(mbids)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks SET enriched_at = 0
		 WHERE enriched_at > 0
		   AND json_extract(tags_json, '$.artistMBID') IN (SELECT value FROM json_each(?))
	`, string(blob))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ApplyAlbumArtistReconciliation rewrites tags_json for each supplied
// track (its AlbumArtist already set to the reconciled value) and bumps
// indexed_at so iOS delta-sync surfaces the change. enriched_at is
// deliberately LEFT UNTOUCHED — reconciliation is a metadata-consistency
// pass, NOT (re-)enrichment, so it must neither reset the enricher's
// progress (which would re-trigger MusicBrainz lookups) nor mark
// unenriched rows done. DB-only; no network.
//
// One transaction + one reused prepared statement, mirroring
// UpsertTrackBatch. Holds s.mu for the writer contract. indexed_at uses
// the same strict-advance CASE WHEN form as MarkEnriched so a
// same-nanosecond clock still produces a strictly-greater value, keeping
// delta-sync's `> since` boundary correct. Returns the number of rows
// actually updated.
func (s *Store) ApplyAlbumArtistReconciliation(ctx context.Context, changed []Track) (int, error) {
	return s.applyReconciledTracks(ctx, changed)
}

// ApplyYearReconciliation persists year-reconciled tracks (the fill-missing
// pass in reconcileYears). Identical contract to
// ApplyAlbumArtistReconciliation: rewrites tags_json, strict-advances
// indexed_at so iOS delta-sync surfaces the change, and leaves enriched_at
// untouched (reconciliation is a metadata-consistency pass, not
// re-enrichment).
func (s *Store) ApplyYearReconciliation(ctx context.Context, changed []Track) (int, error) {
	return s.applyReconciledTracks(ctx, changed)
}

// ApplyTrackNumberReconciliation persists track-number-backfilled tracks (the
// per-file fill in backfillTrackNumbersFromPath). Identical contract to
// ApplyYearReconciliation: rewrites tags_json, strict-advances indexed_at so
// iOS delta-sync surfaces the new track number, and leaves enriched_at
// untouched (a metadata-consistency pass, not re-enrichment).
func (s *Store) ApplyTrackNumberReconciliation(ctx context.Context, changed []Track) (int, error) {
	return s.applyReconciledTracks(ctx, changed)
}

// ApplyAlbumTitleReconciliation persists album-title-reconciled tracks (the
// folder-name-garbage rewrite in reconcileAlbumTitles). Identical contract to
// ApplyAlbumArtistReconciliation: rewrites tags_json in one transaction,
// strict-advances indexed_at so iOS delta-sync surfaces the change, and leaves
// enriched_at untouched (a metadata-consistency pass, not re-enrichment).
func (s *Store) ApplyAlbumTitleReconciliation(ctx context.Context, changed []Track) (int, error) {
	return s.applyReconciledTracks(ctx, changed)
}

// applyReconciledTracks is the shared writer behind the post-scan
// metadata-reconciliation passes (AlbumArtist, Year, TrackNumber). See
// ApplyAlbumArtistReconciliation's docblock above for the full invariants.
func (s *Store) applyReconciledTracks(ctx context.Context, changed []Track) (int, error) {
	if len(changed) == 0 {
		return 0, nil
	}
	// Pre-marshal every row OUTSIDE s.mu so the writer mutex only covers the
	// actual SQLite writes — mirrors UpsertTrackBatch (Gemini on PR #71). On a
	// split-album-heavy first reconcile `changed` can be thousands of rows;
	// marshaling under the lock would block the enricher/scanner writers for
	// the whole batch. A marshal error returns before any lock/tx is taken
	// (fails safe — strictly cleaner than the prior rollback-on-marshal path).
	// `changed` is freshly built by loadAndApplyReconciled from per-row
	// GetTrack results and isn't shared with the enricher/scanner goroutines,
	// so reading it unlocked here is safe.
	type reconciledRow struct {
		path string
		raw  []byte
	}
	rows := make([]reconciledRow, len(changed))
	for i := range changed {
		raw, err := marshalForStorage(&changed[i])
		if err != nil {
			return 0, err
		}
		rows[i] = reconciledRow{path: changed[i].Path, raw: raw}
	}
	// A scan cancelled mid-reconcile shouldn't acquire the writer mutex and
	// open a write transaction after the (now-unlocked) marshal loop.
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// The v25 format-fact columns are deliberately NOT re-stamped here:
	// reconciliation edits AlbumArtist / Year only — never the format
	// fields — so the columns stamped at Upsert time can't drift.
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE tracks
		SET tags_json = ?,
		    indexed_at = CASE
		        WHEN indexed_at >= ? THEN indexed_at + 1
		        ELSE ?
		    END
		WHERE path = ?
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now := s.now().UnixNano()
	n := 0
	for _, r := range rows {
		res, err := stmt.ExecContext(ctx, r.raw, now, now, r.path)
		if err != nil {
			return 0, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err // rollback on failed commit → nothing persisted.
	}
	return n, nil
}

// StampExtractorVersionBatch advances `extractor_version` to the current
// build's constant for the given paths WITHOUT touching `indexed_at`,
// `enriched_at`, or `tags_json` — the store half of the version-stale
// diff-guard (scanner.go `reExtractUnchanged`): a re-extraction whose
// merged result is byte-identical to the stored row must not surface in
// the iOS delta (`indexed_at` is the delta watermark), must not re-queue
// enrichment (`enriched_at`), and must not rewrite the blob. Deliberately
// NOT an `enriched_at` writer, so it stays outside that column's
// sanctioned-writers set. `missing_count = 0` preserves the "seen this
// scan" resilience contract the fast-skip path maintains via
// ResetTrackMissingCount.
//
// Holds `s.mu` per the writer contract on Store; one transaction with a
// prepared statement (the applyReconciledTracks template).
func (s *Store) StampExtractorVersionBatch(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE tracks
		SET extractor_version = ?,
		    missing_count = 0
		WHERE path = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, ExtractorVersion, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ----- tracks -----

// UpsertTrack writes or replaces the row for t.Path. The tags are encoded
// as JSON so the schema can evolve without column migrations during v0.
//
// Holds `s.mu` per the writer contract on Store.
//
// indexed_at uses the same strict-advance CASE WHEN form as UpsertVariant /
// MarkEnriched (lines 565 + 2730) — without it, a back-to-back UpsertTrack
// at the same nanosecond (rapid test seeds, low-resolution wall clocks, an
// mtime-changed-but-clock-stable scan tick) would leave indexed_at
// unchanged, and a client that synced at the equal timestamp would miss
// the second mutation under the `WHERE indexed_at > since` delta-sync
// filter. The `excluded.indexed_at` reference keeps the bind count at 5
// (the original UPSERT shape) rather than broadening to 7.
func (s *Store) UpsertTrack(ctx context.Context, t *Track) error {
	raw, err := marshalForStorage(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rate, bits, isDSD, codec := formatColumnBinds(t)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at,
		                   sample_rate, bits_per_sample, is_dsd, codec,
		                   extractor_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size          = excluded.size,
			mtime_ns      = excluded.mtime_ns,
			tags_json     = excluded.tags_json,
			indexed_at    = CASE
				WHEN tracks.indexed_at >= excluded.indexed_at THEN tracks.indexed_at + 1
				ELSE excluded.indexed_at
			END,
			enriched_at   = 0,
			-- missing_count reset is UNCONDITIONAL on confirm: the row
			-- being upserted is by definition "seen this scan", which
			-- is exactly the signal the counter measures. A pure
			-- mtime-equal path that skipped the reset would leak
			-- counter increments across scans and cause spurious
			-- deletes on long-stable libraries. See migration v5 doc.
			missing_count = 0,
			-- A re-scan whose artworkMBID CHANGED can't keep the old premium
			-- cover version (it belonged to the previous MBID) — clear it so the
			-- manifest falls back to the new MBID and the refetch re-establishes
			-- a version. An UNCHANGED MBID keeps it so a tag-only re-tag doesn't
			-- needlessly re-fetch. SET RHS reads the pre-update row (tracks.* is
			-- the OLD value); IS is NULL-safe.
			artwork_version = CASE
				WHEN json_extract(excluded.tags_json, '$.artworkMBID')
				     IS json_extract(tracks.tags_json, '$.artworkMBID')
				THEN tracks.artwork_version ELSE NULL
			END,
			-- v25 format-fact columns: stamped on every upsert so the
			-- coverage rollups' eligibility SQL never needs json_extract
			-- on the browse hot path. tags_json stays the read-truth for
			-- Go readers (see the v25 migration docblock).
			sample_rate     = excluded.sample_rate,
			bits_per_sample = excluded.bits_per_sample,
			is_dsd          = excluded.is_dsd,
			codec           = excluded.codec,
			-- extractor_version stamped on every upsert (constant per build).
			-- The excluded.extractor_version assignment is MANDATORY: without
			-- it a re-extracted (conflict) row keeps its stale stamp and would
			-- re-extract on every subsequent scan.
			extractor_version = excluded.extractor_version
	`, t.Path, t.Size, t.ModTime.UnixNano(), raw, s.now().UnixNano(),
		rate, bits, isDSD, codec, ExtractorVersion)
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
func (s *Store) UpsertTrackBatch(ctx context.Context, ts []*Track) error {
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
		rate    any
		bits    any
		isDSD   any
		codec   any
	}
	rows := make([]row, len(ts))
	for i, t := range ts {
		raw, err := marshalForStorage(t)
		if err != nil {
			return err
		}
		rate, bits, isDSD, codec := formatColumnBinds(t)
		rows[i] = row{
			path:    t.Path,
			size:    t.Size,
			mtime:   t.ModTime.UnixNano(),
			tagsRaw: raw,
			rate:    rate,
			bits:    bits,
			isDSD:   isDSD,
			codec:   codec,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	beginAt := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	observeLockWait("upsert_batch", beginAt)
	defer tx.Rollback()
	// indexed_at uses the strict-advance CASE WHEN form from UpsertTrack
	// (and UpsertVariant / MarkEnriched). Batch semantics: `now` is computed
	// once per flush (below) and bound to every row's `excluded.indexed_at`;
	// the CASE WHEN holds per-row, comparing each existing track's
	// indexed_at against the shared `now`. A stale row at `now-1ns`
	// advances to `now`; a row already at `now` (or beyond, under a fake
	// clock) advances to `existing+1`. The batch-level shared `now` is
	// the right shape — per-track s.now() calls would burn 500 syscalls
	// per batch on Pi-class hardware and break the deterministic test seam.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at,
		                   sample_rate, bits_per_sample, is_dsd, codec,
		                   extractor_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size          = excluded.size,
			mtime_ns      = excluded.mtime_ns,
			tags_json     = excluded.tags_json,
			indexed_at    = CASE
				WHEN tracks.indexed_at >= excluded.indexed_at THEN tracks.indexed_at + 1
				ELSE excluded.indexed_at
			END,
			enriched_at   = 0,
			-- See UpsertTrack: missing_count reset is unconditional on
			-- every confirm so a stable library can't accumulate stale
			-- counter increments across scans.
			missing_count = 0,
			-- Clear the premium-cover version when the artworkMBID changes (it
			-- belonged to the prior MBID); keep it when unchanged. Mirrors
			-- UpsertTrack — see its docblock. SET RHS reads the pre-update row.
			artwork_version = CASE
				WHEN json_extract(excluded.tags_json, '$.artworkMBID')
				     IS json_extract(tracks.tags_json, '$.artworkMBID')
				THEN tracks.artwork_version ELSE NULL
			END,
			-- v25 format-fact columns — mirrors UpsertTrack.
			sample_rate     = excluded.sample_rate,
			bits_per_sample = excluded.bits_per_sample,
			is_dsd          = excluded.is_dsd,
			codec           = excluded.codec,
			-- extractor_version stamped on every upsert (constant per build).
			-- The excluded.extractor_version assignment is MANDATORY: without
			-- it a re-extracted (conflict) row keeps its stale stamp and would
			-- re-extract on every subsequent scan.
			extractor_version = excluded.extractor_version
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := s.now().UnixNano()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.path, r.size, r.mtime, r.tagsRaw, now,
			r.rate, r.bits, r.isDSD, r.codec, ExtractorVersion); err != nil {
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
func (s *Store) DeleteTrack(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Step 1: enumerate sidecars BEFORE the DB delete. Shared
	// helper keeps the per-policy details (rows.Err handling,
	// log-and-continue per-row) aligned with the bulk-delete
	// paths.
	rows, err := s.db.QueryContext(ctx, `SELECT sidecar_path FROM track_variants WHERE source_path = ?`, path)
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
	// Also enumerate this track's waveform sidecar (track_analysis).
	sidecars = append(sidecars, s.listWaveformSidecars(ctx, "source_path = ?", path)...)
	// Step 2: parent delete. CASCADE clears `track_variants`
	// rows; sidecar files we just enumerated will be unlinked
	// next.
	if _, err = s.db.ExecContext(ctx, `DELETE FROM tracks WHERE path = ?`, path); err != nil {
		return err
	}
	// Step 3: best-effort filesystem cleanup, shared with the
	// bulk-delete paths.
	removeSidecarFiles(sidecars)
	return nil
}

// DeleteTracksBatch removes many tracks in a SINGLE transaction +
// single lock acquisition. Designed for the reconcile sweeps that may
// reap thousands of rows after a configuration change (e.g. a UPnP
// upstream's library reorganisation) — calling DeleteTrack N times
// would acquire s.mu, BEGIN, write, COMMIT, fsync N times and starve
// concurrent readers/writers for the duration.
//
// The whole batch commits ONCE: a concurrent WAL reader never observes a
// partially-wiped library mid-reap, and the reap costs one fsync instead
// of one per chunk. Chunking below bounds only the SQL parameter list —
// it is orthogonal to the transaction boundary, so every chunk's DELETE
// runs inside the one hoisted tx.
//
// Sidecar-file unlinking is preserved: variant + waveform sidecars for
// every doomed track are enumerated (on the tx's connection, before the
// DELETE drops them under FK CASCADE) and accumulated across chunks, then
// unlinked once after commit — still UNDER s.mu, per the writer-contract
// invariant (the SELECT-sidecars -> DELETE-rows -> os.Remove sequence is
// deliberately atomic; matches DeleteTracksByPrefix / WipeAllTracks).
// Empty input is a no-op. Variant enumeration is STRICT (a QueryContext
// failure or truncated iterator aborts + rolls the batch back, so the
// DELETE can never orphan sidecar files); per-row scan errors and
// waveform enumeration stay best-effort.
func (s *Store) DeleteTracksBatch(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// One transaction wraps the ENTIRE batch (the atomicity contract
	// above). All reads AND writes go through tx so the batch stays on
	// tx's single pinned connection — enumerating sidecars on the bare
	// s.db pool handle while the tx holds a write would check out a
	// SECOND connection inside the s.mu critical section (pool-starvation
	// / lock-ordering anti-pattern).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("manifest: DeleteTracksBatch begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit; structural rollback guarantee.

	// Chunk to keep the SQL parameter list (and the `IN (?, ?, ...)`
	// expression) well under SQLite's defaults (999 params on most
	// builds, modernc.org/sqlite raises this but staying conservative
	// keeps the function portable + readable). 200 matches the existing
	// UpsertTrackBatch's batch shape.
	var allSidecars []string
	const chunkSize = 200
	for start := 0; start < len(paths); start += chunkSize {
		end := start + chunkSize
		if end > len(paths) {
			end = len(paths)
		}
		chunk := paths[start:end]

		// Enumerate variant sidecars for THIS chunk before the DELETE so
		// the FK CASCADE doesn't yank rows out from under us.
		// buildPathInQuery emits placeholders only — no caller string
		// reaches the query body, so the dynamic '?,?,?' list is
		// injection-safe by construction. A *sql.Tx pins one connection,
		// so the variant Rows MUST be closed (explicitly — no defer inside
		// the loop) before the waveform read and the DELETE run on tx.
		//
		// STRICT enumeration (Iterator-error refusal — mirrors
		// DeleteTracksByPrefix's listSidecarsByPathPrefix, CodeRabbit
		// Major + Gemini High on PR #210): a QueryContext failure or a
		// truncated iterator would let the DELETE below orphan sidecar
		// files on disk, so abort and roll the whole batch back via the
		// deferred tx.Rollback (the cascade hasn't run). Per-row scan
		// errors stay best-effort (log + continue).
		selectSQL, selArgs := buildPathInQuery("SELECT sidecar_path FROM track_variants WHERE source_path", chunk)
		rows, err := tx.QueryContext(ctx, selectSQL, selArgs...)
		if err != nil {
			return fmt.Errorf("manifest: DeleteTracksBatch list sidecars: %w", err)
		}
		for rows.Next() {
			var sp string
			if scanErr := rows.Scan(&sp); scanErr != nil {
				logger.Warn("delete-tracks-batch: scan sidecar", "err", scanErr)
				continue
			}
			allSidecars = append(allSidecars, sp)
		}
		if iterErr := rows.Err(); iterErr != nil {
			rows.Close()
			return fmt.Errorf("manifest: DeleteTracksBatch iter sidecars: %w", iterErr)
		}
		rows.Close()
		// Waveform sidecars for the same chunk (track_analysis), on tx.
		wfIn, wfArgs := buildPathInQuery("source_path", chunk)
		allSidecars = append(allSidecars, s.listWaveformSidecarsTx(ctx, tx, wfIn, wfArgs...)...)

		// One DELETE for the whole chunk. FK CASCADE handles
		// upnp_track_routing + track_variants + track_analysis in one
		// shot.
		deleteSQL, delArgs := buildPathInQuery("DELETE FROM tracks WHERE path", chunk)
		if _, err := tx.ExecContext(ctx, deleteSQL, delArgs...); err != nil {
			return fmt.Errorf("manifest: DeleteTracksBatch exec: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: DeleteTracksBatch commit: %w", err)
	}

	// Sidecar files unlinked once, after the single commit — still UNDER
	// s.mu (matches DeleteTracksByPrefix / WipeAllTracks). The
	// SELECT-sidecars -> DELETE-rows -> os.Remove-files sequence is
	// deliberately atomic per the writer-contract invariant: releasing the
	// lock before the unlink would let a concurrent UpsertVariant resurrect
	// a row pointing at a content-hashed sidecar we're about to remove.
	removeSidecarFiles(allSidecars)
	return nil
}

// buildPathInQuery returns a complete SQL string of the form
// `<prefix> IN (?,?,...)` plus the matching []any args, suitable for
// passing straight to QueryContext / ExecContext. `prefix` is a
// caller-side compile-time literal (e.g. "DELETE FROM tracks WHERE
// path"); the dynamic suffix is ONLY placeholders + a deterministic
// length derived from len(paths). No caller-supplied string ever
// reaches the SQL body — both inputs are either a literal or a count.
// Injection-safe by construction.
func buildPathInQuery(prefix string, paths []string) (string, []any) {
	if len(paths) == 0 {
		return prefix + " IN ()", nil
	}
	args := make([]any, len(paths))
	var b strings.Builder
	b.Grow(len(prefix) + 5 + 2*len(paths))
	b.WriteString(prefix)
	b.WriteString(" IN (")
	for i, p := range paths {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args[i] = p
	}
	b.WriteByte(')')
	return b.String(), args
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
// TrackStat is the narrow projection the scanner's unchanged-file
// skip gate needs: the two scalars it compares against the walked
// `os.FileInfo`, plus the one tag it inspects for local-artwork cache
// recovery. Deliberately NOT a wire type — it never leaves the
// manifest package, and it carries no `json:` tags (see CLAUDE.md →
// "Wire-type discipline": the SQLite row structs must not gain them).
type TrackStat struct {
	Size        int64
	MTimeNS     int64
	ArtworkMBID string
	// ExtractorVersion is the `extractor_version` column — the
	// manifest.ExtractorVersion that produced the row's tags. The
	// scan-skip gate re-extracts any row whose stamp is < the current
	// constant. NOT NULL DEFAULT 0, so it scans into a plain int (a
	// pre-stamp row reads 0).
	ExtractorVersion int
}

// GetTrackStat is the skip-gate twin of GetTrack: same exact-key
// PRIMARY KEY lookup and the same (nil, nil) miss contract, but it
// projects three scalars instead of hauling the whole `tags_json`
// BLOB into Go and unmarshalling a ~40-field Track out of it.
//
// The gate runs once per file per scan (default every 6 h, and on
// every watcher-triggered subtree scan), so at 100k tracks the old
// shape read hundreds of MB of BLOB and ran 100k full JSON parses
// purely to compare a size and an mtime.
//
// **`size` and `mtime_ns` are safe to read as columns here even
// though `Track.ModTime` also lives inside `tags_json`.** Both
// UpsertTrack and UpsertTrackBatch bind them from the same `*Track`
// they marshal (`t.Size`, `t.ModTime.UnixNano()`), so the column and
// the JSON are written atomically from one source. The other
// `tags_json` writers (MarkEnriched, applyReconciledTracks, the
// artwork-version / booklet-tag stampers) round-trip a Track that
// already carries the original ModTime, so they can't drift either.
// Verified empirically against a live 15,373-row hybrid library
// (filesystem + UPnP-routed): zero disagreement on size, and zero on
// mtime to nanosecond precision.
//
// Comparing `MTimeNS` against `info.ModTime().UnixNano()` is exactly
// the instant-equality `time.Time.Equal` gave us — which matters
// because UPnP-routed rows serialise their mtime with a `+HH:MM`
// offset while scanner rows use `Z`, and both forms must compare
// equal on the same instant.
func (s *Store) GetTrackStat(ctx context.Context, path string) (*TrackStat, error) {
	var st TrackStat
	err := s.db.QueryRowContext(ctx, `
		SELECT size, mtime_ns, COALESCE(json_extract(tags_json, '$.artworkMBID'), ''),
		       extractor_version
		FROM tracks WHERE path = ?`, path).
		Scan(&st.Size, &st.MTimeNS, &st.ArtworkMBID, &st.ExtractorVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) GetTrack(ctx context.Context, path string) (*Track, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT tags_json FROM tracks WHERE path = ?`, path).Scan(&raw)
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
func (s *Store) LookupTrack(ctx context.Context, path string) (*Track, error) {
	if t, err := s.GetTrack(ctx, path); err != nil || t != nil {
		return t, err
	}
	cleaned := normalizePathForLookup(path)
	if cleaned == path {
		// Exact already missed and the cleaned form is identical
		// — no further fallback to attempt that wouldn't repeat
		// the same query.
		return s.lookupTrackByLowerCase(ctx, cleaned)
	}
	// Try the leading-slash-stripped form as a back-compat
	// exact match before falling through to the case-folded
	// scan; some iOS code paths only strip the slash without
	// lowercasing, and that exact form should land cheaply.
	if t, err := s.GetTrack(ctx, cleaned); err != nil || t != nil {
		return t, err
	}
	return s.lookupTrackByLowerCase(ctx, cleaned)
}

func (s *Store) lookupTrackByLowerCase(ctx context.Context, cleaned string) (*Track, error) {
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT tags_json FROM tracks WHERE unicode_lower(path) = unicode_lower(?) LIMIT 2`,
		cleaned,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		// rows.Err() distinguishes a genuine empty result from an
		// iteration error (transient SQLite I/O / malformed index); a
		// missing check silently misreads a real error as "track not
		// found". (DeepSeek review.)
		if err := rows.Err(); err != nil {
			return nil, err
		}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var t Track
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// boolPtr returns a heap-allocated *bool for the given value. Used by
// the read paths (ListTracks / StreamTracks / ListTracksPage) to fill
// `Track.Enriched`, which is *bool for wire-shape reasons (nil
// distinguishes "pre-v1.1 server, field absent" from explicit false).
//
// Per-row allocation is deliberate (replaces a prior pair of shared
// package-level singletons). The earlier singleton optimisation was
// premature: 50k tracks × 8-byte pointer + 1-byte bool ≈ 450 KB total
// — noise next to the Track struct itself (~200 B + heap strings per
// row) and SQLite query / JSON marshalling that already dominate the
// read path. The singletons created an external-mutation footgun
// because Track is exported: a downstream consumer writing
// `*track.Enriched = ...` would have clobbered every subsequent read
// for the process lifetime.
func boolPtr(b bool) *bool { return &b }

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

// waveformTagSQL is the correlated-subquery suffix that splices the
// offline-analysis waveform content tag onto each row. Returns the
// `track_analysis.waveform_tag` for the track, or NULL when no
// analysis row exists (or the tag is empty). Spliced onto
// Track.WaveformTag at read time — column-derived, never persisted in
// tags_json (same discipline as Enriched / Variants). One indexed
// point-lookup on the track_analysis PRIMARY KEY per row; the
// NULLIF keeps a defensively-empty tag from surfacing as a present
// field on the wire. Appended after variantsAggSQL in the manifest
// read paths.
const waveformTagSQL = `(SELECT NULLIF(waveform_tag, '')
	 FROM track_analysis WHERE source_path = tracks.path) AS waveform_tag`

// replayGainSQL is the correlated-subquery suffix that splices the
// offline-analysis ReplayGain track gain (dB) onto each row, or NULL when
// no analysis row exists / loudness wasn't computed. The splice is
// tag-absent-ONLY: each read site fills Track.ReplayGainTrackDB from this
// column only when the value decoded from tags_json is nil, so a curated
// ReplayGain tag always wins. One more indexed PK point-lookup on
// track_analysis per row, alongside waveformTagSQL.
const replayGainSQL = `(SELECT replaygain_track_db
	 FROM track_analysis WHERE source_path = tracks.path) AS analysis_replaygain_track_db`

// spliceAnalysisReplayGain fills Track.ReplayGainTrackDB from the
// offline-analysis value (scanned via replayGainSQL) ONLY when the track
// carries no ReplayGain from its own tags — curated tags always win.
// A no-op when the analysis value is NULL or a tag value is already
// present. The three manifest read paths share it so the tag-absent-only
// contract lives in one place.
func spliceAnalysisReplayGain(t *Track, rg sql.NullFloat64) {
	if t.ReplayGainTrackDB == nil && rg.Valid {
		v := rg.Float64
		// track_analysis is reachable by an external sqlite3 CLI, so
		// the REAL column is not trusted input: a hand-written NaN/±Inf
		// would crash /v1/manifest mid-stream at enc.Encode
		// (json.Marshal rejects non-finite floats). Skip it — the track
		// simply surfaces with no loudness.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		t.ReplayGainTrackDB = &v
		// Mark provenance so marshalForStorage scrubs this (analysis-
		// derived) value on any write-back, never freezing it into
		// tags_json as a faux curated tag. (CodeRabbit on #396.)
		t.replayGainFromAnalysis = true
	}
}

// analysisScalarsSQL splices the estimated key + tempo AND the wf4
// quality scalars (true peak / DR / audio-MD5 state) onto each row as
// ONE json_object (vs six scalar subqueries — same single indexed PK
// lookup on track_analysis the waveform/replaygain splices use). NULL
// when no analysis row exists; `{"root":null,...}` when the row exists
// but a value is absent. Decoded + spliced by spliceAnalysisScalars.
// (Renamed from the old keyTempo splice when the quality scalars joined the bundle.)
const analysisScalarsSQL = `(SELECT json_object('root', key_root, 'mode', key_mode, 'bpm', bpm,
	 'tp', true_peak_db, 'dr', dr_score, 'md5', audio_md5_state)
	 FROM track_analysis WHERE source_path = tracks.path) AS analysis_scalars`

// analysisScalars is the decode target for analysisScalarsSQL's json_object.
type analysisScalars struct {
	Root     *int     `json:"root"`
	Mode     *string  `json:"mode"`
	BPM      *int     `json:"bpm"`
	TruePeak *float64 `json:"tp"`
	DR       *int     `json:"dr"`
	MD5      *string  `json:"md5"`
}

// spliceAnalysisScalars fills Track.KeyRoot/KeyMode (always — no tag
// source today), Track.BPM (tag-absent-only — a curated TBPM/BPM tag
// always wins), and the analysis-only quality scalars TruePeakDB /
// DRScore / AudioMD5State from the analysis json_object. Malformed JSON
// is ignored (the track stays playable without the estimates). Like
// ReplayGain, the BPM splice marks provenance so marshalForStorage
// scrubs only the analysis-derived value on write-back; the analysis-only
// fields are zeroed unconditionally there. TruePeak carries the same
// non-finite guard as spliceAnalysisReplayGain — `track_analysis` is
// reachable by an external sqlite3 CLI, and json.Marshal rejects
// non-finite floats, which would crash /v1/manifest mid-stream.
func spliceAnalysisScalars(t *Track, raw sql.NullString) {
	if !raw.Valid || raw.String == "" {
		return
	}
	var kt analysisScalars
	if err := json.Unmarshal([]byte(raw.String), &kt); err != nil {
		return
	}
	if kt.Root != nil {
		r := *kt.Root
		t.KeyRoot = &r
	}
	if kt.Mode != nil {
		t.KeyMode = *kt.Mode
	}
	if t.BPM == nil && kt.BPM != nil {
		b := *kt.BPM
		t.BPM = &b
		t.bpmFromAnalysis = true
	}
	if kt.TruePeak != nil && !math.IsNaN(*kt.TruePeak) && !math.IsInf(*kt.TruePeak, 0) {
		v := *kt.TruePeak
		t.TruePeakDB = &v
	}
	if kt.DR != nil {
		d := *kt.DR
		t.DRScore = &d
	}
	if kt.MD5 != nil {
		t.AudioMD5State = *kt.MD5
	}
}

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
// the picker UI / operator-facing diagnostics. Two producer prefixes:
//
//   - `upscaled-` → "Upscaled FLAC 24/96" (operator-driven hi-res
//     render for home-DAC playback).
//   - `optimized-` → "Optimized FLAC 16/44.1" (CarPlay-targeted
//     downsample, runtime-routed on iOS — invisible in the wand
//     long-press menu for v1, but surfaces in admin web UI and
//     server logs).
//
// **Don't reintroduce the hardcoded "Upscaled" literal** at any
// new variant-label site — branch on the prefix.
func humanLabelForVariant(v Variant) string {
	rateLabel := formatSampleRateLabel(v.SampleRate)
	kind := "Upscaled"
	if strings.HasPrefix(v.ID, "optimized-") {
		kind = "Optimized"
	}
	switch {
	case v.Format == "flac":
		return fmt.Sprintf("%s FLAC %d/%s", kind, v.BitsPerSample, rateLabel)
	default:
		// Non-FLAC variants (none today — sox forces -t flac) still carry
		// the operator-facing kind prefix + an upper-cased format token.
		// Don't drop `kind` here (see the docblock's "branch on the prefix").
		return fmt.Sprintf("%s %s %d/%s", kind, strings.ToUpper(v.Format), v.BitsPerSample, rateLabel)
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
func (s *Store) ListTracks(ctx context.Context, since *time.Time) ([]Track, error) {
	q := `SELECT tags_json, enriched_at, ` + variantsAggSQL + `, ` + waveformTagSQL + `, ` + replayGainSQL + `, ` + analysisScalarsSQL + `, artwork_version, booklet_tag FROM tracks`
	args := []any{}
	if since != nil {
		q += ` WHERE indexed_at > ?`
		args = append(args, since.UnixNano())
	}
	q += ` ORDER BY path ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Track{}
	for rows.Next() {
		var raw []byte
		var enrichedAt int64
		var variantsRaw []byte
		var wfTag sql.NullString
		var rg sql.NullFloat64
		var ktRaw sql.NullString
		var artVer sql.NullString
		var bkTag sql.NullString
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw, &wfTag, &rg, &ktRaw, &artVer, &bkTag); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		scanTrackVariants(&t, variantsRaw)
		t.Enriched = boolPtr(enrichedAt != 0)
		t.WaveformTag = wfTag.String
		t.ArtworkVersion = artVer.String
		t.BookletTag = bkTag.String
		spliceAnalysisReplayGain(&t, rg)
		spliceAnalysisScalars(&t, ktRaw)
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
func (s *Store) StreamTracks(ctx context.Context, sp *time.Time, fn func(*Track) error) error {
	if fn == nil {
		// Defensive guard: invoking the callback later would panic with
		// a nil-deref. CodeRabbit on PR #70 — surface a clear error
		// before doing any DB work so misuse is obvious instead of a
		// production crash deep in the streaming-manifest path.
		return errors.New("StreamTracks: nil callback")
	}
	q := `SELECT tags_json, enriched_at, ` + variantsAggSQL + `, ` + waveformTagSQL + `, ` + replayGainSQL + `, ` + analysisScalarsSQL + `, artwork_version, booklet_tag FROM tracks`
	args := []any{}
	if sp != nil {
		q += ` WHERE indexed_at > ?`
		args = append(args, sp.UnixNano())
	}
	q += ` ORDER BY path ASC`
	// **QueryContext (not Query)** so a client disconnect mid-stream
	// terminates the SQLite scan instead of holding the read lock +
	// CPU until SQLite exhausts the result set. Senior-audit
	// follow-up. The per-row `ctx.Err()` check in the caller
	// (writeManifestGated) catches the disconnect within one row of
	// the next pulse; this widens the cancellation surface to the
	// query itself so a slow SELECT (5k-folder library with
	// dependent CTEs in variantsAggSQL) cancels mid-execution
	// rather than running to completion.
	rows, err := s.db.QueryContext(ctx, q, args...)
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
		var wfTag sql.NullString
		var rg sql.NullFloat64
		var ktRaw sql.NullString
		var artVer sql.NullString
		var bkTag sql.NullString
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw, &wfTag, &rg, &ktRaw, &artVer, &bkTag); err != nil {
			return err
		}
		t = Track{}
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		t.Enriched = boolPtr(enrichedAt != 0)
		scanTrackVariants(&t, variantsRaw)
		t.WaveformTag = wfTag.String
		t.ArtworkVersion = artVer.String
		t.BookletTag = bkTag.String
		spliceAnalysisReplayGain(&t, rg)
		spliceAnalysisScalars(&t, ktRaw)
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
func (s *Store) ListTracksPage(ctx context.Context, afterPath string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tags_json, enriched_at, `+variantsAggSQL+`, `+waveformTagSQL+`, `+replayGainSQL+`, `+analysisScalarsSQL+`, artwork_version, booklet_tag FROM tracks
		WHERE path > ?
		ORDER BY path ASC
		LIMIT ?
	`, afterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Pre-size to the page bound. `limit` is already defaulted to 1000 above
	// and the sole caller clamps it to <=5001; min(...,8192) caps a
	// hypothetical future caller passing an unbounded limit straight to the
	// store, so the prealloc can't blow up memory on a near-empty table.
	out := make([]Track, 0, min(limit, 8192))
	for rows.Next() {
		var raw []byte
		var enrichedAt int64
		var variantsRaw []byte
		var wfTag sql.NullString
		var rg sql.NullFloat64
		var ktRaw sql.NullString
		var artVer sql.NullString
		var bkTag sql.NullString
		if err := rows.Scan(&raw, &enrichedAt, &variantsRaw, &wfTag, &rg, &ktRaw, &artVer, &bkTag); err != nil {
			return nil, err
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		scanTrackVariants(&t, variantsRaw)
		t.Enriched = boolPtr(enrichedAt != 0)
		t.WaveformTag = wfTag.String
		t.ArtworkVersion = artVer.String
		t.BookletTag = bkTag.String
		spliceAnalysisReplayGain(&t, rg)
		spliceAnalysisScalars(&t, ktRaw)
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
func (s *Store) HasTrackWithArtworkMBID(ctx context.Context, mbid string) bool {
	if mbid == "" {
		return false
	}
	return s.hasTrackWithJSONField(ctx, artworkMBIDField, mbid)
}

// HasTrackWithArtistMBID mirrors HasTrackWithArtworkMBID for the
// `/v1/artist-image/{mbid}` handler. Same 202-vs-404 distinction.
// Also indexed (see `migrate`).
func (s *Store) HasTrackWithArtistMBID(ctx context.Context, mbid string) bool {
	if mbid == "" {
		return false
	}
	return s.hasTrackWithJSONField(ctx, artistMBIDField, mbid)
}

// DistinctArtistMBIDs returns every distinct, non-empty MusicBrainz artist GID
// present in the library — the set the Atlas bulk-harvest client submits for
// enrichment. Reads are un-mutexed (WAL handles concurrent readers). The artist
// GID lives in the per-track tags_json blob under `$.artistMBID`, so this is a
// full-table json_extract scan; it runs on a slow cadence (harvest submit), not
// a request hot path.
func (s *Store) DistinctArtistMBIDs(ctx context.Context) ([]string, error) {
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artistMBID')
		  FROM tracks
		 WHERE json_extract(tags_json, '$.artistMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artistMBID') != ''
	`))
}

// DistinctReleaseMBIDs returns every distinct MusicBrainz release GID the
// library has a cached cover for — the UUID-form `$.artworkMBID` values
// (excluding the `local-<hash>` curated-art sentinels). This is the set the
// Atlas cover bulk-harvest submits (kind=release) and re-fetches premium covers
// for. The enricher sets ArtworkMBID to the release MBID after a CoverArt fetch,
// so a UUID artworkMBID IS the release GID; `/v1/artwork/{mbid}` and Atlas's
// `/release/{mbid}` both key on it. Un-mutexed read; slow cadence (harvest), not
// a hot path.
func (s *Store) DistinctReleaseMBIDs(ctx context.Context) ([]string, error) {
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artworkMBID')
		  FROM tracks
		 WHERE json_extract(tags_json, '$.artworkMBID') IS NOT NULL
		   AND json_extract(tags_json, '$.artworkMBID') != ''
		   AND json_extract(tags_json, '$.artworkMBID') NOT LIKE 'local-%'
	`))
}

// DistinctReleaseTextMBIDs enumerates the library's distinct musicBrainzAlbumID
// release UUIDs that DistinctReleaseMBIDs does NOT already cover (i.e. not in the
// artworkMBID set). These are MB-matched albums that kept LOCAL artwork
// (artworkMBID=local-…): iOS reads their "About this album" by the release MBID,
// but the cover harvest never submitted them, so their descriptions never got
// resolved. They're submitted as TEXT-ONLY release subscriptions (Phase D) so
// the album text is harvested without a wasted cover reverse-resolve.
func (s *Store) DistinctReleaseTextMBIDs(ctx context.Context) ([]string, error) {
	// NOT EXISTS (correlated) over NOT IN — NULL-safe + index-friendlier, and the
	// codebase convention for anti-joins (cf. the UPnP-routing anti-joins). The
	// equality naturally excludes empty / local- artworkMBID values (they can't
	// equal a non-empty UUID), so the subquery needs no extra filters.
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(t.tags_json, '$.musicBrainzAlbumID')
		  FROM tracks t
		 WHERE json_extract(t.tags_json, '$.musicBrainzAlbumID') IS NOT NULL
		   AND json_extract(t.tags_json, '$.musicBrainzAlbumID') != ''
		   AND json_extract(t.tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
		   AND NOT EXISTS (
		       SELECT 1 FROM tracks a
		        WHERE json_extract(a.tags_json, '$.artworkMBID')
		              = json_extract(t.tags_json, '$.musicBrainzAlbumID')
		   )
	`))
}

// collectStringColumn drains a single-text-column query into a []string,
// closing the rows. Shared by the Distinct*MBIDs enumerators. Takes the
// (rows, err) pair directly so callers stay one-liners.
func collectStringColumn(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

func (s *Store) hasTrackWithJSONField(ctx context.Context, field jsonField, value string) bool {
	var q string
	switch field {
	case artworkMBIDField:
		q = `SELECT EXISTS(SELECT 1 FROM tracks WHERE json_extract(tags_json, '$.artworkMBID') = ?)`
	case artistMBIDField:
		q = `SELECT EXISTS(SELECT 1 FROM tracks WHERE json_extract(tags_json, '$.artistMBID') = ?)`
	default:
		// Unknown field — by construction unreachable, but refusing
		// quietly is safer than compiling a bogus query.
		return false
	}
	// SELECT EXISTS(...) always yields exactly one row (0 or 1), so there's
	// no sql.ErrNoRows outcome to special-case — any error here is a genuine
	// database fault (disk I/O, connection closed, migration mid-flight).
	var found bool
	if err := s.db.QueryRowContext(ctx, q, value).Scan(&found); err != nil {
		logger.Error("hasTrackWithJSONField", "field", field, "err", err)
		return false
	}
	return found
}

// CountTracks returns the total number of track rows. /v1/health polls
// this frequently, so it's backed by a SELECT COUNT(*) instead of a
// full path-materialization + len().
func (s *Store) CountTracks(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracks`).Scan(&n)
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
func (s *Store) ArtworkMBIDsInUse(ctx context.Context) ([]string, error) {
	// The WHERE clause already guarantees non-null, non-empty values, so
	// the shared enumerator's plain-string scan is safe — no NullString
	// re-filter needed. Mirrors DistinctReleaseMBIDs / DistinctArtistMBIDs.
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.artworkMBID')
		FROM tracks
		WHERE json_extract(tags_json, '$.artworkMBID') IS NOT NULL
		  AND json_extract(tags_json, '$.artworkMBID') != ''
	`))
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
func (s *Store) EnrichmentCounts(ctx context.Context) (enriched int, lastEnrichedAt *time.Time, err error) {
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
	err = s.db.QueryRowContext(ctx, `
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

// EnrichmentBreakdown returns the derived per-state track counts the admin
// dashboard renders so the operator can watch the enrichment trickle drain and
// spot coverage gaps:
//
//   - pending: never been through the enricher (`enriched_at = 0`).
//   - matched: enriched AND nothing left to fill — artwork, artist and release
//     MBIDs all landed.
//   - missing: enriched but still short at least one of those. This is the
//     coverage-gap signal, and it is EXACTLY the set the "Retry missing" button
//     re-queues.
//
// That last equality is the point of sharing enrichmentMissPredicateSQL here.
// `matched` used to be "non-NULL `$.artworkMBID`" alone, which made the card
// disagree with its own button in both directions: a `local-<sha256>` sentinel
// (embedded APIC / folder.jpg art) counted as matched even with no release MBID
// at all, and a row with a cover but no artist MBID counted as matched too.
// Measured on the production bridge when this was corrected (2026-07-29): the
// card read 553 missing while the retry re-queued 10,194 — an operator watching
// the number had no way to predict what the button would do. Same class of bug
// as the predicate gap the button itself had (PR #595), one layer up in the
// display.
//
// No new column: the split still derives from `enriched_at` + the tags, so it
// stays exactly consistent with what `enrichOne` writes — a track is
// MarkEnriched even when a facet misses. COALESCE-to-” is what makes the test
// exact: `Track`'s MBID fields are `omitempty`, so absent is the normal shape
// for a gap, but an explicitly-empty value must read the same way.
//
// Four subqueries in one statement (one consistent read, no divergence window
// between separate COUNTs): `pending` and `total` are COUNTs against
// `idx_tracks_enriched` / the table, and `missing` is DERIVED as
// `total - pending - matched` (every track is pending XOR enriched, and enriched
// splits matched/missing) so it costs no second json_extract pass.
//
// `matched` no longer rides the `idx_tracks_artwork_mbid` functional index —
// a three-field predicate can't — so it parses `tags_json` per row. Measured on
// the 19,482-track production library: 34ms -> 64ms for the whole statement.
// That is affordable because the admin caller TTL-caches (enrichmentCacheTTL)
// and single-flights it; if this ever needs to run un-cached, count `missing`
// against a purpose-built index rather than reverting to a predicate that
// disagrees with the retry. Read-only, no `s.mu` (WAL concurrent-reader), so a
// slow read can't stall a writer. (Gemini review on PR #490.)
//
// `lastEnrichedAt` mirrors EnrichmentCounts exactly: MAX(enriched_at) is a valid
// `0` (NOT SQL NULL) on a fresh all-unenriched library, so the `!= 0` guard maps
// it to nil ("never"); the stored unit is UnixNano, hence time.Unix(0, ns).
func (s *Store) EnrichmentBreakdown(ctx context.Context) (pending, matched, missing int, lastEnrichedAt *time.Time, err error) {
	var lastNs sql.NullInt64
	var total int
	err = s.db.QueryRowContext(ctx, enrichmentBreakdownSQL).Scan(&pending, &matched, &total, &lastNs)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	// Derive missing rather than counting it: every track is pending XOR
	// enriched, and enriched splits into matched/missing — so this avoids a
	// second json_extract pass. total >= pending+matched always holds.
	missing = total - pending - matched
	if lastNs.Valid && lastNs.Int64 != 0 {
		t := time.Unix(0, lastNs.Int64).UTC()
		lastEnrichedAt = &t
	}
	return pending, matched, missing, lastEnrichedAt, nil
}

// CountTracksUnderRoot returns the number of track rows belonging to
// the given library root. Used by the scanner's FUSE drop-mode guards
// to distinguish "operator legitimately wiped the root" from "the
// FUSE mount has dropped and the WalkDir came back empty for a root
// the DB carries history for."
//
// Track paths in the DB use two distinct layouts depending on whether
// the bridge is in single-root or multi-root mode (see CLAUDE.md
// "Single ↔ multi-root storage form flips"):
//   - Single-root mode: paths have no root prefix
//     (e.g. "Artist/Album/Track.flac"). Count is the whole table.
//   - Multi-root mode: paths are prefixed with the root's base
//     directory name (e.g. "music/Artist/Album/Track.flac"). Count
//     is the prefix-scoped subset.
//
// A multi-root miss in single-root mode (or vice-versa) returns 0
// and silently bypasses the protective gate, so the bind value MUST
// match the storage form. Callers pass `multiRoot = len(roots) > 1`
// — the same boolean the scanner threads through walkRoot and
// fillFromPath.
func (s *Store) CountTracksUnderRoot(ctx context.Context, rootBase string, multiRoot bool) (int, error) {
	if !multiRoot {
		return s.CountTracks(ctx)
	}
	return s.CountTracksByPrefix(ctx, filepath.Base(rootBase)+"/")
}

// CountTracksByPrefix returns the number of track rows under the folder
// prefix. In multi-root mode the admin console passes "<rootBasename>/"
// to get a per-root count.
//
// Scoped with the byte-range form `path >= base||'/' AND path < base||'0'`
// (base = prefix with any trailing '/' stripped; '0' is the ASCII
// successor of '/') — the established sibling idiom (RollupByPrefix /
// EligibleRollupByPrefix). Numerically identical to the prior
// `LIKE 'base/%'` form for folder prefixes, but the range predicate rides
// the BINARY-collated `path` PRIMARY KEY instead of a case-insensitive
// LIKE that forfeits the index and full-scans. A pure byte comparison
// needs no LIKE-wildcard escaping, so a root named "foo_bar" is matched
// literally without an ESCAPE clause.
func (s *Store) CountTracksByPrefix(ctx context.Context, prefix string) (int, error) {
	// TrimRight, not TrimSuffix: a caller passing "Album//" would keep one
	// slash and rebuild the same broken pattern.
	//
	// DECIDE AFTER THE TRIM. All four prefix helpers (here,
	// RollupByPrefix, EligibleRollupByPrefix,
	// ListTrackProjectionsUnderPrefix) treat a prefix that is EMPTY
	// ONCE TRIMMED as whole-library — keep them in step. Deciding
	// before the trim splits the family: "//" then means "everything"
	// to two of them and "nothing" to the other two, and the admin
	// Inspector renders a rollup and a projection from the same input
	// side by side.
	//
	// Without the branch, base "" builds `path >= '/' AND path < '0'`,
	// which matches nothing (library-relative paths never start with
	// '/'). No caller passes "" today; the reachable degenerate is a
	// multi-root `/` root, where filepath.Base gives "/" — and there
	// the whole-table count is the FAIL-SAFE answer, because this
	// feeds the scanner's FUSE drop-mode guard and a 0 silently
	// bypasses it (see CountTracksUnderRoot's docblock).
	base := strings.TrimRight(prefix, "/")
	if base == "" {
		return s.CountTracks(ctx)
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracks WHERE path >= ? || '/' AND path < ? || '0'`,
		base, base,
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
func (s *Store) DeleteTracksByPrefix(ctx context.Context, prefix string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Byte-range, NOT `LIKE prefix||'%'`. SQLite's default LIKE folds
	// ASCII case (nothing sets case_sensitive_like — see OpenStore's
	// DSN), and this prefix is a library-root BASENAME, not the
	// folder-derived prefix the PR #532 conversion could assume never
	// differs only by case: ValidateRoots compares basenames
	// case-sensitively, so /srv/Music and /srv/music are both accepted
	// as roots. Removing one then matched BOTH — deleting the
	// survivor's rows and, via the two sidecar enumerations below,
	// UNLINKING ITS VARIANT AND WAVEFORM FILES FROM DISK. Worse, the
	// count the operator confirmed against came from
	// CountTracksByPrefix, which is already case-sensitive, so it
	// understated the damage.
	//
	// The range rides the BINARY-collated `path` PRIMARY KEY (verified:
	// SEARCH ... USING COVERING INDEX (path>? AND path<?), where the
	// LIKE form was a full SCAN). '0' is the ASCII successor of '/'.
	// Non-ASCII prefixes need no special handling: UTF-8 continuation
	// bytes are all >= 0x80, so they sort after both '/' (0x2F) and
	// '0' (0x30) and the range still bounds exactly the prefix+"/" set.
	base := strings.TrimRight(prefix, "/")
	if base == "" {
		// Deliberately an ERROR, not a whole-table delete. The LIKE
		// form matched nothing here (`likeEscape("//")+"%"` is `//%`,
		// and library-relative paths never start with '/'), so a
		// silent no-op was the old behaviour — but the range form's
		// natural reading of an empty base is "everything", and this
		// is the one method in the family where guessing wrong wipes
		// the library. CountTracksByPrefix can fail safe to a whole-
		// table COUNT because a count is harmless; a DELETE cannot.
		// Whole-library removal is WipeAllTracks / WipeFilesystemTracks.
		return 0, fmt.Errorf("delete tracks by prefix: empty prefix %q — use WipeAllTracks/WipeFilesystemTracks for a whole-library delete", prefix)
	}
	// Step 1: enumerate doomed sidecars BEFORE the cascade drops
	// the rows. Reuses the proactive-cleanup contract documented
	// on DeleteTrack.
	//
	// **Iterator-error refusal** (CodeRabbit Major + Gemini High on
	// PR #210): if the enumeration was truncated mid-scan, the
	// downstream `removeSidecarFiles` would leak orphan sidecar
	// files on disk — the row-cascade hasn't run yet so we can
	// abort cleanly and surface the error to the caller. Refusing
	// upfront is better than committing a partial delete.
	doomedSidecars, err := s.listSidecarsByPathPrefix(ctx, base)
	if err != nil {
		return 0, err
	}
	// Waveform sidecars under the same prefix (track_analysis). Best-
	// effort like the rest of the analysis cleanup; an orphan is a
	// `bridge analyze --gc` problem, not a reason to fail the delete.
	// Same byte-range bounds as the DELETE — these two MUST agree, or
	// the unlink set and the row set diverge.
	doomedSidecars = append(doomedSidecars,
		s.listWaveformSidecars(ctx,
			`source_path COLLATE BINARY >= ? || '/' AND source_path COLLATE BINARY < ? || '0'`,
			base, base)...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM tracks WHERE path COLLATE BINARY >= ? || '/' AND path COLLATE BINARY < ? || '0'`,
		base, base,
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

// listSidecarsByPathPrefix returns every sidecar_path whose source_path
// lies under base (a slash-free prefix — the caller has already trimmed).
// Used by DeleteTracksByPrefix's pre-cascade enumeration. Caller MUST
// hold s.mu (writer-serialization contract).
//
// Byte-range rather than LIKE, and the bounds MUST stay identical to
// the DELETE in DeleteTracksByPrefix: this list is what gets UNLINKED
// FROM DISK, so a predicate that matches more than the row delete does
// destroys files belonging to rows that survive. That is exactly what
// the case-folding LIKE did for two roots differing only in case.
//
// **Iterator-error returns propagate** (PR-C, audit follow-up):
// the previous shape logged `rows.Err()` and returned `nil`,
// masking partial-result truncation. Callers (the cascading
// delete path) need to know when the enumeration was truncated
// so they don't silently leave orphan sidecar files on disk.
func (s *Store) listSidecarsByPathPrefix(ctx context.Context, base string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sidecar_path FROM track_variants
		  WHERE source_path COLLATE BINARY >= ? || '/'
		    AND source_path COLLATE BINARY < ? || '0'`,
		base, base,
	)
	if err != nil {
		logger.Warn("list sidecars by prefix", "prefix", base, "err", err)
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
		return out, iterErr
	}
	return out, nil
}

// listAllSidecars returns every sidecar_path in the table. Used
// by WipeAllTracks. Same writer-lock contract.
//
// **Iterator-error returns propagate** — same rationale as
// listSidecarsByPathPrefix above. WipeAllTracks treats a
// truncated enumeration as a fatal error because the on-disk
// cleanup must be complete to fulfil the wipe contract.
func (s *Store) listAllSidecars(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sidecar_path FROM track_variants`)
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
		return out, iterErr
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

// rowQueryer is the read surface shared by *sql.DB and *sql.Tx, so a
// helper can enumerate rows either on the pool (default) or pinned to an
// in-flight transaction's single connection. Lets listWaveformSidecars
// serve both the pooled callers and DeleteTracksBatch's tx-pinned batch
// from one implementation.
type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listWaveformSidecars enumerates waveform sidecars via the pooled db
// handle. Callers that hold an open transaction and need the read on the
// SAME connection (so their tx's write doesn't tie up a second pooled
// connection while it runs) must use listWaveformSidecarsTx instead.
func (s *Store) listWaveformSidecars(ctx context.Context, where string, args ...any) []string {
	return listWaveformSidecarsQ(ctx, s.db, where, args...)
}

// listWaveformSidecarsTx is the transaction-pinned variant: the read runs
// on tx's connection so DeleteTracksBatch keeps its entire enumerate+delete
// batch on the one connection its single transaction pins.
func (s *Store) listWaveformSidecarsTx(ctx context.Context, tx *sql.Tx, where string, args ...any) []string {
	return listWaveformSidecarsQ(ctx, tx, where, args...)
}

// listWaveformSidecarsQ returns the non-empty `track_analysis.waveform_path`
// values matching the given WHERE clause, for the delete paths to unlink
// alongside variant sidecars (CASCADE drops the row but not the on-disk
// file — same contract as track_variants). Best-effort: enumeration
// failure logs and returns what it has, so a waveform orphan becomes
// `bridge analyze --gc`'s problem and never blocks the parent delete.
// The `where` argument is a caller-side compile-time literal (e.g.
// "source_path = ?") — never caller text — so the concatenation is
// injection-safe; all values flow through `args` placeholders. Caller
// holds s.mu (writer-serialization contract).
func listWaveformSidecarsQ(ctx context.Context, q rowQueryer, where string, args ...any) []string {
	rows, err := q.QueryContext(ctx,
		`SELECT waveform_path FROM track_analysis WHERE waveform_path != '' AND (`+where+`)`, args...)
	if err != nil {
		logger.Warn("list waveform sidecars", "err", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var wp string
		if scanErr := rows.Scan(&wp); scanErr != nil {
			logger.Warn("scan waveform sidecar", "err", scanErr)
			continue
		}
		out = append(out, wp)
	}
	if iterErr := rows.Err(); iterErr != nil {
		logger.Warn("iter waveform sidecars", "err", iterErr)
	}
	return out
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
func (s *Store) WipeAllTracks(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// **Iterator-error refusal** (CodeRabbit Major + Gemini High on
	// PR #210): a truncated enumeration would skip orphan-cleanup
	// for an unknown number of sidecars while still committing
	// the rows-wipe. Surface the error and let the caller retry
	// rather than commit a partial wipe whose on-disk leaks `--gc`
	// would have to clean up later.
	doomedSidecars, err := s.listAllSidecars(ctx)
	if err != nil {
		return err
	}
	// All waveform sidecars too (track_analysis). CASCADE clears the
	// rows on the tracks-wipe; the files are unlinked alongside the
	// variant sidecars below.
	doomedSidecars = append(doomedSidecars, s.listWaveformSidecars(ctx, "1=1")...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tracks`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	removeSidecarFiles(doomedSidecars)
	return nil
}

// WipeFilesystemTracks drops every FILESYSTEM-sourced track plus all
// folder rows, but SPARES UPnP-routed tracks (rows whose path is
// registered in `upnp_track_routing`). This is the correct primitive for
// the library-root add/remove storage-form flip: changing the FS root
// count rewrites every filesystem track's stored path form (bare
// "Artist/…" ↔ "<basename>/Artist/…"), so those rows must be wiped and
// re-scanned — but a UPnP-routed row carries the "<server>/…" form
// independent of the FS root count, and its lifecycle belongs solely to
// the upstream-ingest reconcile (the PR #370 "scanner spares routed rows"
// invariant). `WipeAllTracks` here would CASCADE-delete the entire
// upstream library + its cached enrichment (`tags_json` MBIDs), forcing a
// full re-ingest + re-enrich of every routed track — a severe,
// user-surprising side effect when a hybrid library (local roots + a
// Chord/MinimServer/… upstream) merely toggles its FS root count.
//
// Folder rows are filesystem-only (UPnP ingest never writes the `folders`
// table) AND their paths flip form with the root count, so ALL folder
// rows are wiped — the rescan rebuilds them. Spared UPnP tracks never
// carry variant or waveform sidecars (they're remote — there is no local
// file for the sox upscale/analysis pipelines to read), so listing every
// sidecar for removal can't orphan a spared row's cache. Same writer-lock
// + transaction + sidecar-cleanup contract as `WipeAllTracks`.
func (s *Store) WipeFilesystemTracks(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// UPnP-routed tracks can't be transcoded/analyzed, so every cached
	// sidecar belongs to a filesystem track being wiped here. Refuse a
	// truncated enumeration for the same reason WipeAllTracks does.
	doomedSidecars, err := s.listAllSidecars(ctx)
	if err != nil {
		return err
	}
	doomedSidecars = append(doomedSidecars, s.listWaveformSidecars(ctx, "1=1")...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// NOT EXISTS anti-join, keyed on the `upnp_track_routing` PRIMARY KEY
	// (`source_path`) so it stays index-backed even on a 15k-row upstream.
	// NOT EXISTS over NOT IN: idiomatic + NULL-safe should a future schema
	// change ever make source_path nullable (Gemini on PR #404).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tracks
		 WHERE NOT EXISTS (
			SELECT 1 FROM upnp_track_routing r WHERE r.source_path = tracks.path
		 )
	`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders`); err != nil {
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

// subtreeRangeBase reduces `prefix` to the slash-free base that the
// subtree byte-range bounds are built from, and reports whether the
// query should be scoped at all.
//
// Callers pair it with the clause
//
//	<col> COLLATE BINARY >= ? || '/' AND <col> COLLATE BINARY < ? || '0'
//
// binding the returned base TWICE. The clause is written out at each
// call site rather than assembled here: concatenating SQL reads as an
// injection risk to a reviewer and to SonarCloud's go:S2077 alike, even
// when every fragment is a compile-time literal.
//
// **This replaced a LIKE-pattern builder, and that is the point.**
// SQLite's default LIKE folds ASCII case (nothing sets
// case_sensitive_like — see OpenStore's DSN), so every one of these
// helpers silently covered case-twin sibling directories. On the write
// in this family — ResetEnrichedMissesUnderPrefix, one of the few
// sanctioned `enriched_at` writers — that meant re-queueing tracks in a
// folder the operator never selected, sending real MB/CAA/Deezer
// traffic for it. The range form also rides the BINARY-collated `path`
// PRIMARY KEY, where `LIKE 'prefix/%'` forced a full table scan.
//
// It exists so the decide-after-trim rule is applied structurally
// rather than restated at each call site. Two failure modes it closes,
// both of which have shipped here before:
//
//   - The bounds append their own '/', so a caller's trailing slash
//     builds `>= 'Album//'` and matches NOTHING — a silently-empty
//     result, not an error. TrimRight (not TrimSuffix) so "Album//"
//     doesn't keep one slash and rebuild the same broken bound.
//   - A prefix that is empty ONCE TRIMMED means whole-library. Deciding
//     before the trim makes "//" mean "everything" to some helpers and
//     "nothing" to others.
//
// scoped=false means "no WHERE clause" — the caller must NOT bind the
// returned base in that case.
func subtreeRangeBase(prefix string) (base string, scoped bool) {
	base = strings.TrimRight(prefix, "/")
	if base == "" {
		return "", false
	}
	return base, true
}

// subtreeLikePattern is the case-FOLDING sibling of subtreeRangeBase,
// applying the identical trim and decide-after-trim rules.
//
// **Pick deliberately, and default to subtreeRangeBase.** Case-folding
// is the wrong answer for anything that writes, deletes, or decides a
// scope the walk will be compared against — a case-twin sibling
// directory is a DIFFERENT directory on a case-sensitive filesystem,
// and treating it as the same one is how rows and their on-disk
// sidecars got destroyed. This form remains only where matching
// case-insensitively is the deliberate intent:
//
//   - ListVariantsByPathPrefix, whose query is explicitly
//     `unicode_lower(source_path) LIKE unicode_lower(?)` so the variant
//     GC finds sidecars written under a differently-cased source path
//     (PR #477).
//   - ListTrackProjectionsUnderPrefix, a display-only projection that
//     also needs a match-everything fallback the range form can't
//     express in one static statement.
//
// If you are adding a caller and cannot point at a reason case-folding
// is *wanted*, you want subtreeRangeBase.
func subtreeLikePattern(prefix string) (pattern string, scoped bool) {
	base, scoped := subtreeRangeBase(prefix)
	if !scoped {
		return "", false
	}
	return likeEscape(base) + `/%`, true
}

// TrackPaths returns every known track path (sorted), INCLUDING rows
// routed from a UPnP upstream. Used by the scanner's "remove tracks
// deleted from disk" pass.
//
// Including routed rows is load-bearing there and must stay: the
// deletion pass spares them by looking them up in the routed set, and
// it can only spare a row it was told to consider. Excluding them here
// would put every routed row outside the snapshot entirely — which is
// not "spared", it is invisible, and any future branch that reasons
// about the snapshot would silently do the wrong thing for 15k rows.
//
// Anything that wants "tracks that exist as files on this host" wants
// TrackPathsLocal instead.
func (s *Store) TrackPaths(ctx context.Context) ([]string, error) {
	return s.trackPaths(ctx, `SELECT path FROM tracks ORDER BY path ASC`)
}

// TrackPathsLocal returns every known FILESYSTEM track path (sorted) —
// TrackPaths minus anything routed from a UPnP upstream.
//
// Routed rows describe media on another device. They are real library
// entries and belong in the manifest, but they have no local file, so
// any caller that is going to resolve a path to something on disk gets
// a guaranteed miss for every one of them.
//
// The analysis sweep was that caller. On a hybrid library (89 local
// tracks, 15,283 routed from a Chord 2Go) each hourly sweep ran 15,283
// futile `ResolveChecked` calls and then reported the misses to the
// operator, so the Jobs page read `total 15372, missing 13553` beside a
// coverage block that correctly said `totalLocal 89`. The two numbers
// came from different queries with different ideas of what the library
// is; this is that anti-join, so they now agree by construction.
//
// The `NOT EXISTS` form (rather than `NOT IN`) matches
// AnalysisCoverage and the rest of the routed-exclusion sites: today
// `source_path` is the routing PK and non-null so the two are
// equivalent, but NOT EXISTS stays correct if that ever changes.
func (s *Store) TrackPathsLocal(ctx context.Context) ([]string, error) {
	return s.trackPaths(ctx, `
		SELECT t.path FROM tracks t
		WHERE NOT EXISTS (
			SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path
		)
		ORDER BY t.path ASC`)
}

// trackPaths is the shared scan body for the two path enumerations
// above. Reads are un-mutexed (WAL handles concurrent readers).
func (s *Store) trackPaths(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
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

// trackScopeBase reduces a scope directory to the slash-free prefix the
// byte-range bounds are built from, shared by TrackPathsUnder and
// FolderPathsUnder so the two can't drift on what "under relDir" means.
//
// The "/." test is deliberately on the two-character suffix, not on "."
// alone: relPath's multi-root whole-root sentinel is "<base>/.", but a
// real directory may legitimately END in a dot ("Artist/Album."), and
// trimming that would silently widen the scope to its parent.
//
// The trailing-slash trim mirrors CountTracksByPrefix's: the range
// appends its own '/', so a caller passing "Album/" would otherwise
// build `path >= 'Album//'` and match NOTHING — a silently-empty scope
// rather than an error. TrimRight, not TrimSuffix, so "Album//" can't
// keep one slash and rebuild the same broken bound.
func trackScopeBase(relDir string) string {
	if strings.HasSuffix(relDir, "/.") {
		relDir = strings.TrimSuffix(relDir, "/.")
	}
	return strings.TrimRight(relDir, "/")
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
//   - otherwise the scope is everything under "<relDir>/".
//
// Tracks are files, never directories, so a track path can never equal
// relDir itself — only the descendant range is needed. (Contrast
// FolderPathsUnder, which must also match the directory's own row.)
//
// Byte-range, NOT `LIKE`. This is the SCOPE SNAPSHOT for ScanSubtree's
// bounded deletion pass, and SQLite's default LIKE folds ASCII case
// (nothing sets case_sensitive_like). On a case-sensitive filesystem
// `Artist/Album` and `Artist/album` are two distinct real directories,
// so scanning one pulled the OTHER's rows into the snapshot. They are
// then absent from `seen` — and `caseOnlyRenames` fold-matches them to
// a path that WAS seen, so the pass reaped them outright, bypassing the
// missing_count debounce entirely.
//
// That reap is only sound because of the premise in caseOnlyRenames'
// docblock: "a stored path that fold-matches a seen entry refers to a
// file the walker DID enumerate this pass". True for a full Scan, false
// here the moment the snapshot is broader than the walk. Keeping this
// query case-exact is what keeps that premise true — the deletion pass
// cannot spare a row it was never told to consider.
//
// Used by ScanSubtree's bounded deletion pass.
func (s *Store) TrackPathsUnder(ctx context.Context, relDir string) ([]string, error) {
	if relDir == "" || relDir == "." {
		return s.TrackPaths(ctx)
	}
	base := trackScopeBase(relDir)
	if base == "" {
		// Only reachable from a malformed sentinel ("/."), which relPath
		// never produces. Whole-library scope matches the ""/"." branch.
		return s.TrackPaths(ctx)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM tracks
		  WHERE path COLLATE BINARY >= ? || '/'
		    AND path COLLATE BINARY < ? || '0'
		  ORDER BY path ASC`,
		base, base,
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
func (s *Store) UpsertFolder(ctx context.Context, f *Folder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folders(path, mtime_ns) VALUES (?, ?)
		ON CONFLICT(path) DO UPDATE SET
			mtime_ns      = excluded.mtime_ns,
			-- Same unconditional reset on confirm as UpsertTrack —
			-- see migration v5 + UpsertTrack docblock for the
			-- rationale (silent-empty-enumeration grace period).
			missing_count = 0
	`, f.Path, f.ModTime.UnixNano())
	return err
}

// FolderMTime returns the stored mtime for a folder path, or the zero time
// if absent.
func (s *Store) FolderMTime(ctx context.Context, path string) (time.Time, error) {
	var ns int64
	err := s.db.QueryRowContext(ctx, `SELECT mtime_ns FROM folders WHERE path = ?`, path).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, ns), nil
}

// ListFolders returns every folder record (sorted).
func (s *Store) ListFolders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, mtime_ns FROM folders ORDER BY path ASC`)
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
func (s *Store) FolderPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM folders ORDER BY path ASC`)
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
// Same normalizations as TrackPathsUnder, via the shared
// trackScopeBase. Unlike tracks (which are always files), the row for
// relDir itself is a real folder row the scanner upserted on its
// previous walk, so the match must include it — otherwise a "directory
// was renamed in place" event would leave the original folder row
// behind.
//
// **The `path = ?` term is load-bearing and must survive any future
// rewrite of this query.** It is the only thing that matches the
// directory's own row (and, in multi-root mode, the "<base>/."
// whole-root sentinel the walker upserts); the range covers strictly
// descendants. Drop it and folder rows stop being reaped on rename.
//
// Byte-range for the descendant half, for the same reason as
// TrackPathsUnder: SQLite's default LIKE folds ASCII case, so a
// case-twin sibling directory's rows were being pulled into a scope
// the walk never visits. See that function's docblock for what the
// deletion pass then does with them.
//
// Used by ScanSubtree's bounded deletion pass.
func (s *Store) FolderPathsUnder(ctx context.Context, relDir string) ([]string, error) {
	if relDir == "" || relDir == "." {
		return s.FolderPaths(ctx)
	}
	base := trackScopeBase(relDir)
	if base == "" {
		return s.FolderPaths(ctx)
	}
	// relDir (not base) on the exact-match arm: for the multi-root
	// sentinel the stored row IS "<base>/.", so matching `base` would
	// miss it.
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM folders
		  WHERE path = ?
		     OR (path COLLATE BINARY >= ? || '/' AND path COLLATE BINARY < ? || '0')
		  ORDER BY path ASC`,
		relDir, base, base,
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
func (s *Store) DeleteFolder(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM folders WHERE path = ?`, path)
	return err
}

// IncrementMissingTracksAndDeleteAtThreshold bumps missing_count on
// every row whose path is in `missingPaths` AND atomically deletes any
// row whose new missing_count >= threshold. Returns the number of rows
// that were deleted.
//
// This is the operational core of the silent-empty-enumeration defence
// (see migration v5 doc). The scanner identifies tracks that were in
// the before-snapshot but NOT in the seen-set this pass AND whose
// subtree is NOT under an errorSubtree guard, and passes that list
// here at end-of-scan. Rows survive (threshold - 1) consecutive
// missing scans before being reaped; that grace period absorbs the
// transient-but-undetected failure modes (SMB re-auth flap, NFS
// brownout, libsmb2 timeout returning an empty Readdir) that
// errorSubtrees can't catch because no error surfaced to fire it.
//
// **Sidecar cleanup: this path does NOT proactively remove on-disk
// sidecar files.** `DeleteTrack` (line 559) walks the `track_variants`
// rows for its parent and `os.Remove`s each sidecar BEFORE issuing
// the DELETE, maintaining the "no orphans on disk" invariant. This
// bulk path is hot-loop in the scanner deletion pass and would pay
// an N-row SELECT + Stat-cluster per scan to do the same — measured
// to be the dominant scanner cost on a 50k-track library, so we
// accept the trade-off of leaving orphan `.flac` sidecars in the
// `transcoded/` directory until the next `bridge upscale --gc` run.
// Operators running large-scale reaping events (decommissioning a
// mount, mass library cleanup) should follow up with `bridge upscale
// --gc` to reclaim the disk space. CASCADE on the parent track row
// still cleans up the `track_variants` SQLite row itself; only the
// on-disk file persists. Gemini bot review on PR #193 flagged this
// docstring inaccuracy.
//
// Both the increment and the delete happen in a single SQLite
// transaction so a crash between them can't leave the counter
// half-bumped — re-run on next scan would re-increment from the
// same starting value, accelerating the eventual delete by one
// scan. (The opposite ordering — delete-then-bump — would risk
// deleting a row whose counter increment had committed but whose
// matching upsert reset was still pending in a parallel writer.)
//
// `threshold` MUST be >= 1. Callers using the YAML default of 3
// preserve the legacy immediate-delete behaviour when set to 1.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) IncrementMissingTracksAndDeleteAtThreshold(ctx context.Context, missingPaths []string, threshold int) (int64, error) {
	if len(missingPaths) == 0 || threshold < 1 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE tracks SET missing_count = missing_count + 1 WHERE path = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, p := range missingPaths {
		// ExecContext (not Exec) so a cancelled / timed-out scan stops
		// the per-row loop instead of running to completion ctx-blind.
		res, err := stmt.ExecContext(ctx, p)
		if err != nil {
			return 0, err
		}
		// Diagnostic: a missingPaths entry that doesn't match any
		// row is a scanner / store-state desync. Pre-fix the
		// silent `_` discard would mask a path-shape drift
		// (e.g. trailing slash mismatch, case-fold drift after
		// schema migration) — every miss-increment would no-op
		// and `IncrementMissingFoldersAndDelete` would never
		// fire its deletion path. Log at warn (the bridge is
		// still functional; this is operator-actionable
		// detection of a real bug upstream).
		if affected, raErr := res.RowsAffected(); raErr == nil && affected == 0 {
			logger.Warn("IncrementMissingTracks: missing path did not match any row",
				"path", p,
			)
		}
	}
	// The delete is SCOPED to the paths this pass actually observed
	// missing, not to every row that happens to sit at the threshold.
	//
	// The scanner has already withheld anything under an errored
	// subtree from `missingPaths` (isUnderErroredSubtree), so scoping
	// makes that guard cover the SQL too. Unscoped it did not: a row
	// already at or above the threshold is reaped by a bare
	// `missing_count >= ?` even on a pass that never looked at it —
	// which is exactly a pass where its subtree errored. The route in
	// is lowering DeleteAfterMissingScans, since rows parked below the
	// old threshold are instantly at-or-above the new one and the very
	// next scan sweeps them whether or not it could see them.
	//
	// This generalises the rule the deletion pass already follows and
	// that has regressed twice (#549, #568): "we could not see this
	// path" must dominate every "…but it looks reapable"
	// classification. A bare threshold predicate is that same mistake
	// expressed in SQL instead of Go.
	//
	// Nothing is stranded by scoping. A genuinely-absent row is in the
	// next scan's `missingPaths`, gets incremented, and is reaped in
	// that pass — at most one scan later than before, and only ever
	// after a pass that actually observed it.
	//
	// The set travels as ONE bound JSON array consumed by json_each
	// (the ResetEnrichedByArtistMBIDs idiom): a single static statement
	// with no placeholder construction and no bind-ceiling chunking,
	// which matters here because a whole-root outage can put tens of
	// thousands of paths in this list.
	//
	// The routed exclusion stays as defense-in-depth behind the
	// scanner-side one (see UPnPRoutedSourcePaths): rows that pre-date
	// that exclusion may carry stale increments, and no caller may ever
	// threshold-delete a routed row — its lifecycle is the ingest's
	// last_seen_at reap, which has its own offline / truncated-walk
	// protections.
	missingBlob, err := json.Marshal(missingPaths)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM tracks
		 WHERE missing_count >= ?
		   AND path IN (SELECT value FROM json_each(?))
		   AND path NOT IN (SELECT source_path FROM upnp_track_routing)
	`, threshold, string(missingBlob))
	if err != nil {
		return 0, err
	}
	deleted, raErr := res.RowsAffected()
	if raErr != nil {
		// The DELETE ran; only the count read failed (modernc/sqlite
		// permits it though it shouldn't in practice). Surface it rather
		// than silently returning a 0 the scanner logs as an undercount,
		// and still commit the reap. Matches RecoverInterruptedBatches.
		logger.Warn("IncrementMissingTracks: RowsAffected after threshold delete failed", "err", raErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// IncrementMissingFoldersAndDeleteAtThreshold is the folders-table sibling
// of IncrementMissingTracksAndDeleteAtThreshold. Same contract, same
// rationale — phantom directories from transient mount failures must
// NOT linger in the folder listing surface across N consecutive scans
// that didn't see them either.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) IncrementMissingFoldersAndDeleteAtThreshold(ctx context.Context, missingPaths []string, threshold int) (int64, error) {
	if len(missingPaths) == 0 || threshold < 1 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE folders SET missing_count = missing_count + 1 WHERE path = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, p := range missingPaths {
		// ExecContext (not Exec) so a cancelled / timed-out scan stops
		// the per-row loop instead of running to completion ctx-blind.
		res, err := stmt.ExecContext(ctx, p)
		if err != nil {
			return 0, err
		}
		// Mirror IncrementMissingTracks: a zero-rows-affected
		// here points at the same path-shape drift bug. Same
		// warn-and-continue policy.
		if affected, raErr := res.RowsAffected(); raErr == nil && affected == 0 {
			logger.Warn("IncrementMissingFolders: missing path did not match any row",
				"path", p,
			)
		}
	}
	// Scoped to this pass's observed-missing set for the reason spelled
	// out on the tracks twin: unscoped, a bare `missing_count >= ?`
	// reaps rows on a pass that never looked at them, which is exactly
	// the pass where their subtree errored. Folders had NO exclusion of
	// any kind here — not even the routed anti-join its sibling
	// carries — so it was the weaker of the two.
	//
	// Folders are filesystem-only (UPnP ingest never writes this
	// table), so there is deliberately no routing anti-join to mirror.
	missingBlob, err := json.Marshal(missingPaths)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM folders
		 WHERE missing_count >= ?
		   AND path IN (SELECT value FROM json_each(?))
	`, threshold, string(missingBlob))
	if err != nil {
		return 0, err
	}
	deleted, raErr := res.RowsAffected()
	if raErr != nil {
		// See IncrementMissingTracks: surface the count-read failure
		// rather than logging a silent 0-row undercount, still commit.
		logger.Warn("IncrementMissingFolders: RowsAffected after threshold delete failed", "err", raErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// PendingDeletions returns the total count of rows across `tracks` and
// `folders` that have a non-zero missing_count but haven't yet hit the
// delete threshold. Exposed for the /v1/health ScanState surface and
// the admin dashboard "X rows pending deletion" hint. Two filtered
// COUNT(*) scans — there is NO dedicated missing_count index, so the
// `missing_count > 0` predicate is a table scan; the /v1/health caller
// TTL-caches the result (healthCountsCache) so an unauthenticated flood
// can't run these per request. Nil-safe under closed Store (returns 0, nil).
func (s *Store) PendingDeletions(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var tracks, folders int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracks WHERE missing_count > 0`).Scan(&tracks); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE missing_count > 0`).Scan(&folders); err != nil {
		return 0, err
	}
	return tracks + folders, nil
}

// ResetTrackMissingCount sets `missing_count = 0` on the row keyed by
// path. Used by the scanner's early-skip path (mtime+size unchanged →
// no extract → no upsert) so a track that was previously marked missing
// still has its counter reset when it reappears unchanged. Without this,
// the unconditional reset in UpsertTrack only fires on the slow path,
// and a flap-then-restore on a long-stable library leaves the counter
// stuck — eventually crossing the threshold and reaping the still-on-
// disk row. Caught by Gemini bot review on PR #193.
//
// Cheap single-row UPDATE on the PRIMARY KEY index. Missing rows
// silently no-op (RowsAffected == 0). Holds `s.mu` per the writer
// contract on Store.
func (s *Store) ResetTrackMissingCount(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET missing_count = 0 WHERE path = ? AND missing_count != 0`, path)
	return err
}

// ClearMissingCounts wipes all rows where missing_count > 0 in tracks +
// folders. Used by the `bridge manifest clear-missing` operator escape
// hatch — an operator who KNOWS a mount has been permanently removed
// and doesn't want to wait N scans for cleanup. Returns the total
// number of rows deleted across both tables.
//
// UPnP-routed tracks are SPARED regardless of their missing_count — the
// tracks delete carries a NOT EXISTS anti-join against upnp_track_routing
// (the codebase-preferred anti-join form since PR #404 — idiomatic +
// NULL-safe; equivalent to the guard on IncrementMissingTracksAndDelete-
// AtThreshold, PR #370). Routed rows can hold stale increments from before
// the scanner-side missing-pass exclusion landed, and their lifecycle
// belongs solely to the ingest's last_seen_at reap — NO caller may
// threshold-delete a routed row. Without the guard this escape hatch
// would silently wipe a live UPnP upstream (and its cached tags_json
// enrichment) on an operator who ran clear-missing to purge an unrelated
// decommissioned filesystem mount. The folders delete needs no guard —
// folders are filesystem-only, never routed.
//
// Both deletes happen in one transaction so a crash mid-clear can't
// leave the operator with a partially-purged state — re-running the
// command picks up exactly the rows the prior attempt missed.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) ClearMissingCounts(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	tRes, err := tx.ExecContext(ctx, `
		DELETE FROM tracks
		 WHERE missing_count > 0
		   AND NOT EXISTS (
			SELECT 1 FROM upnp_track_routing r WHERE r.source_path = tracks.path
		   )`)
	if err != nil {
		return 0, err
	}
	fRes, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE missing_count > 0`)
	if err != nil {
		return 0, err
	}
	// Both DELETEs already ran in the tx; a count-read failure
	// (modernc/sqlite permits it, though it shouldn't in practice) must
	// not silently undercount the operator's cleared total — surface it
	// and still commit. Matches RecoverInterruptedBatches.
	tCount, tErr := tRes.RowsAffected()
	if tErr != nil {
		logger.Warn("ClearMissingCounts: RowsAffected for tracks delete failed", "err", tErr)
	}
	fCount, fErr := fRes.RowsAffected()
	if fErr != nil {
		logger.Warn("ClearMissingCounts: RowsAffected for folders delete failed", "err", fErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return tCount + fCount, nil
}

// ----- scan_state -----

// SetScanState writes a key/value pair to the scan_state table.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) SetScanState(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO scan_state(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v
	`, key, value)
	return err
}

// GetScanState returns the value for key, or "" if missing.
func (s *Store) GetScanState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM scan_state WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ----- upscale target settings (v1.3) -----

// scan_state keys carrying the operator-controlled upscale target.
// Stored as decimal strings; parsing happens at the boundary. Centralised
// constants so the seeder (cmd/bridge/main.go) and the reader can't
// drift on key naming.
const (
	UpscaleTargetRateKey = "upscale_target_hz"
	UpscaleTargetBitsKey = "upscale_target_bits"
)

// ErrUpscaleTargetUnset is returned by GetUpscaleTarget when neither
// scan_state row has been seeded. Callers (typically the coordinator
// at job-Submit time) should fall back to their config-derived default.
var ErrUpscaleTargetUnset = errors.New("upscale target not configured")

// GetUpscaleTarget returns the operator-chosen target rate (Hz) and
// bit depth (16 / 24 / 32) from scan_state. Returns ErrUpscaleTargetUnset
// if either key is missing — the coordinator should seed both with the
// `bridge.yaml` bootstrap defaults via SetUpscaleTarget at startup.
//
// Parse failures (non-numeric stored value) are surfaced as wrapped
// errors rather than silently falling back: a malformed value means
// either a buggy writer or DB corruption, and silently substituting a
// default would mask the bug.
func (s *Store) GetUpscaleTarget(ctx context.Context) (rateHz int, bits int, err error) {
	// Single query reads both keys in one round-trip — atomic vs a
	// concurrent SetUpscaleTarget that could otherwise commit
	// between two separate GetScanState calls and leave callers
	// with a mismatched (rate, bits) pair. Per CodeRabbit medium
	// on PR #199.
	rows, qerr := s.db.QueryContext(ctx,
		`SELECT k, v FROM scan_state WHERE k IN (?, ?)`,
		UpscaleTargetRateKey, UpscaleTargetBitsKey,
	)
	if qerr != nil {
		return 0, 0, fmt.Errorf("read upscale target: %w", qerr)
	}
	defer rows.Close()
	var rateStr, bitsStr string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return 0, 0, fmt.Errorf("scan upscale target: %w", err)
		}
		switch k {
		case UpscaleTargetRateKey:
			rateStr = v
		case UpscaleTargetBitsKey:
			bitsStr = v
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iter upscale target: %w", err)
	}
	if rateStr == "" || bitsStr == "" {
		return 0, 0, ErrUpscaleTargetUnset
	}
	r, err := strconv.Atoi(rateStr)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s=%q: %w", UpscaleTargetRateKey, rateStr, err)
	}
	b, err := strconv.Atoi(bitsStr)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s=%q: %w", UpscaleTargetBitsKey, bitsStr, err)
	}
	// Validate parsed values — a manually-edited scan_state row
	// (sqlite3 CLI session, leaked bug, etc.) could leak garbage
	// into callers. Defense-in-depth alongside the validate-on-
	// write contract in SetUpscaleTarget. Per CodeRabbit medium
	// on PR #199.
	if r <= 0 {
		return 0, 0, fmt.Errorf("invalid upscale target rate %d Hz in scan_state", r)
	}
	switch b {
	case 16, 24, 32:
	default:
		return 0, 0, fmt.Errorf("invalid upscale target bits %d in scan_state (want 16/24/32)", b)
	}
	return r, b, nil
}

// SetUpscaleTarget writes both keys atomically (single transaction so
// a reader after a partial-failure write never sees a mismatched
// rate/bits pair). Rate must be > 0, bits ∈ {16, 24, 32}; out-of-range
// values are rejected with a typed error before the write commits.
func (s *Store) SetUpscaleTarget(ctx context.Context, rateHz, bits int) error {
	if rateHz <= 0 {
		return fmt.Errorf("upscale target rate %d Hz: must be positive", rateHz)
	}
	switch bits {
	case 16, 24, 32:
	default:
		return fmt.Errorf("upscale target bits %d: must be 16, 24, or 32", bits)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	const upsert = `
		INSERT INTO scan_state(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v
	`
	if _, err := tx.ExecContext(ctx, upsert, UpscaleTargetRateKey, strconv.Itoa(rateHz)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, upsert, UpscaleTargetBitsKey, strconv.Itoa(bits)); err != nil {
		return err
	}
	return tx.Commit()
}

// ----- upscale_batches CRUD (v1.3) -----

// UpscaleBatchRow is the on-disk record for one operator-initiated
// batch. Constructed by the coordinator at Submit time; updated as
// the pool drains the contained jobs. PR 3 introduces the full CRUD
// surface; PR 1 ships InsertUpscaleBatch + RecoverInterruptedBatches
// so the migration v6 schema is exercised end-to-end and bridge
// startup can clean up after a crash.
type UpscaleBatchRow struct {
	// ID is a UUID minted by the coordinator at Submit time. Stored
	// as the raw 16-byte BLOB; uuid.UUID's `[16]byte` underlying
	// type makes the conversion trivial at the SQL boundary.
	ID uuid.UUID
	// Path is the library-relative scope (folder, root, or empty for
	// whole-library). Normalisation is the caller's responsibility.
	Path string
	// TargetRate is the resolved output rate in Hz (e.g. 192000),
	// captured at Submit time. A mid-run change to the global setting
	// does not retroactively shift this batch.
	TargetRate int
	// TargetBits is the resolved output bit depth (16/24/32).
	TargetBits int
	// Status is one of 'pending','running','completed','failed',
	// 'cancelled','interrupted'. The CHECK constraint at the SQL
	// layer enforces the enum; an invalid value rejects the insert.
	Status string
	// Counters are bumped by the coordinator on each pool callback
	// (PR 3). PR 1 inserts them at zero or seed values.
	TotalFiles     int
	ProcessedFiles int
	FailedFiles    int
	// SkippedFiles counts tracks the projection saw but Submit /
	// SubmitOptimize did NOT enqueue (rate>target, bits>target,
	// rate==target && bits==target no-op, failed OptimizeEligible,
	// DSD, zero/missing rate-or-bits). Distinct from
	// `AlreadyCovered` (returned in SubmitResult but not persisted)
	// and from FailedFiles (per-job SoX failures during the run).
	// Writeable at insert time only — the Coordinator computes it
	// from the projection loop alongside the candidate list and
	// never updates it post-insert. v1.5 migration v9 added the
	// column; pre-migration rows carry 0.
	SkippedFiles int
	// Error holds redacted sox stderr / coordinator-side failure
	// reason. Empty on the happy path. Multi-line tolerated.
	Error string
	// CreatedAt / UpdatedAt are nanosecond Unix epochs.
	CreatedAt int64
	UpdatedAt int64
}

// InsertUpscaleBatch writes one row. Used by the coordinator at
// Submit time; also called directly from tests covering migration v6.
// The status value is validated against the SQL CHECK constraint by
// SQLite; an invalid string surfaces as a constraint failure.
//
// Holds s.mu per the writer contract.
func (s *Store) InsertUpscaleBatch(ctx context.Context, row UpscaleBatchRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO upscale_batches
			(id, path, target_rate, target_bits, status,
			 total_files, processed_files, failed_files,
			 error, created_at, updated_at, skipped_files)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	`,
		row.ID[:], row.Path, row.TargetRate, row.TargetBits, row.Status,
		row.TotalFiles, row.ProcessedFiles, row.FailedFiles,
		row.Error, row.CreatedAt, row.UpdatedAt, row.SkippedFiles,
	)
	return err
}

// UpdateUpscaleBatchProgress writes the counter columns + status
// + error + updated_at on an existing row. Called from the
// coordinator's pool-callback path; the row is identified by
// row.ID. Idempotent on `id NOT FOUND` (returns nil with zero
// rows affected — the coordinator deliberately calls this against
// rows whose terminal status may have been written by a sibling
// path).
//
// Holds s.mu per the writer contract.
func (s *Store) UpdateUpscaleBatchProgress(ctx context.Context, row UpscaleBatchRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Guard: a non-terminal progress write must not resurrect a batch that has
	// already reached a terminal status (the cancel-vs-late-completion LWW race,
	// B3) — the persist runs outside c.mu, so a stale `running` snapshot could
	// otherwise land after a terminal write. The full terminal set is
	// completed/failed/cancelled/interrupted (see isTerminalStatus in batch.go);
	// 'interrupted' is stamped by boot recovery, so a straggler callback after a
	// crash/restart must not revive it either (Gemini PR #515 round 2).
	// `total_files` is written here as well as at INSERT, because the
	// coordinator SHRINKS it after the row exists: dropDedupedPath
	// decrements per candidate deduped against an in-flight job, and a
	// queue-full break sets it to the real enqueued count. Pre-fix no
	// UPDATE in the codebase touched the column at all, so every one of
	// those adjustments lived only in `liveBatches` and the persisted
	// row kept its pre-dedup total FOREVER — it never self-heals,
	// because the job-completion callbacks persist through this same
	// statement and the batch is dropped from liveBatches once
	// terminal. SSE (in-memory) rendered 7/7 while the admin Jobs page
	// and GET /v1/upscale/batches (both DB-backed) rendered 7/10.
	//
	// CASE WHEN, not a bare assignment: TotalFiles is monotonically
	// NON-INCREASING after INSERT (the only writers are the decrement
	// and the truncation set), so this makes a stale snapshot landing
	// out of order unable to regress the column. Same strict-monotonic
	// shape as the indexed_at writes in UpsertTrack / UpsertVariant.
	_, err := s.db.ExecContext(ctx, `
		UPDATE upscale_batches
		   SET status         = ?,
		       total_files    = CASE WHEN total_files > ? THEN ? ELSE total_files END,
		       processed_files = ?,
		       failed_files    = ?,
		       error          = ?,
		       updated_at     = ?
		 WHERE id = ?
		   AND status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
	`, row.Status, row.TotalFiles, row.TotalFiles,
		row.ProcessedFiles, row.FailedFiles, row.Error, row.UpdatedAt, row.ID[:])
	return err
}

// UpdateUpscaleBatchStatus is the narrow flavour of
// UpdateUpscaleBatchProgress for the cases that flip status + error +
// updated_at without touching the processed/failed counters. Used by
// `Coordinator.Cancel`, `transitionStatus`, and pending→running
// promotion at Submit.
//
// It DOES carry total_files, under the same monotonic guard as the
// progress form. The fully-deduped batch reaches its terminal status
// through here and nowhere else, so without this a re-submit that
// overlaps an in-flight batch entirely would persist `0/N` — the
// coordinator correctly shrinks the live row to 0, and the DB kept the
// original candidate count. Every caller routes through
// `transitionStatus`, which always builds its row copy from the live
// `liveBatches` entry, so the value is authoritative at each site.
func (s *Store) UpdateUpscaleBatchStatus(ctx context.Context, row UpscaleBatchRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Same terminal guard the progress form carries. transitionStatus
	// builds its row copy under Coordinator.mu but persists OUTSIDE it,
	// so two concurrent transitions (Submit's pending→running promotion
	// racing a Cancel) can land in the opposite order and let the
	// non-terminal write clobber the terminal one. Nothing corrects it
	// afterwards: the terminal transition removed the batch from
	// liveBatches, so no later callback revisits the row and it sits
	// `running` forever.
	//
	// Boot recovery's 'interrupted' stamp is a separate statement
	// (RecoverInterruptedBatches) and is unaffected. Blocking a
	// terminal→terminal write costs nothing either: transitionStatus
	// early-returns once the batch has left liveBatches, so the only
	// writes this rejects are the stale racing ones.
	_, err := s.db.ExecContext(ctx, `
		UPDATE upscale_batches
		   SET status      = ?,
		       total_files = CASE WHEN total_files > ? THEN ? ELSE total_files END,
		       error       = ?,
		       updated_at  = ?
		 WHERE id = ?
		   AND status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
	`, row.Status, row.TotalFiles, row.TotalFiles,
		row.Error, row.UpdatedAt, row.ID[:])
	return err
}

// ListUpscaleBatches returns up to `limit` rows ordered by
// created_at DESC. limit ≤ 0 falls back to a sensible default
// (100, matching the admin Jobs page's pagination). Used by the
// `/v1/upscale/batches` endpoint and the admin Jobs page.
func (s *Store) ListUpscaleBatches(ctx context.Context, limit int) ([]UpscaleBatchRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, target_rate, target_bits, status,
		       total_files, processed_files, failed_files,
		       COALESCE(error, ''), created_at, updated_at,
		       skipped_files
		  FROM upscale_batches
		 ORDER BY created_at DESC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list upscale batches: %w", err)
	}
	defer rows.Close()
	out := []UpscaleBatchRow{}
	for rows.Next() {
		var (
			row    UpscaleBatchRow
			idBlob []byte
		)
		if err := rows.Scan(
			&idBlob, &row.Path, &row.TargetRate, &row.TargetBits, &row.Status,
			&row.TotalFiles, &row.ProcessedFiles, &row.FailedFiles,
			&row.Error, &row.CreatedAt, &row.UpdatedAt,
			&row.SkippedFiles,
		); err != nil {
			return nil, err
		}
		if len(idBlob) != 16 {
			return nil, fmt.Errorf("list upscale batches: id blob length %d, want 16", len(idBlob))
		}
		copy(row.ID[:], idBlob)
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecoverInterruptedBatches transitions any `pending` or `running`
// batches to `interrupted` and is intended to run exactly once at
// bridge boot, BEFORE the coordinator accepts new submissions.
//
// Why this matters: a crash, `kill -9`, or scheduler-shutdown signal
// mid-batch leaves rows stuck in `running` forever. The admin Jobs
// page would render them as phantom in-flight batches and the
// coordinator's in-memory state (which is rebuilt empty at boot)
// could never resync. The transition gives the operator a
// deterministic "interrupted" status they can Retry from the Jobs
// page — `Coordinator.Submit` already filters tracks that have
// variants at the target, so retry picks up only the still-unfinished
// work.
//
// Returns the number of rows updated for observability (admin can log
// it). Idempotent — repeated calls are no-ops once all rows have
// terminal status.
//
// Holds s.mu per the writer contract.
func (s *Store) RecoverInterruptedBatches(ctx context.Context, nowUnixNS int64) (rowsAffected int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE upscale_batches
		   SET status = 'interrupted', updated_at = ?
		 WHERE status IN ('pending','running')
	`, nowUnixNS)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// RowsAffected on modernc/sqlite shouldn't error in practice,
		// but the contract permits it. Treat as success-without-count
		// rather than fail the boot path.
		return 0, nil
	}
	return n, nil
}

// ----- Library Browse (v1.3 admin Library Inspector) -----

// Variant-kind prefix discriminators for `variant_id` LIKE filters.
// These MIRROR `transcode.VariantPrefixUpscaled` /
// `transcode.VariantPrefixOptimized` (transcode/transcode.go:87–88)
// and are duplicated here because the `manifest` package can't import
// `transcode` (would be circular — transcode imports manifest). The
// trailing `-` byte appended at LIKE-pattern construction time is
// defensive against any hypothetical `optimizedX-...` collision;
// current variant IDs always have `<prefix>-<schemaVersion>-…`
// shape so this is belt-and-braces, not load-bearing.
//
// **Version-agnostic** — match `upscaled-%` / `optimized-%`, NOT
// `upscaled-v2-%`. The transcode docblock at transcode.go:55–63
// explicitly preserves pre-v2 sidecars until `bridge upscale --gc`
// runs; filtering by `v2` would silently hide legacy v1 coverage
// from the inspector counters.
//
// Exported so admin / transcode callers pass these into
// `ListTrackProjectionsUnderPrefix(ctx, prefix, kind)` rather than
// repeating the literal "upscaled" / "optimized" string at every
// call site.
const (
	VariantKindPrefixUpscaled  = "upscaled"
	VariantKindPrefixOptimized = "optimized"
)

// childFolderRollupSelect is the shared SELECT-projection block used
// by ListChildFolders + ListChildFoldersPage's `parent == ""` AND
// `parent != ""` branches. Extracted as a string constant so the
// 40-line correlated-subquery body isn't duplicated four times
// (was a SonarCloud duplication trip on PR #276 round 3).
//
// Caller appends the `FROM folders f WHERE …` clause + bind params.
// Result column order MUST stay in lockstep with the per-row
// `rows.Scan(&row.Path, &row.TrackCount, &row.UpscaledTrackCount,
// &row.OptimizedTrackCount, &row.TotalSizeBytes,
// &row.UpscaledSizeBytes, &row.OptimizedSizeBytes)` shape.
//
// **Variant counters / sizes split by `variant_id` prefix** — see
// VariantKindPrefix* docblock above for why the LIKE pattern is
// version-agnostic (`upscaled-%` matches both v1 legacy AND v2
// sidecars). **Range comparisons (not LIKE) on `f.path`** prevent
// LIKE-metacharacter collisions on folder names containing `_` or
// `%`; `0` (0x30) is the ASCII byte after `/` (0x2F) so the
// exclusive upper bound captures everything starting with
// "f.path/" and nothing past — index-friendly range scan against
// the existing PRIMARY KEY on tracks(path) / track_variants(source_path).
// Per Gemini medium on PR #200.
const childFolderRollupSelect = `
	SELECT f.path,
	       (SELECT COUNT(*) FROM tracks t
	          WHERE t.path >= f.path || '/' AND t.path < f.path || '0') AS track_count,
	       (SELECT COUNT(DISTINCT tv.source_path) FROM track_variants tv
	          WHERE tv.source_path >= f.path || '/' AND tv.source_path < f.path || '0'
	            AND tv.variant_id LIKE 'upscaled-%') AS upscaled_count,
	       (SELECT COUNT(DISTINCT tv.source_path) FROM track_variants tv
	          WHERE tv.source_path >= f.path || '/' AND tv.source_path < f.path || '0'
	            AND tv.variant_id LIKE 'optimized-%') AS optimized_count,
	       (SELECT COALESCE(SUM(t.size), 0) FROM tracks t
	          WHERE t.path >= f.path || '/' AND t.path < f.path || '0') AS total_size,
	       (SELECT COALESCE(SUM(tv.size_bytes), 0) FROM track_variants tv
	          WHERE tv.source_path >= f.path || '/' AND tv.source_path < f.path || '0'
	            AND tv.variant_id LIKE 'upscaled-%') AS upscaled_size,
	       (SELECT COALESCE(SUM(tv.size_bytes), 0) FROM track_variants tv
	          WHERE tv.source_path >= f.path || '/' AND tv.source_path < f.path || '0'
	            AND tv.variant_id LIKE 'optimized-%') AS optimized_size`

// childTrackRowSelect mirrors childFolderRollupSelect for the
// per-track browse rows (ListChildTracks + ListChildTracksPage's
// two branches). Same dedup motivation. Result column order
// matches the `rows.Scan(&ct.Path, &ct.Size, &rate, &bits, &codec,
// &isDSDRaw, &upscaled, &optimized)` call site.
const childTrackRowSelect = `
	SELECT t.path, t.size,
	       json_extract(t.tags_json, '$.sampleRate')    AS sample_rate,
	       json_extract(t.tags_json, '$.bitsPerSample') AS bits_per_sample,
	       json_extract(t.tags_json, '$.codec')         AS codec,
	       json_extract(t.tags_json, '$.isDSD')         AS is_dsd,
	       EXISTS(SELECT 1 FROM track_variants tv
	               WHERE tv.source_path = t.path
	                 AND tv.variant_id LIKE 'upscaled-%')  AS is_upscaled,
	       EXISTS(SELECT 1 FROM track_variants tv
	               WHERE tv.source_path = t.path
	                 AND tv.variant_id LIKE 'optimized-%') AS is_optimized`

// FolderRollup is the recursive aggregation under one path prefix:
// total tracks, tracks with at least one variant (split by kind),
// sum of source sizes, sum of variant sizes (split by kind). Used
// by `RollupByPrefix` and embedded in `ChildFolderRollup` for browse
// rows.
//
// Sizes are int64 bytes; counts are int (a library plausibly past
// MaxInt32 tracks is implausible for the bridge's target deployment).
//
// **Per-kind split** (PR feat/library-inspector-tiles): variant
// counters and size totals were previously kind-agnostic (any
// `track_variants` row counted). Splitting by `variant_id` prefix
// (`upscaled-%` vs `optimized-%`) lets the Library Inspector render
// dual coverage bars per folder tile without an extra round-trip.
// The legacy field names `UpscaledTrackCount` / `UpscaledSizeBytes`
// are preserved (now scoped to upscale variants only); the new
// `Optimized*` fields surface the CarPlay-optimize variant counts.
type FolderRollup struct {
	TrackCount          int
	UpscaledTrackCount  int
	OptimizedTrackCount int
	TotalSizeBytes      int64
	UpscaledSizeBytes   int64
	OptimizedSizeBytes  int64
}

// ChildFolderRollup represents one folder row in the admin Library
// Inspector's tree view. Each carries the recursive rollup for its
// subtree so the UI can render the per-folder status ring + size
// summary without a follow-up call per row.
type ChildFolderRollup struct {
	Path string
	FolderRollup
}

// ChildTrack is the projection a track exposes to the browse API.
// Reads json_extract for sample rate / bit depth / codec / isDSD so
// the admin endpoint doesn't deserialise the full tags_json blob for
// every row. `IsUpscaled` / `IsOptimized` are determined by EXISTS
// subqueries against `track_variants`, filtered by `variant_id`
// prefix so the two flags discriminate kind.
type ChildTrack struct {
	Path          string
	Size          int64
	SampleRate    *float64
	BitsPerSample *int
	Codec         string
	IsDSD         *bool
	IsUpscaled    bool
	IsOptimized   bool
}

// topLevelFSFolderSource is the row-source for the empty-parent ("library
// root") browse: the DISTINCT first path segment of every FILESYSTEM
// track, exposed as column `path` so it drops into childFolderRollupSelect's
// `f.path` correlated rollups unchanged (and into `SELECT COUNT(*) FROM …`
// for the count path).
//
// Derived from `tracks` rather than the `folders` table because that table
// has two blind spots at the root level: (1) in MULTI-ROOT mode the scanner
// records each root's contents under "<basename>/…" but never inserts a
// bare "<basename>" folder row, so the old `WHERE instr(path,'/')=0` match
// found nothing and the inspector root rendered empty; (2) UPnP-upstream
// ingest never writes the folders table at all. Deriving from track paths
// fixes (1) — single-root album folders and multi-root basenames both fall
// out as the first segment. The `NOT IN (upnp_track_routing)` anti-join
// (keyed on that table's PRIMARY KEY, so it's index-backed) deliberately
// keeps (2) hidden: UPnP-routed tracks are remote and can't be
// upscaled/optimized, so the inspector — a variant-generation surface —
// intentionally lists only filesystem roots. `instr(path,'/')>0` skips
// loose root-level files, which surface via the empty-parent
// ListChildTracks* path instead.
const topLevelFSFolderSource = `(
		SELECT DISTINCT substr(t.path, 1, instr(t.path, '/') - 1) AS path
		  FROM tracks t
		 WHERE instr(t.path, '/') > 0
		   AND NOT EXISTS (
			SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path
		   )
	) f`

// ListChildFolders returns the immediate-child folders of `parent`,
// each carrying rollup counters for its subtree (tracks + upscaled
// variants). `parent` is the library-relative folder path — no
// leading slash, no trailing slash. Empty string means "the library
// root" and returns the filesystem roots (multi-root basenames or
// single-root album folders), derived from track paths via
// topLevelFSFolderSource so multi-root + UPnP-hybrid libraries render
// correctly.
//
// The rollup is computed via correlated subqueries against `tracks`
// and `track_variants`. Loopback admin-only callers; no rate limit
// needed. SQLite plans the subqueries against the existing PRIMARY
// KEY indexes (tracks.path, track_variants.source_path).
func (s *Store) ListChildFolders(ctx context.Context, parent string) ([]ChildFolderRollup, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if parent == "" {
		// Top-level: folders whose path has no slash. Matches
		// multi-root basenames (e.g. "MusicRootA") AND single-root
		// album folders (e.g. "AlbumA"). SELECT projection shared
		// via childFolderRollupSelect — see its docblock for the
		// per-kind variant_id LIKE split + the range-comparison
		// rationale.
		rows, err = s.db.QueryContext(ctx, childFolderRollupSelect+`
			  FROM `+topLevelFSFolderSource+`
			 ORDER BY f.path ASC
		`)
	} else {
		likePat := likeEscape(parent) + `/%`
		// `length(?)` in SQL (NOT Go's len()) so UTF-8 parents with
		// multi-byte characters compute the substring offset on
		// the same character-counted basis as substr(). Go's
		// len() returns byte count for a UTF-8 string and would
		// over-shoot by `bytes - chars` on a parent like "Müller".
		rows, err = s.db.QueryContext(ctx, childFolderRollupSelect+`
			  FROM folders f
			 WHERE f.path LIKE ? ESCAPE '\'
			   AND instr(substr(f.path, length(?) + 2), '/') = 0
			   AND f.path NOT LIKE '%/.'
			 ORDER BY f.path ASC
		`, likePat, parent)
	}
	if err != nil {
		return nil, fmt.Errorf("list child folders %q: %w", parent, err)
	}
	defer rows.Close()
	out := []ChildFolderRollup{}
	for rows.Next() {
		var row ChildFolderRollup
		if err := rows.Scan(
			&row.Path,
			&row.TrackCount,
			&row.UpscaledTrackCount,
			&row.OptimizedTrackCount,
			&row.TotalSizeBytes,
			&row.UpscaledSizeBytes,
			&row.OptimizedSizeBytes,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListChildTracks returns the immediate-child tracks of `parent`
// (one level deep, NOT recursive). `parent == ""` returns tracks
// whose path contains no slash (single-root libraries with audio
// files at the FS root, rare but possible).
//
// Sample rate / bits / codec / isDSD are read via `json_extract`
// against `tags_json` — typed via NULL-safe Scan into pointer fields
// so an extractor that left a field empty returns `nil` rather than
// a zero-valued non-pointer that pretends to be set.
//
// `IsUpscaled` / `IsOptimized` are determined by EXISTS against
// `track_variants` filtered by `variant_id` prefix — kind-specific
// "at least one variant exists" signals. The admin Library Inspector
// uses these to render dual per-row "variant ready" dots without
// joining the full variant rows. Version-agnostic prefix match
// (`upscaled-%` / `optimized-%`) covers BOTH v1 (legacy) and v2
// sidecars.
func (s *Store) ListChildTracks(ctx context.Context, parent string) ([]ChildTrack, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if parent == "" {
		rows, err = s.db.QueryContext(ctx, childTrackRowSelect+`
			  FROM tracks t
			 WHERE instr(t.path, '/') = 0
			 ORDER BY t.path ASC
		`)
	} else {
		likePat := likeEscape(parent) + `/%`
		// `length(?)` (SQLite character count) — see ListChildFolders
		// for the UTF-8 byte-vs-char rationale.
		rows, err = s.db.QueryContext(ctx, childTrackRowSelect+`
			  FROM tracks t
			 WHERE t.path LIKE ? ESCAPE '\'
			   AND instr(substr(t.path, length(?) + 2), '/') = 0
			 ORDER BY t.path ASC
		`, likePat, parent)
	}
	if err != nil {
		return nil, fmt.Errorf("list child tracks %q: %w", parent, err)
	}
	defer rows.Close()
	out := []ChildTrack{}
	for rows.Next() {
		var (
			ct        ChildTrack
			rate      sql.NullFloat64
			bits      sql.NullInt64
			codec     sql.NullString
			isDSDRaw  sql.NullInt64 // SQLite returns 0/1 for booleans; isDSD is stored as bool JSON, json_extract gives 0/1
			upscaled  int           // EXISTS returns 0/1
			optimized int           // EXISTS returns 0/1
		)
		if err := rows.Scan(&ct.Path, &ct.Size, &rate, &bits, &codec, &isDSDRaw, &upscaled, &optimized); err != nil {
			return nil, err
		}
		if rate.Valid {
			v := rate.Float64
			ct.SampleRate = &v
		}
		if bits.Valid {
			v := int(bits.Int64)
			ct.BitsPerSample = &v
		}
		if codec.Valid {
			ct.Codec = codec.String
		}
		if isDSDRaw.Valid {
			b := isDSDRaw.Int64 != 0
			ct.IsDSD = &b
		}
		ct.IsUpscaled = upscaled != 0
		ct.IsOptimized = optimized != 0
		out = append(out, ct)
	}
	return out, rows.Err()
}

// ListChildFoldersPage is the cursor-paginated variant of
// ListChildFolders (v1.4 PR C). `after` is the last folder path
// the caller received in the previous page (empty means "first
// page"); `limit` caps the result count.
//
// Cursor semantics: SQL `WHERE f.path > ? ORDER BY f.path ASC LIMIT ?`.
// O(log N) B-tree seek on the existing PRIMARY KEY index — beats
// the O(N) scan that `OFFSET` does at deep ranges. The caller's
// next-page cursor is the last path of the current page; an empty
// cursor on a fresh response signals "no more rows."
//
// Rollup subqueries are unchanged — each folder row still carries
// its full recursive aggregation. The count-only path skips the
// rollups (see `CountChildFolders`).
func (s *Store) ListChildFoldersPage(ctx context.Context, parent, after string, limit int) ([]ChildFolderRollup, error) {
	if limit <= 0 {
		// Conservative default; admin browse uses 500 explicitly.
		limit = 500
	}
	var (
		rows *sql.Rows
		err  error
	)
	if parent == "" {
		rows, err = s.db.QueryContext(ctx, childFolderRollupSelect+`
			  FROM `+topLevelFSFolderSource+`
			 WHERE f.path > ?
			 ORDER BY f.path ASC
			 LIMIT ?
		`, after, limit)
	} else {
		likePat := likeEscape(parent) + `/%`
		rows, err = s.db.QueryContext(ctx, childFolderRollupSelect+`
			  FROM folders f
			 WHERE f.path LIKE ? ESCAPE '\'
			   AND instr(substr(f.path, length(?) + 2), '/') = 0
			   AND f.path NOT LIKE '%/.'
			   AND f.path > ?
			 ORDER BY f.path ASC
			 LIMIT ?
		`, likePat, parent, after, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list child folders page %q: %w", parent, err)
	}
	defer rows.Close()
	out := []ChildFolderRollup{}
	for rows.Next() {
		var row ChildFolderRollup
		if err := rows.Scan(
			&row.Path,
			&row.TrackCount,
			&row.UpscaledTrackCount,
			&row.OptimizedTrackCount,
			&row.TotalSizeBytes,
			&row.UpscaledSizeBytes,
			&row.OptimizedSizeBytes,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListChildTracksPage is the cursor-paginated variant of
// ListChildTracks. Mirrors ListChildFoldersPage's contract.
func (s *Store) ListChildTracksPage(ctx context.Context, parent, after string, limit int) ([]ChildTrack, error) {
	if limit <= 0 {
		limit = 500
	}
	var (
		rows *sql.Rows
		err  error
	)
	if parent == "" {
		rows, err = s.db.QueryContext(ctx, childTrackRowSelect+`
			  FROM tracks t
			 WHERE instr(t.path, '/') = 0
			   AND t.path > ?
			 ORDER BY t.path ASC
			 LIMIT ?
		`, after, limit)
	} else {
		likePat := likeEscape(parent) + `/%`
		rows, err = s.db.QueryContext(ctx, childTrackRowSelect+`
			  FROM tracks t
			 WHERE t.path LIKE ? ESCAPE '\'
			   AND instr(substr(t.path, length(?) + 2), '/') = 0
			   AND t.path > ?
			 ORDER BY t.path ASC
			 LIMIT ?
		`, likePat, parent, after, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list child tracks page %q: %w", parent, err)
	}
	return scanChildTrackRows(rows)
}

// scanChildTrackRows materialises ChildTrack rows from any query built on
// childTrackRowSelect (identical column order). Shared by the path-prefix
// browse (ListChildTracksPage) and the harmonic-key filter
// (ListTracksByKeyPage) so the json_extract → typed-pointer mapping lives in
// one place. Takes ownership of rows (closes them).
func scanChildTrackRows(rows *sql.Rows) ([]ChildTrack, error) {
	defer rows.Close()
	out := []ChildTrack{}
	for rows.Next() {
		var (
			ct        ChildTrack
			rate      sql.NullFloat64
			bits      sql.NullInt64
			codec     sql.NullString
			isDSDRaw  sql.NullInt64
			upscaled  int
			optimized int
		)
		if err := rows.Scan(&ct.Path, &ct.Size, &rate, &bits, &codec, &isDSDRaw, &upscaled, &optimized); err != nil {
			return nil, err
		}
		if rate.Valid {
			v := rate.Float64
			ct.SampleRate = &v
		}
		if bits.Valid {
			v := int(bits.Int64)
			ct.BitsPerSample = &v
		}
		if codec.Valid {
			ct.Codec = codec.String
		}
		if isDSDRaw.Valid {
			b := isDSDRaw.Int64 != 0
			ct.IsDSD = &b
		}
		ct.IsUpscaled = upscaled != 0
		ct.IsOptimized = optimized != 0
		out = append(out, ct)
	}
	return out, rows.Err()
}

// tracksByKeyQuery is childTrackRowSelect + the harmonic-key FROM/WHERE, a
// compile-time CONSTANT.
//
// KNOWN SonarCloud FALSE POSITIVE — go:S2077 "dynamically formatted SQL":
// the rule fires on the `+` syntactically, even though both operands are
// constants. It is safe — every dynamic value (keyRoot, keyMode, the path
// cursor, the limit) is a bound ? parameter in ListTracksByKeyPage, so no
// user-controlled text ever reaches the SQL string. It's the same shape as
// the 4 pre-existing childTrackRowSelect+ query sites (ListChildTracksPage
// etc.), which are unflagged only because they predate this PR. Resolve it
// as a false positive in SonarCloud: the Go analyzer does NOT honor in-code
// // NOSONAR, and inlining the literal to drop the `+` would duplicate
// childTrackRowSelect's ~12 SELECT columns and risk a column-order desync
// with scanChildTrackRows.
const tracksByKeyQuery = childTrackRowSelect + `
	  FROM tracks t
	  JOIN track_analysis ta ON ta.source_path = t.path
	 WHERE ta.key_root = ? AND ta.key_mode = ?
	   AND t.path > ?
	 ORDER BY t.path ASC
	 LIMIT ?`

// ListTracksByKeyPage returns tracks whose analysis key matches
// (keyRoot, keyMode), library-wide, cursor-paginated by path ASC. Backs the
// admin Library Inspector's harmonic-key filter (the coverage wheel's
// click-to-scope deep-link). The INNER JOIN on track_analysis means only
// analyzed tracks appear — UPnP-routed/remote rows have no analysis row and
// are excluded for free, matching the inspector's local-only,
// variant-generation scope. keyMode is "minor"/"major" (see ToCamelot).
func (s *Store) ListTracksByKeyPage(ctx context.Context, keyRoot int, keyMode, after string, limit int) ([]ChildTrack, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, tracksByKeyQuery, keyRoot, keyMode, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list tracks by key (%d,%s): %w", keyRoot, keyMode, err)
	}
	return scanChildTrackRows(rows)
}

// CountTracksByKey returns the total analyzed tracks matching
// (keyRoot, keyMode) — pairs with ListTracksByKeyPage for the "X of Y" hint.
// Queries track_analysis directly: its source_path FK references
// tracks(path) ON DELETE CASCADE, so every analyzed row has a live track
// and this count equals the tracks⋈track_analysis join without the join
// overhead (Gemini on PR #444). ListTracksByKeyPage still needs the join —
// it reads track columns.
func (s *Store) CountTracksByKey(ctx context.Context, keyRoot int, keyMode string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM track_analysis
		 WHERE key_root = ? AND key_mode = ?
	`, keyRoot, keyMode).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count tracks by key (%d,%s): %w", keyRoot, keyMode, err)
	}
	return n, nil
}

// CountChildFolders returns the total count of immediate-child
// folders under `parent`, WITHOUT running the expensive rollup
// subqueries that `ListChildFolders` does per row. The admin
// Library Inspector uses this to render the "X of Y" page hint
// alongside cursor-based pagination at zero rollup cost.
func (s *Store) CountChildFolders(ctx context.Context, parent string) (int, error) {
	var n int
	var err error
	if parent == "" {
		// Top-level FS roots, derived from track paths (see
		// topLevelFSFolderSource) so multi-root + UPnP-hybrid libraries
		// count correctly. COUNT(*) over the DISTINCT-segment subquery.
		err = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM `+topLevelFSFolderSource+`
		`).Scan(&n)
	} else {
		likePat := likeEscape(parent) + `/%`
		err = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM folders
			 WHERE path LIKE ? ESCAPE '\'
			   AND instr(substr(path, length(?) + 2), '/') = 0
			   AND path NOT LIKE '%/.'
		`, likePat, parent).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count child folders %q: %w", parent, err)
	}
	return n, nil
}

// CountChildTracks returns the total count of immediate-child
// tracks under `parent`. Pairs with CountChildFolders for the
// cursor-paginated admin browse — both avoid the rollup overhead.
func (s *Store) CountChildTracks(ctx context.Context, parent string) (int, error) {
	var n int
	var err error
	if parent == "" {
		err = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tracks
			 WHERE instr(path, '/') = 0
		`).Scan(&n)
	} else {
		likePat := likeEscape(parent) + `/%`
		err = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tracks
			 WHERE path LIKE ? ESCAPE '\'
			   AND instr(substr(path, length(?) + 2), '/') = 0
		`, likePat, parent).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count child tracks %q: %w", parent, err)
	}
	return n, nil
}

// RollupByPrefix returns the recursive aggregation under `prefix`:
// total track count, tracks with at least one variant, sum of source
// sizes, sum of variant sizes. Empty prefix means "the whole library."
//
// Used by the admin Library Inspector to render per-root summaries
// on the empty-parent browse, and by the projection endpoint
// (apiLibraryBrowseProjection) to back out the source-size sum the
// operator's pre-flight needs before a batch submit.
//
// The four counters come from two scalar queries (tracks + variants)
// so the round-trip cost is two SQLite calls regardless of subtree
// size. Indexes on `tracks(path)` and `track_variants(source_path)`
// make these O(matched rows) lookups.
func (s *Store) RollupByPrefix(ctx context.Context, prefix string) (FolderRollup, error) {
	// Global fast path: the empty prefix means "the whole library".
	// The general path below builds a `%`-pattern and runs
	// `WHERE path LIKE '%'`, which forces SQLite to evaluate the LIKE
	// against every row and forfeits index-only counting. The
	// dashboard's "Library composition" tile calls this per page load,
	// so on a 50k+ track library the unconstrained-LIKE form is a
	// needless O(N) scan. Run the aggregates with no WHERE clause
	// instead — SQLite can satisfy the COUNT/SUM directly.
	//
	// Gated on the TRIMMED prefix, not the raw one: "/" and "//" are
	// whole-library here exactly as they are in
	// ListTrackProjectionsUnderPrefix / EligibleRollupByPrefix. Testing
	// the raw string sent them down the range branch with base "",
	// building `path >= '/' AND path < '0'` — a match on nothing, while
	// the projection helper called the same input the whole library.
	// The Inspector renders both from one submit, so the two disagreed
	// on screen.
	if strings.TrimRight(prefix, "/") == "" {
		// One round-trip: two scalar subqueries cover `tracks`, and the
		// conditional aggregation scans `track_variants` exactly once
		// instead of twice (Gemini on PR #340).
		var out FolderRollup
		if err := s.db.QueryRowContext(ctx, `
			SELECT
			  (SELECT COUNT(*) FROM tracks),
			  (SELECT COALESCE(SUM(size), 0) FROM tracks),
			  COUNT(DISTINCT CASE WHEN variant_id LIKE 'upscaled-%'  THEN source_path END),
			  COALESCE(SUM(CASE WHEN variant_id LIKE 'upscaled-%'  THEN size_bytes END), 0),
			  COUNT(DISTINCT CASE WHEN variant_id LIKE 'optimized-%' THEN source_path END),
			  COALESCE(SUM(CASE WHEN variant_id LIKE 'optimized-%' THEN size_bytes END), 0)
			FROM track_variants
		`).Scan(
			&out.TrackCount, &out.TotalSizeBytes,
			&out.UpscaledTrackCount, &out.UpscaledSizeBytes,
			&out.OptimizedTrackCount, &out.OptimizedSizeBytes,
		); err != nil {
			return FolderRollup{}, fmt.Errorf("rollup global stats: %w", err)
		}
		return out, nil
	}
	// Non-empty prefix: scope to the subtree with the byte-range form
	// `path >= prefix||'/' AND path < prefix||'0'` ('0' is the ASCII
	// successor of '/'), the established sibling idiom in
	// EligibleRollupByPrefix / childFolderRollupSelect. SQLite's default
	// LIKE is case-insensitive, so `path LIKE 'prefix/%'` can't be
	// answered from the BINARY-collated `path` PRIMARY KEY and forces a
	// full-table scan; the range predicate rides the index. The two
	// track_variants counters collapse into ONE conditional-aggregation
	// scan over the same source_path range (mirrors the empty-prefix
	// branch above; Gemini on PR #340) instead of scanning the table
	// twice. Numerically identical to the prior `LIKE 'prefix/%'` form
	// for folder-derived prefixes (which never differ only by case).
	// Strip a trailing slash before binding, mirroring CountTracksByPrefix: the
	// range appends its own '/', so a caller passing "Album/" would otherwise
	// build `path >= 'Album//'` and match NOTHING — a silently-empty rollup
	// rather than an error (Gemini HIGH, post-merge review of #532).
	// TrimRight, not TrimSuffix: a caller passing "Album//" would keep one
	// slash and rebuild the same broken pattern.
	//
	// All four prefix helpers (here, CountTracksByPrefix,
	// EligibleRollupByPrefix, ListTrackProjectionsUnderPrefix) share the
	// trim AND the decide-after-trim empty-base rule — keep them in
	// step. They do NOT all share a query form: this one, its two
	// byte-range siblings, and ListTrackProjectionsUnderPrefix (which
	// uses an escaped LIKE) differ deliberately.
	base := strings.TrimRight(prefix, "/")
	var out FolderRollup
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0)
		  FROM tracks
		 WHERE path >= ? || '/' AND path < ? || '0'
	`, base, base).Scan(&out.TrackCount, &out.TotalSizeBytes); err != nil {
		return FolderRollup{}, fmt.Errorf("rollup tracks %q: %w", prefix, err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(DISTINCT CASE WHEN variant_id LIKE 'upscaled-%'  THEN source_path END),
		  COALESCE(SUM(CASE WHEN variant_id LIKE 'upscaled-%'  THEN size_bytes END), 0),
		  COUNT(DISTINCT CASE WHEN variant_id LIKE 'optimized-%' THEN source_path END),
		  COALESCE(SUM(CASE WHEN variant_id LIKE 'optimized-%' THEN size_bytes END), 0)
		  FROM track_variants
		 WHERE source_path >= ? || '/' AND source_path < ? || '0'
	`, base, base).Scan(
		&out.UpscaledTrackCount, &out.UpscaledSizeBytes,
		&out.OptimizedTrackCount, &out.OptimizedSizeBytes,
	); err != nil {
		return FolderRollup{}, fmt.Errorf("rollup variants %q: %w", prefix, err)
	}
	return out, nil
}

// CountVariantsByKind returns the total `track_variants.size_bytes`
// summed per variant kind ("upscale" / "optimize"). Used by the admin
// Library Inspector's storage bar to render a per-kind usage
// breakdown alongside the existing total Used / Free counters.
//
// The CASE expression buckets by `variant_id` LIKE prefix so the
// classification is version-agnostic — both v1 (legacy) and v2
// sidecars roll into their respective kind. Rows whose variant_id
// matches neither prefix land in the "unknown" bucket (defensive,
// should be empty in practice — callers can choose to log if
// non-zero so operators see drift).
//
// Returned map always carries both "upscale" and "optimize" keys
// (zero-valued when no matching rows) so call sites can read
// `m["upscale"]` without a nil-map check. The "unknown" key only
// appears if the bucket is non-empty.
//
// Single SQL round-trip; SQLite plans the GROUP BY against
// `track_variants`'s primary key on (source_path, variant_id) — no
// extra index needed.
//
// **Architectural note**: this method exists on `*Store` (rather
// than as raw SQL inline in the admin handler) to preserve the
// existing boundary where `s.db` references stay private to the
// `manifest` package. Admin / API layers talk to typed Store
// methods only.
func (s *Store) CountVariantsByKind(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		  CASE
		    WHEN variant_id LIKE 'upscaled-%'  THEN 'upscale'
		    WHEN variant_id LIKE 'optimized-%' THEN 'optimize'
		    ELSE 'unknown'
		  END AS kind,
		  COALESCE(SUM(size_bytes), 0) AS total
		FROM track_variants
		GROUP BY kind
	`)
	if err != nil {
		return nil, fmt.Errorf("count variants by kind: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{
		"upscale":  0,
		"optimize": 0,
	}
	for rows.Next() {
		var kind string
		var total int64
		if err := rows.Scan(&kind, &total); err != nil {
			return nil, err
		}
		out[kind] = total
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// VariantKindStat is the per-kind file count + combined byte size for
// one `track_variants` prefix grouping ("upscale" / "optimize"). Used
// by the admin dashboard's "Library composition" tile and the Settings
// → Audio quality stats card, which need the *file* count (a single
// source track may carry several upscaled targets, so file count ≠ the
// DISTINCT-source counts `RollupByPrefix` returns).
type VariantKindStat struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// VariantStatsByKind returns per-kind file counts and combined byte
// sizes, keyed "upscale" / "optimize" (and "unknown" only when that
// bucket is non-empty — defensive, should stay empty in practice).
//
// Sibling of `CountVariantsByKind` (which returns bytes only): this one
// also carries `COUNT(*)` so the admin surfaces can render
// "Upscaled: N files (X)" / "Optimized: M files (Y)" honestly instead
// of the conflated all-variants total the Settings tile showed before.
//
// The result map is **pre-seeded** with zero-valued "upscale" +
// "optimize" entries, because a `GROUP BY` over an empty
// `track_variants` returns zero rows — pre-seeding keeps the JSON
// payload shape stable so the frontend never reads `undefined`.
//
// Single SQL round-trip; the GROUP BY is planned against the
// `(source_path, variant_id)` primary key — no extra index needed.
func (s *Store) VariantStatsByKind(ctx context.Context) (map[string]VariantKindStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		  CASE
		    WHEN variant_id LIKE 'upscaled-%'  THEN 'upscale'
		    WHEN variant_id LIKE 'optimized-%' THEN 'optimize'
		    ELSE 'unknown'
		  END AS kind,
		  COUNT(*) AS files,
		  COALESCE(SUM(size_bytes), 0) AS total_bytes
		FROM track_variants
		GROUP BY kind
	`)
	if err != nil {
		return nil, fmt.Errorf("variant stats by kind: %w", err)
	}
	defer rows.Close()
	out := map[string]VariantKindStat{
		"upscale":  {},
		"optimize": {},
	}
	for rows.Next() {
		var kind string
		var stat VariantKindStat
		if err := rows.Scan(&kind, &stat.Files, &stat.Bytes); err != nil {
			return nil, err
		}
		out[kind] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FormatGroup is one (codec, sampleRate, bitsPerSample, isDSD) bucket
// with its track count, as returned by FormatDistribution. The format
// fields live in the `tags_json` BLOB (the `tracks` table has no
// dedicated format columns), so the GROUP BY pays a full-table
// json_extract scan — callers MUST cache the result rather than calling
// it on a hot path (the admin console wraps it in a TTL + single-flight
// cache). SampleRate is Hz, BitsPerSample is 16/24/32 (1 for DSD).
type FormatGroup struct {
	Codec         string
	SampleRate    int
	BitsPerSample int
	IsDSD         bool
	Count         int
}

// FormatDistribution returns the library's master-quality breakdown as
// raw (codec, sampleRate, bitsPerSample, isDSD) groups with per-group
// track counts. Includes EVERY row in `tracks` (UPnP-routed rows too)
// so the caller's buckets sum to the same total as CountTracks /
// RollupByPrefix("").TrackCount; a row with no extractable format
// (sampleRate 0, not DSD) lands in its own group the caller surfaces as
// "Unknown".
//
// The `is_dsd` extraction mirrors the proven production form in
// ListTrackProjectionsUnderPrefix: SQLite's json_extract returns SQL
// INTEGER 1/0 for a JSON boolean, so the `CAST(... AS INTEGER) != 0`
// round-trip is correct (it already drives the live DSD-skip gate).
//
// Full-table json_extract scan — NOT for hot paths; see FormatGroup.
// Read-only, so no `s.mu` (WAL handles concurrent readers).
func (s *Store) FormatDistribution(ctx context.Context) ([]FormatGroup, error) {
	// ORDINAL GROUP BY (1..4) is load-bearing since migration v25:
	// `tracks` now has real `codec` / `is_dsd` columns, and SQLite
	// resolves bare GROUP BY names to TABLE COLUMNS before SELECT
	// aliases — `GROUP BY codec, ..., is_dsd` silently switched from
	// the tags_json aliases to the (possibly NULL) columns and
	// misgrouped rows. tags_json stays this query's read-truth (the
	// v25 column contract); the ordinals pin the aliases.
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(json_extract(tags_json, '$.codec'), '')                         AS codec,
		       CAST(COALESCE(json_extract(tags_json, '$.sampleRate'), 0) AS INTEGER)    AS rate,
		       CAST(COALESCE(json_extract(tags_json, '$.bitsPerSample'), 0) AS INTEGER) AS bits,
		       CAST(COALESCE(json_extract(tags_json, '$.isDSD'), 0) AS INTEGER)         AS is_dsd,
		       COUNT(*)                                                                  AS n
		  FROM tracks
		 GROUP BY 1, 2, 3, 4
	`)
	if err != nil {
		return nil, fmt.Errorf("format distribution: %w", err)
	}
	defer rows.Close()
	out := []FormatGroup{}
	for rows.Next() {
		var g FormatGroup
		var isDSD int
		if err := rows.Scan(&g.Codec, &g.SampleRate, &g.BitsPerSample, &isDSD, &g.Count); err != nil {
			return nil, err
		}
		g.IsDSD = isDSD != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

// KeyCount is one (key_root, key_mode) group from KeyDistribution: the
// number of analyzed tracks in that musical key. key_root is 0..11 (C=0);
// key_mode is "major" / "minor".
type KeyCount struct {
	KeyRoot int
	KeyMode string
	Count   int
}

// KeyDistribution returns the count of analyzed tracks per musical key,
// grouped by (key_root, key_mode). Only rows WITH an estimated key are
// counted (key_root / key_mode NOT NULL) — tracks the analyzer couldn't
// key are excluded. Naturally local-only: track_analysis holds only
// locally analyzed files (UPnP-routed remote tracks are never analyzed).
// Cheap GROUP BY on real columns (no json_extract); the admin Camelot
// wheel maps each (root, mode) to a wheel code. Read-only, no s.mu.
func (s *Store) KeyDistribution(ctx context.Context) ([]KeyCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_root, key_mode, COUNT(*) AS n
		  FROM track_analysis
		 WHERE key_root IS NOT NULL AND key_mode IS NOT NULL AND key_mode <> ''
		 GROUP BY key_root, key_mode
	`)
	if err != nil {
		return nil, fmt.Errorf("key distribution: %w", err)
	}
	defer rows.Close()
	out := []KeyCount{}
	for rows.Next() {
		var k KeyCount
		if err := rows.Scan(&k.KeyRoot, &k.KeyMode, &k.Count); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TrackProjection carries the per-track fields the operator pre-
// flight (apiLibraryBrowseProjection) needs to call ProjectedSize.
// Slim shape so listing every track under a path doesn't pay the
// per-row JSON allocation for unused tag fields.
type TrackProjection struct {
	Path          string
	Size          int64
	MTimeNS       int64 // for VariantRow.SourceMTimeNS at JobSpec construction
	SampleRate    int
	BitsPerSample int
	// Codec is the upper-case canonical codec string (FLAC / ALAC /
	// WAV / AIFF / AAC / MP3 / DSF / DFF) the scanner stamped into
	// `tags_json`. Empty for legacy pre-codec-column rows; consumers
	// that need codec discrimination on legacy DBs fall back to the
	// on-disk extension (see transcode.OptimizeEligible).
	Codec string
	// IsDSD distinguishes DSF / DFF tracks (which the upscale
	// pipeline rejects — DSD is 1-bit modulated and not a SoX-
	// resampleable source) from PCM. The admin projection loop
	// folds DSD into the `unknownFormat` bucket so the surfaced
	// "X tracks here can't be upscaled" count reflects reality;
	// without this gate DSF folders showed a projectable size +
	// active Upscale button, but the submit returned
	// `enqueuedCount: 0`. User-reported on the v1.4 followup
	// inspector polish.
	IsDSD      bool
	HasVariant bool
}

// ListTrackProjectionsUnderPrefix iterates every track under `prefix`
// (recursive — NOT one-level) and returns the slim projection fields
// the admin pre-flight needs to compute the batch's projected
// variant size. `HasVariant` is true when at least one variant
// matching `variantPrefix` exists at the source path, so the caller
// can subtract tracks already covered from the projected total.
//
// `variantPrefix` is one of `VariantKindPrefixUpscaled` /
// `VariantKindPrefixOptimized` (e.g. "upscaled" or "optimized").
// The internal LIKE pattern appends `-%` so the match is scoped
// to "<prefix>-<schemaVersion>-…" variant IDs. Version-agnostic
// to cover both v1 (legacy) and v2 sidecars in the same kind.
//
// **Why kind-specific HasVariant matters**: the upscale projection
// must not see optimize variants as "already covered" — running
// the optimize projection against a track that has only an upscale
// variant would otherwise mis-count it and skip generation entirely
// (silently zero out the projected file count). Senior-review fix
// to PR feat/library-inspector-tiles.
//
// Tracks with no `sampleRate` or `bitsPerSample` in `tags_json`
// (extractor couldn't read the format) return zero in those fields;
// `ProjectedSize` returns 0 for zero rates / bits, so they
// naturally contribute nothing to the projection. The admin can
// surface a separate "X unknown-format tracks" counter if needed.
func (s *Store) ListTrackProjectionsUnderPrefix(ctx context.Context, prefix, variantPrefix string) ([]TrackProjection, error) {
	// See subtreeRangeBase for why the trim and the decide-after-trim
	// rule are load-bearing. Whole-library is a legal scope here, so an
	// unscoped call falls back to the match-everything pattern rather
	// than dropping the WHERE clause.
	//
	// Still on the case-FOLDING form: the range shape can't express
	// "everything" in one static statement, and this is a display-only
	// projection, so the folding is a cosmetic over-count on a case-twin
	// folder rather than a write. Tracked as follow-up alongside the
	// other browse-side LIKE readers (ListChildFolders / ListChildTracks
	// and their Count/Page twins).
	pattern, scoped := subtreeLikePattern(prefix)
	if !scoped {
		pattern = `%`
	}
	// **Parameter binding order is load-bearing**: the new `?` for
	// `variantPrefix` lives inside the SELECT-block EXISTS subquery,
	// so it appears POSITIONALLY BEFORE the `WHERE t.path LIKE ?`
	// placeholder. Bind `variantPrefix` first, `pattern` second.
	// Swapping them silently returns zero rows because SQLite would
	// search track paths for the variant-prefix string. Locked by
	// TestListTrackProjectionsUnderPrefix_bindingOrder.
	variantLike := variantPrefix + `-%`
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.path, t.size, t.mtime_ns,
		       CAST(COALESCE(json_extract(t.tags_json, '$.sampleRate'),    0) AS INTEGER) AS rate,
		       CAST(COALESCE(json_extract(t.tags_json, '$.bitsPerSample'), 0) AS INTEGER) AS bits,
		       COALESCE(json_extract(t.tags_json, '$.codec'),              '') AS codec,
		       CAST(COALESCE(json_extract(t.tags_json, '$.isDSD'),         0) AS INTEGER) AS is_dsd,
		       EXISTS(SELECT 1 FROM track_variants tv
		               WHERE tv.source_path = t.path
		                 AND tv.variant_id LIKE ?) AS has_variant
		  FROM tracks t
		 WHERE t.path LIKE ? ESCAPE '\'
		 ORDER BY t.path ASC
	`, variantLike, pattern)
	if err != nil {
		return nil, fmt.Errorf("list track projections %q: %w", prefix, err)
	}
	defer rows.Close()
	out := []TrackProjection{}
	for rows.Next() {
		var tp TrackProjection
		var isDSD, has int
		if err := rows.Scan(&tp.Path, &tp.Size, &tp.MTimeNS, &tp.SampleRate, &tp.BitsPerSample, &tp.Codec, &isDSD, &has); err != nil {
			return nil, err
		}
		tp.IsDSD = isDSD != 0
		tp.HasVariant = has != 0
		out = append(out, tp)
	}
	return out, rows.Err()
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
func (s *Store) UpsertVariant(ctx context.Context, v VariantRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit; structural rollback guarantee.
	if _, err := tx.ExecContext(ctx, `
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
	if _, err := tx.ExecContext(ctx, `
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

// SetArtworkVersionAndBumpIndex records the content version of a freshly
// (re)fetched premium cover for every track whose artworkMBID matches, and
// STRICTLY advances those tracks' indexed_at so the iOS delta-sync re-receives
// them and picks up the new ArtworkVersion — the cover cache-bust signal. The
// /v1/artwork/{mbid} URL is stable while the bytes change on a CAA→premium
// upgrade, so without this iOS keeps its cached cover (albumID-keyed, not
// URL-keyed) until a manual cache clear.
//
// Idempotent: the `artwork_version <> ?` guard means re-fetching the SAME
// premium bytes (same hash) is a no-op — no version write, no index bump, no
// manifest churn. The indexed_at form mirrors UpsertVariant (PR #156):
// monotonic + strictly-advancing even under a rewound clock; it does NOT touch
// enriched_at (a cover upgrade is not re-enrichment — touching it would re-arm
// the MB/CAA/Deezer treadmill). The WHERE is backed by the functional index on
// json_extract(tags_json,'$.artworkMBID'). Returns the number of track rows
// updated (0 when unchanged or no track carries the MBID). Holds s.mu (SQLite
// single-writer contract).
func (s *Store) SetArtworkVersionAndBumpIndex(ctx context.Context, artworkMBID, version string) (int64, error) {
	if artworkMBID == "" || version == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks SET
			artwork_version = ?,
			indexed_at = CASE
				WHEN indexed_at >= ? THEN indexed_at + 1
				ELSE ?
			END
		WHERE json_extract(tags_json, '$.artworkMBID') = ?
		  AND COALESCE(artwork_version, '') <> ?
	`, version, now, now, artworkMBID, version)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetVariant fetches one row by (source_path, variant_id). Returns
// (nil, nil) if absent — same convention as GetTrack.
func (s *Store) GetVariant(ctx context.Context, sourcePath, variantID string) (*VariantRow, error) {
	var v VariantRow
	// Exact match by design — `track_variants.source_path` is part
	// of the SQL PRIMARY KEY and case-insensitive lookups would risk
	// returning an arbitrary case-colliding row's sidecar path on
	// case-sensitive filesystems. Use `LookupVariant` for callers
	// that hand in iOS-shaped paths from `share.normalize`. (Qodo
	// on PR #126: variant lookup non-determinism could stream the
	// wrong sidecar from /v1/download.)
	err := s.db.QueryRowContext(ctx, `
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
func (s *Store) LookupVariant(ctx context.Context, sourcePath, variantID string) (*VariantRow, error) {
	if v, err := s.GetVariant(ctx, sourcePath, variantID); err != nil || v != nil {
		return v, err
	}
	cleaned := normalizePathForLookup(sourcePath)
	if cleaned == sourcePath {
		return s.lookupVariantByLowerCase(ctx, cleaned, variantID)
	}
	if v, err := s.GetVariant(ctx, cleaned, variantID); err != nil || v != nil {
		return v, err
	}
	return s.lookupVariantByLowerCase(ctx, cleaned, variantID)
}

func (s *Store) lookupVariantByLowerCase(ctx context.Context, cleanedSourcePath, variantID string) (*VariantRow, error) {
	// Same fail-closed-on-ambiguity contract as
	// `lookupTrackByLowerCase` — see that function's comment for
	// the case-collision rationale. Two distinct case-colliding
	// `track_variants.source_path` rows under the same
	// `variant_id` would otherwise let LIMIT 1 stream the wrong
	// sidecar from /v1/download. (CodeRabbit on PR #126.)
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		WHERE unicode_lower(source_path) = unicode_lower(?) AND variant_id = ?
		LIMIT 2
	`, cleanedSourcePath, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		// rows.Err() distinguishes a genuine empty result from an
		// iteration error (transient SQLite I/O / malformed index); a
		// missing check silently misreads a real error as "no variant",
		// failing the download or re-creating the sidecar. (DeepSeek review.)
		if err := rows.Err(); err != nil {
			return nil, err
		}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &v, nil
}

// AllVariants returns every row in track_variants. Used by `bridge
// upscale --gc` to drive the mark-and-sweep against the on-disk
// `<dataDir>/transcoded/` directory.
// AllSidecarPaths returns the set of `sidecar_path` strings currently
// recorded in `track_variants`, projected as a map for O(1) lookup
// by the integrity package's forward-sweep (orphan sidecar) watcher.
//
// **Why a single SELECT, no explicit transaction**: SQLite in WAL
// mode (the project default — see `internal/manifest/migrations`) gives
// every SELECT a consistent snapshot via its built-in MVCC; the bare
// query produces a point-in-time view without blocking writers, which
// is exactly the guarantee the sweeper needs to safely diff against
// the filesystem. An explicit `BEGIN DEFERRED` would only matter for
// multi-statement consistency, which this single projection doesn't
// need. CLAUDE.md "Bridge background GC" docs the snapshot semantics
// in more detail.
//
// **Memory shape**: returns a `map[string]struct{}` keyed on the
// absolute sidecar path. A 50k-variant library projects to ~5 MB of
// strings (avg sidecar path ~100 bytes); a 500k-variant library
// projects to ~50 MB. The sweeper holds the map for the duration of
// one tick (typically seconds), then drops it. If a future library
// scale pushes this past comfortable RAM, the next migration is a
// streaming variant `EachSidecarPath(ctx, func(path string) bool)` —
// but the projection-map shape is simpler to reason about and matches
// the existing `AllVariants` API surface.
func (s *Store) AllSidecarPaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sidecar_path
		FROM track_variants
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var sidecar string
		if err := rows.Scan(&sidecar); err != nil {
			return nil, err
		}
		out[sidecar] = struct{}{}
	}
	return out, rows.Err()
}

func (s *Store) AllVariants(ctx context.Context) ([]VariantRow, error) {
	rows, err := s.db.QueryContext(ctx, `
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

// ListVariantsByPathPrefix returns every variant row whose
// source_path starts with `prefix` (case-insensitively via the
// project's `unicode_lower` SQLite scalar; Unicode case-folding
// matches every other path lookup in the store). Used by the
// admin DELETE /v1/upscale/variants?prefix=<rel-path> route to
// resolve the deletion target set BEFORE unlinking sidecars from
// disk.
//
// `prefix` is escaped via `likeEscape` so a literal `%` or `_` in
// an album folder name (`Albums/20%_Hits/...`) does not match
// every album starting with `Albums/20`. The `ESCAPE '\'` clause
// pairs with the `likeEscape` helper.
//
// Empty `prefix` matches every row — caller is responsible for
// rejecting accidental deletes (the handler refuses an unscoped
// delete-all without an explicit `?confirm=true` query parameter).
//
// Returns `(out, rows.Err())` rather than `(out, nil)` — the
// caller-side cleanup loop hands real iterator errors back to the
// handler so a partial result never silently leaks. Hits the v4
// `idx_track_variants_source_path_unicode_lower` index.
func (s *Store) ListVariantsByPathPrefix(ctx context.Context, prefix string) ([]VariantRow, error) {
	// The helper appends its OWN separator, so a caller supplies the
	// folder name alone. Pre-fix this built a bare `prefix%`, which
	// over-matched every sibling sharing the name as a prefix:
	// `?prefix=Album` also reaped variants under `Album 2/`,
	// `Albums/`, `Album Live/`. Silent, and the files are gone.
	//
	// The API layer could not work around it either — validateRelativePath
	// rejects any prefix carrying a trailing slash (`cleaned != p`), so
	// there was no input that produced a correctly-scoped delete. Fixing
	// the query rather than loosening the validator keeps the primitive
	// safe for every future caller and leaves the 400 correct as-is.
	//
	// Same decide-after-trim rule as the four tracks-side prefix
	// helpers; empty stays "every row" for the delete-all path (the
	// handler gates that behind ?confirm=true).
	pattern, scoped := subtreeLikePattern(prefix)
	if !scoped {
		pattern = `%`
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		WHERE unicode_lower(source_path) LIKE unicode_lower(?) ESCAPE '\'
		ORDER BY source_path ASC, variant_id ASC
	`, pattern)
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

// ListVariantsForPath returns every variant row whose source_path
// equals `sourcePath` (case-insensitively via the project's
// `unicode_lower` scalar; matches DeleteVariant / LookupVariant
// case-folding semantics). Used by DELETE /v1/upscale/variants?path=<rel>
// — a single source file typically has 0 or 1 variants, but the
// schema doesn't enforce that (different `variant_id` values for
// the same source path coexist via the composite primary key) so
// this returns a slice rather than `*VariantRow`.
//
// Returns `(out, rows.Err())` — same iterator-error discipline as
// ListVariantsByPathPrefix.
func (s *Store) ListVariantsForPath(ctx context.Context, sourcePath string) ([]VariantRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, variant_id, sidecar_path, format,
		       sample_rate, bits_per_sample, size_bytes,
		       source_mtime_ns, source_size, sox_settings, created_at
		FROM track_variants
		WHERE unicode_lower(source_path) = unicode_lower(?)
		ORDER BY variant_id ASC
	`, sourcePath)
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
func (s *Store) DeleteVariant(ctx context.Context, sourcePath, variantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM track_variants WHERE source_path = ? AND variant_id = ?`,
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
		if _, err := tx.ExecContext(ctx, `
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

// UpdateVariantSidecarPath rewrites the `sidecar_path` of a single
// `track_variants` row keyed by `(source_path, variant_id)`. Used by
// `bridge variants move` to update DB rows after a successful
// filesystem move/copy.
//
// **Does NOT bump the parent track's `indexed_at`** (unlike
// `UpsertVariant`). A path-only update doesn't change the variant's
// content from iOS's perspective; bumping indexed_at across 5000
// variants in one bulk operation would trigger a wasteful full
// delta-sync. Mirrors the same intent as `UpsertVariant`'s
// indexed_at bump, but in reverse — we explicitly skip it here.
//
// Returns `sql.ErrNoRows` (wrapped) when the keyed row doesn't exist.
func (s *Store) UpdateVariantSidecarPath(ctx context.Context, sourcePath, variantID, newSidecarPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE track_variants
		   SET sidecar_path = ?
		 WHERE source_path = ? AND variant_id = ?
	`, newSidecarPath, sourcePath, variantID)
	if err != nil {
		return fmt.Errorf("update variant sidecar_path: %w", err)
	}
	// Check RowsAffected error explicitly (Gemini medium on PR D2):
	// an unchecked `res.RowsAffected()` that errors would silently
	// surface as `sql.ErrNoRows` via the n == 0 fall-through,
	// masking the real driver-level failure.
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("variant row not found for source=%q variant=%q: %w",
			sourcePath, variantID, sql.ErrNoRows)
	}
	return nil
}

// CountVariantsNotUnderPrefix returns (count, totalSizeBytes) of
// variants whose sidecar_path is NOT a descendant of `prefix`. Used
// by the admin variants-dir endpoint to surface a "Migrate legacy
// variants (N)" affordance without dragging every row into memory.
//
// `prefix` MUST end with the platform path separator so the LIKE
// pattern matches only true descendants (`/data/transcoded/` matches
// `/data/transcoded/foo.flac` but not `/data/transcoded2/...`). Empty
// `prefix` returns the total count + size (every variant is "not
// under empty").
//
// Implemented as a single COALESCE-aggregate query — O(rows scanned)
// inside SQLite vs the prior in-Go AllVariants + loop approach which
// allocated every VariantRow into the admin handler's working set
// while holding `s.mu`. Gemini medium on PR D2.
func (s *Store) CountVariantsNotUnderPrefix(ctx context.Context, prefix string) (int, int64, error) {
	var (
		count int
		bytes sql.NullInt64
	)
	// Empty prefix → every variant is "not under empty" (documented
	// contract above). The NOT LIKE path below would build pattern `%`,
	// match every row, and return 0 — contradicting the doc. The sole
	// production caller (countLegacyVariants) guards "" today; this
	// honours the contract for any future caller. (DeepSeek review.)
	if prefix == "" {
		row := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM track_variants`)
		if err := row.Scan(&count, &bytes); err != nil {
			return 0, 0, fmt.Errorf("count variants (empty prefix): %w", err)
		}
		return count, bytes.Int64, nil
	}
	// Escape LIKE metacharacters (% and _) in the prefix so a literal
	// underscore in the operator's path doesn't false-match. The
	// existing `likeEscape` helper handles the prefix sanitisation
	// the rest of the manifest already uses.
	pattern := likeEscape(prefix) + `%`
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		  FROM track_variants
		 WHERE sidecar_path NOT LIKE ? ESCAPE '\'
	`, pattern)
	if err := row.Scan(&count, &bytes); err != nil {
		return 0, 0, fmt.Errorf("count variants not under prefix: %w", err)
	}
	return count, bytes.Int64, nil
}

// CountVariants returns (rowCount, totalSizeBytes) across the
// whole `track_variants` table. Used by the admin console's
// upscale stats card. Single SQL aggregate — cheap even on
// large tables.
//
// Returns (0, 0, nil) when the table is empty (or the upscale
// feature has never been used). Errors propagate; the admin
// handler degrades to "stats unavailable" on failure.
func (s *Store) CountVariants(ctx context.Context) (int, int64, error) {
	var (
		count int
		bytes sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM track_variants`)
	if err := row.Scan(&count, &bytes); err != nil {
		return 0, 0, err
	}
	return count, bytes.Int64, nil
}

// AnalysisRow is the on-disk record for one track's offline audio
// analysis (the `track_analysis` table). Phase 1 carries the waveform
// sidecar pointer + content tag + freshness fields; a later phase adds
// the signal-derived scalar columns. Constructed by the analyze
// package when a `bridge analyze` job completes; read by the
// `/v1/waveform` handler, the manifest read-splice (waveform_tag only),
// and `bridge analyze --gc`.
type AnalysisRow struct {
	SourcePath    string
	WaveformPath  string // absolute on-disk sidecar path (authoritative)
	WaveformTag   string // 8 hex of sidecar bytes' SHA-256 (iOS cache key)
	WaveformSize  int64
	SourceMTimeNS int64
	SourceSize    int64
	SchemaVersion string
	CreatedAt     int64

	// ReplayGainTrackDB is the EBU R128 / ReplayGain 2.0 track gain in dB
	// (gain to the -18 LUFS reference), or nil when loudness hasn't been
	// computed for this row. nil is load-bearing: the scan-skip gate
	// re-analyzes a waveform-fresh row whose loudness is nil, so a library
	// analyzed before the loudness column existed backfills on the next
	// pass. Spliced onto Track.ReplayGainTrackDB at read time only when
	// the source carries no ReplayGain tag.
	ReplayGainTrackDB *float64

	// KeyRoot (tonic 0..11, C=0) + KeyMode ("major"/"minor") are the
	// estimated musical key; BPM is the estimated tempo. All nil/"" when
	// not estimated. Spliced at read time: KeyRoot/KeyMode always (no tag
	// source today), BPM only when the source has no BPM tag.
	KeyRoot *int
	KeyMode string
	BPM     *int

	// The wf4 quality scalars (all analysis-only, no tag source):
	// TruePeakDB — BS.1770-style 4x-oversampled true peak in dB of the
	// 48 kHz analysis rendering; DRScore — the community DR value;
	// AudioMD5State — "" / "verified" / "mismatch" (FLAC STREAMINFO
	// audio-checksum verification; see internal/analyze/flacmd5.go for
	// the failure direction).
	TruePeakDB    *float64
	DRScore       *int
	AudioMD5State string

	// AudioMD5Attempts counts MD5 passes that failed for a reason that
	// says nothing about the file. READ-ONLY on the row: producers
	// leave it zero and UpsertAnalysis computes the stored value from
	// AudioMD5Retryable, inside the same lock as the existing-row read,
	// so two analyses of one path can't both increment from the same
	// starting value.
	//
	// At AudioMD5MaxAttempts the row stops asking and the "" verdict is
	// final. A permanent verdict jumps straight to the cap rather than
	// counting up — same gate, no extra column, and WantsAudioMD5Retry
	// is the only thing that reads either.
	AudioMD5Attempts int

	// AudioMD5Retryable is WRITE-INTENT, not state: set by the producer
	// to say this run's empty verdict was "could not ask" rather than
	// "cannot be verified". Never populated on read — reading it back
	// would invite treating it as the stored value, which it is not.
	AudioMD5Retryable bool
}

// AudioMD5MaxAttempts bounds how many times a FLAC whose audio-MD5 pass
// keeps failing transiently is re-analysed before its "" verdict is
// taken as final.
//
// 3, matching the pairing store's revoke retries; the booklet checker's
// 8 is the other precedent but suits a cheap HTTP HEAD, and each retry
// here is a full decode of the file. Three covers the realistic
// transient — a saturated pool, a mount blinking, one OOM kill — without
// letting a genuinely undecodable file cost more than a few passes
// before it settles on the same permanent answer it has today.
const AudioMD5MaxAttempts = 3

// WantsAudioMD5Retry reports whether this row's audio-MD5 verdict is
// still worth another attempt: empty (nothing learned) and under the
// cap.
//
// The analysis scan-skip gate consults this, which is the whole point —
// mtime, size and schema version are all unchanged for these rows, so
// without it the row is skipped forever and a transient failure becomes
// permanent. Nil-safe so the gate can call it on a lookup that missed.
func (r *AnalysisRow) WantsAudioMD5Retry() bool {
	return r != nil && r.AudioMD5State == "" && r.AudioMD5Attempts < AudioMD5MaxAttempts
}

// intPtrEqual compares two optional ints by value (both nil equal, one nil
// not, else value compare) — the *int twin of float64PtrEqual.
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// nullIntPtr lifts a scanned nullable INTEGER into the *int an AnalysisRow
// carries: NULL → nil, present → a fresh pointer.
func nullIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// analysisScalarScan holds the nullable signal-derived columns
// (replaygain / key / bpm) for the three AnalysisRow read sites, so the
// NULL→pointer lifting lives in one place. Its four fields are passed to
// Scan in SELECT column order (replaygain_track_db, key_root, key_mode,
// bpm), then applyTo lifts them onto the row.
type analysisScalarScan struct {
	rg       sql.NullFloat64
	keyRoot  sql.NullInt64
	keyMode  sql.NullString
	bpm      sql.NullInt64
	truePeak sql.NullFloat64
	drScore  sql.NullInt64
	md5State sql.NullString
	// md5Attempts is NOT NULL DEFAULT 0 in the schema, but stays a
	// NullInt64 so the three read sites survive a hand-repaired DB (or
	// a future LEFT JOIN) that yields NULL — Valid==false lifts to 0,
	// which reads as "eligible for a retry" rather than erroring the
	// whole analysis lookup.
	md5Attempts sql.NullInt64
}

func (n *analysisScalarScan) applyTo(a *AnalysisRow) {
	a.ReplayGainTrackDB = nullFloatPtr(n.rg)
	a.KeyRoot = nullIntPtr(n.keyRoot)
	a.KeyMode = n.keyMode.String
	a.BPM = nullIntPtr(n.bpm)
	a.TruePeakDB = nullFloatPtr(n.truePeak)
	a.DRScore = nullIntPtr(n.drScore)
	a.AudioMD5State = n.md5State.String
	a.AudioMD5Attempts = int(n.md5Attempts.Int64)
}

// float64PtrEqual compares two optional float64s by value: both nil is
// equal, one nil is not, otherwise an exact value compare. Exact (no
// tolerance) is correct here because the same source file decodes
// deterministically to the same loudness, so an identical recompute
// yields the identical float.
func float64PtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// nullFloatPtr lifts a scanned nullable REAL into the *float64 the
// AnalysisRow carries: NULL → nil, present → a fresh pointer.
func nullFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

// analysisRowsEqual reports whether the analysis already on disk is
// identical to a freshly-computed one — same source freshness, same
// waveform sidecar, same schema, same loudness. Used by UpsertAnalysis to
// skip the `tracks.indexed_at` bump (and the write entirely) on an
// identical recompute, so a `bridge analyze --gc` / forced re-run can't
// trigger a 50k-track iOS delta-sync storm for no actual change (the PR
// #369 walkFieldsEqual lesson, applied to analysis). Loudness is part of
// the comparison so a loudness backfill (nil → value) on a waveform-fresh
// row still bumps indexed_at exactly once.
func analysisRowsEqual(a, b *AnalysisRow) bool {
	return a.WaveformPath == b.WaveformPath &&
		a.WaveformTag == b.WaveformTag &&
		a.WaveformSize == b.WaveformSize &&
		a.SourceMTimeNS == b.SourceMTimeNS &&
		a.SourceSize == b.SourceSize &&
		a.SchemaVersion == b.SchemaVersion &&
		float64PtrEqual(a.ReplayGainTrackDB, b.ReplayGainTrackDB) &&
		intPtrEqual(a.KeyRoot, b.KeyRoot) &&
		a.KeyMode == b.KeyMode &&
		intPtrEqual(a.BPM, b.BPM) &&
		float64PtrEqual(a.TruePeakDB, b.TruePeakDB) &&
		intPtrEqual(a.DRScore, b.DRScore) &&
		a.AudioMD5State == b.AudioMD5State &&
		a.AudioMD5Attempts == b.AudioMD5Attempts
}

// nextAudioMD5Attempts resolves the stored attempt counter for a fresh
// analysis result, given whatever is already on disk.
//
//   - A real verdict (verified / mismatch) clears the counter. The
//     question is answered, and clearing means a later source edit that
//     re-opens it starts from a full budget rather than an exhausted one.
//   - "Could not ask" (retryable) advances by one, saturating at the cap
//     so a long outage can't run the number away.
//   - "Cannot be verified" jumps straight to the cap. There is nothing
//     to retry — no stored checksum, an odd bit depth, an unknown sample
//     count, a length-mismatched decode — and counting up to it would
//     spend two more full decodes to reach the answer already in hand.
//
// `fresh` is the producer's row, whose AudioMD5Attempts is always zero;
// only AudioMD5Retryable is read from it. nil `existing` is a first
// analysis, which starts from zero.
func nextAudioMD5Attempts(existing, fresh *AnalysisRow) int {
	if fresh.AudioMD5State != "" {
		return 0
	}
	if !fresh.AudioMD5Retryable {
		return AudioMD5MaxAttempts
	}
	prior := 0
	if existing != nil {
		prior = existing.AudioMD5Attempts
	}
	if prior+1 > AudioMD5MaxAttempts {
		return AudioMD5MaxAttempts
	}
	return prior + 1
}

// UpsertAnalysis writes (or replaces) one `track_analysis` row AND
// bumps the parent track's `indexed_at` so iOS delta-sync surfaces the
// new `waveformTag` — BUT only when the computed values actually differ
// from what's stored. An identical recompute is a complete no-op (no
// write, no bump), so re-running analysis over an unchanged library
// doesn't churn the manifest. Both the row write and the bump run in
// one transaction under `s.mu` per the writer contract; the bump uses
// the same strictly-advancing CASE-WHEN form as UpsertVariant.
func (s *Store) UpsertAnalysis(ctx context.Context, a AnalysisRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read the existing row inside the lock so the equality check and
	// the write are atomic vs. any concurrent writer (all serialised by
	// s.mu anyway, but the read-then-write stays consistent).
	existing, err := s.getAnalysisLocked(ctx, a.SourcePath)
	if err != nil {
		return err
	}

	// Resolve the audio-MD5 attempt counter from the producer's intent
	// plus what is already stored. Inside the lock, and BEFORE the
	// equality check, for two reasons: two analyses of one path can't
	// both increment from the same starting value, and a run whose only
	// change is the counter must still be recognised as a change (an
	// exhausted retry looks identical to its predecessor in every other
	// column, and skipping that write would leave the row asking
	// forever).
	a.AudioMD5Attempts = nextAudioMD5Attempts(existing, &a)

	if existing != nil && analysisRowsEqual(existing, &a) {
		return nil // identical — no write, no indexed_at bump.
	}

	// Bind the optional scalars as NULL when absent. database/sql's
	// pointer handling is driver-dependent, so convert explicitly.
	var rgArg, keyRootArg, keyModeArg, bpmArg interface{}
	if a.ReplayGainTrackDB != nil {
		rgArg = *a.ReplayGainTrackDB
	}
	if a.KeyRoot != nil {
		keyRootArg = *a.KeyRoot
	}
	if a.KeyMode != "" {
		keyModeArg = a.KeyMode
	}
	if a.BPM != nil {
		bpmArg = *a.BPM
	}
	var truePeakArg, drScoreArg, md5StateArg interface{}
	if a.TruePeakDB != nil {
		truePeakArg = *a.TruePeakDB
	}
	if a.DRScore != nil {
		drScoreArg = *a.DRScore
	}
	if a.AudioMD5State != "" {
		md5StateArg = a.AudioMD5State
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit; structural rollback guarantee.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO track_analysis
			(source_path, waveform_path, waveform_tag, waveform_size,
			 source_mtime_ns, source_size, schema_version, created_at,
			 replaygain_track_db, key_root, key_mode, bpm,
			 true_peak_db, dr_score, audio_md5_state, audio_md5_attempts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (source_path) DO UPDATE SET
			waveform_path       = excluded.waveform_path,
			waveform_tag        = excluded.waveform_tag,
			waveform_size       = excluded.waveform_size,
			source_mtime_ns     = excluded.source_mtime_ns,
			source_size         = excluded.source_size,
			schema_version      = excluded.schema_version,
			created_at          = excluded.created_at,
			replaygain_track_db = excluded.replaygain_track_db,
			key_root            = excluded.key_root,
			key_mode            = excluded.key_mode,
			bpm                 = excluded.bpm,
			true_peak_db        = excluded.true_peak_db,
			dr_score            = excluded.dr_score,
			audio_md5_state     = excluded.audio_md5_state,
			audio_md5_attempts  = excluded.audio_md5_attempts
	`, a.SourcePath, a.WaveformPath, a.WaveformTag, a.WaveformSize,
		a.SourceMTimeNS, a.SourceSize, a.SchemaVersion, a.CreatedAt,
		rgArg, keyRootArg, keyModeArg, bpmArg,
		truePeakArg, drScoreArg, md5StateArg, a.AudioMD5Attempts); err != nil {
		return err
	}
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		UPDATE tracks SET indexed_at = CASE
			WHEN indexed_at >= ? THEN indexed_at + 1
			ELSE ?
		END
		WHERE path = ?
	`, now, now, a.SourcePath); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAnalysis fetches one analysis row by exact source_path. Returns
// (nil, nil) when absent — same convention as GetVariant. Does NOT
// hold s.mu (reads are un-mutexed per the Store contract; WAL handles
// concurrent readers).
func (s *Store) GetAnalysis(ctx context.Context, sourcePath string) (*AnalysisRow, error) {
	return s.getAnalysisLocked(ctx, sourcePath)
}

// getAnalysisLocked is the shared row-fetch used by both the public
// GetAnalysis and the under-lock read inside UpsertAnalysis. It issues
// only a SELECT, so it's safe to call whether or not s.mu is held.
func (s *Store) getAnalysisLocked(ctx context.Context, sourcePath string) (*AnalysisRow, error) {
	var a AnalysisRow
	var sc analysisScalarScan
	err := s.db.QueryRowContext(ctx, `
		SELECT source_path, waveform_path, waveform_tag, waveform_size,
		       source_mtime_ns, source_size, schema_version, created_at,
		       replaygain_track_db, key_root, key_mode, bpm,
		       true_peak_db, dr_score, audio_md5_state, audio_md5_attempts
		FROM track_analysis
		WHERE source_path = ?
	`, sourcePath).Scan(
		&a.SourcePath, &a.WaveformPath, &a.WaveformTag, &a.WaveformSize,
		&a.SourceMTimeNS, &a.SourceSize, &a.SchemaVersion, &a.CreatedAt,
		&sc.rg, &sc.keyRoot, &sc.keyMode, &sc.bpm,
		&sc.truePeak, &sc.drScore, &sc.md5State, &sc.md5Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.applyTo(&a)
	return &a, nil
}

// LookupAnalysis resolves an iOS-shaped source path (lowercase +
// leading slash from `share.normalize`) against the manifest's
// case-preserved `track_analysis.source_path`. Same two-stage shape as
// LookupVariant: exact first, then path-cleaned exact, then a
// fail-closed `unicode_lower` fold. The `/v1/waveform` handler is the
// canonical caller — it must agree with LookupTrack on which row it's
// inspecting.
func (s *Store) LookupAnalysis(ctx context.Context, sourcePath string) (*AnalysisRow, error) {
	if a, err := s.GetAnalysis(ctx, sourcePath); err != nil || a != nil {
		return a, err
	}
	cleaned := normalizePathForLookup(sourcePath)
	if cleaned != sourcePath {
		if a, err := s.GetAnalysis(ctx, cleaned); err != nil || a != nil {
			return a, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, waveform_path, waveform_tag, waveform_size,
		       source_mtime_ns, source_size, schema_version, created_at,
		       replaygain_track_db, key_root, key_mode, bpm,
		       true_peak_db, dr_score, audio_md5_state, audio_md5_attempts
		FROM track_analysis
		WHERE unicode_lower(source_path) = unicode_lower(?)
		LIMIT 2
	`, cleaned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var a AnalysisRow
	var sc analysisScalarScan
	if err := rows.Scan(
		&a.SourcePath, &a.WaveformPath, &a.WaveformTag, &a.WaveformSize,
		&a.SourceMTimeNS, &a.SourceSize, &a.SchemaVersion, &a.CreatedAt,
		&sc.rg, &sc.keyRoot, &sc.keyMode, &sc.bpm,
		&sc.truePeak, &sc.drScore, &sc.md5State, &sc.md5Attempts); err != nil {
		return nil, err
	}
	sc.applyTo(&a)
	if rows.Next() {
		// Ambiguous case-fold — refuse to pick a row rather than serve
		// the wrong sidecar. Mirrors lookupVariantByLowerCase.
		logger.Warn("LookupAnalysis: case-folded fallback is ambiguous, refusing to pick a row",
			"path", sourcePath)
		return nil, nil
	}
	// A driver/I-O error during the ambiguity rows.Next() surfaces here,
	// not as a scan error — propagate it so a transient fault can't be
	// misread as "unambiguous row found". (CodeRabbit on #395.)
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &a, nil
}

// AllAnalysisRows returns every analysis row. Used by `bridge analyze
// --gc` to reconcile the on-disk waveform tree against the DB
// (mark-and-sweep of orphan sidecars). Reads are un-mutexed.
func (s *Store) AllAnalysisRows(ctx context.Context) ([]AnalysisRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path, waveform_path, waveform_tag, waveform_size,
		       source_mtime_ns, source_size, schema_version, created_at,
		       replaygain_track_db, key_root, key_mode, bpm,
		       true_peak_db, dr_score, audio_md5_state, audio_md5_attempts
		FROM track_analysis
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnalysisRow{}
	for rows.Next() {
		var a AnalysisRow
		var sc analysisScalarScan
		if err := rows.Scan(
			&a.SourcePath, &a.WaveformPath, &a.WaveformTag, &a.WaveformSize,
			&a.SourceMTimeNS, &a.SourceSize, &a.SchemaVersion, &a.CreatedAt,
			&sc.rg, &sc.keyRoot, &sc.keyMode, &sc.bpm,
			&sc.truePeak, &sc.drScore, &sc.md5State, &sc.md5Attempts); err != nil {
			return nil, err
		}
		sc.applyTo(&a)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAnalysis removes one analysis row and bumps the parent track's
// `indexed_at` so iOS drops the now-absent `waveformTag` on its next
// delta sync. Best-effort sidecar-file unlink is the caller's job (the
// row only stores the path). No-op (no bump) when the row is absent, so
// a `--gc` pass that finds nothing to delete doesn't churn the
// manifest. Holds `s.mu` per the writer contract.
func (s *Store) DeleteAnalysis(ctx context.Context, sourcePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM track_analysis WHERE source_path = ?`, sourcePath)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err // don't silently treat a driver fault as "nothing deleted" (CodeRabbit #395)
	}
	if n == 0 {
		return tx.Commit() // nothing deleted → no bump.
	}
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		UPDATE tracks SET indexed_at = CASE
			WHEN indexed_at >= ? THEN indexed_at + 1
			ELSE ?
		END
		WHERE path = ?
	`, now, now, sourcePath); err != nil {
		return err
	}
	return tx.Commit()
}

// AnalysisCoverage is the whole-library analysed-vs-eligible breakdown
// backing the admin Jobs page's coverage bar. Eligible = TotalLocal -
// DSDExcluded - ZeroByteExcluded; the buckets are DISJOINT in the
// analysis sweeper's own precedence order (DSD before zero-byte,
// matching collectAnalysisCandidates' control flow in
// cmd/bridge/analyze.go — pinned by the lockstep test there).
type AnalysisCoverage struct {
	// TotalLocal counts filesystem-backed tracks only — UPnP-routed
	// rows never resolve on disk and are never analysed.
	//
	// The sweeper agrees: it enumerates via Store.TrackPathsLocal,
	// which carries the same anti-join. It did NOT until that method
	// existed — it walked every row, so all 15,283 routed tracks on the
	// hybrid fixture landed in its `missing` bucket on every hourly
	// tick, and the Jobs page showed `total 15372, missing 13553`
	// directly beside this field reading 89. Keep the two enumerations
	// on the same predicate: they are rendered side by side, so a
	// divergence here is not a subtle inconsistency, it is one panel
	// contradicting the other.
	TotalLocal int
	// DSDExcluded: .dsf/.dff sources — sox can't decode 1-bit DSD, so
	// these are permanently out of scope, not a backlog.
	DSDExcluded int
	// ZeroByteExcluded: zero-byte sources (failed/incomplete uploads)
	// skipped at collection time. Size is the SCAN-TIME size.
	ZeroByteExcluded int
	// AnalysedFresh / AnalysedStale split eligible tracks' analysis
	// rows by schema version. APPROXIMATION, documented for the UI:
	// per-row disk mtime/size freshness (the sweeper's real skip gate)
	// is not SQL-computable, so a row whose source changed on disk
	// still counts as analysed here — the sweeper's last-run counts
	// are the exact truth.
	AnalysedFresh int
	AnalysedStale int
}

// AnalysisCoverage computes the coverage snapshot in ONE pass over
// tracks LEFT JOINed to track_analysis (PK source_path, ON DELETE
// CASCADE — deleted-track orphans can't exist; the join conditions
// additionally exclude analysis rows for tracks that have since become
// DSD/zero-byte, so analysed <= eligible holds by construction).
// Plain-column SQL (~ms at 20k rows) but call sites cache it behind a
// TTL + singleflight — the admin polls this.
func (s *Store) AnalysisCoverage(ctx context.Context, schemaVersion string) (AnalysisCoverage, error) {
	var c AnalysisCoverage
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN lower(t.path) LIKE '%.dsf' OR lower(t.path) LIKE '%.dff' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.size = 0 AND NOT (lower(t.path) LIKE '%.dsf' OR lower(t.path) LIKE '%.dff') THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ta.waveform_tag != '' AND ta.schema_version = ?1
		                          AND NOT (lower(t.path) LIKE '%.dsf' OR lower(t.path) LIKE '%.dff' OR t.size = 0)
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ta.waveform_tag != '' AND ta.schema_version != ?1
		                          AND NOT (lower(t.path) LIKE '%.dsf' OR lower(t.path) LIKE '%.dff' OR t.size = 0)
		                         THEN 1 ELSE 0 END), 0)
		FROM tracks t
		LEFT JOIN track_analysis ta ON ta.source_path = t.path
		WHERE NOT EXISTS (SELECT 1 FROM upnp_track_routing r WHERE r.source_path = t.path)`,
		schemaVersion)
	if err := row.Scan(&c.TotalLocal, &c.DSDExcluded, &c.ZeroByteExcluded, &c.AnalysedFresh, &c.AnalysedStale); err != nil {
		return AnalysisCoverage{}, err
	}
	return c, nil
}

// CountAnalysis returns (rows-with-a-waveform, total waveform bytes)
// for the analysis stats tile. Mirrors CountVariants' shape; degrades
// to "stats unavailable" on error at the handler. Reads un-mutexed.
func (s *Store) CountAnalysis(ctx context.Context) (int, int64, error) {
	var (
		count int
		bytes sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(waveform_size), 0)
		FROM track_analysis WHERE waveform_tag != ''`)
	if err := row.Scan(&count, &bytes); err != nil {
		return 0, 0, err
	}
	return count, bytes.Int64, nil
}
