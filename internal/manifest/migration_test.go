package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestMigrationLadderFreshDB asserts a freshly created DB lands at
// the highest migration version after OpenStore.
func TestMigrationLadderFreshDB(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	v := readUserVersion(t, s.db)
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Errorf("user_version = %d, want %d", v, want)
	}
}

// TestMigrationLadderIdempotent re-opens the same DB and asserts
// migrations don't re-run (no error, version stays).
func TestMigrationLadderIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "twice.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (1): %v", err)
	}
	v1 := readUserVersion(t, s.db)
	s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (2): %v", err)
	}
	defer s2.Close()
	v2 := readUserVersion(t, s2.db)
	if v1 != v2 {
		t.Errorf("user_version changed across reopen: %d -> %d", v1, v2)
	}
}

// TestMigrationLadderPreLadderUpgrade simulates a pre-ladder DB:
// schema applied via the legacy code path (CREATE TABLE IF NOT
// EXISTS + swallowed ALTER) but user_version still 0. Re-opening
// it should idempotently bump to the current head.
func TestMigrationLadderPreLadderUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preLadder.db")

	// Simulate the pre-ladder schema by hand-applying migration 1
	// and then explicitly clearing user_version.
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(migrations[0].sql); err != nil {
		t.Fatalf("apply baseline schema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("clear user_version: %v", err)
	}
	db.Close()

	// Now open through the migration ladder. Should run migration 1
	// (idempotent, no-op effects on already-existent tables) and
	// bump user_version.
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	v := readUserVersion(t, s.db)
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Errorf("user_version = %d, want %d (current head)", v, want)
	}
}

// TestMigrationV25ToV26RebuildsUnicodeLowerIndexes simulates an
// operator DB parked at v25 by hand-applying migrations 1..25,
// inserting NFD-path track / variant / analysis rows at v25 (so the
// rows PREDATE the upgrade), and stamping user_version, then lets
// OpenStore run v26 (same shape as the pre-ladder upgrade test
// above). Asserts the ladder lands on the head version, that all
// three indexes embedding unicode_lower exist with their rebuilt
// definitions, and that the pre-existing NFD rows resolve through
// LookupTrack / LookupVariant / LookupAnalysis via the NFC shape iOS
// sends. (Gemini round 1: the first version inserted the NFD row
// after the upgrade, so it never exercised the rebuild over
// pre-existing data.)
//
// Honest limitation: the pre-NFC unicode_lower can't be resurrected
// in-process (the registered function is process-global), so the v25
// inserts write NFC-composed index entries too — genuinely byte-stale
// NFD-keyed entries can't be forged from Go. What this pins is that
// v26 applies cleanly over populated tables and that rows written
// before the upgrade resolve through the rebuilt indexes afterwards.
func TestMigrationV25ToV26RebuildsUnicodeLowerIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v25.db")

	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	const preNFCHead = 25
	for _, m := range migrations {
		if m.version > preNFCHead {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
	}

	// Seed the NFD-shaped rows at v25, before the upgrade. Raw SQL
	// mirrors what the scanner/variant/analysis writers would have
	// persisted; tags_json carries the wire shape LookupTrack
	// deserialises.
	const nfd = "Sigur Ro\u0301s/A\u0301gætis byrjun/01 Svefn-g-englar.flac"
	const ioshape = "/sigur rós/ágætis byrjun/01 svefn-g-englar.flac"
	const variantID = "v176400-24"
	tagsJSON := `{"path":"` + nfd + `","size":1}`
	for _, stmt := range []struct {
		sql  string
		args []interface{}
	}{
		{`INSERT INTO tracks (path, size, mtime_ns, tags_json, indexed_at)
		  VALUES (?, 1, 1, ?, 1)`, []interface{}{nfd, tagsJSON}},
		{`INSERT INTO track_variants (source_path, variant_id, sidecar_path, format,
		  sample_rate, bits_per_sample, size_bytes, source_mtime_ns, source_size,
		  sox_settings, created_at)
		  VALUES (?, ?, '/cache/variant.flac', 'flac', 176400, 24, 1024, 1, 1, '{}', 1)`,
			[]interface{}{nfd, variantID}},
		{`INSERT INTO track_analysis (source_path, waveform_path, waveform_tag,
		  waveform_size, source_mtime_ns, source_size, schema_version, created_at)
		  VALUES (?, '/cache/01.wave', '0123456789abcdef', 10, 1, 1, 'peak-v1', 1)`,
			[]interface{}{nfd}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert NFD row at v25: %v", err)
		}
	}

	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, preNFCHead)); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	db.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	if v, want := readUserVersion(t, s.db), migrations[len(migrations)-1].version; v != want {
		t.Errorf("user_version = %d, want %d (current head)", v, want)
	}

	// v26 drops and recreates every functional index that embeds
	// unicode_lower so NFD-stored rows are re-keyed under the
	// NFC-composed fold. Verify the definitions survived the upgrade.
	for _, idx := range []string{
		"idx_tracks_path_unicode_lower",
		"idx_track_variants_source_path_unicode_lower",
		"idx_track_analysis_source_path_unicode_lower",
	} {
		var def string
		if err := s.db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, idx,
		).Scan(&def); err != nil {
			t.Errorf("index %s missing after v26 upgrade: %v", idx, err)
			continue
		}
		if !strings.Contains(def, "unicode_lower") {
			t.Errorf("index %s definition %q does not embed unicode_lower", idx, def)
		}
	}

	// End-to-end on the upgraded DB: the rows seeded at v25 resolve
	// via the NFC + lowercase shape iOS sends.
	tr, err := s.LookupTrack(context.Background(), ioshape)
	if err != nil {
		t.Fatalf("LookupTrack: %v", err)
	}
	if tr == nil || tr.Path != nfd {
		t.Errorf("LookupTrack after v26 upgrade = %v; want the pre-existing NFD-stored row", tr)
	}

	vr, err := s.LookupVariant(context.Background(), ioshape, variantID)
	if err != nil {
		t.Fatalf("LookupVariant: %v", err)
	}
	if vr == nil || vr.SourcePath != nfd {
		t.Errorf("LookupVariant after v26 upgrade = %v; want the pre-existing NFD-stored row", vr)
	}

	ar, err := s.LookupAnalysis(context.Background(), ioshape)
	if err != nil {
		t.Fatalf("LookupAnalysis: %v", err)
	}
	if ar == nil || ar.SourcePath != nfd {
		t.Errorf("LookupAnalysis after v26 upgrade = %v; want the pre-existing NFD-stored row", ar)
	}
}

