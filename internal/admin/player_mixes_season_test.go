package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedChristmasCache caches the Christmas family beside Heavy Rotation,
// over tracks seedCollectionLibrary indexed.
func seedChristmasCache(t *testing.T, srv *Server) {
	t.Helper()
	seedCollectionLibrary(t, srv.deps.Manifest)
	if err := srv.deps.Manifest.ReplaceSmartPlaylists(t.Context(), []manifest.StoredSmartPlaylist{
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Position: 0, RefreshedAt: 1,
			ItemsJSON: []byte(`[{"position":0,"path":"A/Alpha/01.flac"}]`)},
		{Slug: "christmas", Kind: "christmas", Title: "Christmas", Position: 1, RefreshedAt: 1,
			Subtitle:  "Carols and Christmas albums from your library",
			ItemsJSON: []byte(`[{"position":0,"path":"C/Charlie/01.flac"},{"position":1,"path":"D/Delta/01.flac"}]`)},
	}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists: %v", err)
	}
}

// outOfSeasonIn finds the collection id in a decoded list and says whether
// it carries the mark, and whether the key is there at all.
func outOfSeasonIn(rows []any, id string) (marked, present, found bool) {
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["id"] != id && row["slug"] != id {
			continue
		}
		v, ok := row["outOfSeason"]
		b, _ := v.(bool)
		return b, ok, true
	}
	return false, false, false
}

// The console lists the Christmas family all year and marks it out of
// season wherever GET /v1/smart-playlists leaves it out: in the player's
// grid and detail, and in the inspection list, by the same window
// (season.InWindow) at the same edges, read in UTC. No other family is ever
// marked. Out of season the operator's own actions still work: the detail
// hydrates its tracks for playback, and Save as playlist saves them.
func TestTheConsoleMarksTheChristmasMixOutOfSeason(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedChristmasCache(t, srv)
	var at time.Time
	srv.testNow = func() time.Time { return at }
	plus14 := time.FixedZone("UTC+14", 14*60*60)
	for _, c := range []struct {
		at  time.Time
		out bool
	}{
		{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), true},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), false},
		{time.Date(2027, 1, 5, 23, 59, 59, 0, time.UTC), false},
		{time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 12, 1, 5, 0, 0, 0, plus14), true},
	} {
		at = c.at
		when := at.Format(time.RFC3339)

		w, body := playerGet(t, srv, "/api/player/mixes")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: /api/player/mixes answered %d: %s", when, w.Code, w.Body.String())
		}
		rows, _ := body["collections"].([]any)
		if marked, _, found := outOfSeasonIn(rows, "christmas"); !found || marked != c.out {
			t.Errorf("%s: the grid lists christmas %v, marked out of season %v, want listed and %v", when, found, marked, c.out)
		}
		if _, present, _ := outOfSeasonIn(rows, "heavy-rotation"); present {
			t.Errorf("%s: heavy rotation carries an out-of-season mark", when)
		}

		w, body = playerGet(t, srv, "/api/player/mixes/christmas")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the detail answered %d: %s", when, w.Code, w.Body.String())
		}
		coll, _ := body["collection"].(map[string]any)
		if marked, _ := coll["outOfSeason"].(bool); marked != c.out {
			t.Errorf("%s: the detail is marked out of season %v, want %v", when, marked, c.out)
		}
		if tracks, _ := body["tracks"].([]any); len(tracks) != 2 {
			t.Errorf("%s: the detail hydrates %d tracks, want both, playable in or out of season", when, len(tracks))
		}

		w, body = playerGet(t, srv, "/api/smart-playlists")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: /api/smart-playlists answered %d", when, w.Code)
		}
		list, _ := body["playlists"].([]any)
		if marked, _, found := outOfSeasonIn(list, "christmas"); !found || marked != c.out {
			t.Errorf("%s: the inspection list lists christmas %v, marked %v, want listed and %v", when, found, marked, c.out)
		}
	}

	at = time.Date(2027, 7, 1, 12, 0, 0, 0, time.UTC)
	var saved struct {
		TrackCount int `json:"trackCount"`
	}
	if code := doJSON(t, srv.Handler(), "POST", "/api/smart-playlists/christmas/save-as-playlist",
		map[string]string{"name": "Christmas, kept"}, &saved); code != http.StatusOK || saved.TrackCount != 2 {
		t.Fatalf("Save as playlist out of season answered %d with %d tracks, want 200 and 2", code, saved.TrackCount)
	}
}

