package dlna

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingArtworkSource is the ArtworkSource double: it records every key
// it was asked for and answers 200 with a marker body, so the tests can
// tell "the wrapper refused" from "the wrapper called the source".
type recordingArtworkSource struct {
	keys []string
}

func (s *recordingArtworkSource) ServeArtwork(w http.ResponseWriter, r *http.Request, key string) {
	s.keys = append(s.keys, key)
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("jpeg:" + key))
	}
}

// mountedMux builds the server's REAL handler tree — mountHandlers is what
// Start runs before wrapping the mux in telemetry and binding the listener.
// SSDP multicast is what a test host usually lacks, and it is not what is
// under test here; the routing table is.
func mountedMux(t *testing.T, cfg ServerConfig) http.Handler {
	t.Helper()
	if cfg.UDN == "" {
		cfg.UDN = "uuid:test-artwork"
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":0"
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://static.example"
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.mux = http.NewServeMux()
	s.mountHandlers()
	return s.mux
}

func Test_ArtworkHandler_GETAndHEADReachTheSource(t *testing.T) {
	src := &recordingArtworkSource{}
	h := ArtworkHandler(src)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/dlna/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa?size=500", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("GET: status %d body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodHead, "/dlna/artwork/local-"+strings.Repeat("ab", 32), nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d body %q", rec.Code, rec.Body.String())
	}
	if len(src.keys) != 2 || src.keys[0] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || src.keys[1] != "local-"+strings.Repeat("ab", 32) {
		t.Fatalf("source saw keys %q", src.keys)
	}
}

// Read-only: everything but GET / HEAD is 405, and the source is never
// consulted for it — the refusal is the wrapper's, before any lookup.
func Test_ArtworkHandler_RefusesEveryOtherMethod(t *testing.T) {
	src := &recordingArtworkSource{}
	h := ArtworkHandler(src)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(m, "/dlna/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, rec.Code)
		}
	}
	if len(src.keys) != 0 {
		t.Fatalf("a refused method reached the source: %q", src.keys)
	}
}

// Opaque, non-enumerable: no key, a nested path, or a foreign prefix is a
// plain 404 from the wrapper. The key grammar itself (UUID / local-hash /
// alias → 400 otherwise) is the source's and is pinned in internal/api.
func Test_ArtworkHandler_KeyIsOneSegmentOrNothing(t *testing.T) {
	src := &recordingArtworkSource{}
	h := ArtworkHandler(src)
	for _, p := range []string{"/dlna/artwork/", "/dlna/artwork/abc/def", "/dlna/artwork/abc/", "/dlna/file/abc"} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
	if len(src.keys) != 0 {
		t.Fatalf("a malformed path reached the source: %q", src.keys)
	}
	if got, ok := artworkKeyFromPath("/dlna/artwork/abc-def"); !ok || got != "abc-def" {
		t.Fatalf("artworkKeyFromPath = %q, %v", got, ok)
	}
}

func Test_ArtworkURLFor(t *testing.T) {
	if got := ArtworkURLFor("http://192.168.0.14:7790", "abc"); got != "http://192.168.0.14:7790/dlna/artwork/abc" {
		t.Fatalf("ArtworkURLFor = %q", got)
	}
	if got := ArtworkURLFor("http://192.168.0.14:7790", ""); got != "" {
		t.Fatalf("an empty key composes no URL, got %q", got)
	}
}

