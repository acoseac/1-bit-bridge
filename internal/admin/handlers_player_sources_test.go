package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
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

// TestSourcesFacetGateFollowsTheLibraryNotTheConfig pins both halves of
// the gate's one signal.
//
// A configured upstream with no tracks must NOT open the facet: the
// handler skips zero-track sources, so the page would show a lone "This
// bridge" and the rail entry would lead nowhere. And routed tracks with
// no config row MUST open it: removing the last upstream leaves the
// ingest with nothing to start, so its orphan sweep never runs and the
// facet is the only surface that explains where those tracks came from.
func TestSourcesFacetGateFollowsTheLibraryNotTheConfig(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	if srv.sourcesFacetWorthShowing() {
		t.Error("empty library: facet should stay hidden")
	}

	// A configured upstream is not enough on its own.
	cfg.UPnPUpstream.Enabled = true
	cfg.UPnPUpstream.Servers = []config.UPnPUpstreamServerConfig{
		{Name: "Chord 2go", UDN: testUpstreamKey},
	}
	withTestUpstream(srv, true)
	if srv.sourcesFacetWorthShowing() {
		t.Error("configured upstream with no ingested tracks: the facet page " +
			"would show one row, so the rail entry must stay hidden")
	}

	seedHybridLibrary(t, srv.deps.Manifest)
	srv.statsMu.Lock()
	srv.statsDBValid = false
	srv.statsMu.Unlock()
	if !srv.sourcesFacetWorthShowing() {
		t.Error("routed tracks present: facet must show")
	}

	// ...and it must stay shown once the config row is gone.
	cfg.UPnPUpstream.Servers = nil
	srv.deps.UPnPSources = nil
	if !srv.sourcesFacetWorthShowing() {
		t.Error("routed tracks with no config row: facet must still show, " +
			"or the orphans have no surface at all")
	}
}

// TestRenderSourcesClearsTheToolbar pins the one thing route() does NOT
// do for a view.
//
// Each view owns the toolbar. renderSources did not touch it, so arriving
// from the album grid left its sort / quality / variant selects on screen
// over the Sources page — and they still worked, writing ?sort= into the
// Sources URL. Reproduced in a browser before it was fixed; a source scan
// is what keeps it fixed, since nothing else connects the two views.
func TestRenderSourcesClearsTheToolbar(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "views.js")), "renderSources")
	if !strings.Contains(fn, "setToolbar(null)") {
		t.Error("renderSources does not clear the toolbar; the previous view's " +
			"controls will stay on screen and stay live")
	}
}

// TestSourceRailKeepsTheStatusColourOffTheRow pins the shape that a
// screenshot caught and no assertion about the DOM would have.
//
// The status classes set `color`, which the dot reads through
// currentcolor. Applied to the ROW they also recolour the source's NAME,
// so an offline upstream rendered as red text in the rail — which reads
// as an error rather than a status, and fights the rail's own "you are
// here" ink. They belong on a wrapper, exactly as on the Sources page.
func TestSourceRailKeepsTheStatusColourOffTheRow(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "boot.js")), "sourceNavRow")
	if strings.Contains(fn, "row.classList.add(cls)") {
		t.Error("sourceNavRow puts the status class on the row; it recolours the " +
			"source name as well as the dot")
	}
	if !strings.Contains(fn, "source-status ${cls}") {
		t.Error("sourceNavRow no longer wraps the dot in a .source-status span; " +
			"the dot has nothing to read its colour from")
	}
}

// TestSourceRailRefreshesAndFollowsNavigation pins the two calls that
// make the rail's dot mean anything.
//
// Without the refresh in route() the rail is painted once at mount and an
// upstream that drops mid-session keeps its green dot for as long as the
// tab stays open. Without markActiveSource there, following a source link
// leaves the highlight on whichever source was picked first.
func TestSourceRailRefreshesAndFollowsNavigation(t *testing.T) {
	src := readFile(t, filepath.Join("static", "player", "boot.js"))
	fn := extractJSFunction(t, src, "route")
	for _, call := range []string{"markActiveSource()", "refreshSourceNav("} {
		if !strings.Contains(fn, call) {
			t.Errorf("route() no longer calls %s; the source rail stops tracking "+
				"the current view", call)
		}
	}
	// And the refresh must be TTL-guarded, or every hop around the library
	// costs a request — the cost the banner's name lookup was memoised to
	// avoid, reintroduced one level up.
	if !strings.Contains(src, "SOURCE_NAV_TTL_MS") {
		t.Error("the source rail refresh is no longer TTL-guarded")
	}
}
