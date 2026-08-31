package admin

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
)

// An album can exist in BOTH places at once — the same release ripped
// locally and also present on an upstream. It is an ordinary shape on a
// hybrid bridge, and until this the album page gave no sign of it: one
// track count, one modal quality chip, and nothing to say that some rows
// live somewhere that can go offline.

// TestAlbumTracksCarryTheirSource pins the per-row provenance the mixed
// view is built on, in both directions.
//
// Absence means the filesystem — that is what keeps the field off every
// row of a pure-filesystem library, where the answer is never in question.
func TestAlbumTracksCarryTheirSource(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)
	albums := seededAlbumIDs(t, srv)

	// Split holds one local track and one routed one.
	if got := albumTrackSources(t, srv, albums, "Split"); len(got) != 2 {
		t.Errorf("the mixed album reports %v, want both sources", got)
	}
	// Home is local only: every row resolves to local, and none of them
	// carries the field on the wire.
	if got := albumTrackSources(t, srv, albums, "Home"); len(got) != 1 ||
		got[0] != librarycat.LocalSourceID {
		t.Errorf("local-only album reports %v, want just the filesystem", got)
	}
	// Remote is upstream only, and DOES name its upstream.
	if got := albumTrackSources(t, srv, albums, "Remote"); len(got) != 1 ||
		got[0] != upstreamSourceID() {
		t.Errorf("upstream-only album reports %v, want just the upstream", got)
	}
}

// seededAlbumIDs maps album title to catalog id for the fixture library.
func seededAlbumIDs(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	_, body := playerGet(t, srv, "/api/player/albums?limit=50")
	rows, _ := body["albums"].([]any)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		title, _ := m["title"].(string)
		id, _ := m["id"].(string)
		out[title] = id
	}
	return out
}

// albumTrackSources returns the distinct sources one album's tracks come
// from, in first-appearance order.
func albumTrackSources(t *testing.T, srv *Server, albums map[string]string, title string) []string {
	t.Helper()
	id, ok := albums[title]
	if !ok {
		t.Fatalf("album %q not in the seeded library: %v", title, albums)
	}
	w, d := playerGet(t, srv, "/api/player/albums/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("album detail %s = %d", title, w.Code)
	}
	tracks, _ := d["tracks"].([]any)
	if len(tracks) == 0 {
		t.Fatalf("album %q returned no tracks", title)
	}
	seen := map[string]bool{}
	var out []string
	for _, tr := range tracks {
		m, _ := tr.(map[string]any)
		// The wire omits the field for a filesystem track; the client
		// reads that absence as local, and so does this.
		src, _ := m["sourceId"].(string)
		if src == "" {
			src = librarycat.LocalSourceID
		}
		if !seen[src] {
			seen[src] = true
			out = append(out, src)
		}
	}
	return out
}

// TestMixedAlbumUIIsGatedOnActuallyBeingMixed pins that the split view
// appears only when there is a split.
//
// On a single-source album every row would carry the same chip, which is
// wallpaper rather than information — the same reasoning that keeps the
// variant marks off tracks with nothing to say.
func TestMixedAlbumUIIsGatedOnActuallyBeingMixed(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "views.js")), "albumTracksPanel")
	if !strings.Contains(fn, "ids.length < 2") {
		t.Error("albumTracksPanel no longer gates on the album spanning two " +
			"sources; a single-source album would grow a filter offering one choice")
	}
	// The combined view is the default and the filtered ones are opt-in:
	// an album is one release, and opening on a subset reads as tracks
	// missing rather than as a filter.
	if !strings.Contains(fn, `let current = ""`) {
		t.Error("the source filter no longer defaults to the combined view")
	}
}

var (
	subgridColsRe = regexp.MustCompile(
		`\.tracks\s*\{[^}]*grid-template-columns:\s*([^;}]+)`)
	repeatWholeRe = regexp.MustCompile(`repeat\(\s*(\d+)\s*,[^)]*\)`)
)

// TestTrackSubgridColumnsMatchTheRow is the half the sibling grid test
// cannot see.
//
// That one passes as soon as the BASE rule uses `grid-auto-flow: column`,
// because implicit columns cannot disagree with a cell count. The
// @supports block is the opposite: it declares an explicit template, and
// a constant cell count matched to it is precisely what makes the rows
// share columns. Add a cell to trackRow without widening that template
// and every column past the new one shifts by one — silently, on the
// layout most readers see.
func TestTrackSubgridColumnsMatchTheRow(t *testing.T) {
	views := readNormalized(t, "static/player/views.js")
	css := cssCommentRe.ReplaceAllString(readNormalized(t, "static/player.css"), "")

	start := strings.Index(views, "function trackRow(")
	if start < 0 {
		t.Fatal("trackRow not found — this test has stopped checking anything")
	}
	end := strings.Index(views[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of trackRow")
	}
	cells := len(trackAppendRe.FindAllString(views[start:start+end], -1))
	if cells < 6 {
		t.Fatalf("only %d appends scraped from trackRow — the regex has stopped "+
			"matching, which would make this test pass while checking nothing", cells)
	}

	sup := strings.Index(css, "@supports (grid-template-columns: subgrid)")
	if sup < 0 {
		t.Fatal("no subgrid @supports block in player.css")
	}
	m := subgridColsRe.FindStringSubmatch(css[sup:])
	if m == nil {
		t.Fatal("the subgrid block declares no grid-template-columns for .tracks")
	}
	decl := m[1]
	// The repeat() expression is REMOVED before counting the rest, not
	// skipped field by field: "repeat(7, auto)" splits on whitespace into
	// two fields, and skipping only the one that starts with "repeat("
	// counts the trailing "auto)" as a column of its own.
	cols := 0
	if r := repeatWholeRe.FindStringSubmatch(decl); r != nil {
		n, err := strconv.Atoi(r[1])
		if err != nil {
			t.Fatalf("unreadable repeat() count in %q", decl)
		}
		cols += n
		decl = repeatWholeRe.ReplaceAllString(decl, "")
	}
	cols += len(strings.Fields(decl))
	if cols != cells {
		t.Errorf("the subgrid declares %d columns but trackRow appends %d cells "+
			"(template was %q) — every column past the mismatch shifts by one",
			cols, cells, strings.TrimSpace(m[1]))
	}
}

// TestToolchainProbeIsAskedOncePerRequest pins that the sox probe is
// hoisted out of the per-track loop.
//
// deps.SoxCanDecode is the 30s-TTL toolchain cache behind a mutex and
// its answer is fixed for the life of a request, so asking it per track
// is a lock per track for one stable value. A playlist can carry tens of
// thousands of paths.
//
// A source scan cannot pin this — the call reads identically wherever it
// sits — so this counts real invocations through a real request.
func TestToolchainProbeIsAskedOncePerRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	var calls int
	srv.deps.SoxCanDecode = func() func(string) bool {
		calls++
		return func(string) bool { return true }
	}

	albums := seededAlbumIDs(t, srv)
	id, ok := albums["Split"]
	if !ok {
		t.Fatal("fixture album missing — this test has stopped checking anything")
	}
	_, body := playerGet(t, srv, "/api/player/albums/"+id)
	tracks, _ := body["tracks"].([]any)
	if len(tracks) < 2 {
		t.Fatalf("album returned %d tracks; need at least 2 for the count to mean anything",
			len(tracks))
	}

	if calls != 1 {
		t.Errorf("the toolchain probe was asked %d times for %d tracks, want 1 — "+
			"it is a mutex-guarded TTL cache and the answer cannot change mid-request",
			calls, len(tracks))
	}
}
