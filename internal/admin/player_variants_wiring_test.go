package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// variantsWiringHarness renders the SHIPPED album and artist views under node
// on a server that answers their detail with a variant summary, and drives
// the panel's gear the way a save does: the tray's onSaved for a field. It
// counts what the page then asks the server for and what the shell is told to
// do, because the panel's in-place redraw is worth what the views hand it:
// a panel that could redraw itself and was never given the way to fetch a
// summary would fall back to the route's re-render, and only a run of the
// view can see that.
const variantsWiringHarness = `
document.title = "Album — B68 Library";
let details = 0;
let summary = { enabled: false, soxAvailable: true, optimizeActive: false, sourceBytes: 0, variantBytes: 0,
  upscale: { covered: 0, eligible: 2, exempt: 0, stale: 0 }, optimize: { covered: 0, eligible: 2, exempt: 0, stale: 0 } };
globalThis.fetch = async (url) => {
  let body = {};
  const u = String(url);
  if (u.startsWith("/api/player/albums/")) {
    details++;
    body = { album: { title: "Slow Light", artistId: "ar1", albumArtist: "Meridian Glass", trackCount: 0,
      discCount: 1, duration: 0, sizeBytes: 0 }, tracks: [], release: null, variants: summary };
  } else if (u.startsWith("/api/player/artists/")) {
    details++;
    body = { artist: { name: "Meridian Glass", albumCount: 0, trackCount: 0, artistMBID: "" }, albums: [],
      about: null, hasImage: false, variants: summary };
  } else if (u.startsWith("/api/settings")) {
    body = { allowDelete: false };
  }
  return { ok: true, status: 200, statusText: "OK", json: async () => JSON.parse(JSON.stringify(body)) };
};
const events = [];
window.dispatchEvent = (ev) => { events.push(ev.type); };

const builds = [];
window.BridgeFeatureTray = {
  build(spec) {
    const button = new Node("button"); button.own = "gear";
    const tray = new Node("div");
    builds.push({ spec, button, tray });
    return { button, tray };
  },
};

const { renderAlbum, renderArtist } = await import(process.argv[2]);
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise((r) => setTimeout(r, 0)); };
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const has = (n, c) => (n.className || "").split(/\s+/).includes(c);
// The panel-wide note the panel shows now, "" when it shows none.
const panelNote = (view) => {
  let text = "";
  walk(view, (n) => {
    if (n.tagName === "section" && has(n, "variants")) {
      for (const c of n.children) walk(c, (m) => { if (has(m, "variants-blocked") && m.textContent) text = m.textContent; });
    }
  });
  return text;
};

const out = {};
for (const [name, render] of [["album", renderAlbum], ["artist", renderArtist]]) {
  details = 0; events.length = 0; builds.length = 0;
  summary = { ...summary, enabled: false };
  let gen = 1;
  const view = new Node("main");
  const ctx = { id: "x1", gen: () => gen, setToolbar() {}, setCrumb() {}, trail: [], params: new URLSearchParams() };
  await render(view, ctx);
  const drawn = { details, builds: builds.length, note: panelNote(view) };
  // The reader turns generation on from the panel's gear.
  summary = { ...summary, enabled: true, optimizeActive: true };
  builds[0].spec.onSaved("upscaleEnabled");
  await settle();
  const saved = { details, events: [...events], note: panelNote(view) };
  // Then the route moves on while a second answer is out.
  gen++;
  summary = { ...summary, enabled: false };
  builds[0].spec.onSaved("upscaleEnabled");
  await settle();
  const movedOn = { details, events: [...events], note: panelNote(view) };
  out[name] = { drawn, saved, movedOn };
}
console.log(JSON.stringify(out));
`

// TestTheAlbumAndArtistViewsLetTheirVariantPanelRedrawInPlace runs the
// shipped renderAlbum and renderArtist under node and pins what a save in the
// Variants panel's gear makes the page do.
//
// The panel redraws itself in place from a fresh summary, and it can only if
// the view gives it the fetch and the route it belongs to. So a save fetches
// the album's (or artist's) detail once more and asks the shell for nothing:
// no player:rerender, which is the whole-route re-render that took the tray,
// its "Saved." and the focus. The panel then shows what the new summary says.
// And once the route has moved on the save still fetches (it cannot know), but
// paints nothing over the page that replaced it.
func TestTheAlbumAndArtistViewsLetTheirVariantPanelRedrawInPlace(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped player module")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "wiring.mjs")
	if err := os.WriteFile(script, []byte(playerViewStub+variantsWiringHarness), 0o600); err != nil {
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
	type step struct {
		Details int      `json:"details"`
		Builds  int      `json:"builds"`
		Events  []string `json:"events"`
		Note    string   `json:"note"`
	}
	var out map[string]struct {
		Drawn   step `json:"drawn"`
		Saved   step `json:"saved"`
		MovedOn step `json:"movedOn"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	for _, view := range []string{"album", "artist"} {
		r, ok := out[view]
		if !ok {
			t.Fatalf("the harness did not render the %s view: %s", view, raw)
		}
		if r.Drawn.Details != 1 || r.Drawn.Builds != 1 || r.Drawn.Note == "" {
			t.Fatalf("%s: the view fetched %d details, built %d trays and drew the note %q; want one fetch, the "+
				"panel's tray and the block's note, or the run measures nothing", view, r.Drawn.Details, r.Drawn.Builds, r.Drawn.Note)
		}
		if r.Saved.Details != 2 || len(r.Saved.Events) != 0 || r.Saved.Note != "" {
			t.Errorf("%s: after a save the page has fetched %d details, asked the shell for %v and shows the note "+
				"%q; want one more fetch, no whole-route re-render and the block lifted", view, r.Saved.Details,
				r.Saved.Events, r.Saved.Note)
		}
		if r.MovedOn.Details != 3 || len(r.MovedOn.Events) != 0 || r.MovedOn.Note != "" {
			t.Errorf("%s: a save after the route moved on: %d details fetched, shell asked for %v, the note %q; want "+
				"the fetch made and nothing painted over the new page", view, r.MovedOn.Details, r.MovedOn.Events, r.MovedOn.Note)
		}
	}
}
