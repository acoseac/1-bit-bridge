package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const (
	metaUUIDRelease = "aaaaaaaa-1111-2222-3333-444444444444"
	metaUUIDArtist  = "bbbbbbbb-1111-2222-3333-444444444444"
	metaUUIDOther   = "cccccccc-1111-2222-3333-444444444444"
	metaLocalSha    = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func seedMetaLibrary(t *testing.T, srv *Server) {
	t.Helper()
	seed := func(path, artwork, artist, release string) {
		t.Helper()
		if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
			Path: path, Size: 100,
			ArtworkMBID: artwork, ArtistMBID: artist, MusicBrainzAlbumID: release,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", path, err)
		}
	}
	// AlbumFolder: one release → kind "album", with a booklet.
	seed("AlbumFolder/01.flac", metaUUIDRelease, metaUUIDArtist, metaUUIDRelease)
	seed("AlbumFolder/02.flac", metaUUIDRelease, metaUUIDArtist, metaUUIDRelease)
	// ArtistFolder: one artist, two releases → kind "artist".
	seed("ArtistFolder/A1/01.flac", metaUUIDRelease, metaUUIDArtist, metaUUIDRelease)
	seed("ArtistFolder/A2/01.flac", metaUUIDOther, metaUUIDArtist, metaUUIDOther)
	// LocalFolder: curated local cover only.
	seed("LocalFolder/01.flac", metaLocalSha, "", "")
	if err := srv.deps.Manifest.UpsertBookletAvailability(
		context.Background(), metaUUIDRelease, true, "etag", 100); err != nil {
		t.Fatal(err)
	}
}

func enableAtlas(t *testing.T, srv *Server) {
	t.Helper()
	next := config.Clone(srv.deps.CfgHolder.Load())
	next.Atlas.Enabled = true
	srv.deps.CfgHolder.Store(next)
}

// TestApiLibraryEnrichmentRefs pins the children grouping: kind
// heuristic, representative cover (UUID preferred over local-),
// booklet flag, loose tracks excluded, and the 60s cache (a second
// request must serve the cached payload even after the store mutates).
func TestApiLibraryEnrichmentRefs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedMetaLibrary(t, srv)
	enableAtlas(t, srv)
	srv.deps.BookletPath = func(mbid string) string { return filepath.Join(t.TempDir(), mbid+".pdf") }

	var resp libraryMetaRefsResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/enrichment?path=", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("refs: %d", code)
	}
	if !resp.AtlasEnabled || !resp.BookletsEnabled {
		t.Errorf("flags = atlas %v booklets %v, want true/true", resp.AtlasEnabled, resp.BookletsEnabled)
	}
	album := resp.Children["AlbumFolder"]
	if album.Kind != "album" || album.ArtworkMBID != metaUUIDRelease || !album.HasBooklet {
		t.Errorf("AlbumFolder = %+v, want kind=album artwork=%s hasBooklet", album, metaUUIDRelease)
	}
	artist := resp.Children["ArtistFolder"]
	if artist.Kind != "artist" || artist.ArtistMBID != metaUUIDArtist {
		t.Errorf("ArtistFolder = %+v, want kind=artist artist=%s", artist, metaUUIDArtist)
	}
	localRef := resp.Children["LocalFolder"]
	if localRef.ArtworkMBID != metaLocalSha {
		t.Errorf("LocalFolder cover = %q, want the local- sentinel", localRef.ArtworkMBID)
	}

	// Cache: mutate the store, re-request — the cached payload (60s
	// TTL) must come back unchanged.
	if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
		Path: "NewFolder/01.flac", Size: 1, ArtworkMBID: metaUUIDOther,
	}); err != nil {
		t.Fatal(err)
	}
	var again libraryMetaRefsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/enrichment?path=", nil, &again); code != http.StatusOK {
		t.Fatalf("refs(2): %d", code)
	}
	if _, present := again.Children["NewFolder"]; present {
		t.Errorf("second request saw fresh data — the TTL cache regressed")
	}
}

