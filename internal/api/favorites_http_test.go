package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// newFavoritesTestServer wires a real manifest.Store as the favorites store
// behind the authed() middleware (the newPlaylistTestServer shape).
func newFavoritesTestServer(t *testing.T) (string, string, *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{t.TempDir()},
		ListenAddress: ":7788",
		LibraryName:   "T",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	mstore, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mstore.Close() })
	srv := New(cfg, authStore, nil, "fp").WithDeviceRegistrar(mstore).WithFavoritesStore(mstore)
	return raw, "deadbeef", srv
}

func decodeFavoritesBody(t *testing.T, resp *http.Response) favoritesDTO {
	t.Helper()
	defer resp.Body.Close()
	var dto favoritesDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode favorites body: %v", err)
	}
	return dto
}

func TestFavoritesPutGetRoundTrip(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	body := `{"lastModifiedAt": 1000,
		"tracks": [
			{"path": "Pink Floyd/DSOTM/Money.flac", "favoritedAt": 300},
			{"originFingerprint": "smb", "originPath": "/music/x.flac",
			 "title": "X", "artist": "Y", "favoritedAt": 200}],
		"albums": [
			{"albumArtist": "Pink Floyd", "album": "DSOTM", "year": 1973, "favoritedAt": 100}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: status %d", resp.StatusCode)
	}
	var stored favoritesStoredResponse
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil || !stored.Stored {
		t.Fatalf("stored response mangled: %+v (%v)", stored, err)
	}
	resp.Body.Close()

	get := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d", get.StatusCode)
	}
	dto := decodeFavoritesBody(t, get)
	if dto.LastModifiedAt != 1000 || len(dto.Tracks) != 2 || len(dto.Albums) != 1 {
		t.Fatalf("round-trip mismatch: %+v", dto)
	}
	if dto.Tracks[0].Path != "Pink Floyd/DSOTM/Money.flac" || dto.Tracks[0].FavoritedAt != 300 {
		t.Errorf("local track mangled: %+v", dto.Tracks[0])
	}
	if dto.Tracks[1].OriginFingerprint != "smb" || dto.Tracks[1].Title != "X" {
		t.Errorf("foreign track mangled: %+v", dto.Tracks[1])
	}
	if dto.Albums[0].Album != "DSOTM" || dto.Albums[0].Year != 1973 {
		t.Errorf("album mangled: %+v", dto.Albums[0])
	}
}

// Never-stored GET serves the EMPTY document (lastModifiedAt 0, [] arrays)
// — singleton semantics, never a 404-as-missing.
func TestFavoritesGetNeverStoredReturnsEmptyDoc(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d", resp.StatusCode)
	}
	// Assert the raw wire shape carries [] (not null) for both arrays.
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["lastModifiedAt"]) != "0" {
		t.Errorf("want lastModifiedAt 0, got %s", raw["lastModifiedAt"])
	}
	if string(raw["tracks"]) != "[]" || string(raw["albums"]) != "[]" {
		t.Errorf("want [] arrays, got tracks=%s albums=%s", raw["tracks"], raw["albums"])
	}
}

// A strictly-older PUT 409s WITH the full server copy in the body — the
// load-bearing half of the contract (iOS union-merges from it).
func TestFavoritesPutStaleCarriesFullServerCopy(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	first := `{"lastModifiedAt": 2000,
		"tracks": [{"path": "a/b.flac", "favoritedAt": 10}], "albums": []}`
	resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first put: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	stale := `{"lastModifiedAt": 1999,
		"tracks": [{"path": "c/d.flac", "favoritedAt": 20}], "albums": []}`
	resp2 := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, stale)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("stale put: status %d", resp2.StatusCode)
	}
	defer resp2.Body.Close()
	var staleResp favoritesStaleResponse
	if err := json.NewDecoder(resp2.Body).Decode(&staleResp); err != nil {
		t.Fatalf("decode stale body: %v", err)
	}
	if staleResp.Error != "stale" || staleResp.Server.LastModifiedAt != 2000 ||
		len(staleResp.Server.Tracks) != 1 || staleResp.Server.Tracks[0].Path != "a/b.flac" {
		t.Errorf("409 body must carry the full server copy: %+v", staleResp)
	}
	// Equal stamp is accepted (idempotent re-push).
	equal := `{"lastModifiedAt": 2000,
		"tracks": [{"path": "a/b.flac", "favoritedAt": 10}], "albums": []}`
	resp3 := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, equal)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("equal-stamp put must be accepted: status %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// Strict local-XOR-foreign per entry — partially-filled entries are 400s.
func TestFavoritesPutRejectsMixedLocalForeign(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	for _, tracks := range []string{
		`[{"path": "a.flac", "originFingerprint": "smb", "originPath": "/x", "favoritedAt": 1}]`, // both
		`[{"originFingerprint": "smb", "favoritedAt": 1}]`,                                       // fp without path
		`[{"favoritedAt": 1}]`, // neither
	} {
		body := fmt.Sprintf(`{"lastModifiedAt": 1000, "tracks": %s, "albums": []}`, tracks)
		resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("tracks=%s: want 400, got %d", tracks, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestFavoritesPutValidation(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	for name, body := range map[string]string{
		"zero lastModifiedAt":    `{"lastModifiedAt": 0, "tracks": [], "albums": []}`,
		"zero track favoritedAt": `{"lastModifiedAt": 1, "tracks": [{"path": "a.flac", "favoritedAt": 0}], "albums": []}`,
		"album without name":     `{"lastModifiedAt": 1, "tracks": [], "albums": [{"favoritedAt": 1}]}`,
		"negative album year":    `{"lastModifiedAt": 1, "tracks": [], "albums": [{"album": "A", "year": -1, "favoritedAt": 1}]}`,
		"zero album favoritedAt": `{"lastModifiedAt": 1, "tracks": [], "albums": [{"album": "A", "favoritedAt": 0}]}`,
		"slash-only local path":  `{"lastModifiedAt": 1, "tracks": [{"path": "/", "favoritedAt": 1}], "albums": []}`,
		"items not an array":     `{"lastModifiedAt": 1, "tracks": {"a": 1}, "albums": []}`,
	} {
		resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// Empty-set PUT is VALID ("no favorites") — the unfavorite direction
// propagates via full replace; there is no DELETE route.
func TestFavoritesPutEmptySetStores(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt": 1000, "tracks": [], "albums": []}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty-set put: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	get := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	dto := decodeFavoritesBody(t, get)
	if dto.LastModifiedAt != 1000 || len(dto.Tracks) != 0 || len(dto.Albums) != 0 {
		t.Errorf("empty doc mismatch: %+v", dto)
	}
}

// Server-side normalization: backslashes → slashes on both path fields;
// a single leading slash stripped on LOCAL paths only (foreign originPath
// stays opaque).
func TestFavoritesPutPathNormalization(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	body := `{"lastModifiedAt": 1000,
		"tracks": [
			{"path": "/Artist\\Album\\01 Song.flac", "favoritedAt": 1},
			{"originFingerprint": "smb", "originPath": "\\music\\x.flac", "favoritedAt": 2}],
		"albums": []}`
	resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	get := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	dto := decodeFavoritesBody(t, get)
	if len(dto.Tracks) != 2 {
		t.Fatalf("want 2 tracks, got %d", len(dto.Tracks))
	}
	// GET orders newest-favorited first — look entries up by shape rather
	// than assuming insertion order.
	var local, foreign *favoriteTrackDTO
	for i := range dto.Tracks {
		if dto.Tracks[i].Path != "" {
			local = &dto.Tracks[i]
		} else {
			foreign = &dto.Tracks[i]
		}
	}
	if local == nil || local.Path != "Artist/Album/01 Song.flac" {
		t.Errorf("local path not normalized: %+v", local)
	}
	// Foreign originPath: backslashes normalized, leading slash PRESERVED.
	if foreign == nil || foreign.OriginPath != "/music/x.flac" {
		t.Errorf("foreign originPath mangled: %+v", foreign)
	}
}

// A duplicate-bearing payload dedups last-wins and still stores 200 — the
// partial UNIQUE indexes must never surface as a 500 for client data.
func TestFavoritesPutDuplicatePayloadDedupsLastWins(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	body := `{"lastModifiedAt": 1000,
		"tracks": [
			{"path": "a/b.flac", "favoritedAt": 1},
			{"path": "/a/b.flac", "favoritedAt": 9},
			{"originFingerprint": "smb", "originPath": "/x.flac", "favoritedAt": 1},
			{"originFingerprint": "smb", "originPath": "/x.flac", "favoritedAt": 8}],
		"albums": [
			{"albumArtist": "A", "album": "B", "year": 2001, "favoritedAt": 1},
			{"albumArtist": "A", "album": "B", "year": 2001, "favoritedAt": 7}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dup payload must store cleanly: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	get := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	dto := decodeFavoritesBody(t, get)
	if len(dto.Tracks) != 2 || len(dto.Albums) != 1 {
		t.Fatalf("dedup mismatch: tracks=%d albums=%d", len(dto.Tracks), len(dto.Albums))
	}
	// Last-wins: the normalized-identical "/a/b.flac" (favoritedAt 9) wins;
	// the later foreign (8) + album (7) entries win.
	byPath := map[string]int64{}
	for _, tr := range dto.Tracks {
		key := tr.Path
		if key == "" {
			key = tr.OriginFingerprint + "|" + tr.OriginPath
		}
		byPath[key] = tr.FavoritedAt
	}
	if byPath["a/b.flac"] != 9 || byPath["smb|/x.flac"] != 8 {
		t.Errorf("last-wins dedup mismatch: %+v", byPath)
	}
	if dto.Albums[0].FavoritedAt != 7 {
		t.Errorf("album last-wins mismatch: %+v", dto.Albums[0])
	}
}

func TestFavoritesRequiresDeviceToken(t *testing.T) {
	token, _, srv := newFavoritesTestServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/favorites", token, "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 device_token_required, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error != "device_token_required" {
		t.Errorf("want device_token_required envelope, got %+v (%v)", e, err)
	}
}

// Feature-off (store unwired) → 404 favorites_not_supported on both routes.
func TestFavoritesFeatureOff404s(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, authStore, nil, "fp") // NO WithFavoritesStore
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		resp := doReq(t, srv, m, "/v1/favorites", raw, "deadbeef",
			`{"lastModifiedAt": 1, "tracks": [], "albums": []}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404 feature-off, got %d", m, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- capped-decoder unit tests (the streaming-cap contract) ---

func TestCappedFavoriteTracksAbortsAtCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i <= maxFavoriteTracks; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"path":"a","favoritedAt":1}`)
	}
	sb.WriteString("]")
	var c cappedFavoriteTracks
	if err := c.UnmarshalJSON([]byte(sb.String())); !errors.Is(err, errTooManyFavoriteTracks) {
		t.Fatalf("want errTooManyFavoriteTracks, got %v", err)
	}
}

func TestCappedFavoriteAlbumsAbortsAtCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i <= maxFavoriteAlbums; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"album":"a","favoritedAt":1}`)
	}
	sb.WriteString("]")
	var c cappedFavoriteAlbums
	if err := c.UnmarshalJSON([]byte(sb.String())); !errors.Is(err, errTooManyFavoriteAlbums) {
		t.Fatalf("want errTooManyFavoriteAlbums, got %v", err)
	}
}

func TestCappedFavoritesDecoderShapes(t *testing.T) {
	var c cappedFavoriteTracks
	if err := c.UnmarshalJSON([]byte(`null`)); err != nil || c != nil {
		t.Errorf("null must decode to nil: %v (%v)", c, err)
	}
	if err := c.UnmarshalJSON([]byte(`{"a":1}`)); !errors.Is(err, errFavoritesNotArray) {
		t.Errorf("non-array must be rejected: %v", err)
	}
	if err := c.UnmarshalJSON([]byte(`[{"path":"a","favoritedAt":1}] garbage`)); !errors.Is(err, errFavoritesNotArray) {
		t.Errorf("trailing garbage must be rejected: %v", err)
	}
	if err := c.UnmarshalJSON([]byte(`[{"path":"a","favoritedAt":1}]`)); err != nil || len(c) != 1 {
		t.Errorf("well-formed array must decode: %v (%v)", c, err)
	}
}

// Health advertises `favorites` when the store is wired, in alpha position
// (between dlnaServer and keyTempo: d < f < k), and stays absent when not.
func TestHealthAdvertisesFavoritesFeature(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/health", token, dt, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d", resp.StatusCode)
	}
	body := readAllOrFail(t, resp)
	resp.Body.Close()
	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !containsString(got.Features, "favorites") {
		t.Errorf("Features missing favorites; got %v", got.Features)
	}
	assertAlphaSorted(t, got.Features)
}

func TestHealthOmitsFavoritesWhenUnwired(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, authStore, nil, "fp") // NO WithFavoritesStore
	resp := doReq(t, srv, http.MethodGet, "/v1/health", raw, "", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()
	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if containsString(got.Features, "favorites") {
		t.Errorf("unwired store must not advertise favorites; got %v", got.Features)
	}
}
