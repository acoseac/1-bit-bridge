package admin

import (
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedFavoritesLibrary lays down two albums with distinct identity
// triples — one with a year and one without, since year 0 ("tag absent")
// is its own case in the album-identity key.
func seedFavoritesLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	ctx := t.Context()
	rate, bits := 44100.0, 16
	mk := func(path, title, album, albumArtist string, year int) *manifest.Track {
		tr := &manifest.Track{
			Path: path, Title: title, Album: album, AlbumArtist: albumArtist,
			Artist: albumArtist, Codec: "FLAC", Size: 1000,
			ModTime: time.Unix(1, 0), SampleRate: &rate, BitsPerSample: &bits,
			ArtworkMBID: "local-" + repeat64('a'),
		}
		if year > 0 {
			y := year
			tr.Year = &y
		}
		return tr
	}
	for _, tr := range []*manifest.Track{
		mk("Krall/Wallflower/01.flac", "K1", "Wallflower", "Diana Krall", 2015),
		mk("Krall/Wallflower/02.flac", "K2", "Wallflower", "Diana Krall", 2015),
		mk("Stone/Black Diamond/01.flac", "S1", "Black Diamond", "Angie Stone", 0),
	} {
		if err := st.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
}

// seedFavorites writes the singleton favorites document through the same
// store call the wire handler uses, so the fixtures exercise the real
// local-XOR-foreign row shapes rather than a hand-built table.
func seedFavorites(t *testing.T, st *manifest.Store,
	tracks []manifest.FavoriteTrackRow, albums []manifest.FavoriteAlbumRow) {
	t.Helper()
	if err := st.UpsertFavorites(t.Context(), "device-token",
		time.Now().UnixNano(), tracks, albums); err != nil {
		t.Fatalf("seed favorites: %v", err)
	}
}

// TestFavoriteAlbumIDMatchesTheCatalogsOwnIdentity is the load-bearing
// contract of this endpoint: a hearted album is found by the SAME key
// the catalog grouped on, not by a resemblance test.
//
// Asserting against cat.Albums[i].ID rather than a hard-coded digest is
// what makes this a real pin — a hand-written hash would only prove the
// test author and the handler agree, while this fails the moment the
// two derivations diverge for any reason (a change in dupes'
// normalization, in HashID, or in what the builder stamps).
func TestFavoriteAlbumIDMatchesTheCatalogsOwnIdentity(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedFavoritesLibrary(t, srv.deps.Manifest)
	cat := catalogFor(t, srv)

	for _, a := range cat.Albums {
		got := favoriteAlbumID(a.AlbumArtist, a.Title, a.Year)
		if got != a.ID {
			t.Errorf("favoriteAlbumID(%q, %q, %d) = %s, want the catalog's own id %s",
				a.AlbumArtist, a.Title, a.Year, got, a.ID)
		}
	}
}

// TestFavoritesResolvesAlbumsAndTracks walks the whole endpoint: hearted
// albums come back as full album rows (id, cover ref, geometry — a tile,
// not a string), and hearted tracks come back hydrated and playable.
func TestFavoritesResolvesAlbumsAndTracks(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedFavoritesLibrary(t, srv.deps.Manifest)
	now := time.Now().UnixNano()
	seedFavorites(t, srv.deps.Manifest,
		[]manifest.FavoriteTrackRow{
			{Path: "Krall/Wallflower/01.flac", FavoritedAt: now},
			{Path: "Stone/Black Diamond/01.flac", FavoritedAt: now - 1},
		},
		[]manifest.FavoriteAlbumRow{
			{AlbumArtist: "Diana Krall", Album: "Wallflower", Year: 2015, FavoritedAt: now},
			// Year 0 is "tag absent", not "year zero" — the identity
			// key drops it entirely, so this must still resolve.
			{AlbumArtist: "Angie Stone", Album: "Black Diamond", FavoritedAt: now - 1},
		})

	w, body := playerGet(t, srv, "/api/player/favorites")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if stored, _ := body["stored"].(bool); !stored {
		t.Error("stored = false after a document was written")
	}
	albums, _ := body["albums"].([]any)
	if len(albums) != 2 {
		t.Fatalf("resolved %d albums, want 2: %v", len(albums), body["albums"])
	}
	first, _ := albums[0].(map[string]any)
	if first["id"] == "" || first["id"] == nil {
		t.Error("resolved album carries no id — the tile could not link anywhere")
	}
	if first["artworkMBID"] == nil {
		t.Error("resolved album carries no artwork ref — it would render as a blank tile")
	}
	if first["title"] != "Wallflower" {
		t.Errorf("albums[0].title = %v, want Wallflower (newest heart first)", first["title"])
	}
	tracks, _ := body["tracks"].([]any)
	if len(tracks) != 2 {
		t.Fatalf("hydrated %d tracks, want 2: %v", len(tracks), body["tracks"])
	}
	tr, _ := tracks[0].(map[string]any)
	if play, _ := tr["play"].(map[string]any); play == nil || play["kind"] == "" {
		t.Errorf("hydrated track carries no playability verdict: %v", tr)
	}
	if _, present := body["unresolvedAlbums"]; present {
		t.Errorf("unresolvedAlbums present when everything resolved: %v", body["unresolvedAlbums"])
	}
	if _, present := body["unresolvedTracks"]; present {
		t.Errorf("unresolvedTracks present when everything resolved: %v", body["unresolvedTracks"])
	}
}

// TestFavoritesCountsWhatItCannotResolve pins that hearts belonging to
// somewhere else are COUNTED rather than silently dropped. Both flavours
// are here because they arrive by different routes: a foreign track ref
// has no local path at all, while an album absent from this library
// fails the identity lookup.
func TestFavoritesCountsWhatItCannotResolve(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedFavoritesLibrary(t, srv.deps.Manifest)
	now := time.Now().UnixNano()
	seedFavorites(t, srv.deps.Manifest,
		[]manifest.FavoriteTrackRow{
			{Path: "Krall/Wallflower/01.flac", FavoritedAt: now},
			{OriginFingerprint: "smb", OriginPath: "NAS/x.flac", FavoritedAt: now - 1},
			// Local, but gone from the library since it was hearted.
			{Path: "Krall/Deleted/09.flac", FavoritedAt: now - 2},
		},
		[]manifest.FavoriteAlbumRow{
			{AlbumArtist: "Diana Krall", Album: "Wallflower", Year: 2015, FavoritedAt: now},
			{AlbumArtist: "Some Other Band", Album: "Not Here", Year: 1999, FavoritedAt: now - 1},
		})

	_, body := playerGet(t, srv, "/api/player/favorites")
	if n, _ := body["unresolvedAlbums"].(float64); int(n) != 1 {
		t.Errorf("unresolvedAlbums = %v, want 1", body["unresolvedAlbums"])
	}
	if n, _ := body["unresolvedTracks"].(float64); int(n) != 2 {
		t.Errorf("unresolvedTracks = %v, want 2 (one foreign, one deleted)", body["unresolvedTracks"])
	}
	albums, _ := body["albums"].([]any)
	if len(albums) != 1 {
		t.Errorf("resolved %d albums, want 1 — an unresolvable heart must not be fabricated", len(albums))
	}
}

// TestFavoritesDoesNotFuzzyMatchOnYear pins the deliberate absence of a
// looser fallback. A heart whose year disagrees with the library's is a
// DIFFERENT album by the identity both sides share; matching it anyway
// would attribute the heart to the wrong record while looking like it
// worked.
func TestFavoritesDoesNotFuzzyMatchOnYear(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedFavoritesLibrary(t, srv.deps.Manifest)
	// "Black Diamond" is in the library with NO year, and this heart
	// claims 1999. That direction is what makes this a control: the
	// obvious looser rule — retry with the year dropped — would FIND
	// the library's copy, so a test built the other way round (a heart
	// with a year against a library album with a different year) passes
	// under the fallback and pins nothing. Verified by building it.
	seedFavorites(t, srv.deps.Manifest, nil, []manifest.FavoriteAlbumRow{
		{AlbumArtist: "Angie Stone", Album: "Black Diamond", Year: 1999,
			FavoritedAt: time.Now().UnixNano()},
	})

	_, body := playerGet(t, srv, "/api/player/favorites")
	if albums, _ := body["albums"].([]any); len(albums) != 0 {
		t.Errorf("a year mismatch resolved to %d albums, want 0 — a looser retry "+
			"would attribute this heart to a different record: %v", len(albums), body["albums"])
	}
	if n, _ := body["unresolvedAlbums"].(float64); int(n) != 1 {
		t.Errorf("unresolvedAlbums = %v, want 1", body["unresolvedAlbums"])
	}
}

// TestFavoritesNeverStoredShape pins that a bridge no device has ever
// synced to answers with empty ARRAYS and stored:false — not null, which
// the client would have to special-case, and not a 404, which is
// reserved for a route that does not exist.
func TestFavoritesNeverStoredShape(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedFavoritesLibrary(t, srv.deps.Manifest)

	w, body := playerGet(t, srv, "/api/player/favorites")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if stored, _ := body["stored"].(bool); stored {
		t.Error("stored = true with no document written")
	}
	for _, key := range []string{"albums", "tracks"} {
		v, present := body[key]
		if !present {
			t.Errorf("%s absent — the client should get [], not undefined", key)
			continue
		}
		if _, ok := v.([]any); !ok {
			t.Errorf("%s = %v, want an empty array", key, v)
		}
	}
}
