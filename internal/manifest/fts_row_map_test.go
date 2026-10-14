package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestFTSDeleteAddressesTheRowMap pins the delete the bulk path pays for.
// The statement is the one inside tracks_fts_ad, with old.path replaced by
// a literal, so a trigger that goes back to WHERE path = loses the rowid
// lookup. The plan text is the same on a 1,000-row store and a 4,000-row
// store: a rowid equality on an FTS5 table is a constant-cost lookup.
func TestFTSDeleteAddressesTheRowMap(t *testing.T) {
	small := explainFTSDelete(t, 1000)
	large := explainFTSDelete(t, 4000)
	if small != large {
		t.Errorf("plan changed with table size\n1000 rows:\n%s\n4000 rows:\n%s", small, large)
	}
	if !strings.Contains(small, "INDEX 0:=") {
		t.Errorf("plan = %q, want a rowid equality (INDEX 0:=)", small)
	}
	if !strings.Contains(small, "SEARCH tracks_fts_row USING PRIMARY KEY") {
		t.Errorf("plan = %q, want the map's primary key", small)
	}
}

func explainFTSDelete(t *testing.T, n int) string {
	t.Helper()
	s := openTestStore(t)
	seedPlainTracks(t, s, n)
	ad := triggerSQL(t, s, "tracks_fts_ad")
	del := ftsDeleteStatement(t, ad)
	if strings.Contains(del, "DELETE FROM tracks_fts WHERE path =") {
		t.Errorf("fts delete keys on path: %s", del)
	}
	au := triggerSQL(t, s, "tracks_fts_au")
	if !strings.Contains(au, "AFTER UPDATE OF path, tags_json ON tracks") {
		t.Errorf("au trigger = %s", au)
	}
	if !strings.Contains(au, "json_extract(old.tags_json, '$.title')") {
		t.Errorf("au WHEN does not read the stored title: %s", au)
	}
	if !strings.Contains(au, "json_valid(") {
		t.Errorf("au WHEN does not guard invalid JSON: %s", au)
	}
	ai := triggerSQL(t, s, "tracks_fts_ai")
	if !strings.Contains(ai, "last_insert_rowid()") || !strings.Contains(ai, "tracks_fts_row") {
		t.Errorf("ai trigger does not record the fts rowid: %s", ai)
	}
	return explainQueryPlan(t, s.db, del)
}