// TestApiLibraryEnrichmentRefs_AtlasOff pins the atlas-off short-
// circuit shape.
func TestApiLibraryEnrichmentRefs_AtlasOff(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedMetaLibrary(t, srv)

	var resp libraryMetaRefsResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/enrichment?path=", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("refs: %d", code)
	}
	if resp.AtlasEnabled {
		t.Errorf("atlasEnabled = true, want false")
	}
	// Artwork refs are data-driven regardless of Atlas — tiles still
	// get covers on non-Atlas bridges.
	if resp.Children["AlbumFolder"].ArtworkMBID != metaUUIDRelease {
		t.Errorf("atlas-off response lost the artwork refs")
	}
}

// TestApiLibraryEnrichmentDetail_States pins the per-facet states:
// found (with mandatory attribution), tombstone → missing,
// empty-text-found → missing, never-checked → unchecked.
func TestApiLibraryEnrichmentDetail_States(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedMetaLibrary(t, srv)
	enableAtlas(t, srv)
	srv.deps.BookletPath = func(mbid string) string { return filepath.Join(t.TempDir(), mbid+".pdf") }
	ctx := context.Background()

	if err := srv.deps.Manifest.UpsertArtistAtlasMeta(ctx, manifest.ArtistAtlasMeta{
		ArtistMBID: metaUUIDArtist, Found: true,
		Bio: "Full bio text.", BioSummary: "Short bio.",
		Source: "wiki", SourceURL: "https://en.wikipedia.org/wiki/X",
	}); err != nil {
		t.Fatal(err)
	}
	// Release: tombstone (checked, nothing there) → "missing".
	if err := srv.deps.Manifest.UpsertReleaseAtlasMeta(ctx, manifest.ReleaseAtlasMeta{
		ReleaseMBID: metaUUIDRelease, Found: false,
	}); err != nil {
		t.Fatal(err)
	}

	var resp libraryMetaDetailResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/enrichment/detail?path=AlbumFolder", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	if resp.Artist == nil || resp.Artist.State != "found" ||
		resp.Artist.BioSummary != "Short bio." || resp.Artist.Source != "wiki" ||
		resp.Artist.SourceURL == "" {
		t.Errorf("artist = %+v, want found with attribution", resp.Artist)
	}
	if resp.Release == nil || resp.Release.State != "missing" {
		t.Errorf("release = %+v, want tombstone → missing", resp.Release)
	}
	if len(resp.Booklets) != 1 || resp.Booklets[0].State != "pending" {
		t.Errorf("booklets = %+v, want one pending row", resp.Booklets)
	}

	// Never-checked artist → unchecked.
	var other libraryMetaDetailResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/enrichment/detail?path=ArtistFolder/A2", nil, &other); code != http.StatusOK {
		t.Fatalf("detail(2): %d", code)
	}
	if other.Artist == nil || other.Artist.State != "found" {
		// Same dominant artist as AlbumFolder — found there too.
		t.Errorf("A2 artist = %+v, want found (same dominant artist)", other.Artist)
	}
	if other.Release == nil || other.Release.State != "unchecked" {
		t.Errorf("A2 release = %+v, want unchecked (never ingested)", other.Release)
	}
}

// TestApiLibraryEnrichmentDetail_Caches pins the 60s TTL + single-
// flight on the detail walk — the twin of TestApiLibraryEnrichmentRefs'
// cache block. The detail walk is the same json_extract cost class as
// refs (full-table at the root), so it must never run uncached: a
// second expand inside the window serves the cached payload.
func TestApiLibraryEnrichmentDetail_Caches(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedMetaLibrary(t, srv)
	enableAtlas(t, srv)
	ctx := context.Background()

	// A2's dominant release (metaUUIDOther) has never been ingested.
	var first libraryMetaDetailResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment/detail?path=ArtistFolder/A2", nil, &first); code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	if first.Release == nil || first.Release.State != "unchecked" {
		t.Fatalf("release = %+v, want unchecked (never ingested)", first.Release)
	}

	// Mutate the overlay, re-request — the cached payload must come back
	// unchanged.
	if err := srv.deps.Manifest.UpsertReleaseAtlasMeta(ctx, manifest.ReleaseAtlasMeta{
		ReleaseMBID: metaUUIDOther, Found: true, Description: "Fresh description.",
	}); err != nil {
		t.Fatal(err)
	}
	var again libraryMetaDetailResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment/detail?path=ArtistFolder/A2", nil, &again); code != http.StatusOK {
		t.Fatalf("detail(2): %d", code)
	}
	if again.Release == nil || again.Release.State != "unchecked" {
		t.Errorf("second request saw fresh data (%+v) — the detail walk is uncached", again.Release)
	}
}

