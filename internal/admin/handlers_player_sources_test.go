package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The routing key a test upstream stamps into upnp_track_routing.
// Lowercase on purpose: that is what upnpingest.StableServerKey emits,
// and the mismatch between it and a device's raw UDN is the whole
// reason UPnPSource carries both.
const testUpstreamKey = "uuid:test-2go"

// seedHybridLibrary lays down a library with all three shapes the
// source facet has to tell apart: local-only, upstream-only, and one
// album holding a track from each.
func seedHybridLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	ctx := t.Context()
	mk := func(path, album, artist, genre string) *manifest.Track {
		return &manifest.Track{
			Path: path, Title: path, Album: album,
			AlbumArtist: artist, Artist: artist, Genre: genre,
			Codec: "FLAC", Size: 1000, ModTime: time.Unix(1, 0),
		}
	}
	local := []*manifest.Track{
		mk("Local/Home/01.flac", "Home", "Local Band", "Rock"),
		mk("Local/Home/02.flac", "Home", "Local Band", "Rock"),
		mk("Shared/Split/01.flac", "Split", "Both Band", "Jazz"),
		// Cross Band spans two SINGLE-source albums, which is what
		// makes a filtered count differ from the library one.
		mk("Local/CrossHere/01.flac", "CrossHere", "Cross Band", "Cross"),
		mk("Local/CrossHere/02.flac", "CrossHere", "Cross Band", "Cross"),
	}
	routed := []*manifest.Track{
		mk("2go/Remote/01.flac", "Remote", "Remote Band", "Jazz"),
		mk("2go/Remote/02.flac", "Remote", "Remote Band", "Jazz"),
		mk("2go/Remote/03.flac", "Remote", "Remote Band", "Jazz"),
		// Same album identity as the local Shared/Split track, so this
		// album belongs to BOTH sources.
		mk("2go/Split/02.flac", "Split", "Both Band", "Jazz"),
		mk("2go/CrossThere/01.flac", "CrossThere", "Cross Band", "Cross"),
		mk("2go/CrossThere/02.flac", "CrossThere", "Cross Band", "Cross"),
		mk("2go/CrossThere/03.flac", "CrossThere", "Cross Band", "Cross"),
	}
	// Composers are assigned per ALBUM, not per side, so the composer axis
	// narrows unambiguously. Tagging every local track would put "Home
	// Composer" on the SHARED album too — and that album belongs to both
	// sources, so the composer would legitimately survive an upstream
	// scope and the axis would not appear to filter at all. Home and
	// Remote are each single-source.
	for _, tr := range local {
		if tr.Album == "Home" {
			tr.Composer = "Home Composer"
		}
	}
	for _, tr := range routed {
		if tr.Album == "Remote" {
			tr.Composer = "Away Composer"
		}
	}
	for _, tr := range append(append([]*manifest.Track{}, local...), routed...) {
		if err := st.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range routed {
		if err := st.UpsertUPnPRouting(ctx, &manifest.UPnPRouting{
			SourcePath: tr.Path, ServerUDN: testUpstreamKey,
			ObjectID: "1", ResURL: "http://10.0.0.5:8200/x", LastSeenAt: time.Unix(2, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func withTestUpstream(srv *Server, online bool) {
	srv.deps.UPnPSources = func() []UPnPSource {
		return []UPnPSource{{Key: testUpstreamKey, Name: "Chord 2go", Online: online}}
	}
}

func upstreamSourceID() string { return librarycat.SourceID(testUpstreamKey) }

// TestPlayerSourcesReportsBothKindsWithLiveness pins the facet's whole
// wire contract on a hybrid library: one filesystem row, one upstream
// row, exact per-track totals, and album counts that credit a
// split album to both.
func TestPlayerSourcesReportsBothKindsWithLiveness(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	w, _ := playerGet(t, srv, "/api/player/sources")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got playerSourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("got %d sources, want 2: %+v", len(got.Sources), got.Sources)
	}

	local, upstream := got.Sources[0], got.Sources[1]
	if local.ID != librarycat.LocalSourceID || local.Kind != sourceKindFilesystem {
		t.Fatalf("first row = %+v, want the filesystem source first", local)
	}
	// Five local tracks across three albums, one of them shared.
	if local.TrackCount != 5 || local.AlbumCount != 3 {
		t.Errorf("local counts = %d tracks / %d albums, want 5/3", local.TrackCount, local.AlbumCount)
	}
	if local.Online != nil {
		t.Errorf("filesystem source reported liveness %v; it has no such state", *local.Online)
	}

	if upstream.ID != upstreamSourceID() || upstream.Kind != sourceKindUPnP {
		t.Fatalf("second row = %+v, want the upstream", upstream)
	}
	if upstream.Name != "Chord 2go" {
		t.Errorf("upstream name = %q, want the configured name", upstream.Name)
	}
	if upstream.TrackCount != 7 || upstream.AlbumCount != 3 {
		t.Errorf("upstream counts = %d tracks / %d albums, want 7/3",
			upstream.TrackCount, upstream.AlbumCount)
	}
	if upstream.Online == nil || !*upstream.Online {
		t.Errorf("upstream online = %v, want true", upstream.Online)
	}
	// Per-track attribution must sum to the library even though the
	// shared album is counted by both rows.
	if n := local.TrackCount + upstream.TrackCount; n != 12 {
		t.Errorf("track counts sum to %d, want the library's 12", n)
	}
}

// TestPlayerSourcesDistinguishesOfflineFromUnknown pins the three-state
// contract. Collapsing unknown into offline would paint every upstream
// red on a bridge whose discovery is simply not wired.
func TestPlayerSourcesDistinguishesOfflineFromUnknown(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		seedHybridLibrary(t, srv.deps.Manifest)
		withTestUpstream(srv, false)
		var got playerSourcesResponse
		w, _ := playerGet(t, srv, "/api/player/sources")
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		up := got.Sources[len(got.Sources)-1]
		if up.Online == nil || *up.Online {
			t.Fatalf("online = %v, want an explicit false", up.Online)
		}
		if !up.Configured {
			t.Error("a configured upstream that is down must still read as configured")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		seedHybridLibrary(t, srv.deps.Manifest)
		// No UPnPSources wiring: the bridge cannot ask.
		var got playerSourcesResponse
		w, _ := playerGet(t, srv, "/api/player/sources")
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		up := got.Sources[len(got.Sources)-1]
		if up.Online != nil {
			t.Fatalf("online = %v, want omitted when the answer is unknown", *up.Online)
		}
		if up.Configured {
			t.Error("an upstream with no config row must not read as configured")
		}
		if up.TrackCount != 7 {
			t.Errorf("orphan track count = %d, want 7 — its tracks are still in the library",
				up.TrackCount)
		}
	})
}

// TestPlayerSourceFilterNarrowsEveryBrowseAxis is the feature's point:
// picking a source scopes albums, artists and the genre axis to it.
func TestPlayerSourceFilterNarrowsEveryBrowseAxis(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	names := func(target, key, field string) []string {
		t.Helper()
		w, body := playerGet(t, srv, target)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body.String())
		}
		rows, _ := body[key].([]any)
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			m, _ := r.(map[string]any)
			s, _ := m[field].(string)
			out = append(out, s)
		}
		return out
	}
	has := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}

	up := "source=" + upstreamSourceID()

	albums := names("/api/player/albums?"+up, "albums", "title")
	if has(albums, "Home") {
		t.Errorf("upstream-scoped albums included the local-only %q: %v", "Home", albums)
	}
	if !has(albums, "Remote") || !has(albums, "Split") {
		t.Errorf("upstream-scoped albums = %v, want Remote and the shared Split", albums)
	}

	local := names("/api/player/albums?source="+librarycat.LocalSourceID, "albums", "title")
	if has(local, "Remote") {
		t.Errorf("filesystem-scoped albums included the upstream-only %q: %v", "Remote", local)
	}
	if !has(local, "Home") || !has(local, "Split") {
		t.Errorf("filesystem-scoped albums = %v, want Home and the shared Split", local)
	}

	artists := names("/api/player/artists?"+up, "artists", "name")
	if has(artists, "Local Band") {
		t.Errorf("upstream-scoped artists included a local-only artist: %v", artists)
	}
	if !has(artists, "Remote Band") {
		t.Errorf("upstream-scoped artists = %v, want Remote Band", artists)
	}

	genres := names("/api/player/genres?"+up, "entries", "name")
	if has(genres, "Rock") {
		t.Errorf("upstream-scoped genres included Rock, which only the local album carries: %v", genres)
	}
	if !has(genres, "Jazz") {
		t.Errorf("upstream-scoped genres = %v, want Jazz", genres)
	}
}

