package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedCollectionLibrary lays down four albums where only SOME carry
// artwork, which is the shape the mosaic rule turns on.
func seedCollectionLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	ctx := t.Context()
	mk := func(path, title, album, artwork string) *manifest.Track {
		rate, bits := 44100.0, 16
		return &manifest.Track{
			Path: path, Title: title, Album: album, AlbumArtist: album + " Artist",
			Artist: album + " Artist", Codec: "FLAC", Size: 1000,
			ModTime: time.Unix(1, 0), SampleRate: &rate, BitsPerSample: &bits,
			ArtworkMBID: artwork,
		}
	}
	for _, tr := range []*manifest.Track{
		mk("A/Alpha/01.flac", "A1", "Alpha", "local-"+repeat64('a')),
		mk("A/Alpha/02.flac", "A2", "Alpha", "local-"+repeat64('a')), // same album
		mk("B/Bravo/01.flac", "B1", "Bravo", ""),                     // NO artwork
		mk("C/Charlie/01.flac", "C1", "Charlie", "local-"+repeat64('c')),
		mk("D/Delta/01.flac", "D1", "Delta", "local-"+repeat64('d')),
		mk("E/Echo/01.flac", "E1", "Echo", "local-"+repeat64('e')),
	} {
		if err := st.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
}

func repeat64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func catalogFor(t *testing.T, srv *Server) *librarycat.Catalog {
	t.Helper()
	cat, err := srv.libraryCatalog(t.Context())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return cat
}

// TestMosaicSkipsArtworklessAlbumsBeforeCounting is the rule that keeps
// a mosaic from rendering with a hole in it: an album with no cover must
// be skipped BEFORE the four slots are filled, not counted into them and
// dropped at render time.
func TestMosaicSkipsArtworklessAlbumsBeforeCounting(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	cat := catalogFor(t, srv)

	// Bravo has no artwork and sits SECOND, so a rule that filtered
	// after picking four would return three usable covers.
	paths := []string{
		"A/Alpha/01.flac",
		"B/Bravo/01.flac",
		"C/Charlie/01.flac",
		"D/Delta/01.flac",
		"E/Echo/01.flac",
	}
	got := mosaicFor(cat, paths)
	if len(got) != mosaicCovers {
		t.Fatalf("mosaic returned %d covers, want %d — an artworkless album consumed a slot: %+v",
			len(got), mosaicCovers, got)
	}
	for _, ref := range got {
		if ref.ArtworkMBID == "" {
			t.Errorf("mosaic contains an empty artwork ref: %+v", got)
		}
	}
}

// TestMosaicDedupesByAlbum pins that repeats of one album do not fill
// the grid with the same picture — the common shape for a playlist that
// opens with several tracks from one record.
func TestMosaicDedupesByAlbum(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	cat := catalogFor(t, srv)

	got := mosaicFor(cat, []string{"A/Alpha/01.flac", "A/Alpha/02.flac"})
	if len(got) != 1 {
		t.Fatalf("two tracks from one album produced %d covers, want 1: %+v", len(got), got)
	}
}

