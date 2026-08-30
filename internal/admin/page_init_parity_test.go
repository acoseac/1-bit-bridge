package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Every operator page's controls are wired by a per-page initX, dispatched
// from app.js's dispatchPageInit on the page's TAB name. The tab name is
// the page key in `pages` (admin.go); nothing connects the two but a
// string, and a mismatch is silent in the worst way — the markup renders,
// the buttons look live, and clicking them does nothing.
//
// This is not hypothetical either. PR #739 moved the operator dashboard
// off "/" and renamed its page key "dashboard" → "stats", leaving
// `case "dashboard": initDashboard()` behind. Every lookup inside that
// function is nil-guarded, so nothing threw: it simply never ran, and
// "Scan now", "Which tracks?" and "Retry missing" on Stats — plus
// "Check now" and "Roll back", which the same function wired for the
// Settings page — had no click handler at all for two days.

// tabsWithoutInit are page tabs that legitimately have no case in the
// dispatch switch. Add to this list only with a reason.
var tabsWithoutInit = map[string]string{
	"player": "the player module (boot.js) owns its own mount and routing",
	// The 404 page is prose and five links. It has no control to wire,
	// and giving it an empty init case just to satisfy this guard would
	// make the guard weaker for every page that does need one.
	"notfound": "static page — prose and anchors, no interactive controls",
}

var dispatchCaseRe = regexp.MustCompile(`case "([a-z]+)":\s*init`)

func TestEveryPageTabHasAnInitCase(t *testing.T) {
	srv, _, _ := newTestServer(t)

	app, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	// Normalized to LF before any offset arithmetic. A Windows checkout
	// carries CRLF (there is no .gitattributes pinning eol), so the
	// "\n}\n" terminator below simply is not present in the bytes and
	// this test failed with "could not find the end of dispatchPageInit"
	// on windows-latest from the day it was added — a parity guard that
	// is permanently red on one platform checks nothing on that platform
	// while looking like it does.
	body := strings.ReplaceAll(string(app), "\r\n", "\n")
	start := strings.Index(body, "function dispatchPageInit(")
	if start < 0 {
		t.Fatal("dispatchPageInit not found in app.js — this test has stopped checking anything")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of dispatchPageInit")
	}
	dispatch := body[start : start+end]

	cases := map[string]bool{}
	for _, m := range dispatchCaseRe.FindAllStringSubmatch(dispatch, -1) {
		cases[m[1]] = true
	}
	if len(cases) < 5 {
		t.Fatalf("only %d init cases scraped — the regex has stopped matching, "+
			"which would make this test pass while checking nothing", len(cases))
	}

	// Tabs come from the SERVER's own page table, read off the constructed
	// server rather than restated here: a hand-listed copy is the second
	// place that can be wrong, and the drift this catches is exactly a
	// name being changed in one place and not the other.
	var missing []string
	for tab := range srv.pageTmpls {
		if _, ok := tabsWithoutInit[tab]; ok {
			continue
		}
		if !cases[tab] {
			missing = append(missing, tab)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("pages with no dispatchPageInit case: %v\n"+
			"Their controls render with correct markup and no click handler. "+
			"Either add a case, or add the tab to tabsWithoutInit WITH the reason.",
			missing)
	}

	// And the other direction: a case naming a tab no page renders is the
	// exact shape of the bug above — dead code that looks live.
	var stale []string
	for tab := range cases {
		if _, ok := srv.pageTmpls[tab]; !ok {
			stale = append(stale, tab)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("dispatchPageInit cases for tabs no page renders: %v\n"+
			"These never fire. A page was probably renamed without updating the switch.",
			stale)
	}
}

// Every player route renders the "player" tab AND the "player" section, so
// nothing in the sidebar can be keyed on either alone without lighting on
// all of them. playerNavEntry is the discriminator that lets one player
// sub-section own its own rail entry, and it has to agree with three other
// places: the layout's data-player-section attributes, the entries'
// highlight conditions, and boot.js, which re-applies the same rule for
// navigations that never reach the server.
//
// Since Playlists and Smart mixes were removed from the sidebar (Browse's
// own section rail already lists both), NO section owns an entry — so the
// interesting assertion is that /playlists and /mixes light Browse, and
// that exactly one entry is current on each. Those two rows are the ones
// that would catch a half-finished re-add: an entry declared in the layout
// but not returned here lights nothing, and the reverse double-lights.
func TestSidebarPlayerNavHighlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		path, wantEntry string
	}{
		{"/", "Browse"},
		{"/albums", "Browse"},
		{"/artists", "Browse"},
		{"/playlists", "Browse"},
		{"/playlist/abc", "Browse"},
		{"/mixes", "Browse"},
		{"/mix/heavy-rotation", "Browse"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", tc.path, w.Code)
			continue
		}
		nav := primaryNavMarkup(t, tc.path, w.Body.String())
		if n := strings.Count(nav, `aria-current="page"`); n != 1 {
			t.Errorf("GET %s: %d nav entries current, want exactly 1\n%s", tc.path, n, nav)
			continue
		}
		// The current entry is the one whose <span> label follows the
		// aria-current marker within the same <a>.
		cut := nav[strings.Index(nav, `aria-current="page"`):]
		if !strings.Contains(cut[:min(len(cut), 300)], ">"+tc.wantEntry+"<") {
			t.Errorf("GET %s: expected the %q entry to be current; nav was\n%s",
				tc.path, tc.wantEntry, nav)
		}
	}
}