func seedPlainTracks(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// A no-op once Commit has run; it ends the transaction when a
	// t.Fatal below stops the seed early.
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at,
		sample_rate, bits_per_sample, is_dsd, codec, extractor_version, audio_md5,
		compression, folder_art_key, extract_refused, first_indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		tags := fmt.Sprintf(`{"title":"T%d","artist":"A","album":"B"}`, i)
		if _, err := stmt.Exec(fmt.Sprintf("Album/%06d.flac", i), 1, 1, tags, 1, 0, 0, 0, "FLAC", 1, "", "", "", 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func triggerSQL(t *testing.T, s *Store, name string) string {
	t.Helper()
	var body string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&body); err != nil {
		t.Fatalf("trigger %s: %v", name, err)
	}
	return body
}

// ftsDeleteStatement returns the DELETE FROM tracks_fts inside a trigger,
// with old.path replaced by a literal so EXPLAIN can plan it.
func ftsDeleteStatement(t *testing.T, body string) string {
	t.Helper()
	const lead = "DELETE FROM tracks_fts WHERE"
	i := strings.Index(body, lead)
	if i < 0 {
		t.Fatalf("trigger has no fts delete: %s", body)
	}
	rest := body[i:]
	semi := strings.Index(rest, ";")
	if semi < 0 {
		t.Fatalf("fts delete does not end: %s", rest)
	}
	del := rest[:semi]
	return strings.ReplaceAll(del, "old.path", "'Album/000000.flac'")
}

func explainQueryPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, vals[len(vals)-1].String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestFTSCountsStayEqualAndMatchStaysRight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	putTrack(t, s, &Track{Path: "AlbumA/01.flac", Title: "Zephyr Alpha", Artist: "A", Album: "L"})
	putTrack(t, s, &Track{Path: "AlbumA/02.flac", Title: "Zephyr Beta", Artist: "A", Album: "L"})
	putTrack(t, s, &Track{Path: "AlbumB/01.flac", Title: "OnlyOther", Artist: "B", Album: "M"})
	order := searchPaths(t, s, "zephyr")
	requireSearch(t, s, "zephyr", order, "a repeat search")
	if len(order) != 2 {
		t.Fatalf("zephyr hits = %v", order)
	}

	id := mappedRowID(t, s, "AlbumA/01.flac")
	putTrack(t, s, &Track{Path: "AlbumA/01.flac", Title: "Zephyr Alpha", Artist: "A", Album: "L"})
	requireRowIDKept(t, s, "AlbumA/01.flac", id, "same-bytes upsert")
	requireSearch(t, s, "zephyr", order, "after same-bytes upsert")
	requireConsistent(t, s)

	putTrack(t, s, &Track{
		Path: "AlbumA/01.flac", Title: "Zephyr Alpha", Artist: "A", Album: "L",
		MusicBrainzAlbumID: "11111111-1111-1111-1111-111111111111",
	})
	requireRowIDKept(t, s, "AlbumA/01.flac", id, "mbid-only update")
	requireSearch(t, s, "zephyr", order, "after mbid update")
	requireConsistent(t, s)

	mustDeletePrefix(ctx, t, s, "AlbumB")
	requireSearch(t, s, "zephyr", order, "after an unrelated delete")
	requireSearch(t, s, "onlyother", nil, "a deleted track")
	requireConsistent(t, s)

	putTrack(t, s, &Track{Path: "AlbumA/01.flac", Title: "Zephyr Alpha Revised", Artist: "A", Album: "L"})
	requireRowIDMoved(t, s, "AlbumA/01.flac", id, "a title change")
	requireSearch(t, s, "revised", []string{"AlbumA/01.flac"}, "after a title change")
	requireConsistent(t, s)

	// A row that is the highest FTS rowid is reused by the insert the
	// rename's trigger does. Hold a later row so the reuse cannot hide
	// a trigger that did not rewrite the document.
	putTrack(t, s, &Track{Path: "AlbumZ/01.flac", Title: "Holder", Artist: "Z", Album: "Z"})
	beforeRename := mappedRowID(t, s, "AlbumA/01.flac")
	if _, err := s.db.Exec(`UPDATE tracks SET path = ? WHERE path = ?`, "AlbumA/01-renamed.flac", "AlbumA/01.flac"); err != nil {
		t.Fatal(err)
	}
	if n := queryCount(t, s, `SELECT COUNT(*) FROM tracks_fts_row WHERE path = ?`, "AlbumA/01.flac"); n != 0 {
		t.Fatalf("old path still mapped, count %d", n)
	}
	requireRowIDMoved(t, s, "AlbumA/01-renamed.flac", beforeRename, "a path rename")
	renamed := searchPaths(t, s, "zephyr")
	if !slices.Contains(renamed, "AlbumA/01-renamed.flac") || slices.Contains(renamed, "AlbumA/01.flac") {
		t.Fatalf("zephyr after rename = %v", renamed)
	}
	requireConsistent(t, s)

	mustDeletePrefix(ctx, t, s, "AlbumA")
	requireSearch(t, s, "zephyr", nil, "after a prefix delete")
	requireConsistent(t, s)

	putTrack(t, s, &Track{Path: "AlbumC/01.flac", Title: "After", Artist: "C", Album: "N"})
	if err := s.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	requireConsistent(t, s)
	if n := countRows(t, s, "tracks"); n != 0 {
		t.Fatalf("wipe left %d tracks", n)
	}
}

// requireSearch fails unless a MATCH for q returns want, in order. A nil
// want means no hits.
func requireSearch(t *testing.T, s *Store, q string, want []string, when string) {
	t.Helper()
	if got := searchPaths(t, s, q); !slices.Equal(got, want) {
		t.Fatalf("%q %s = %v, want %v", q, when, got, want)
	}
}

// requireRowIDKept fails if path's FTS row moved off id.
func requireRowIDKept(t *testing.T, s *Store, path string, id int64, what string) {
	t.Helper()
	if got := mappedRowID(t, s, path); got != id {
		t.Fatalf("%s moved the fts rowid %d -> %d", what, id, got)
	}
}

// requireRowIDMoved fails if path's FTS row is still id: the trigger did
// not rewrite the document.
func requireRowIDMoved(t *testing.T, s *Store, path string, id int64, what string) {
	t.Helper()
	if mappedRowID(t, s, path) == id {
		t.Fatalf("%s left the fts rowid in place", what)
	}
}

