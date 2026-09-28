package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The CarPlay kind's own switch on the variant panel.
//
// POST /api/upscale/batch refuses the optimize kind while
// `upscale.optimizeEnabled` is off (#1060), and until 2026-09-28 the variant
// summary the panel reads carried no such switch, so with upscaling on and
// the switch off "Generate CarPlay" stayed live and a click answered 503
// `optimize-disabled`. The summary now carries the submit's own predicate
// (Server.optimizeActive) as `optimizeActive`, and the panel disables the
// kind's button on it, with the reason and the switch beside it.

// TestTheVariantSummaryCarriesTheSwitchTheSubmitReads pins that the album
// and artist variant summaries report the CarPlay switch as the batch
// submit reads it, request by request.
//
// The field is worth only what its agreement with the submit is worth, so
// each state of the switch (on, off, on again, and unwired, which reads as
// on) is checked against a real submit of the optimize kind for the same
// album, on one server whose switch moves between requests as a settings
// PATCH moves it.
func TestTheVariantSummaryCarriesTheSwitchTheSubmitReads(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantAlbum(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	albumID := albumIDByTitle(t, srv, "Album")
	artistID := artistIDByName(t, srv, "Artist")
	on := func() bool { return true }
	off := func() bool { return false }

	for _, step := range []struct {
		name string
		gate func() bool
		want bool
	}{
		{"on", on, true},
		{"off", off, false},
		{"on again", on, true},
		{"unwired", nil, true},
	} {
		srv.deps.OptimizeActive = step.gate
		_, errCode, _ := submitCounting(t, srv, stub, `{"albumIds":["`+albumID+`"],"kind":"optimize"}`)
		refused := errCode == "optimize-disabled"
		if refused == step.want {
			t.Errorf("switch %s: the submit answered %q", step.name, errCode)
		}
		for _, target := range []string{"/api/player/albums/" + albumID, "/api/player/artists/" + artistID} {
			w, body := playerGet(t, srv, target)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: GET %s: status %d body %s", step.name, target, w.Code, w.Body.String())
			}
			sum, _ := body["variants"].(map[string]any)
			says, ok := sum["optimizeActive"].(bool)
			if !ok || says != step.want {
				t.Errorf("switch %s: %s reports optimizeActive %v, want %v",
					step.name, target, sum["optimizeActive"], step.want)
			}
			if ok && says == refused {
				t.Errorf("switch %s: %s says optimizeActive %v while the submit answered %q; "+
					"the panel would offer a button the submit refuses, or disable one it accepts",
					step.name, target, says, errCode)
			}
		}
	}
}

// panelRow is one kind's row of the variant panel as the node harness below
// reads it back.
type panelRow struct {
	Title            string   `json:"title"`
	GenerateDisabled bool     `json:"generateDisabled"`
	Notes            []string `json:"notes"`
	Links            []string `json:"links"`
}

// renderedPanel is one variant panel as the node harness reads it back: its
// rows, the notes above them, and the trays it asked app.js to build.
// RedrawnBy lists the tray's fields whose live save redraws the panel.
type renderedPanel struct {
	Name       string     `json:"name"`
	Rows       []panelRow `json:"rows"`
	PanelNotes []string   `json:"panelNotes"`
	Trays      []struct {
		Title     string   `json:"title"`
		Fields    []string `json:"fields"`
		RedrawnBy []string `json:"redrawnBy"`
	} `json:"trays"`
}