// The route exists exactly when the CDS advertises it. With no source the
// mux has no `/dlna/artwork/` and the DIDL carries no albumArtURI even for
// a track that has a key — a URI a strict renderer would 404 and decline
// the whole item over. With a source, both halves appear, composed against
// the REQUEST's host (the same per-interface rule the <res> URL follows).
func Test_Server_ArtworkRouteAndAlbumArtURIAreGatedTogether(t *testing.T) {
	track := testTrack("t1", "Cover Me")
	track.RelativePath = "Artist/Album/Cover Me.dsf"
	track.ArtworkKey = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	bare := testTrack("t2", "No Cover")
	bare.RelativePath = "Artist/Album/No Cover.dsf"
	lib := newTestLib(track, bare)

	browse := func(t *testing.T, mux http.Handler, objectID, flag string) string {
		t.Helper()
		req := buildBrowseRequest(t, objectID, flag, 0, 100)
		req.Host = "10.0.0.7:7790"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Browse %s %s: status %d: %s", objectID, flag, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	albumFolder := FolderObjectID("Artist/Album")
	artistFolder := FolderObjectID("Artist")
	const wantURI = "http://10.0.0.7:7790/dlna/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	t.Run("no source: no route, no URI", func(t *testing.T) {
		mux := mountedMux(t, ServerConfig{Library: lib})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dlna/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("route without a source: status %d, want 404", rec.Code)
		}
		for _, c := range []struct{ id, flag string }{
			{allTracksObjectID, "BrowseDirectChildren"},
			{albumFolder, "BrowseDirectChildren"},
			{albumFolder, "BrowseMetadata"},
			{artistFolder, "BrowseDirectChildren"},
		} {
			if body := browse(t, mux, c.id, c.flag); strings.Contains(body, "albumArtURI") {
				t.Fatalf("Browse %s %s emitted an albumArtURI on a server without the route:\n%s", c.id, c.flag, body)
			}
		}
	})

	t.Run("with source: route serves, items and album folder carry the URI", func(t *testing.T) {
		src := &recordingArtworkSource{}
		mux := mountedMux(t, ServerConfig{Library: lib, Artwork: src})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dlna/artwork/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "jpeg:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Fatalf("route: status %d body %q", rec.Code, rec.Body.String())
		}
		// The DIDL is XML-escaped inside the SOAP envelope, so the
		// element reads as &lt;upnp:albumArtURI&gt;…
		escaped := func(uri string) string { return "&lt;upnp:albumArtURI&gt;" + uri + "&lt;/upnp:albumArtURI&gt;" }

		// Item, from the flat All Tracks list: the keyed track carries the
		// URI, the keyless one does not.
		body := browse(t, mux, allTracksObjectID, "BrowseDirectChildren")
		if strings.Count(body, "albumArtURI") != 2 || !strings.Contains(body, escaped(wantURI)) {
			t.Fatalf("All Tracks: want exactly one albumArtURI element (%s), got:\n%s", wantURI, body)
		}
		// Item, from the folder axis and from its own BrowseMetadata.
		if body := browse(t, mux, albumFolder, "BrowseDirectChildren"); !strings.Contains(body, escaped(wantURI)) {
			t.Fatalf("album folder children: no albumArtURI on the item:\n%s", body)
		}
		if body := browse(t, mux, "t1", "BrowseMetadata"); !strings.Contains(body, escaped(wantURI)) {
			t.Fatalf("track BrowseMetadata: no albumArtURI:\n%s", body)
		}
		// The album FOLDER container — as a child of the artist folder, and
		// as its own BrowseMetadata — advertises its tracks' cover…
		if body := browse(t, mux, artistFolder, "BrowseDirectChildren"); !strings.Contains(body, escaped(wantURI)) {
			t.Fatalf("album container (as artist child): no albumArtURI:\n%s", body)
		}
		if body := browse(t, mux, albumFolder, "BrowseMetadata"); !strings.Contains(body, escaped(wantURI)) {
			t.Fatalf("album container (BrowseMetadata): no albumArtURI:\n%s", body)
		}
		// …while the artist folder, whose direct children are folders,
		// advertises none: there is no single cover to name.
		if body := browse(t, mux, foldersRootObjectID, "BrowseDirectChildren"); strings.Contains(body, "albumArtURI") {
			t.Fatalf("artist container carried an albumArtURI:\n%s", body)
		}
		if body := browse(t, mux, artistFolder, "BrowseMetadata"); strings.Contains(body, "albumArtURI") {
			t.Fatalf("artist container BrowseMetadata carried an albumArtURI:\n%s", body)
		}
	})
}

// The URI follows the request's Host like the <res> file URL does, so a
// renderer on a second interface gets a cover URL on the address it dialed.
func Test_CDS_AlbumArtURIFollowsRequestHost(t *testing.T) {
	track := testTrack("t1", "Cover Me")
	track.ArtworkKey = "local-" + strings.Repeat("ab", 32)
	mux := mountedMux(t, ServerConfig{Library: newTestLib(track), Artwork: &recordingArtworkSource{}})
	for _, host := range []string{"192.168.1.5:7790", "100.64.0.9:7790"} {
		req := buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", 0, 10)
		req.Host = host
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		want := "http://" + host + "/dlna/artwork/local-" + strings.Repeat("ab", 32)
		if !strings.Contains(string(body), want) {
			t.Fatalf("host %s: albumArtURI not composed against the request host (want %s):\n%s", host, want, body)
		}
	}
}

// FolderIndex.artworkKeyFor picks the first keyed DIRECT child in the
// node's sorted order and never descends.
func Test_FolderIndex_artworkKeyFor(t *testing.T) {
	a := testTrack("a", "01 First")
	a.RelativePath = "Artist/Album/01 First.dsf"
	b := testTrack("b", "02 Second")
	b.RelativePath = "Artist/Album/02 Second.dsf"
	b.ArtworkKey = "keyB"
	c := testTrack("c", "03 Third")
	c.RelativePath = "Artist/Album/03 Third.dsf"
	c.ArtworkKey = "keyC"
	idx := BuildFolderIndex([]TrackInfo{c, b, a})
	album := idx.Folders[FolderObjectID("Artist/Album")]
	if got := idx.artworkKeyFor(album); got != "keyB" {
		t.Fatalf("first keyed child in path order is keyB, got %q", got)
	}
	artist := idx.Folders[FolderObjectID("Artist")]
	if got := idx.artworkKeyFor(artist); got != "" {
		t.Fatalf("a folder with no direct tracks names no cover, got %q", got)
	}
	if got := idx.artworkKeyFor(FolderNode{}); got != "" {
		t.Fatalf("an empty node names no cover, got %q", got)
	}
}

// The listener this route lives on never starts in public deployment mode,
// so the demo bridge (public, read-only) and every public bridge have no
// such route — the same reason `/dlna/file/` needs no demo-mode branch.
func Test_ArtworkRoute_IsBehindThePublicModeRefusal(t *testing.T) {
	ok, reason := ShouldEnableDLNA(DLNAConfig{Enabled: true}, DeploymentPublic)
	if ok || !strings.Contains(reason, "public") {
		t.Fatalf("public mode must refuse the DLNA listener: ok=%v reason=%q", ok, reason)
	}
}