// TestApiLibraryEnrichmentRetryScoped pins the retry facets + the
// per-path guard: same path 429s inside the window, a different path
// passes.
func TestApiLibraryEnrichmentRetryScoped(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableAtlas(t, srv)
	ctx := context.Background()
	// Two enriched-but-incomplete tracks under Gap/, one complete.
	seed := func(path, artwork, artist string) {
		t.Helper()
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
			Path: path, Size: 1, ArtworkMBID: artwork, ArtistMBID: artist,
		}); err != nil {
			t.Fatal(err)
		}
		if err := srv.deps.Manifest.MarkEnriched(ctx, &manifest.Track{
			Path: path, Size: 1, ArtworkMBID: artwork, ArtistMBID: artist,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("Gap/01.flac", "", "")
	seed("Gap/02.flac", "", "")
	seed("Full/01.flac", metaUUIDRelease, metaUUIDArtist)

	var resp libraryMetaRetryResponse
	code := doJSON(t, srv.Handler(), "POST", "/api/library/enrichment/retry",
		map[string]any{"path": "Gap"}, &resp)
	if code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}
	if resp.ResetTracks != 2 {
		t.Errorf("resetTracks = %d, want 2", resp.ResetTracks)
	}

	// Same path inside the window → 429.
	code = doJSON(t, srv.Handler(), "POST", "/api/library/enrichment/retry",
		map[string]any{"path": "Gap"}, nil)
	if code != http.StatusTooManyRequests {
		t.Errorf("same-path repeat = %d, want 429", code)
	}
	// Different path → allowed (per-path guard, not global).
	code = doJSON(t, srv.Handler(), "POST", "/api/library/enrichment/retry",
		map[string]any{"path": "Full"}, &resp)
	if code != http.StatusOK {
		t.Errorf("different-path retry = %d, want 200 (per-path guard)", code)
	}
}

// TestApiLibraryEnrichmentRetryScoped_InvalidatesMetaCaches pins the
// both-cache sweep. The About card deliberately doesn't auto-refetch
// after a retry — it refreshes on next open — which a stale detail
// entry would defeat. Retries on an ANCESTOR path must sweep the
// descendant's entries too.
func TestApiLibraryEnrichmentRetryScoped_InvalidatesMetaCaches(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedMetaLibrary(t, srv)
	enableAtlas(t, srv)
	ctx := context.Background()

	// Prime both caches for a descendant path.
	var refs libraryMetaRefsResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment?path=ArtistFolder", nil, &refs); code != http.StatusOK {
		t.Fatalf("refs: %d", code)
	}
	var detail libraryMetaDetailResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment/detail?path=ArtistFolder/A2", nil, &detail); code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	if detail.Release == nil || detail.Release.State != "unchecked" {
		t.Fatalf("release = %+v, want unchecked", detail.Release)
	}

	// Mutate what each cached path actually covers, then retry at the
	// ROOT — an ancestor of both. The refs mutation must land UNDER
	// ArtistFolder: a new top-level folder wouldn't show up in
	// ArtistFolder's refs whether or not the cache was swept, so it
	// would pin nothing.
	if err := srv.deps.Manifest.UpsertReleaseAtlasMeta(ctx, manifest.ReleaseAtlasMeta{
		ReleaseMBID: metaUUIDOther, Found: true, Description: "Fresh description.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
		Path: "ArtistFolder/A3/01.flac", Size: 1, ArtworkMBID: metaUUIDOther,
	}); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/enrichment/retry",
		map[string]any{"path": ""}, nil); code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}

	var refs2 libraryMetaRefsResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment?path=ArtistFolder", nil, &refs2); code != http.StatusOK {
		t.Fatalf("refs(2): %d", code)
	}
	if _, ok := refs2.Children["A3"]; !ok {
		t.Errorf("refs after retry = %v, want the new A3 child — the refs cache wasn't swept",
			refs2.Children)
	}
	var detail2 libraryMetaDetailResponse
	if code := doJSON(t, srv.Handler(), "GET",
		"/api/library/enrichment/detail?path=ArtistFolder/A2", nil, &detail2); code != http.StatusOK {
		t.Fatalf("detail(2): %d", code)
	}
	if detail2.Release == nil || detail2.Release.State != "found" {
		t.Errorf("detail after retry = %+v, want found — the detail cache wasn't swept", detail2.Release)
	}
}