// panelHarness builds each summary into the SHIPPED variant panel under node
// and reads the panels back.
//
// Its DOM is small and real enough for that: elements keep their children,
// class, text, attributes and disabled state. testdata/domstub.mjs keeps
// nothing, deliberately, since its job is only to let a module finish
// evaluating. window.BridgeFeatureTray is app.js's, a classic script this
// harness does not load, so a case that asks for a tray gets a stand-in
// that records the spec; one that does not gets none, the panel's fallback
// path. Once the panel is built, each tray's onSaved is called with each of
// its fields, as saveTrayField calls it after a save the server applied
// live, and a field whose call reaches the panel's onChanged is recorded.
const panelHarness = `
class Node {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.attributes = {};
    this.className = ""; this.own = ""; this.style = {}; this.disabled = false;
  }
  get textContent() {
    return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own;
  }
  set textContent(v) { this.children = []; this.own = String(v); }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  appendChild(c) { this.children.push(c); return c; }
  addEventListener() {}
  get firstChild() { return this.children[0] || null; }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; }
}
globalThis.document = {
  createElement: (tag) => new Node(tag),
  createTextNode: (s) => { const n = new Node("#text"); n.own = String(s); return n; },
};
globalThis.window = {};
const { variantPanel } = await import(process.argv[2]);
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[3], "utf8"));

const has = (n, c) => (n.className || "").split(/\s+/).includes(c);
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const out = [];
for (const c of cases) {
  const trays = [];
  let redraws = 0;
  window.BridgeFeatureTray = c.tray ? {
    build(spec) {
      trays.push({ title: spec.title, fields: (spec.rows || []).map((r) => r.field), spec });
      return { button: new Node("button"), tray: new Node("div") };
    },
  } : undefined;
  const panel = variantPanel(c.summary, { albumIds: ["0123456789abcdef"] }, () => { redraws++; }, { plain: true });
  for (const t of trays) {
    t.redrawnBy = [];
    for (const field of t.fields) {
      const before = redraws;
      t.spec.onSaved?.(field);
      if (redraws > before) t.redrawnBy.push(field);
    }
    delete t.spec;
  }
  const rows = [];
  const panelNotes = [];
  for (const child of panel.children) {
    if (!has(child, "variant-kind")) {
      walk(child, (n) => { if (has(n, "variants-blocked")) panelNotes.push(n.textContent); });
      continue;
    }
    const row = { title: "", generateDisabled: false, notes: [], links: [] };
    walk(child, (n) => {
      if (has(n, "variant-kind-title")) row.title = n.textContent;
      if (n.tagName === "button" && n.textContent.startsWith("Generate")) row.generateDisabled = n.disabled;
      if (has(n, "variants-blocked")) row.notes.push(n.textContent);
      if (n.tagName === "a") row.links.push(n.getAttribute("href"));
    });
    rows.push(row);
  }
  out.push({ name: c.name, rows, panelNotes, trays });
}
console.log(JSON.stringify(out));
`