// TestPlayerSourceFilterRestatesGroupCounts is the half a membership
// filter alone would get wrong: an artist or genre that survives the
// filter must report what is left, not what the whole library holds.
//
// Cross Band is the discriminator — two albums, one per source — so a
// count that ignored the filter would read 5 where 3 is true.
func TestPlayerSourceFilterRestatesGroupCounts(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	find := func(target, key, field, want string) map[string]any {
		t.Helper()
		_, body := playerGet(t, srv, target)
		rows, _ := body[key].([]any)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if s, _ := m[field].(string); s == want {
				return m
			}
		}
		t.Fatalf("%q not found in %s: %v", want, target, rows)
		return nil
	}
	counts := func(m map[string]any) (albums, tracks int) {
		a, _ := m["albumCount"].(float64)
		tr, _ := m["trackCount"].(float64)
		return int(a), int(tr)
	}

	whole := find("/api/player/artists", "artists", "name", "Cross Band")
	if a, tr := counts(whole); a != 2 || tr != 5 {
		t.Fatalf("unfiltered Cross Band = %d albums / %d tracks, want 2/5", a, tr)
	}
	scoped := find("/api/player/artists?source="+upstreamSourceID(), "artists", "name", "Cross Band")
	if a, tr := counts(scoped); a != 1 || tr != 3 {
		t.Errorf("upstream-scoped Cross Band = %d albums / %d tracks, want 1/3 "+
			"(the whole-library numbers are 2/5)", a, tr)
	}

	genre := find("/api/player/genres?source="+librarycat.LocalSourceID, "entries", "name", "Cross")
	if a, tr := counts(genre); a != 1 || tr != 2 {
		t.Errorf("local-scoped Cross genre = %d albums / %d tracks, want 1/2", a, tr)
	}
}

