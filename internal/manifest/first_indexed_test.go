package manifest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func firstIndexedColumn(t *testing.T, s *Store, path string) (int64, int64) {
	t.Helper()
	var first sql.NullInt64
	var indexed int64
	err := s.db.QueryRow(
		`SELECT first_indexed_at, indexed_at FROM tracks WHERE path = ?`, path,
	).Scan(&first, &indexed)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !first.Valid {
		t.Fatalf("%s first_indexed_at is NULL", path)
	}
	return first.Int64, indexed
}

func TestV53BackfillCopiesTheStoredMtimeAndLeavesTheCursor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seed := time.Date(2018, 1, 2, 3, 4, 5, 0, time.UTC)
	clockAt(s, seed)
	for _, path := range []string{"past.flac", "zero.flac", "future.flac"} {
		err := s.UpsertTrack(ctx, &Track{Path: path, Size: 1, ModTime: seed})
		if err != nil {
			t.Fatal(err)
		}
	}
	past := time.Date(2014, 5, 6, 7, 8, 9, 0, time.UTC).UnixNano()
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	const cursor int64 = 42
	_, err := s.db.Exec(`
		UPDATE tracks SET first_indexed_at = NULL, indexed_at = ?,
			mtime_ns = CASE path
				WHEN 'past.flac' THEN ?
				WHEN 'zero.flac' THEN 0
				ELSE ?
			END`, cursor, past, future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 52`); err != nil {
		t.Fatal(err)
	}
	migrated := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	clockAt(s, migrated)
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}

	gotPast, gotCursor := firstIndexedColumn(t, s, "past.flac")
	if gotPast != past || gotCursor != cursor {
		t.Fatalf("past date %d cursor %d, want %d and %d", gotPast, gotCursor, past, cursor)
	}
	for _, path := range []string{"zero.flac", "future.flac"} {
		got, idx := firstIndexedColumn(t, s, path)
		if got != migrated.UnixNano() || idx != cursor {
			t.Fatalf("%s date %d cursor %d, want migration clock %d and cursor %d",
				path, got, idx, migrated.UnixNano(), cursor)
		}
	}
}

func TestAnUpdateOfTheSamePathLeavesTheFirstIndexedDate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	later := time.Date(2022, 8, 9, 10, 11, 12, 0, time.UTC)
	clockAt(s, first)
	path := "Artist/Album/song.flac"
	err := s.UpsertTrack(ctx, &Track{Path: path, Size: 4, ModTime: first})
	if err != nil {
		t.Fatal(err)
	}
	clockAt(s, later)
	again := &Track{Path: path, Size: 9, ModTime: later, Title: "Retitled"}
	again.carryFirstIndexedNS = later.UnixNano()
	if err := s.UpsertTrack(ctx, again); err != nil {
		t.Fatal(err)
	}
	batched := &Track{Path: path, Size: 11, ModTime: later, Title: "Batched"}
	batched.carryFirstIndexedNS = later.UnixNano()
	if err := s.UpsertTrackBatch(ctx, []*Track{batched}); err != nil {
		t.Fatal(err)
	}
	got, _ := firstIndexedColumn(t, s, path)
	if got != first.UnixNano() {
		t.Fatalf("update moved the date to %d, want %d", got, first.UnixNano())
	}
}

func TestANewPathCopiesTheDateOnlyWhenTheCallerPassesIt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	carried := time.Date(2011, 2, 3, 4, 5, 6, 0, time.UTC)
	now := time.Date(2023, 7, 8, 9, 10, 11, 0, time.UTC)
	clockAt(s, now)
	with := &Track{Path: "kept.flac", Size: 1, ModTime: now}
	with.carryFirstIndexedNS = carried.UnixNano()
	without := &Track{Path: "fresh.flac", Size: 1, ModTime: now}
	if err := s.UpsertTrack(ctx, with); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrackBatch(ctx, []*Track{without}); err != nil {
		t.Fatal(err)
	}
	gotKept, _ := firstIndexedColumn(t, s, "kept.flac")
	gotFresh, _ := firstIndexedColumn(t, s, "fresh.flac")
	if gotKept != carried.UnixNano() {
		t.Fatalf("carried insert stored %d, want %d", gotKept, carried.UnixNano())
	}
	if gotFresh != now.UnixNano() {
		t.Fatalf("plain insert stored %d, want the store clock %d", gotFresh, now.UnixNano())
	}
}

func TestTheExtractorStampLeavesTheFirstIndexedDate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first := time.Date(2016, 4, 5, 6, 7, 8, 0, time.UTC)
	clockAt(s, first)
	path := "stamped.flac"
	err := s.UpsertTrack(ctx, &Track{Path: path, Size: 2, ModTime: first})
	if err != nil {
		t.Fatal(err)
	}
	stamped := &Track{Path: path}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{stamped}); err != nil {
		t.Fatal(err)
	}
	got, _ := firstIndexedColumn(t, s, path)
	if got != first.UnixNano() {
		t.Fatalf("stamp moved the date to %d", got)
	}
}

func TestFirstIndexedAtIsOmittedUntilTheRowHasOneAndIsNotStoredInTags(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	when := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	clockAt(s, when)
	row := &Track{Path: "a.flac", Size: 1, ModTime: when, Title: "A"}
	row.FirstIndexedAt = &when
	if err := s.UpsertTrack(ctx, row); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	err := s.db.QueryRow(`SELECT tags_json FROM tracks WHERE path = ?`, "a.flac").Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("firstIndexedAt")) {
		t.Fatalf("tags_json stored the spliced field: %s", raw)
	}
	got, err := s.GetTrack(ctx, "a.flac")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`"firstIndexedAt":"2019-03-04T05:06:07Z"`)
	if !bytes.Contains(body, want) {
		t.Fatalf("manifest JSON %s", body)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET first_indexed_at = NULL WHERE path = ?`, "a.flac"); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d rows", len(listed))
	}
	body, err = json.Marshal(listed[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("firstIndexedAt")) {
		t.Fatalf("a null column reached the wire: %s", body)
	}
}

