package manifest

import (
	"context"
	"errors"
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
	// The v36 CHECK rejects rows with no identity, both identities, or a
	// partial foreign identity — even via direct SQL that bypasses
	// UpsertFavorites' own validation.
	for name, stmt := range map[string]string{
		"no identity":      `INSERT INTO favorite_tracks (favorited_at) VALUES (9)`,
		"both identities":  `INSERT INTO favorite_tracks (path, origin_fingerprint, origin_path, favorited_at) VALUES ('b/c.flac', 'fp', '/y.flac', 9)`,
		"partial foreign":  `INSERT INTO favorite_tracks (origin_fingerprint, favorited_at) VALUES ('fp-only', 9)`,
		"empty local path": `INSERT INTO favorite_tracks (path, favorited_at) VALUES ('', 9)`,
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Errorf("%s must violate the local-XOR-foreign CHECK", name)
		}
	}
}

// UpsertFavorites mirrors the table CHECK at the store layer so a future
// caller that bypasses the API validation still can't persist a row with a
// broken identity.
func TestUpsertFavoritesRejectsBrokenIdentity(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	for name, row := range map[string]FavoriteTrackRow{
		"no identity":     {FavoritedAt: 1},
		"both identities": {Path: "a.flac", OriginFingerprint: "fp", OriginPath: "/x.flac", FavoritedAt: 1},
		"partial foreign": {OriginFingerprint: "fp", FavoritedAt: 1},
	} {
		if err := s.UpsertFavorites(ctx, "devA", 1, []FavoriteTrackRow{row}, nil); err == nil {
			t.Errorf("%s must be rejected by UpsertFavorites", name)
		}
	}
}
