package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func playerGet(t *testing.T, srv *Server, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var body map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(w.Body.Bytes(), &body)
	}
	return w, body
}

func seedPlayerLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	ctx := t.Context()
	mk := func(path, title, album, albumArtist, codec string, rate float64, bits int, dsd bool, year int) *manifest.Track {
		tr := &manifest.Track{Path: path, Title: title, Album: album,
			AlbumArtist: albumArtist, Artist: albumArtist, Codec: codec,
			Size: 1000, ModTime: time.Unix(1, 0)}
		if rate > 0 {
			tr.SampleRate = &rate
		}
		if bits > 0 {
			b := bits
			tr.BitsPerSample = &b
		}
		if dsd {
			d := true
			tr.IsDSD = &d
		}
		if year > 0 {
			y := year
			tr.Year = &y
		}
		return tr
	}
	for _, tr := range []*manifest.Track{
		mk("Rock/Alpha/01.flac", "One", "Alpha", "The Aardvarks", "FLAC", 44100, 16, false, 2001),
		mk("Rock/Alpha/02.flac", "Two", "Alpha", "The Aardvarks", "FLAC", 44100, 16, false, 2001),
		mk("Jazz/Beta/01.flac", "Three", "Beta", "Zebra Trio", "FLAC", 96000, 24, false, 2019),
		mk("DSD/Gamma/01.dsf", "Four", "Gamma", "Mu Ensemble", "DSF", 0, 0, true, 0),
	} {
		if err := st.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPlayerAlbumsSortsAndFilters exercises the sort vocabulary and the
// quality filter against a seeded library.
func TestPlayerAlbumsSortsAndFilters(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	_ = cfg
	seedPlayerLibrary(t, st)

	w, body := playerGet(t, srv, "/api/player/albums?sort=artist")
	if w.Code != http.StatusOK {
		t.Fatalf("albums: status %d body %s", w.Code, w.Body.String())
	}
	albums, _ := body["albums"].([]any)
	if len(albums) != 3 {
		t.Fatalf("got %d albums, want 3: %s", len(albums), w.Body.String())
	}
	// "The Aardvarks" must file under A, so it leads the artist sort —
	// the article strip reaching all the way through to the wire.
	first, _ := albums[0].(map[string]any)
	if first["albumArtist"] != "The Aardvarks" {
		t.Errorf("artist sort leads with %v, want The Aardvarks (article-stripped to A)",
			first["albumArtist"])
	}

	_, body = playerGet(t, srv, "/api/player/albums?quality=hiresPCM")
	albums, _ = body["albums"].([]any)
	if len(albums) != 1 {
		t.Fatalf("hiresPCM filter got %d albums, want 1", len(albums))
	}

	// The any-DSD value is the only way the rate-less DSD rows are
	// selectable — they classify into none of iOS's three DSD tiers.
	_, body = playerGet(t, srv, "/api/player/albums?quality=dsd")
	albums, _ = body["albums"].([]any)
	if len(albums) != 1 {
		t.Fatalf("dsd filter got %d albums, want 1", len(albums))
	}

	w, _ = playerGet(t, srv, "/api/player/albums?quality=nonsense")
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown quality: status %d, want 400", w.Code)
	}
}

// TestPlayerAlbumsPagingIsStable — an offset past the end is an empty
// page, not an error: a client racing a rebuild that shrank the library
// should see "no more", not a fault.
func TestPlayerAlbumsPagingIsStable(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	_ = cfg
	seedPlayerLibrary(t, st)

	_, body := playerGet(t, srv, "/api/player/albums?limit=2&offset=0")
	page1, _ := body["albums"].([]any)
	_, body = playerGet(t, srv, "/api/player/albums?limit=2&offset=2")
	page2, _ := body["albums"].([]any)
	if len(page1) != 2 || len(page2) != 1 {
		t.Fatalf("pages: %d + %d, want 2 + 1", len(page1), len(page2))
	}
	seen := map[string]bool{}
	for _, a := range append(page1, page2...) {
		m := a.(map[string]any)
		id := m["id"].(string)
		if seen[id] {
			t.Errorf("album %s appeared on both pages — paging is not stable", id)
		}
		seen[id] = true
	}

	w, body := playerGet(t, srv, "/api/player/albums?offset=999")
	if w.Code != http.StatusOK {
		t.Errorf("offset past the end: status %d, want 200", w.Code)
	}
	if albums, _ := body["albums"].([]any); len(albums) != 0 {
		t.Errorf("offset past the end returned %d albums, want 0", len(albums))
	}
}

func TestPlayerAlbumDetail(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	_ = cfg
	seedPlayerLibrary(t, st)

	_, body := playerGet(t, srv, "/api/player/albums?sort=artist")
	albums, _ := body["albums"].([]any)
	id := albums[0].(map[string]any)["id"].(string)

	w, detail := playerGet(t, srv, "/api/player/albums/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("detail: status %d body %s", w.Code, w.Body.String())
	}
	tracks, _ := detail["tracks"].([]any)
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}
	tr := tracks[0].(map[string]any)
	play, _ := tr["play"].(map[string]any)
	if play["kind"] != playUniversal {
		t.Errorf("FLAC playability kind = %v, want %q", play["kind"], playUniversal)
	}
	if play["contentType"] != "audio/flac" {
		t.Errorf("FLAC contentType = %v, want audio/flac — audio/x-flac is the DLNA "+
			"spelling and browsers do not claim it", play["contentType"])
	}

	w, _ = playerGet(t, srv, "/api/player/albums/deadbeefdeadbeef")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown album: status %d, want 404", w.Code)
	}
	w, _ = playerGet(t, srv, "/api/player/albums/not-a-valid-id")
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed album id: status %d, want 400", w.Code)
	}
}