// TestPlaylistDetailReportsUnresolvedMembers pins that a member which
// cannot become a playable row is COUNTED and REPORTED rather than
// silently dropped. A foreign reference (another bridge's track) is the
// realistic case, and hiding it would make this page disagree with the
// count on its own tile and on the operator's Data page.
func TestPlaylistDetailReportsUnresolvedMembers(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	ctx := t.Context()

	const id = "11111111-1111-4111-8111-111111111111"
	err := srv.deps.Manifest.UpsertPlaylist(ctx, "dev-token", manifest.PlaylistRow{
		ID: id, Name: "Mixed", LastModifiedAt: time.Now().UnixNano(),
	}, []manifest.PlaylistItemRow{
		{Position: 0, Path: "A/Alpha/01.flac"},
		{Position: 1, OriginFingerprint: "AA:BB", OriginPath: "/elsewhere/x.flac"},
		{Position: 2, Path: "C/Charlie/01.flac"},
	})
	if err != nil {
		t.Fatalf("seed playlist: %v", err)
	}

	w, body := playerGet(t, srv, "/api/player/playlists/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	tracks, _ := body["tracks"].([]any)
	if len(tracks) != 2 {
		t.Errorf("hydrated %d tracks, want 2 (the foreign ref cannot resolve)", len(tracks))
	}
	coll, _ := body["collection"].(map[string]any)
	if n, _ := coll["count"].(float64); int(n) != 3 {
		t.Errorf("count = %v, want 3 — the foreign member still counts", coll["count"])
	}
	if n, _ := body["unresolved"].(float64); int(n) != 1 {
		t.Errorf("unresolved = %v, want 1", body["unresolved"])
	}
}

func TestPlaylistDetailUnknownIDIs404(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	w, _ := playerGet(t, srv, "/api/player/playlists/99999999-9999-4999-8999-999999999999")
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// TestCollectionCoverRejectsUnknownScopeAndKey pins the bounded-alphabet
// gate. Both values end up in a FILENAME, so they are validated against
// closed sets before any path join — the discipline the artwork routes
// already follow.
func TestCollectionCoverRejectsUnknownScopeAndKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		target string
		want   int
		why    string
	}{
		{"/api/library/collection-cover/bogus/abc", http.StatusBadRequest, "unknown scope"},
		{"/api/library/collection-cover/playlist/..%2F..%2Fetc%2Fpasswd", http.StatusBadRequest, "traversal"},
		{"/api/library/collection-cover/playlist/UPPER..dots", http.StatusBadRequest, "outside the alphabet"},
		{"/api/library/collection-cover/playlist/no-such-key", http.StatusNotFound, "valid but absent"},
		{"/api/library/collection-cover/smartmix/daily-mix", http.StatusNotFound, "valid but absent"},
	} {
		w, _ := playerGet(t, srv, tc.target)
		if w.Code != tc.want {
			t.Errorf("%s (%s): status %d, want %d", tc.target, tc.why, w.Code, tc.want)
		}
	}
}

// TestMixDetailFlattensTimeOfDay pins that the time-of-day family's 24
// hourly pools arrive as ONE deduped list, matching what the operator
// page shows — the player and that page must not disagree about what a
// mix contains.
func TestMixDetailFlattensTimeOfDay(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	ctx := t.Context()

	paths := []string{"A/Alpha/01.flac", "C/Charlie/01.flac", "D/Delta/01.flac"}
	hourly := map[int][]manifest.SmartPlaylistItem{}
	for h := 0; h < 24; h++ {
		for k, p := range paths {
			hourly[h] = append(hourly[h], manifest.SmartPlaylistItem{Position: k, Path: p})
		}
	}
	blob, err := json.Marshal(manifest.SmartPlaylistHourlyBlob{Hourly: hourly})
	if err != nil {
		t.Fatal(err)
	}
	err = srv.deps.Manifest.ReplaceSmartPlaylists(ctx, []manifest.StoredSmartPlaylist{{
		Slug: "time-of-day", Kind: "timeOfDay", Title: "Time of Day",
		RefreshedAt: time.Now().UnixNano(), ItemsJSON: blob,
	}})
	if err != nil {
		t.Fatalf("seed mix: %v", err)
	}

	w, body := playerGet(t, srv, "/api/player/mixes/time-of-day")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	tracks, _ := body["tracks"].([]any)
	// 24 pools x 3 entries, but only 3 DISTINCT paths.
	if len(tracks) != len(paths) {
		t.Errorf("got %d tracks, want %d — the hourly pools were not deduped", len(tracks), len(paths))
	}
}

func TestMixDetailRejectsBadSlug(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	// Percent-encoded: httptest.NewRequest PANICS on a raw space or a
	// bare "..", so the malformed shapes have to reach the router the
	// way a browser would send them.
	for _, slug := range []string{"UPPER", "with%20space", "%2E%2E", "a%2Fb", "9leading-digit-ok"} {
		w, _ := playerGet(t, srv, "/api/player/mixes/"+slug)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("slug %q: status %d, want 400 or 404", slug, w.Code)
		}
	}
}