func TestRecordFirstIndexedCarryKeepsTheEarliestAndSkipsRoutedRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rootA := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	rootB := time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC)
	clockAt(s, rootA)
	err := s.UpsertTrack(ctx, &Track{Path: "RootA/Album/a.flac", Size: 1, ModTime: rootA})
	if err != nil {
		t.Fatal(err)
	}
	clockAt(s, rootB)
	err = s.UpsertTrack(ctx, &Track{Path: "RootB/Album/a.flac", Size: 1, ModTime: rootB})
	if err != nil {
		t.Fatal(err)
	}
	routedAt := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	clockAt(s, routedAt)
	err = s.UpsertTrack(ctx, &Track{Path: "routed/song.flac", Size: 1, ModTime: routedAt})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`
		INSERT INTO upnp_track_routing
			(source_path, server_udn, object_id, res_url, last_seen_at)
		VALUES ('routed/song.flac', 'udn', '1', 'http://upstream/song', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordFirstIndexedCarry(ctx, true, "RootA"); err != nil {
		t.Fatal(err)
	}
	var n int
	var ns int64
	err = s.db.QueryRow(`SELECT COUNT(*), MIN(first_indexed_at) FROM first_indexed_carry`).Scan(&n, &ns)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || ns != rootA.UnixNano() {
		t.Fatalf("carry rows %d date %d, want 1 and the survivor's %d", n, ns, rootA.UnixNano())
	}
	var key string
	err = s.db.QueryRow(`SELECT path_key FROM first_indexed_carry`).Scan(&key)
	if err != nil {
		t.Fatal(err)
	}
	if key != "RootA/Album/a.flac" {
		t.Fatalf("carry key %q", key)
	}
}

func TestASecondRecordAfterTheWipeKeepsTheCarry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	when := time.Date(2012, 2, 2, 0, 0, 0, 0, time.UTC)
	clockAt(s, when)
	err := s.UpsertTrack(ctx, &Track{Path: "Artist/Album/song.flac", Size: 1, ModTime: when})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordFirstIndexedCarry(ctx, false, "Music"); err != nil {
		t.Fatal(err)
	}
	if err := s.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordFirstIndexedCarry(ctx, false, "Music"); err != nil {
		t.Fatal(err)
	}
	var ns int64
	err = s.db.QueryRow(
		`SELECT first_indexed_at FROM first_indexed_carry WHERE path_key = ?`,
		"Music/Artist/Album/song.flac",
	).Scan(&ns)
	if err != nil {
		t.Fatal(err)
	}
	if ns != when.UnixNano() {
		t.Fatalf("retry replaced the carry with %d", ns)
	}
}

func TestARowWithoutAFirstIndexedDateIsNotReady(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	err := s.UpsertTrack(ctx, &Track{Path: "a.flac", Size: 1, ModTime: time.Unix(10, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET first_indexed_at = NULL WHERE path = ?`, "a.flac"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.FirstIndexedAtReady(ctx)
	if err != nil || ok {
		t.Fatalf("unfinished backfill ready=%v err=%v", ok, err)
	}
	if NewProvider(s, nil).FirstIndexedAtReady(ctx) {
		t.Fatal("provider advertised a library the backfill has not finished")
	}
	if _, err := s.db.Exec(`DELETE FROM tracks`); err != nil {
		t.Fatal(err)
	}
	ok, err = s.FirstIndexedAtReady(ctx)
	if err != nil || !ok {
		t.Fatalf("empty library ready=%v err=%v", ok, err)
	}
	closed, err := OpenStore(t.TempDir() + "/closed.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	ok, err = closed.FirstIndexedAtReady(ctx)
	if err == nil || ok {
		t.Fatalf("closed store ready=%v err=%v", ok, err)
	}
	if NewProvider(closed, nil).FirstIndexedAtReady(ctx) {
		t.Fatal("provider advertised a count it could not read")
	}
	var none *Provider
	if none.FirstIndexedAtReady(ctx) {
		t.Fatal("nil provider is ready")
	}
}