// TestLibMetaCache_ErrorDoesNotCache pins serve's put-on-success-only
// contract: a failed walk must surface 500 and leave nothing behind,
// or the whole TTL would serve a degraded response.
func TestLibMetaCache_ErrorDoesNotCache(t *testing.T) {
	var c libMetaCache[string]
	calls := 0
	compute := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("walk exploded")
		}
		return "real", nil
	}
	serve := func() *httptest.ResponseRecorder {
		t.Helper()
		rw := httptest.NewRecorder()
		c.serve(rw, httptest.NewRequest("GET", "/x", nil), "p", "meta-test", compute)
		return rw
	}

	if rw := serve(); rw.Code != http.StatusInternalServerError {
		t.Fatalf("first (failing) request = %d, want 500", rw.Code)
	}
	rw := serve()
	if rw.Code != http.StatusOK {
		t.Fatalf("second request = %d, want 200 — the error was cached", rw.Code)
	}
	if calls != 2 {
		t.Errorf("compute calls = %d, want 2 — the error short-circuited the recompute", calls)
	}
}

// TestLibMetaCache_BoundsEntries pins the map bound AND that
// overflowing evicts rather than resetting. A `len <= max` assertion
// alone would pass for a clear()-the-whole-map policy, which drops 64
// fresh entries and makes a fast-browsing operator re-walk all of them
// — so assert the cache stays FULL, and that the most recent writes
// (the least expired) are the survivors.
func TestLibMetaCache_BoundsEntries(t *testing.T) {
	var c libMetaCache[string]
	const overflow = 10
	for i := range libMetaCacheMaxEntries + overflow {
		c.put(strconv.Itoa(i), "v")
	}
	if len(c.m) != libMetaCacheMaxEntries {
		t.Errorf("len = %d, want exactly %d — overflow reset the map instead of evicting",
			len(c.m), libMetaCacheMaxEntries)
	}
	// The last-written key must survive.
	newest := strconv.Itoa(libMetaCacheMaxEntries + overflow - 1)
	if _, ok := c.m[newest]; !ok {
		t.Errorf("newest key %q evicted", newest)
	}
	// Exactly `overflow` entries had to go, and they must come from the
	// oldest end.
	//
	// Asserted over the oldest BAND rather than pinning key "0"
	// specifically. Eviction orders by the entry's time.Now() stamp, and
	// this loop writes all 74 entries within a single clock tick on a
	// platform with coarse timer granularity — Windows especially. With
	// the stamps tied, which of the equally-old keys goes is genuinely
	// undefined, so pinning "0" was asserting map iteration order.
	//
	// This still fails everything it needs to: a clear()-the-map policy
	// trips the length check above, and a policy evicting from the NEWEST
	// end (or at random) leaves survivors in the oldest band. Production
	// puts are click-driven and seconds apart, so the tie is a property
	// of the test loop, not of the cache.
	survivingOldest := 0
	for i := range overflow {
		if _, ok := c.m[strconv.Itoa(i)]; ok {
			survivingOldest++
		}
	}
	if survivingOldest == overflow {
		t.Errorf("all %d oldest keys survived — eviction isn't expiry-ordered", overflow)
	}
}