func TestPlayerDSDReportsUnplayable(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	_ = cfg
	seedPlayerLibrary(t, st)
	_, body := playerGet(t, srv, "/api/player/albums?quality=dsd")
	albums, _ := body["albums"].([]any)
	id := albums[0].(map[string]any)["id"].(string)
	_, detail := playerGet(t, srv, "/api/player/albums/"+id)
	tracks, _ := detail["tracks"].([]any)
	play := tracks[0].(map[string]any)["play"].(map[string]any)
	if play["kind"] != playNone {
		t.Errorf("DSD kind = %v, want %q — no browser decodes a 1-bit stream",
			play["kind"], playNone)
	}
	if play["downloadable"] != true {
		t.Error("an unplayable track must still be downloadable; that IS the affordance")
	}
	if play["contentType"] != "application/octet-stream" {
		t.Errorf("DSD contentType = %v — announcing audio/x-dsf would only invite an "+
			"engine to try", play["contentType"])
	}
}

func TestPlayerArtistsAndAxes(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	_ = cfg
	seedPlayerLibrary(t, st)

	w, body := playerGet(t, srv, "/api/player/artists")
	if w.Code != http.StatusOK {
		t.Fatalf("artists: status %d", w.Code)
	}
	artists, _ := body["artists"].([]any)
	if len(artists) != 3 {
		t.Fatalf("got %d artists, want 3", len(artists))
	}
	id := artists[0].(map[string]any)["id"].(string)
	w, detail := playerGet(t, srv, "/api/player/artists/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("artist detail: status %d", w.Code)
	}
	if albums, _ := detail["albums"].([]any); len(albums) == 0 {
		t.Error("artist detail returned no albums")
	}

	for _, path := range []string{"/api/player/genres", "/api/player/composers"} {
		w, _ := playerGet(t, srv, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", path, w.Code)
		}
	}
}

// TestPlayerAudioRejectsTraversal — the guard is ResolveChecked, not
// the path normaliser that runs before it, but neither may let one
// through.
func TestPlayerAudioRejectsTraversal(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, bad := range []string{
		"../../etc/passwd", "/etc/passwd", "a/../../b", "..\\..\\windows",
		"", "a/../..",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/api/player/audio?path="+strings.ReplaceAll(bad, " ", "%20"), nil)
		req.RemoteAddr = "127.0.0.1:1"
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("path %q was served — traversal guard failed", bad)
		}
	}
}

// TestPlayerAudioServesRangeAndBrowserMIME is the end-to-end pin for
// the byte route: a real file, a real Range request, the browser MIME.
func TestPlayerAudioServesRangeAndBrowserMIME(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	dir := cfg.LibraryRoots[0]
	rel := "Rock/Alpha/01.flac"
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("A", 4096)
	if err := os.WriteFile(abs, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(t.Context(), &manifest.Track{
		Path: rel, Title: "One", Album: "Alpha", AlbumArtist: "A", Codec: "FLAC",
		Size: int64(len(payload)), ModTime: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/player/audio?path="+rel, nil)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("full GET: status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/flac" {
		t.Errorf("Content-Type = %q, want audio/flac", ct)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/player/audio?path="+rel, nil)
	req.Header.Set("Range", "bytes=100-199")
	req.RemoteAddr = "127.0.0.1:1"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET: status %d, want 206 — without it the browser cannot scrub", w.Code)
	}
	if got := w.Body.Len(); got != 100 {
		t.Errorf("ranged body = %d bytes, want 100", got)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 100-199/4096" {
		t.Errorf("Content-Range = %q", cr)
	}

	// <audio> probes with HEAD before it streams. Go's ServeMux matches
	// HEAD against the GET pattern, so this needs no separate route —
	// but it does need the handler to accept the method, which is what
	// this asserts.
	req = httptest.NewRequest(http.MethodHead, "/api/player/audio?path="+rel, nil)
	req.RemoteAddr = "127.0.0.1:1"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("HEAD: status %d, want 200", w.Code)
	}
}

func TestPlayerDownloadSetsAttachment(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	st := srv.deps.Manifest
	dir := cfg.LibraryRoots[0]
	rel := "Rock/Ünïcödé Álbum/01 — Trëck.flac"
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(t.Context(), &manifest.Track{
		Path: rel, Title: "T", Size: 4, ModTime: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/player/download?path="+
		strings.ReplaceAll(rel, " ", "%20"), nil)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download: status %d body %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition = %q — RFC 5987 filename* is not optional in a "+
			"library full of non-ASCII names", cd)
	}
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition = %q — the ASCII fallback must stay a well-formed "+
			"quoted-string", cd)
	}
}
