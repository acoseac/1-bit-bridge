package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedExportFixture puts one of everything into the store, including the two
// things that must NOT come back out: a device token and a playback row
// attributed to it.
func seedExportFixture(t *testing.T, st *manifest.Store) (deviceToken string) {
	t.Helper()
	ctx := context.Background()
	deviceToken = "d3adb33fd3adb33fd3adb33fd3adb33f"

	if err := st.UpsertTrack(ctx, &manifest.Track{
		Path: "Ada/Album/01 Song.flac", Title: "Song", Artist: "Ada",
		Album: "Album", Size: 10, ModTime: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDeviceRegistration(ctx, deviceToken, "tok-1", "Ada's iPhone"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := st.UpsertPlaylist(ctx, deviceToken,
		manifest.PlaylistRow{
			ID: "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a", DeviceToken: deviceToken,
			Name: "Late Night", LastModifiedAt: time.Now().UnixNano(),
		},
		[]manifest.PlaylistItemRow{{
			Position: 0, Path: "Ada/Album/01 Song.flac", Title: "Song", Artist: "Ada",
		}}); err != nil {
		t.Fatalf("seed playlist: %v", err)
	}
	if err := st.UpsertFavorites(ctx, deviceToken, time.Now().UnixNano(),
		[]manifest.FavoriteTrackRow{{
			Path: "Ada/Album/01 Song.flac", Title: "Song", Artist: "Ada",
			FavoritedAt: time.Now().UnixNano(),
		}},
		[]manifest.FavoriteAlbumRow{{
			AlbumArtist: "Ada", Album: "Album", Year: 2026,
			FavoritedAt: time.Now().UnixNano(),
		}}); err != nil {
		t.Fatalf("seed favorites: %v", err)
	}
	if err := st.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{{
		DeviceToken:  deviceToken,
		Path:         "Ada/Album/01 Song.flac",
		StartedAt:    time.Now().Add(-time.Hour).UnixNano(),
		DurationUsed: 123,
		Codec:        "flac",
	}}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	return deviceToken
}

// The one assertion that matters most: an export is a privacy feature, so it
// must not hand a working credential to whoever the file is forwarded to.
//
// DeviceRegistration.DeviceToken is the iOS Keychain recovery token and every
// history row carries it. Both are one struct field away from the wire.
func TestExportCarriesNoCredentials(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token := seedExportFixture(t, srv.deps.Manifest)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/export", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, token) {
		t.Fatal("the export contains a device token — a live Keychain credential")
	}
	if strings.Contains(body, "tok-1") {
		t.Fatal("the export contains an auth token id")
	}
	// The control for the two assertions above: the fixture really did reach
	// the bundle, so their silence means absence and not an empty export.
	if !strings.Contains(body, "Ada's iPhone") {
		t.Fatal("the device never reached the export; the token checks proved nothing")
	}
}

// It downloads as a file, and says what it is.
func TestExportIsADownloadAndSelfDescribing(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedExportFixture(t, srv.deps.Manifest)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/export", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	srv.Handler().ServeHTTP(rec, r)

	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, ".json") {
		t.Errorf("Content-Disposition = %q, want a .json attachment", cd)
	}
	var got exportBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the export is not valid JSON: %v", err)
	}
	if got.Format != exportFormat {
		t.Errorf("format = %q, want %q — a reader has to be able to tell", got.Format, exportFormat)
	}
	if got.LibraryName != "Test Library" {
		t.Errorf("libraryName = %q", got.LibraryName)
	}
	if len(got.Devices) != 1 || got.Devices[0].Name != "Ada's iPhone" {
		t.Errorf("devices = %+v", got.Devices)
	}
	if len(got.History) != 1 || got.History[0].DeviceName != "Ada's iPhone" {
		t.Errorf("history = %+v; the device is named, not tokenised", got.History)
	}
	// The populated playlist and favourite paths, which nothing exercised until
	// CodeRabbit pointed out that the fixture created neither — two loops that
	// could have been wrong in any way and still passed.
	if len(got.Playlists) != 1 || got.Playlists[0].Name != "Late Night" {
		t.Fatalf("playlists = %+v", got.Playlists)
	}
	if len(got.Playlists[0].Items) != 1 || got.Playlists[0].Items[0].Title != "Song" {
		t.Errorf("playlist items = %+v; a playlist without its items is not an export",
			got.Playlists[0].Items)
	}
	if len(got.Favorites.Tracks) != 1 || got.Favorites.Tracks[0].Artist != "Ada" {
		t.Errorf("favorite tracks = %+v", got.Favorites.Tracks)
	}
	if len(got.Favorites.Albums) != 1 || got.Favorites.Albums[0].Album != "Album" {
		t.Errorf("favorite albums = %+v", got.Favorites.Albums)
	}
	if got.Favorites.Tracks[0].FavoritedAt == nil {
		t.Error("favoritedAt is nil for a track that was favourited")
	}
}

// Empty collections must marshal as [] and not null, so a consumer can iterate
// without a nil check — the same rule the SSE snapshots follow.
func TestExportEmptyCollectionsAreArrays(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/export", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	srv.Handler().ServeHTTP(rec, r)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"playlists", "playbackHistory", "devices"} {
		if string(raw[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, raw[key])
		}
	}
	// And a bundle with nothing in it is not "truncated".
	if _, ok := raw["truncated"]; ok {
		t.Error("an empty export claims to be truncated")
	}
}