func mustDeletePrefix(ctx context.Context, t *testing.T, s *Store, prefix string) {
	t.Helper()
	if _, err := s.DeleteTracksByPrefix(ctx, prefix); err != nil {
		t.Fatal(err)
	}
}

// queryCount runs a COUNT(*) query with arguments; countRows (in
// retention_test.go) counts a whole table.
func queryCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSetFTSRowMapVersionMatchesTheVersion keeps the literal PRAGMA the
// migration stamps equal to ftsRowMapVersion.
func TestSetFTSRowMapVersionMatchesTheVersion(t *testing.T) {
	if want := fmt.Sprintf("PRAGMA user_version = %d", ftsRowMapVersion); setFTSRowMapVersion != want {
		t.Fatalf("setFTSRowMapVersion = %q, want %q", setFTSRowMapVersion, want)
	}
}

func TestAnInvalidTagsJSONStillUpdates(t *testing.T) {
	s := openTestStore(t)
	putTrack(t, s, &Track{Path: "AlbumA/01.flac", Title: "Zephyr Keep", Artist: "A", Album: "L"})
	// json_extract raises on malformed JSON. The MBID expression indexes
	// call it on every tags_json assignment, and every writer in this
	// process marshals with encoding/json, so a stored row cannot be
	// invalid and the trigger's read of old.tags_json cannot raise.
	if _, err := s.db.Exec(`UPDATE tracks SET tags_json = X'FFFF' WHERE path = ?`, "AlbumA/01.flac"); err == nil {
		t.Fatal("invalid JSON was stored")
	}
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("zephyr hits = %v", got)
	}
	requireConsistent(t, s)

	// With those indexes gone a blob can sit in the column. The trigger
	// guards each extract, so a later valid write still replaces the
	// document. A plain UPDATE, because the upsert's artwork_version
	// clause extracts the old blob itself.
	for _, name := range []string{"idx_tracks_artwork_mbid", "idx_tracks_artist_mbid", "idx_tracks_release_mbid"} {
		if _, err := s.db.Exec(`DROP INDEX ` + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE tracks SET tags_json = X'FFFF' WHERE path = ?`, "AlbumA/01.flac"); err != nil {
		t.Fatal(err)
	}
	requireConsistent(t, s)
	if _, err := s.db.Exec(`UPDATE tracks SET tags_json = ? WHERE path = ?`,
		`{"title":"Recovered Title","artist":"A","album":"L"}`, "AlbumA/01.flac"); err != nil {
		t.Fatal(err)
	}
	if got := searchPaths(t, s, "recovered"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("recovered hits = %v", got)
	}
	requireConsistent(t, s)
}

func TestAFreshStoreEndsWithTheRowMap(t *testing.T) {
	rec := loggingtest.Record(t)
	s := openTestStore(t)
	if got := userVersion(t, s.db); got != migrations[len(migrations)-1].version {
		t.Fatalf("version = %d", got)
	}
	requireConsistent(t, s)
	if lines := rec.Lines("tracks_fts row map rebuilt"); len(lines) != 0 {
		t.Fatalf("fresh store rebuilt: %v", lines)
	}
}

func TestAV54StoreWithRowsEndsConsistent(t *testing.T) {
	rec := loggingtest.Record(t)
	path := libraryRewoundToV54(t, "")
	s := openTestStoreAt(t, path)
	if got := userVersion(t, s.db); got != ftsRowMapVersion {
		t.Fatalf("version = %d", got)
	}
	requireConsistent(t, s)
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("hits = %v", got)
	}
	if lines := rec.Lines("tracks_fts row map rebuilt"); len(lines) != 0 {
		t.Fatalf("consistent store rebuilt: %v", lines)
	}
	ad := triggerSQL(t, s, "tracks_fts_ad")
	if strings.Contains(ftsDeleteStatement(t, ad), "DELETE FROM tracks_fts WHERE path =") {
		t.Fatalf("v54 reopen left the path delete: %s", ad)
	}
}

func TestADuplicateFTSRowIsRebuilt(t *testing.T) {
	rec := loggingtest.Record(t)
	path := libraryRewoundToV54(t, `INSERT INTO tracks_fts(path, title, artist, album) VALUES ('AlbumA/01.flac', 'GhostTitle', '', '')`)
	s := openTestStoreAt(t, path)
	requireConsistent(t, s)
	assertRebuildLogged(t, rec, "AlbumA/01.flac", "GhostTitle")
	if got := searchPaths(t, s, "ghosttitle"); len(got) != 0 {
		t.Fatalf("ghost still matches: %v", got)
	}
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestAnOrphanFTSRowIsRebuilt(t *testing.T) {
	rec := loggingtest.Record(t)
	path := libraryRewoundToV54(t, `INSERT INTO tracks_fts(path, title, artist, album) VALUES ('Secret/Album/01.flac', 'GhostTitle', '', '')`)
	s := openTestStoreAt(t, path)
	requireConsistent(t, s)
	assertRebuildLogged(t, rec, "Secret/Album/01.flac", "GhostTitle")
	if got := searchPaths(t, s, "ghosttitle"); len(got) != 0 {
		t.Fatalf("orphan still matches: %v", got)
	}
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestATrackMissingItsFTSRowIsRebuilt(t *testing.T) {
	rec := loggingtest.Record(t)
	path := libraryRewoundToV54(t, `DELETE FROM tracks_fts WHERE path = 'AlbumA/01.flac'`)
	s := openTestStoreAt(t, path)
	requireConsistent(t, s)
	assertRebuildLogged(t, rec, "AlbumA/01.flac")
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestAnErrorAfterTheMapFillLeavesVersion54(t *testing.T) {
	path := libraryRewoundToV54(t, "")
	ftsRowMapAfterFill = func() error { return errors.New("injected after fill") }
	t.Cleanup(func() { ftsRowMapAfterFill = nil })
	if _, err := OpenStore(path); err == nil || !strings.Contains(err.Error(), "injected after fill") {
		t.Fatalf("open = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, db); got != 54 {
		t.Fatalf("version = %d, want 54", got)
	}
	var mapN int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'tracks_fts_row'`).Scan(&mapN); err != nil {
		t.Fatal(err)
	}
	if mapN != 0 {
		t.Fatalf("map table survived, count %d", mapN)
	}
	var ad string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'tracks_fts_ad'`).Scan(&ad); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ad, "WHERE path = old.path") {
		t.Fatalf("rolled-back trigger = %s", ad)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ftsRowMapAfterFill = nil
	s := openTestStoreAt(t, path)
	if got := userVersion(t, s.db); got != ftsRowMapVersion {
		t.Fatalf("version after retry = %d", got)
	}
	requireConsistent(t, s)
	if got := searchPaths(t, s, "zephyr"); !slices.Equal(got, []string{"AlbumA/01.flac"}) {
		t.Fatalf("hits = %v", got)
	}
}

func TestTwoOpenersLeaveTheMapToTheOneThatMigrates(t *testing.T) {
	path := libraryRewoundToV54(t, "")
	// The ladder lock keeps a second opener out of the ladder while the
	// first runs it (backlog B185). A lock that cannot be taken lets both
	// in, as before it, which is the case the map's own guard is for: a
	// directory where the lock file goes is one. The rewind's own open left
	// the file there; nothing holds it now.
	if err := os.Remove(path + ladderLockSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+ladderLockSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	before := ftsRowIDs(t, path)

	entered := make(chan struct{}, 1)
	secondIn := make(chan struct{}, 1)
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	var first atomic.Bool
	var openedMu sync.Mutex
	var opened []*Store
	var wg sync.WaitGroup
	t.Cleanup(func() {
		release()
		ftsRowMapArrived = nil
		ftsRowMapBeforeCommit = nil
		ftsRowMapAfterFill = nil
		wg.Wait()
		openedMu.Lock()
		defer openedMu.Unlock()
		for _, st := range opened {
			_ = st.Close()
		}
	})
	ftsRowMapArrived = func() {
		if first.CompareAndSwap(false, true) {
			return
		}
		secondIn <- struct{}{}
	}
	ftsRowMapBeforeCommit = func() {
		entered <- struct{}{}
		<-hold
	}
	ftsRowMapBuilds.Store(0)
	ftsRowMapSkipped.Store(0)

	type result struct {
		err error
	}
	firstDone := make(chan result, 1)
	secondDone := make(chan result, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, err := OpenStore(path)
		if st != nil {
			openedMu.Lock()
			opened = append(opened, st)
			openedMu.Unlock()
		}
		firstDone <- result{err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first opener did not reach the commit")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, err := OpenStore(path)
		if st != nil {
			openedMu.Lock()
			opened = append(opened, st)
			openedMu.Unlock()
		}
		secondDone <- result{err}
	}()
	select {
	case <-secondIn:
	case <-time.After(10 * time.Second):
		t.Fatal("second opener did not enter the migration")
	}
	release()
	select {
	case res := <-firstDone:
		if res.err != nil {
			t.Fatal(res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first opener did not finish")
	}
	select {
	case res := <-secondDone:
		if res.err != nil {
			t.Fatal(res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second opener did not finish")
	}
	if got := ftsRowMapBuilds.Load(); got != 1 {
		t.Fatalf("builds = %d, want 1", got)
	}
	if got := ftsRowMapSkipped.Load(); got != 1 {
		t.Fatalf("skipped = %d, want 1", got)
	}
	openedMu.Lock()
	st := opened[0]
	openedMu.Unlock()
	requireConsistent(t, st)
	for p, id := range before {
		if got := mappedRowID(t, st, p); got != id {
			t.Fatalf("%s fts rowid %d -> %d", p, id, got)
		}
	}
}

func putTrack(t *testing.T, s *Store, tr *Track) {
	t.Helper()
	tr.Size = 100
	tr.ModTime = time.Unix(1, 0)
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

func searchPaths(t *testing.T, s *Store, query string) []string {
	t.Helper()
	hits, err := s.SearchTracks(context.Background(), query, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}

func mappedRowID(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT fts_rowid FROM tracks_fts_row WHERE path = ?`, path).Scan(&id); err != nil {
		t.Fatalf("map %s: %v", path, err)
	}
	return id
}