// The player's views draw the mark the server sends, in words: a chip on
// the grid's tile, and on the detail a line saying when the app shows the
// mix. Pinned in the source, so it holds where node is not installed;
// TestThePlayersMixGridDrawsTheOutOfSeasonChip runs the grid itself.
func TestTheMixViewsWordTheOutOfSeasonMark(t *testing.T) {
	src := readFile(t, "static/player/views.js")
	tile := extractJSFunction(t, src, "collectionTile")
	if !strings.Contains(tile, "c.outOfSeason") || !strings.Contains(tile, `chip("Out of season"`) {
		t.Errorf("collectionTile does not draw the out-of-season chip from c.outOfSeason:\n%s", tile)
	}
	note := extractJSFunction(t, src, "seasonNote")
	if !strings.Contains(note, "c.outOfSeason") || !strings.Contains(note, "1 December to 5 January (UTC)") {
		t.Errorf("seasonNote does not word the window from c.outOfSeason:\n%s", note)
	}
	detail := extractJSFunction(t, src, "renderCollectionDetail")
	if !strings.Contains(detail, "seasonNote(c)") {
		t.Errorf("the detail never draws seasonNote:\n%s", detail)
	}
}

// outOfSeasonGridHarness renders the shipped mixes grid twice, the Christmas
// mix out of season and then in, and prints the view's text each time.
const outOfSeasonGridHarness = playerViewStub + `
let server = {};
globalThis.fetch = async (url) => {
  const body = String(url).startsWith("/api/player/mixes") ? server : {};
  return { ok: true, status: 200, statusText: "OK", json: async () => JSON.parse(JSON.stringify(body)) };
};
window.BridgeFeatureTray = { build() { return { button: new Node("button"), tray: new Node("div") }; } };
const { renderMixes } = await import(process.argv[2]);
const ctx = { gen: () => 1, setToolbar: noop, setCrumb: noop, params: new URLSearchParams(), id: "", trail: [] };
const out = [];
for (const outOfSeason of [true, false]) {
  const view = new Node("main");
  server = { enabled: true, snapshotAt: "", collections: [
    { id: "christmas", name: "Christmas", kind: "christmas", count: 30, outOfSeason,
      subtitle: "Carols and Christmas albums from your library" },
    { id: "heavy-rotation", name: "Heavy Rotation", kind: "heavyRotation", count: 12 },
  ] };
  await renderMixes(view, ctx);
  out.push({ outOfSeason, view: view.textContent });
}
console.log(JSON.stringify(out));
`

// TestThePlayersMixGridDrawsTheOutOfSeasonChip runs the shipped renderMixes
// under node: the chip is on the grid exactly when the server marks the mix.
func TestThePlayersMixGridDrawsTheOutOfSeasonChip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped player module")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "season.mjs")
	if err := os.WriteFile(script, []byte(outOfSeasonGridHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Join("static", "player", "views.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script, fileURL(module)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var steps []struct {
		OutOfSeason bool   `json:"outOfSeason"`
		View        string `json:"view"`
	}
	if err := json.Unmarshal(raw, &steps); err != nil || len(steps) != 2 {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	for _, s := range steps {
		if !strings.Contains(s.View, "Christmas") || !strings.Contains(s.View, "Heavy Rotation") {
			t.Errorf("out of season %v: the grid says %q, want both mixes listed", s.OutOfSeason, s.View)
		}
		if got := strings.Count(s.View, "Out of season"); (got == 1) != s.OutOfSeason || got > 1 {
			t.Errorf("out of season %v: the grid shows the chip %d times", s.OutOfSeason, got)
		}
	}
}