func TestMigrationV51ToV52KeepsTrackClocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v51.db")
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	const head = 51
	for _, m := range migrations {
		if m.version > head {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO tracks (path, size, mtime_ns, tags_json, indexed_at, enriched_at) VALUES ('a.flac', 1, 1, '{}', 42, 7)`,
		`INSERT INTO favorites_meta (id, last_modified_at, device_token, updated_at) VALUES (1, 1, 'dev', 1)`,
		`INSERT INTO favorite_tracks (path, favorited_at) VALUES ('a.flac', 9)`,
		`INSERT INTO playlists (id, device_token, name, last_modified_at, updated_at) VALUES ('pl', 'dev', 'Kept', 1, 1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %s: %v", stmt, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, head)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := readUserVersion(t, s.db); v != migrations[len(migrations)-1].version {
		t.Fatalf("user_version %d", v)
	}
	var indexed, enriched int64
	if err := s.db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = 'a.flac'`).Scan(&indexed, &enriched); err != nil {
		t.Fatal(err)
	}
	if indexed != 42 || enriched != 7 {
		t.Fatalf("indexed_at %d enriched_at %d", indexed, enriched)
	}
	var fav int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorite_tracks WHERE path = 'a.flac'`).Scan(&fav); err != nil || fav != 1 {
		t.Fatalf("favorite %d err %v", fav, err)
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM playlists WHERE id = 'pl'`).Scan(&name); err != nil || name != "Kept" {
		t.Fatalf("playlist %q err %v", name, err)
	}
	epoch, err := s.BackupEpoch(context.Background())
	if err != nil || epoch == "" {
		t.Fatalf("epoch %q err %v", epoch, err)
	}
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM favorites_meta WHERE id = 1`).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("revision %d err %v", revision, err)
	}
}

func TestMigrationV52GainsAFirstIndexedDate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v52.db")
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	const head = 52
	for _, m := range migrations {
		if m.version > head {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
	}
	const stored = int64(1577836800000000000)
	if _, err := db.Exec(
		`INSERT INTO tracks (path, size, mtime_ns, tags_json, indexed_at) VALUES ('Artist/Album/song.flac', 10, ?, '{}', 42)`,
		stored,
	); err != nil {
		t.Fatal(err)
	}
	var cols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name = 'first_indexed_at'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("a v52 schema already had first_indexed_at")
	}
	var carry string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'first_indexed_carry'`).Scan(&carry)
	if err != sql.ErrNoRows {
		t.Fatalf("v52 already had the carry table (%q, %v)", carry, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, head)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := readUserVersion(t, s.db); v != migrations[len(migrations)-1].version {
		t.Fatalf("user_version %d", v)
	}
	got, err := s.GetTrack(context.Background(), "Artist/Album/song.flac")
	if err != nil || got == nil || got.FirstIndexedAt == nil {
		t.Fatalf("track %v err %v", got, err)
	}
	want := time.Unix(0, stored).UTC()
	if !got.FirstIndexedAt.Equal(want) {
		t.Fatalf("first indexed %s, want %s", got.FirstIndexedAt, want)
	}
	var indexed int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = 'Artist/Album/song.flac'`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 42 {
		t.Fatalf("indexed_at %d", indexed)
	}
	var def string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_tracks_first_indexed_at_null'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "first_indexed_at IS NULL") {
		t.Fatalf("index %s", def)
	}
	var gen int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('first_indexed_carry') WHERE name IN ('generation', 'target_multi')`).Scan(&gen); err != nil || gen != 2 {
		t.Fatalf("carry columns %d err %v", gen, err)
	}
}

func readUserVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