// renderPanelsUnderNode runs panelHarness over the given cases and returns
// the panels in order.
func renderPanelsUnderNode(t *testing.T, node string, cases any) []renderedPanel {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "panel.mjs")
	if err := os.WriteFile(script, []byte(panelHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(casesPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Join("static", "player", "variants.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script, fileURL(module), casesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var panels []renderedPanel
	if err := json.Unmarshal(raw, &panels); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	return panels
}

// TestTheVariantPanelDisablesGenerateCarPlayWhereTheSubmitRefusesIt runs the
// SHIPPED variant panel on the summary the album detail serves, and compares
// each kind's Generate button with what the submit answers for that kind and
// album.
//
// A test of the Go field alone passes with the panel ignoring it, and one of
// the panel alone passes with a summary that never carries it; this one
// needs both halves, and the switches move through the PATCH the Settings
// page and the panel's own tray send. The gates are wired as cmd/bridge
// wires them, from the live config. The fixture's two hi-res FLACs are
// eligible for both kinds, so a disabled Generate here can only mean a
// refusal, never "nothing to do".
func TestTheVariantPanelDisablesGenerateCarPlayWhereTheSubmitRefusesIt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped panel")
	}
	srv, _, _ := newTestServer(t)
	seedVariantAlbum(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	srv.deps.UpscalePrecheck = func() error { return nil }
	srv.deps.UpscaleActive = func() bool { return srv.deps.CfgHolder.Load().Upscale.Enabled }
	srv.deps.OptimizeActive = func() bool {
		live := srv.deps.CfgHolder.Load()
		return live.Upscale.Enabled && live.Upscale.EffectiveOptimizeEnabled()
	}
	albumID := albumIDByTitle(t, srv, "Album")

	type panelCase struct {
		Name    string          `json:"name"`
		Summary json.RawMessage `json:"summary"`
		Tray    bool            `json:"tray"`
	}
	type state struct {
		name    string
		refused map[string]string // kind -> the submit's error code, "" when it accepts
	}
	var cases []panelCase
	var states []state
	for _, s := range []struct {
		name     string
		settings map[string]any
	}{
		{"both on", map[string]any{"upscaleEnabled": true, "optimizeEnabled": true}},
		{"CarPlay off", map[string]any{"optimizeEnabled": false}},
		{"upscaling off", map[string]any{"upscaleEnabled": false}},
	} {
		if code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", s.settings, nil); code != http.StatusOK {
			t.Fatalf("%s: PATCH /api/settings %v answered %d", s.name, s.settings, code)
		}
		w, body := playerGet(t, srv, "/api/player/albums/"+albumID)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: album detail: status %d", s.name, w.Code)
		}
		summary, err := json.Marshal(body["variants"])
		if err != nil {
			t.Fatal(err)
		}
		st := state{name: s.name, refused: map[string]string{}}
		for _, kind := range []string{"upscale", "optimize"} {
			code, errCode, _ := submitCounting(t, srv, stub, `{"albumIds":["`+albumID+`"],"kind":"`+kind+`"}`)
			if code != http.StatusAccepted && errCode == "" {
				t.Fatalf("%s: the %s submit answered %d with no error code", s.name, kind, code)
			}
			st.refused[kind] = errCode
		}
		states = append(states, st)
		cases = append(cases,
			panelCase{Name: s.name, Summary: summary, Tray: true},
			panelCase{Name: s.name + ", no tray", Summary: summary, Tray: false})
	}

	panels := renderPanelsUnderNode(t, node, cases)
	if len(panels) != len(cases) {
		t.Fatalf("the harness rendered %d panels for %d cases", len(panels), len(cases))
	}
	kindByTitle := map[string]string{"Hi-res upscale": "upscale", "CarPlay-optimized": "optimize"}
	for i, p := range panels {
		st := states[i/2]
		withTray := cases[i].Tray
		rows := map[string]panelRow{}
		for _, row := range p.Rows {
			kind, ok := kindByTitle[row.Title]
			if !ok {
				t.Fatalf("%s: a kind row titled %q, which this test does not know", p.Name, row.Title)
			}
			rows[kind] = row
			if refused := st.refused[kind] != ""; row.GenerateDisabled != refused {
				t.Errorf("%s: %q Generate disabled=%v, while the submit of that kind answered %q",
					p.Name, row.Title, row.GenerateDisabled, st.refused[kind])
			}
		}
		if len(rows) != 2 {
			t.Fatalf("%s: the panel has kind rows %+v, want one per kind", p.Name, p.Rows)
		}
		hiRes, carPlay := rows["upscale"], rows["optimize"]
		switch st.name {
		case "both on":
			if len(p.PanelNotes)+len(hiRes.Notes)+len(carPlay.Notes)+len(p.Trays) != 0 {
				t.Errorf("%s: nothing is off, yet the panel says %v / %v / %v and built trays %+v",
					p.Name, p.PanelNotes, hiRes.Notes, carPlay.Notes, p.Trays)
			}
		case "CarPlay off":
			if st.refused["optimize"] != "optimize-disabled" {
				t.Fatalf("%s: the optimize submit answered %q, want optimize-disabled", p.Name, st.refused["optimize"])
			}
			if len(carPlay.Notes) != 1 || !strings.Contains(carPlay.Notes[0], "switched off") {
				t.Errorf("%s: the CarPlay row carries notes %q, want one saying the switch is off",
					p.Name, carPlay.Notes)
			}
			if len(p.PanelNotes)+len(hiRes.Notes) != 0 {
				t.Errorf("%s: the switch stops the CarPlay kind alone, yet the panel says %v and "+
					"the hi-res row %v", p.Name, p.PanelNotes, hiRes.Notes)
			}
			switch {
			case withTray && (len(p.Trays) != 1 || strings.Join(p.Trays[0].Fields, ",") != "optimizeEnabled"):
				t.Errorf("%s: trays %+v, want one holding the optimizeEnabled switch", p.Name, p.Trays)
			case !withTray && !slices.Contains(carPlay.Links, "/settings?tab=audio"):
				t.Errorf("%s: with no tray the CarPlay row links %v, want the audio settings", p.Name, carPlay.Links)
			}
		case "upscaling off":
			if len(p.PanelNotes) != 1 || len(carPlay.Notes) != 0 {
				t.Errorf("%s: panel notes %v and CarPlay row notes %v; a block that stops both "+
					"kinds is said once, above them", p.Name, p.PanelNotes, carPlay.Notes)
			}
			if withTray && (len(p.Trays) != 1 || strings.Join(p.Trays[0].Fields, ",") != "upscaleEnabled,optimizeEnabled") {
				t.Errorf("%s: trays %+v, want the panel-wide one with both switches", p.Name, p.Trays)
			}
		}
	}
}