func requireConsistent(t *testing.T, s *Store) {
	t.Helper()
	c, err := readFTSCensus(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	if !c.consistent() {
		t.Fatalf("fts census %+v", c)
	}
}

func userVersion(t *testing.T, db censusQuerier) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func libraryRewoundToV54(t *testing.T, plant string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	putTrack(t, s, &Track{Path: "AlbumA/01.flac", Title: "Zephyr Keep", Artist: "A", Album: "L"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rewindToV54FTS(t, path, plant)
	return path
}

func rewindToV54FTS(t *testing.T, path, plant string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(v54FTSRewind); err != nil {
		t.Fatal(err)
	}
	if plant == "" {
		return
	}
	if _, err := db.Exec(plant); err != nil {
		t.Fatal(err)
	}
}

// v54FTSRewind is the trigger set a database has after v8 and before v55.
const v54FTSRewind = `
DROP TRIGGER IF EXISTS tracks_fts_ai;
DROP TRIGGER IF EXISTS tracks_fts_au;
DROP TRIGGER IF EXISTS tracks_fts_ad;
DROP TABLE IF EXISTS tracks_fts_row;
CREATE TRIGGER tracks_fts_ai AFTER INSERT ON tracks BEGIN
  INSERT INTO tracks_fts(path, title, artist, album) VALUES (
    new.path,
    COALESCE(json_extract(new.tags_json, '$.title'), ''),
    COALESCE(json_extract(new.tags_json, '$.artist'), ''),
    COALESCE(json_extract(new.tags_json, '$.album'), '')
  );
END;
CREATE TRIGGER tracks_fts_ad AFTER DELETE ON tracks BEGIN
  DELETE FROM tracks_fts WHERE path = old.path;
END;
CREATE TRIGGER tracks_fts_au AFTER UPDATE OF path, tags_json ON tracks BEGIN
  DELETE FROM tracks_fts WHERE path = old.path;
  INSERT INTO tracks_fts(path, title, artist, album) VALUES (
    new.path,
    COALESCE(json_extract(new.tags_json, '$.title'), ''),
    COALESCE(json_extract(new.tags_json, '$.artist'), ''),
    COALESCE(json_extract(new.tags_json, '$.album'), '')
  );
END;
PRAGMA user_version = 54;
`

func assertRebuildLogged(t *testing.T, rec *loggingtest.Recorder, absent ...string) {
	t.Helper()
	lines := rec.Lines("tracks_fts row map rebuilt")
	if len(lines) != 1 {
		t.Fatalf("rebuild lines = %v", lines)
	}
	for _, secret := range absent {
		if strings.Contains(lines[0], secret) {
			t.Fatalf("rebuild line names %q: %s", secret, lines[0])
		}
	}
	if !strings.Contains(lines[0], "tracks=") || !strings.Contains(lines[0], "fts=") {
		t.Fatalf("rebuild line has no counts: %s", lines[0])
	}
}

func ftsRowIDs(t *testing.T, path string) map[string]int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT path, rowid FROM tracks_fts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			t.Fatal(err)
		}
		out[p] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no fts rows")
	}
	return out
}