// TestPlayerNavEntriesMatchTheLayout pins playerNavEntry against the
// data-player-section attributes the layout declares and boot.js queries.
// Adding a second such entry means touching all three, and this is what
// notices when only two of them move.
func TestPlayerNavEntriesMatchTheLayout(t *testing.T) {
	layout, err := os.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-player-section="([a-z]+)"`).
		FindAllStringSubmatch(string(layout), -1) {
		declared[m[1]] = true
	}

	// Every player section the server can resolve, from playerRoutes.
	produced := map[string]bool{}
	for _, r := range playerRoutes {
		head := strings.SplitN(strings.TrimPrefix(r, "/"), "/", 2)[0]
		if e := playerNavEntry(head); e != "" {
			produced[e] = true
		}
	}

	// Assert the CURRENT truth rather than letting both loops pass over
	// empty maps: with no entries on either side this test would otherwise
	// verify nothing at all, which is how a seam rots into paper.
	if len(declared) != len(produced) {
		t.Errorf("sidebar declares %d data-player-section entries but playerNavEntry "+
			"produces %d — the two sides of the contract have diverged",
			len(declared), len(produced))
	}
	if len(declared) == 0 {
		t.Log("no player section owns a sidebar entry (Playlists and Smart mixes " +
			"were removed as duplicates of Browse's section rail); the seam is " +
			"kept empty on purpose — see playerNavEntry")
	}

	for e := range produced {
		if !declared[e] {
			t.Errorf("playerNavEntry returns %q but no sidebar entry declares "+
				`data-player-section=%q — the entry can never be highlighted`, e, e)
		}
	}
	for e := range declared {
		if !produced[e] {
			t.Errorf(`sidebar declares data-player-section=%q but playerNavEntry `+
				"never returns it — the entry is highlighted by nothing, and Browse "+
				"lights up on its route instead", e)
		}
	}

	// boot.js re-applies the rule client-side, because most navigation in
	// the player never reaches the server. It can't be evaluated here, but
	// a rename of the attribute on one side alone is worth catching.
	boot, err := os.ReadFile("static/player/boot.js")
	if err != nil {
		t.Fatalf("read boot.js: %v", err)
	}
	if !strings.Contains(string(boot), "data-player-section") {
		t.Error("boot.js no longer queries data-player-section — client-side " +
			"navigation will leave the sidebar highlight wherever it was")
	}
}

// A boosted navigation swaps <main> and leaves the sidebar in place, so the
// highlight is moved by JS from response headers. X-Bridge-Player-Nav is
// the third of those, and without it a boost into /mixes could only match
// on the tab — which Browse shares with every other player route.
//
// Sent only when there is an entry to name: an absent header means Browse.
func TestPartialResponseCarriesPlayerNav(t *testing.T) {
	srv, _, _ := newTestServer(t)
	//
	// With no player section owning a rail entry, the header is currently
	// never sent — every row wants "". The rows are kept so that a re-add
	// which forgets the header (leaving a boosted navigation unable to move
	// the highlight, since a boost swaps main and leaves the sidebar alone)
	// is caught by an existing test rather than needing a new one.
	for _, tc := range []struct{ path, want string }{
		{"/mixes", ""},
		{"/mix/heavy-rotation", ""},
		{"/playlists", ""},
		{"/playlist/abc", ""},
		{"/albums", ""},
		{"/", ""},
		{"/jobs", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("X-Bridge-Partial", "1")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s (partial): status %d", tc.path, w.Code)
			continue
		}
		if got := w.Header().Get("X-Bridge-Player-Nav"); got != tc.want {
			t.Errorf("GET %s (partial): X-Bridge-Player-Nav = %q; want %q",
				tc.path, got, tc.want)
		}
	}
}

// The harmonic-coverage wheel moved from the retired /smartmixes page to
// Stats: a key distribution is a fact about the library, like the
// composition bars above it, not a control.
//
// Hidden entirely when nothing is analyzed — an empty wheel is a puzzle,
// not a reading — so both halves of that gate are pinned here.
func TestStatsPageCarriesHarmonicCoverage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/stats", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /stats: status %d", w.Code)
		}
		return w.Body.String()
	}

	if body := get(); strings.Contains(body, "harmonic-coverage-panel") {
		t.Error("nothing is analyzed, so /stats must not render an empty wheel")
	}

	// KeyRoot 9 / minor → Camelot 8A.
	rate, bits, isDSD := 44100.0, 16, false
	if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
		Path: "MusicA/Album1/keyed.flac", Size: 100,
		SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	aMinor := 9
	if err := srv.deps.Manifest.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: "MusicA/Album1/keyed.flac", KeyRoot: &aMinor, KeyMode: "minor",
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}

	body := get()
	for _, want := range []string{
		"harmonic-coverage-panel",
		`id="camelot-wheel"`,
		// The JSON island app.js paints from, carrying the real code.
		`id="key-coverage-data"`,
		`"8A"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats missing %q once a track is analyzed", want)
		}
	}
}
