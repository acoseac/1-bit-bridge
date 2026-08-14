package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func sampleFavorites() ([]FavoriteTrackRow, []FavoriteAlbumRow) {
	return []FavoriteTrackRow{
			{Path: "Pink Floyd/DSOTM/Money.flac", FavoritedAt: 300},
			{OriginFingerprint: "smb", OriginPath: "/music/x.flac",
				Title: "X", Artist: "Y", FavoritedAt: 200},
		}, []FavoriteAlbumRow{
			{AlbumArtist: "Pink Floyd", Album: "DSOTM", Year: 1973, FavoritedAt: 100},
		}
}

func TestUpsertGetFavoritesRoundTrip(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	tracks, albums := sampleFavorites()
	if err := s.UpsertFavorites(ctx, "devA", 1000, tracks, albums); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	meta, gotTracks, gotAlbums, err := s.GetFavorites(ctx)
	if err != nil || meta == nil {
		t.Fatalf("get: %v (meta=%v)", err, meta)
	}
	if meta.LastModifiedAt != 1000 || meta.DeviceToken != "devA" || meta.UpdatedAt == 0 {
		t.Errorf("meta mismatch: %+v", meta)
	}
	if len(gotTracks) != 2 || len(gotAlbums) != 1 {
		t.Fatalf("want 2 tracks + 1 album, got %d + %d", len(gotTracks), len(gotAlbums))
	}
	// Newest-favorited first + local-XOR-foreign survives the NULL round-trip.
	if gotTracks[0].Path != "Pink Floyd/DSOTM/Money.flac" || gotTracks[0].OriginFingerprint != "" ||
		gotTracks[0].FavoritedAt != 300 {
		t.Errorf("local track mangled: %+v", gotTracks[0])
	}
	if gotTracks[1].Path != "" || gotTracks[1].OriginFingerprint != "smb" ||
		gotTracks[1].OriginPath != "/music/x.flac" || gotTracks[1].Title != "X" {
		t.Errorf("foreign track mangled: %+v", gotTracks[1])
	}
	if gotAlbums[0].AlbumArtist != "Pink Floyd" || gotAlbums[0].Album != "DSOTM" ||
		gotAlbums[0].Year != 1973 || gotAlbums[0].FavoritedAt != 100 {
		t.Errorf("album mangled: %+v", gotAlbums[0])
	}
}