// TestPlayerSourceFilterCountsWholeAlbumsOfMixedReleases pins the one
// place a scoped count is deliberately NOT a per-source track count.
//
// An album is shown when it has any track from the source, and the
// album's own page is not source-filtered — so the reader clicking
// through sees every one of its tracks. The count therefore describes
// the albums on screen rather than the tracks that made them match,
// and reporting the narrower number would contradict what the next
// click reveals.
func TestPlayerSourceFilterCountsWholeAlbumsOfMixedReleases(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	// Both Band owns only the Split album: one local track, one routed.
	// Either scope keeps the album, so either scope counts both tracks.
	for _, source := range []string{librarycat.LocalSourceID, upstreamSourceID()} {
		_, body := playerGet(t, srv, "/api/player/artists?source="+source)
		rows, _ := body["artists"].([]any)
		var got float64
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if n, _ := m["name"].(string); n == "Both Band" {
				got, _ = m["trackCount"].(float64)
			}
		}
		if got != 2 {
			t.Errorf("source=%s: Both Band trackCount = %v, want the album's full 2",
				source, got)
		}
	}
}

// TestPlayerSourceFilterRejectsAndEmpties pins the two failure shapes
// apart: a malformed token is the caller's error, while a well-formed
// id the snapshot no longer knows is simply an empty view — an
// upstream removed between two page loads must not fault the page.
func TestPlayerSourceFilterRejectsAndEmpties(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)

	for _, target := range []string{
		"/api/player/albums?source=../etc",
		"/api/player/artists?source=NOTHEX",
		"/api/player/genres?source=abc",
	} {
		if w, _ := playerGet(t, srv, target); w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, w.Code)
		}
	}

	unknown := "source=" + librarycat.SourceID("uuid:not-here")
	for _, spec := range []struct{ target, key string }{
		{"/api/player/albums?" + unknown, "albums"},
		{"/api/player/artists?" + unknown, "artists"},
		{"/api/player/genres?" + unknown, "entries"},
	} {
		w, body := playerGet(t, srv, spec.target)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", spec.target, w.Code)
			continue
		}
		if rows, _ := body[spec.key].([]any); len(rows) != 0 {
			t.Errorf("GET %s returned %d rows, want an empty view", spec.target, len(rows))
		}
	}
}