// TestPathOverlaps pins the invalidation predicate, including the
// sibling-prefix false-match guard ("A/B" must not match "A/Bee").
func TestPathOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "", true},                      // root vs root
		{"", "Rock/Live", true},             // cached root entry vs a scoped retry
		{"Rock/Live", "", true},             // cached entry vs a root retry
		{"Rock", "Rock", true},              // identical
		{"Rock/Live", "Rock", true},         // descendant vs ancestor
		{"Rock", "Rock/Live", true},         // ancestor vs descendant
		{"Rock", "Jazz", false},             // siblings
		{"Rock/Live", "Rock/Studio", false}, // siblings, shared parent
		{"A/B", "A/Bee", false},             // sibling-prefix false-match guard
		{"A/Bee", "A/B", false},             // ...and the reverse
		{"Rock", "Rocks", false},            // top-level prefix false-match
	}
	for _, tc := range cases {
		if got := pathOverlaps(tc.a, tc.b); got != tc.want {
			t.Errorf("pathOverlaps(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestApiLibraryArtwork_ServesAndValidates pins the byte route: 200
// with the right caching mode per id shape, 400 on anything failing
// the bounded-alphabet regex, 404 with a nil closure.
func TestApiLibraryArtwork_ServesAndValidates(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	srv.deps.ArtworkPath = func(mbid string, size int) string {
		return filepath.Join(dir, mbid+"-500.jpg")
	}
	if err := os.WriteFile(filepath.Join(dir, metaUUIDRelease+"-500.jpg"), []byte("jpegbytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaLocalSha+"-500.jpg"), []byte("localbytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = "127.0.0.1:54321" // past the loopback boundary middleware
		rw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rw, req)
		return rw
	}

	rw := get("/api/library/artwork/" + metaUUIDRelease)
	if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("uuid cover: %d %q", rw.Code, rw.Header().Get("Content-Type"))
	}
	if cc := rw.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
		t.Errorf("bare-uuid Cache-Control = %q, want the 1-day mode (cover can change on premium refetch)", cc)
	}
	rw = get("/api/library/artwork/" + metaUUIDRelease + "?v=abc")
	if cc := rw.Header().Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Errorf("versioned Cache-Control = %q, want immutable", cc)
	}
	rw = get("/api/library/artwork/" + metaLocalSha)
	if cc := rw.Header().Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Errorf("local- Cache-Control = %q, want immutable (content-addressed)", cc)
	}
	rw = get("/api/library/artwork/..%2F..%2Fetc%2Fpasswd")
	if rw.Code != http.StatusBadRequest {
		t.Errorf("traversal-shaped id = %d, want 400", rw.Code)
	}
	rw = get("/api/library/artwork/not-a-uuid")
	if rw.Code != http.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", rw.Code)
	}

	srv.deps.ArtworkPath = nil
	rw = get("/api/library/artwork/" + metaUUIDRelease)
	if rw.Code != http.StatusNotFound {
		t.Errorf("nil closure = %d, want 404", rw.Code)
	}
}

// TestApiLibraryBooklet_PendingNudges pins the booklet route's twin
// semantics: available-but-unfetched → 202 + Retry-After + a nudge;
// cached file → 200 PDF.
func TestApiLibraryBooklet_PendingNudges(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	srv.deps.BookletPath = func(mbid string) string { return filepath.Join(dir, mbid+".pdf") }
	var nudged []string
	srv.deps.BookletNudge = func(mbid string) { nudged = append(nudged, mbid) }
	if err := srv.deps.Manifest.UpsertBookletAvailability(
		context.Background(), metaUUIDRelease, true, "etag", 100); err != nil {
		t.Fatal(err)
	}

	get := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rw, req)
		return rw
	}

	rw := get("/api/library/booklet/" + metaUUIDRelease)
	if rw.Code != http.StatusAccepted || rw.Header().Get("Retry-After") == "" {
		t.Errorf("pending booklet: %d Retry-After=%q, want 202 + header", rw.Code, rw.Header().Get("Retry-After"))
	}
	if len(nudged) != 1 || nudged[0] != metaUUIDRelease {
		t.Errorf("nudges = %v, want one for the release", nudged)
	}

	if err := os.WriteFile(filepath.Join(dir, metaUUIDRelease+".pdf"), []byte("%PDF-1.4"), 0o600); err != nil {
		t.Fatal(err)
	}
	rw = get("/api/library/booklet/" + metaUUIDRelease)
	if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("cached booklet: %d %q, want 200 application/pdf", rw.Code, rw.Header().Get("Content-Type"))
	}

	// Unknown release → 404.
	rw = get("/api/library/booklet/" + metaUUIDOther)
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown booklet = %d, want 404", rw.Code)
	}
}