func TestCarryKeyIsTheRootAndThePathWithinIt(t *testing.T) {
	got, ok := carryKey("Artist/Album/a.flac", false, "Music")
	if !ok || got != "Music/Artist/Album/a.flac" {
		t.Fatalf("adding a root: %q ok=%v", got, ok)
	}
	got, ok = carryKey("Music/Artist/Album/a.flac", true, "Music")
	if !ok || got != "Music/Artist/Album/a.flac" {
		t.Fatalf("collapse survivor: %q ok=%v", got, ok)
	}
	if _, ok = carryKey("Jazz/Artist/Album/a.flac", true, "Music"); ok {
		t.Fatal("a removed root's row was carried onto the survivor")
	}
	if _, ok = carryKey("Artist/Album/a.flac", false, ""); ok {
		t.Fatal("an empty root name was accepted")
	}
	if _, ok = carryKey("noslash.flac", true, "Music"); ok {
		t.Fatal("a path outside the survivor was carried")
	}
}

func TestTheNullDateFillUsesThePartialIndex(t *testing.T) {
	if strings.Contains(firstIndexedBackfillSQL, "ORDER BY") {
		t.Fatal("ordering the null-date fill makes SQLite skip idx_tracks_first_indexed_at_null")
	}
	fill := strings.Join(strings.Fields(firstIndexedBackfillSQL), " ")
	inner := strings.Join(strings.Fields(firstIndexedNullRowidsSQL), " ")
	if !strings.Contains(fill, inner) {
		t.Fatal("the fill no longer selects the statement this test explains")
	}
	s := openTestStore(t)
	ordered := `SELECT rowid FROM tracks WHERE first_indexed_at IS NULL ORDER BY rowid LIMIT ?`
	if strings.Contains(explainPlan(t, s, ordered, 1), "idx_tracks_first_indexed_at_null") {
		t.Fatal("the ordered select reaches the partial index, so this fixture cannot show that dropping the order is what selects it")
	}
	plan := explainPlan(t, s, firstIndexedNullRowidsSQL, 1)
	if !strings.Contains(plan, "idx_tracks_first_indexed_at_null") {
		t.Fatalf("null-date fill plan %q", plan)
	}
}

func TestFoldedFirstIndexedUsesThePathIndex(t *testing.T) {
	s := openTestStore(t)
	bare := `SELECT MIN(first_indexed_at) FROM tracks
		WHERE path = ? AND first_indexed_at IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path
		  )`
	if strings.Contains(explainPlan(t, s, bare, "Artist/Album/a.flac"), "idx_tracks_path_unicode_lower") {
		t.Fatal("a byte-exact path predicate reaches the folded index, so this fixture cannot tell the two lookups apart")
	}
	plan := explainPlan(t, s, foldedFirstIndexedSQL, "Artist/Album/a.flac")
	if !strings.Contains(plan, "idx_tracks_path_unicode_lower") {
		t.Fatalf("folded date plan %q", plan)
	}
}