// TestPlayerSourceFilterIntersectsWithAxisFilters pins that the source
// filter narrows an existing axis filter rather than replacing it —
// which is what makes following a genre link while scoped stay scoped.
func TestPlayerSourceFilterIntersectsWithAxisFilters(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	_, body := playerGet(t, srv, "/api/player/genres")
	var jazzID string
	rows, _ := body["entries"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if n, _ := m["name"].(string); n == "Jazz" {
			jazzID, _ = m["id"].(string)
		}
	}
	if jazzID == "" {
		t.Fatal("no Jazz genre in the seeded library")
	}

	// Jazz alone holds Split and Remote; Jazz on the filesystem holds
	// only Split. Replacing rather than intersecting would show both.
	w, body := playerGet(t, srv,
		"/api/player/albums?genre="+jazzID+"&source="+librarycat.LocalSourceID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	albums, _ := body["albums"].([]any)
	if len(albums) != 1 {
		t.Fatalf("got %d albums, want the 1 in both Jazz and the filesystem: %v", len(albums), albums)
	}
	m, _ := albums[0].(map[string]any)
	if title, _ := m["title"].(string); title != "Split" {
		t.Errorf("album = %q, want Split", title)
	}
}

// TestRoutedOnlineResolvesTheRoutingKeyNotTheDeviceUDN guards the
// mismatch that made the album badge wrong: Album.RoutedUDNs carries
// the ingest's stable routing key, while UPnPHostOnline is an exact
// lookup on the device's raw UDN. A bridge wired with UPnPSources must
// answer from that, or a manually-configured upstream — whose key is
// "manual:<hash>" and matches no UDN anywhere — reads as offline while
// it is serving.
func TestRoutedOnlineResolvesTheRoutingKeyNotTheDeviceUDN(t *testing.T) {
	srv, _, _ := newTestServer(t)
	const key = "manual:deadbeef"
	srv.deps.UPnPSources = func() []UPnPSource {
		return []UPnPSource{{Key: key, Name: "Manual", Online: true}}
	}
	// Wired to fail the way an exact raw-UDN lookup does for this key.
	srv.deps.UPnPHostOnline = func(string) bool { return false }

	got := srv.routedOnline(key)
	if got == nil || !*got {
		t.Fatalf("routedOnline(%q) = %v, want true", key, got)
	}
	if v := srv.routedOnline("uuid:never-configured"); v != nil {
		t.Errorf("an orphan key reported %v; unknown must stay nil, not offline", *v)
	}
}

// TestSidebarListsUpstreamsWithStatus pins the nav group: the operator's
// upstreams, each linking to the library scoped to it, each carrying
// whether it is reachable.
//
// It lists what was CONFIGURED, not what has been ingested — the opposite
// of the /api/player/sources rule, and deliberately: an upstream that has
// not been walked yet is exactly when its status is most worth seeing.
func TestSidebarListsUpstreamsWithStatus(t *testing.T) {
	srv, _, _ := newTestServer(t)

	if rows := srv.sidebarSources(httptest.NewRequest(http.MethodGet, "/albums", nil)); len(rows) != 0 {
		t.Fatalf("no upstreams configured, got %d rows", len(rows))
	}

	withTestUpstream(srv, false)
	rows := srv.sidebarSources(httptest.NewRequest(http.MethodGet, "/albums", nil))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.ID != upstreamSourceID() {
		t.Errorf("id = %q, want the facet id the source filter accepts", got.ID)
	}
	if got.Name != "Chord 2go" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Status != "offline" || got.Label != "Offline" {
		t.Errorf("status/label = %q/%q, want offline/Offline", got.Status, got.Label)
	}
	if got.Current {
		t.Error("row marked current on an unscoped URL")
	}

	// Status is a STRING and not the API's *bool because a Go template's
	// `if` on a pointer tests non-nil — a pointer to false reads as true,
	// and every offline upstream would render online.
	withTestUpstream(srv, true)
	if rows = srv.sidebarSources(httptest.NewRequest(http.MethodGet, "/albums", nil)); rows[0].Status != "online" {
		t.Errorf("status = %q, want online", rows[0].Status)
	}
}

// TestSidebarMarksTheScopedUpstream pins that the group follows the view,
// and — the half that would otherwise break an existing guard — that
// Browse stands down while it does.
//
// Every player route renders the player section, so without the stand-down
// both Browse and the source row carry aria-current and
// TestPrimaryNavHighlightsEveryEntry fails. It is right to fail: two "you
// are here" marks tell the reader nothing.
func TestSidebarMarksTheScopedUpstream(t *testing.T) {
	srv, _, _ := newTestServer(t)
	withTestUpstream(srv, true)

	scoped := httptest.NewRequest(http.MethodGet, "/albums?source="+upstreamSourceID(), nil)
	rows := srv.sidebarSources(scoped)
	if len(rows) != 1 || !rows[0].Current {
		t.Fatalf("scoped URL did not mark the row: %+v", rows)
	}

	w := httptest.NewRecorder()
	scoped.RemoteAddr = "127.0.0.1:54321"
	srv.Handler().ServeHTTP(w, scoped)
	body := w.Body.String()
	if n := strings.Count(body, `aria-current="page"`); n != 1 {
		t.Errorf("page carries %d aria-current marks, want exactly 1", n)
	}
	if !strings.Contains(body, `data-source-id="`+upstreamSourceID()+`"`) {
		t.Error("the sidebar did not render the upstream row")
	}
	// The word rides the accessible name, never an aria-label on a
	// descendant — that would replace the link's whole subtree in the name
	// computation and drop the server's name (the pairing-badge trap).
	if strings.Contains(body, `class="nav-source-status`) && !strings.Contains(body, `, Online</span>`) {
		t.Error("the status word is not in the row's accessible name")
	}
}

// TestSidebarSourceDotsRefreshFromTheSourcesEvent pins the id that lets
// the live update match.
//
// The sidebar is server-rendered, and a boosted navigation never reloads
// the shell — so without a refresh a dropped upstream keeps its green dot
// for the whole session. app.js repaints from the `sources` SSE event,
// matched on this id rather than on the operator-editable display name.
func TestSidebarSourceDotsRefreshFromTheSourcesEvent(t *testing.T) {
	js := readFile(t, filepath.Join("static", "app.js"))
	fn := extractJSFunction(t, js, "applySidebarSourceStatus")
	if !strings.Contains(fn, "srv.sourceId") {
		t.Error("the sidebar dot update no longer matches on sourceId; matching " +
			"on the display name stops working the moment an upstream is renamed")
	}
	if !strings.Contains(extractJSFunction(t, js, "applySources"), "applySidebarSourceStatus(") {
		t.Error("applySources no longer refreshes the sidebar dots")
	}
}