// TestAVariantTraySaveRedrawsThePanelWhereTheSwitchChangesIt pins which of
// the variant panel's trays redraw it after a save, and that the redraw
// then shows what the switch allows.
//
// A tray saves a switch and redraws nothing else, so until 2026-09-28 the
// CarPlay row kept its "switched off" note and a disabled Generate beside
// the tray's "Saved." until the next render (CodeRabbit on #1068). Each tray
// now calls the panel's onChanged from its onSaved: the CarPlay kind's own
// tray for its one switch, and the panel-wide tray for the upscaling switch
// alone, since nothing the panel draws while generation is off depends on
// the CarPlay one, and a redraw for it would take the tray and its "Saved."
// away for nothing. TestATrayCallsOnSavedOnlyAfterASaveTheServerAppliedLive
// pins the other half, that a tray calls onSaved after a live save.
//
// Each panel is built from the summary the album detail serves after the
// settings PATCH, and the save a tray makes is then sent as the same PATCH,
// so the redrawn panel is the one the re-fetch the redraw makes would draw.
func TestAVariantTraySaveRedrawsThePanelWhereTheSwitchChangesIt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped panel")
	}
	srv, _, _ := newTestServer(t)
	seedVariantAlbum(t, srv.deps.Manifest)
	srv.deps.UpscalePrecheck = func() error { return nil }
	srv.deps.UpscaleActive = func() bool { return srv.deps.CfgHolder.Load().Upscale.Enabled }
	srv.deps.OptimizeActive = func() bool {
		live := srv.deps.CfgHolder.Load()
		return live.Upscale.Enabled && live.Upscale.EffectiveOptimizeEnabled()
	}
	albumID := albumIDByTitle(t, srv, "Album")
	type panelCase struct {
		Name    string          `json:"name"`
		Summary json.RawMessage `json:"summary"`
		Tray    bool            `json:"tray"`
	}
	summaryAfter := func(name string, settings map[string]any) panelCase {
		t.Helper()
		if code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", settings, nil); code != http.StatusOK {
			t.Fatalf("%s: PATCH /api/settings %v answered %d", name, settings, code)
		}
		w, body := playerGet(t, srv, "/api/player/albums/"+albumID)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: album detail: status %d", name, w.Code)
		}
		summary, err := json.Marshal(body["variants"])
		if err != nil {
			t.Fatal(err)
		}
		return panelCase{Name: name, Summary: summary, Tray: true}
	}
	cases := []panelCase{
		summaryAfter("CarPlay off", map[string]any{"upscaleEnabled": true, "optimizeEnabled": false}),
		summaryAfter("CarPlay switched on from its tray", map[string]any{"optimizeEnabled": true}),
		summaryAfter("upscaling off", map[string]any{"upscaleEnabled": false, "optimizeEnabled": false}),
		summaryAfter("upscaling switched on from the panel's tray", map[string]any{"upscaleEnabled": true}),
	}
	panels := renderPanelsUnderNode(t, node, cases)
	if len(panels) != len(cases) {
		t.Fatalf("the harness rendered %d panels for %d cases", len(panels), len(cases))
	}
	carPlayRow := func(p renderedPanel) panelRow {
		t.Helper()
		for _, row := range p.Rows {
			if row.Title == "CarPlay-optimized" {
				return row
			}
		}
		t.Fatalf("%s: no CarPlay row in %+v", p.Name, p.Rows)
		return panelRow{}
	}
	trayIs := func(p renderedPanel, fields, redrawnBy string) {
		t.Helper()
		if len(p.Trays) != 1 {
			t.Errorf("%s: trays %+v, want one", p.Name, p.Trays)
			return
		}
		tr := p.Trays[0]
		if got := strings.Join(tr.Fields, ","); got != fields {
			t.Errorf("%s: the tray holds %s, want %s", p.Name, got, fields)
		}
		if got := strings.Join(tr.RedrawnBy, ","); got != redrawnBy {
			t.Errorf("%s: a save of [%s] redraws the panel, want [%s]", p.Name, got, redrawnBy)
		}
	}

	off, on, blocked, unblocked := panels[0], panels[1], panels[2], panels[3]
	// The CarPlay kind's own tray redraws the panel for its one switch, and
	// the redrawn row is live: no note, no gear, and Generate enabled.
	trayIs(off, "optimizeEnabled", "optimizeEnabled")
	if row := carPlayRow(off); !row.GenerateDisabled {
		t.Errorf("%s: Generate CarPlay is enabled with its switch off", off.Name)
	}
	if row := carPlayRow(on); row.GenerateDisabled || len(row.Notes) != 0 || len(on.Trays) != 0 {
		t.Errorf("%s: the redrawn CarPlay row has Generate disabled=%v, notes %q and trays %+v, "+
			"want a live row", on.Name, row.GenerateDisabled, row.Notes, on.Trays)
	}
	// The panel-wide tray redraws for the upscaling switch alone, and the
	// redrawn panel reads the CarPlay switch from the server: still off, so
	// the CarPlay row now says so itself, with its own tray.
	trayIs(blocked, "upscaleEnabled,optimizeEnabled", "upscaleEnabled")
	if row := carPlayRow(unblocked); len(unblocked.PanelNotes) != 0 || !row.GenerateDisabled ||
		len(row.Notes) != 1 || !strings.Contains(row.Notes[0], "switched off") {
		t.Errorf("%s: panel notes %q, and the CarPlay row has Generate disabled=%v and notes %q; "+
			"want no block and the CarPlay switch's own note", unblocked.Name, unblocked.PanelNotes,
			row.GenerateDisabled, row.Notes)
	}
	trayIs(unblocked, "optimizeEnabled", "optimizeEnabled")
}