// Never-stored bridges return nil meta — the handler's "empty doc with
// lastModifiedAt 0" contract hangs off this.
func TestGetFavoritesNeverStoredReturnsNilMeta(t *testing.T) {
	s := newDeviceTestStore(t)
	meta, tracks, albums, err := s.GetFavorites(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if meta != nil || tracks != nil || albums != nil {
		t.Errorf("expected all-nil never-stored read, got meta=%+v tracks=%v albums=%v",
			meta, tracks, albums)
	}
}

// LWW: a strictly-older PUT stales; EQUAL is accepted (idempotent re-push
// after a partial multi-bridge flush).
func TestUpsertFavoritesLWW(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	tracks, albums := sampleFavorites()
	if err := s.UpsertFavorites(ctx, "devA", 1000, tracks, albums); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Strictly older → stale.
	if err := s.UpsertFavorites(ctx, "devB", 999, nil, nil); !errors.Is(err, ErrFavoritesStale) {
		t.Fatalf("want ErrFavoritesStale for older stamp, got %v", err)
	}
	// The stale write must not have touched the stored document.
	meta, gotTracks, _, err := s.GetFavorites(ctx)
	if err != nil || meta == nil || meta.LastModifiedAt != 1000 || len(gotTracks) != 2 {
		t.Fatalf("stale write leaked: meta=%+v tracks=%d err=%v", meta, len(gotTracks), err)
	}
	// Equal → accepted (idempotent).
	if err := s.UpsertFavorites(ctx, "devB", 1000, tracks, albums); err != nil {
		t.Fatalf("equal-stamp upsert must be accepted: %v", err)
	}
	meta, _, _, err = s.GetFavorites(ctx)
	if err != nil || meta.DeviceToken != "devB" {
		t.Errorf("equal-stamp upsert must record the new writer: %+v (%v)", meta, err)
	}
}

// The document replaces WHOLESALE — including down to the empty set
// ("no favorites"; the unfavorite direction propagates via full replace).
func TestUpsertFavoritesReplacesWholesale(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	tracks, albums := sampleFavorites()
	if err := s.UpsertFavorites(ctx, "devA", 1000, tracks, albums); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Replace with a different, smaller set.
	if err := s.UpsertFavorites(ctx, "devA", 2000,
		[]FavoriteTrackRow{{Path: "Other/track.flac", FavoritedAt: 50}}, nil); err != nil {
		t.Fatalf("replace: %v", err)
	}
	_, gotTracks, gotAlbums, err := s.GetFavorites(ctx)
	if err != nil || len(gotTracks) != 1 || len(gotAlbums) != 0 {
		t.Fatalf("replace not wholesale: tracks=%d albums=%d err=%v",
			len(gotTracks), len(gotAlbums), err)
	}
	// Empty-set clear.
	if err := s.UpsertFavorites(ctx, "devA", 3000, nil, nil); err != nil {
		t.Fatalf("empty-set clear: %v", err)
	}
	meta, gotTracks, gotAlbums, err := s.GetFavorites(ctx)
	if err != nil || meta == nil || meta.LastModifiedAt != 3000 ||
		len(gotTracks) != 0 || len(gotAlbums) != 0 {
		t.Errorf("empty-set clear failed: meta=%+v tracks=%d albums=%d err=%v",
			meta, len(gotTracks), len(gotAlbums), err)
	}
}

func TestUpsertFavoritesRequiresDeviceToken(t *testing.T) {
	s := newDeviceTestStore(t)
	if err := s.UpsertFavorites(context.Background(), "", 1000, nil, nil); err == nil {
		t.Fatal("want error for empty device token")
	}
}

// The partial UNIQUE indexes are the DB-level integrity backstop: a direct
// duplicate insert (bypassing the handler's dedup) must be rejected by
// SQLite itself, for both the local-path and the foreign-pair key.
func TestFavoriteTracksPartialIndexIntegrity(t *testing.T) {
	s := newDeviceTestStore(t)
	// Local duplicate.
	if _, err := s.db.Exec(
		`INSERT INTO favorite_tracks (path, favorited_at) VALUES ('a/b.flac', 1)`); err != nil {
		t.Fatalf("seed local: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO favorite_tracks (path, favorited_at) VALUES ('a/b.flac', 2)`); err == nil {
		t.Fatal("duplicate local path must violate idx_fav_tracks_local")
	}
	// Foreign duplicate.
	if _, err := s.db.Exec(
		`INSERT INTO favorite_tracks (origin_fingerprint, origin_path, favorited_at)
		 VALUES ('smb', '/x.flac', 1)`); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO favorite_tracks (origin_fingerprint, origin_path, favorited_at)
		 VALUES ('smb', '/x.flac', 2)`); err == nil {
		t.Fatal("duplicate foreign pair must violate idx_fav_tracks_foreign")
	}
	// A local row and a foreign row do NOT collide with each other (the
	// partial predicates are disjoint).
	if _, err := s.db.Exec(
		`INSERT INTO favorite_tracks (origin_fingerprint, origin_path, favorited_at)
		 VALUES ('other-fp', '/x.flac', 3)`); err != nil {
		t.Fatalf("distinct foreign fingerprint must insert cleanly: %v", err)
	}
}

// seedFavTrack inserts a minimal tracks row so favorites queries can join
// display metadata.
func seedFavTrack(t *testing.T, s *Store, path, title string) {
	t.Helper()
	tags := `{"title":"` + title + `","artist":"Artist","album":"Album"}`
	if _, err := s.db.Exec(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?,?,?,?,?)`, path, int64(100), int64(1), tags, int64(1)); err != nil {
		t.Fatalf("insert track %q: %v", path, err)
	}
}

// The favorites smart-mix pool is BRIDGE-LOCAL only: foreign favorites
// (however many) never enter it, so they can never satisfy the family's
// MinFavorites floor; dupe-suppressed and since-deleted local favorites are
// excluded too; the survivors come back newest heart first.
func TestFavoritedTrackFeatures_LocalServedOnly(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	seedFavTrack(t, s, "a/keep.flac", "Keep")
	seedFavTrack(t, s, "a/keep2.flac", "Keep2")
	seedFavTrack(t, s, "a/suppressed.flac", "Sup")
	if _, err := s.db.Exec(`UPDATE tracks SET dupe_suppressed = 1 WHERE path = 'a/suppressed.flac'`); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	tracks := []FavoriteTrackRow{
		{Path: "a/keep.flac", FavoritedAt: 100},
		{Path: "a/keep2.flac", FavoritedAt: 300}, // newest heart
		{Path: "a/suppressed.flac", FavoritedAt: 250},
		{Path: "a/deleted-since.flac", FavoritedAt: 200}, // no tracks row
	}
	// A pile of foreign favorites that must never enter the pool.
	for i := 0; i < 10; i++ {
		tracks = append(tracks, FavoriteTrackRow{
			OriginFingerprint: "smb", OriginPath: fmt.Sprintf("/f%d.flac", i),
			FavoritedAt: int64(400 + i)})
	}
	if err := s.UpsertFavorites(ctx, "devA", 1000, tracks, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := s.FavoritedTrackFeatures(ctx)
	if err != nil {
		t.Fatalf("FavoritedTrackFeatures: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want exactly the 2 served local favorites, got %d: %+v", len(rows), rows)
	}
	if rows[0].Path != "a/keep2.flac" || rows[1].Path != "a/keep.flac" {
		t.Errorf("want newest heart first, got %q then %q", rows[0].Path, rows[1].Path)
	}
	if rows[0].Title != "Keep2" {
		t.Errorf("features must hydrate from tags_json, got title %q", rows[0].Title)
	}
}

// ListFavoritesForAdmin resolves local entries' display metadata from the
// track index and falls back to the stored render-fallback for foreign
// entries; nil meta = never stored.
func TestListFavoritesForAdmin_JoinAndFallback(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()

	meta, _, _, err := s.ListFavoritesForAdmin(ctx)
	if err != nil || meta != nil {
		t.Fatalf("never-stored must be (nil, nil err), got meta=%v err=%v", meta, err)
	}

	seedFavTrack(t, s, "a/local.flac", "Local Title")
	tracks := []FavoriteTrackRow{
		{Path: "a/local.flac", FavoritedAt: 300},
		{OriginFingerprint: "smb", OriginPath: "/x.flac",
			Title: "Foreign Title", Artist: "FA", FavoritedAt: 200},
		{Path: "a/vanished.flac", FavoritedAt: 100}, // local, track deleted since
	}
	albums := []FavoriteAlbumRow{
		{AlbumArtist: "AA", Album: "AL", Year: 2001, FavoritedAt: 50},
	}
	if err := s.UpsertFavorites(ctx, "devA", 1000, tracks, albums); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	meta, gotTracks, gotAlbums, err := s.ListFavoritesForAdmin(ctx)
	if err != nil || meta == nil {
		t.Fatalf("list: %v (meta=%v)", err, meta)
	}
	if meta.LastModifiedAt != 1000 || meta.DeviceToken != "devA" {
		t.Errorf("meta mismatch: %+v", meta)
	}
	if len(gotTracks) != 3 || len(gotAlbums) != 1 {
		t.Fatalf("want 3 tracks + 1 album, got %d + %d", len(gotTracks), len(gotAlbums))
	}
	// Newest first; local resolves display fields through the join.
	if gotTracks[0].Path != "a/local.flac" || gotTracks[0].Foreign ||
		gotTracks[0].Title != "Local Title" || gotTracks[0].Album != "Album" {
		t.Errorf("local admin row mangled: %+v", gotTracks[0])
	}
	// Foreign renders from the stored fallback and flags Foreign.
	if !gotTracks[1].Foreign || gotTracks[1].Title != "Foreign Title" ||
		gotTracks[1].OriginPath != "/x.flac" || gotTracks[1].Path != "" {
		t.Errorf("foreign admin row mangled: %+v", gotTracks[1])
	}
	// A local favorite whose track vanished degrades to empty display
	// fields rather than being dropped (the safe is still holding it).
	if gotTracks[2].Foreign || gotTracks[2].Path != "a/vanished.flac" || gotTracks[2].Title != "" {
		t.Errorf("vanished-local admin row mangled: %+v", gotTracks[2])
	}
	if gotAlbums[0].Album != "AL" || gotAlbums[0].Year != 2001 {
		t.Errorf("album admin row mangled: %+v", gotAlbums[0])
	}
}
