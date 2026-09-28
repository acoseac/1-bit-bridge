package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mixesViewHarness imports the shipped views.js under node, on a DOM that
// keeps children, text, attributes and order, and drives renderMixes
// through the steps a reader takes: the page at load, its gear's save of
// the Smart mixes switch (the tray's onSaved, as saveTrayField calls it
// after a live save), a save of the other switch, a save after the reader
// has left the page. window.BridgeFeatureTray is a stand-in that records
// the spec, since app.js is a classic script this harness does not load;
// fetch answers /api/player/mixes from the step's server state and counts
// the calls.
const mixesViewHarness = `
class Node {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.attributes = {}; this.className = "";
    this.own = ""; this.style = {}; this.dataset = {}; this.disabled = false; this.hidden = false;
    this.parentNode = null; this.isFragment = tag === "#fragment";
    this.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
  }
  get textContent() { return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own; }
  set textContent(v) { this.children = []; this.own = String(v); }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  removeAttribute(k) { delete this.attributes[k]; }
  appendChild(c) {
    if (c.isFragment) { for (const x of [...c.children]) this.appendChild(x); return c; }
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this; this.children.push(c); return c;
  }
  insertBefore(c, ref) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    const i = this.children.indexOf(ref);
    if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
    return c;
  }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; return c; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  get firstChild() { return this.children[0] || null; }
  addEventListener() {}
  removeEventListener() {}
  querySelector() { return null; }
  querySelectorAll() { return []; }
  closest() { return null; }
  scrollIntoView() {}
  focus() {}
}
const noop = () => {};
const byId = new Map();
globalThis.document = {
  createElement: (tag) => new Node(tag),
  createElementNS: (_, tag) => new Node(tag),
  createTextNode: (s) => { const n = new Node("#text"); n.own = String(s); return n; },
  createDocumentFragment: () => new Node("#fragment"),
  getElementById: (id) => { if (!byId.has(id)) byId.set(id, new Node("div")); return byId.get(id); },
  querySelector: () => null, querySelectorAll: () => [],
  addEventListener: noop, removeEventListener: noop,
  body: new Node("body"), documentElement: new Node("html"), visibilityState: "visible",
};
globalThis.window = {
  document: globalThis.document, addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
  location: { pathname: "/mixes", search: "", href: "http://x/mixes", assign: noop },
  history: { pushState: noop, replaceState: noop, state: null },
  matchMedia: () => ({ matches: false, addEventListener: noop }),
  scrollTo: noop, getComputedStyle: () => ({}),
};
globalThis.location = globalThis.window.location;
globalThis.history = globalThis.window.history;
globalThis.sessionStorage = { getItem: () => null, setItem: noop, removeItem: noop };
globalThis.localStorage = globalThis.sessionStorage;
globalThis.Audio = function () { return new Node("audio"); };
globalThis.EventSource = function () { return new Node("es"); };
globalThis.IntersectionObserver = class { observe() {} disconnect() {} };
globalThis.requestAnimationFrame = (f) => setTimeout(f, 0);

let server = {};
let mixesFetches = 0;
globalThis.fetch = async (url) => {
  const mixes = String(url).startsWith("/api/player/mixes");
  if (mixes) mixesFetches++;
  const body = mixes ? server : {};
  return { ok: true, status: 200, statusText: "OK", json: async () => JSON.parse(JSON.stringify(body)) };
};

let spec = null;
let builds = 0;
let gear = null;
window.BridgeFeatureTray = {
  build(s) {
    builds++;
    spec = s;
    gear = new Node("button");
    gear.textContent = "gear";
    return { button: gear, tray: new Node("div") };
  },
};

const { renderMixes } = await import(process.argv[2]);
const settle = async () => { for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0)); };
let gen = 1;
let toolbar = null;
const ctx = { gen: () => gen, setToolbar: (n) => { toolbar = n; }, setCrumb: noop,
  params: new URLSearchParams(), id: "", trail: [] };
const view = new Node("main");
const mix = { id: "heavy-rotation", name: "Heavy Rotation", kind: "heavyRotation", count: 1 };
const snap = (step) => {
  const buttons = [];
  const walk = (n) => { if (n.tagName === "button") buttons.push(n.textContent); for (const c of n.children) walk(c); };
  if (toolbar) walk(toolbar);
  const bar = toolbar && toolbar.className === "toolbar-stack" ? toolbar.children[0] : toolbar;
  return {
    step, view: view.textContent, toolbarButtons: buttons, builds, fetches: mixesFetches,
    gearInBar: !!(bar && gear && bar.children.includes(gear)),
  };
};
const save = async (field) => {
  mixesFetches = 0;
  if (typeof spec?.onSaved === "function") spec.onSaved(field);
  await settle();
};

const out = [];
server = { enabled: false, collections: [] };
await renderMixes(view, ctx);
out.push(snap("off at load"));

server = { enabled: true, collections: [mix], snapshotAt: "" };
await save("smartPlaylistsEnabled");
out.push(snap("the gear saved the switch on"));

await save("analysisEnabled");
out.push(snap("the gear saved audio analysis"));

server = { enabled: false, collections: [] };
await save("smartPlaylistsEnabled");
out.push(snap("the gear saved the switch off"));

server = { enabled: false, managed: true, collections: [] };
await save("smartPlaylistsEnabled");
out.push(snap("off, and the control plane owns the switch"));

gen++;
view.textContent = "another page";
server = { enabled: true, collections: [mix] };
await save("smartPlaylistsEnabled");
out.push(snap("a save that lands after the reader left"));
console.log(JSON.stringify(out));
`

// mixesViewStep is one step of mixesViewHarness: the view's text, the
// toolbar's buttons, how many trays were built so far, how many times the
// step fetched the mixes, and whether the gear is still in the bar.
type mixesViewStep struct {
	Step           string   `json:"step"`
	View           string   `json:"view"`
	ToolbarButtons []string `json:"toolbarButtons"`
	Builds         int      `json:"builds"`
	Fetches        int      `json:"fetches"`
	GearInBar      bool     `json:"gearInBar"`
}

// TestTheSmartMixesPageRedrawsInPlaceAfterItsGearSavesTheSwitch runs the
// shipped renderMixes under node and pins what a save in the page's own
// gear does to the page.
//
// Until 2026-09-28 the page read the switch from the page seed and passed
// the tray no onSaved, so a save left "Smart mixes are off" beside "Saved."
// (backlog B35, seen in a browser). Now a save of the Smart mixes switch
// redraws from /api/player/mixes, IN PLACE: the tray is built once and its
// gear stays in the bar, so the tray's "Saved." and the focus in it
// survive, which a whole-route redraw (the variant panel's) does not. A
// save of the other switch fetches nothing, "Regenerate all" is there only
// while the switch is on, the off state points at the gear only when the
// gear can turn it on, and a save that lands after the reader has moved on
// paints nothing.
func TestTheSmartMixesPageRedrawsInPlaceAfterItsGearSavesTheSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped player module")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "mixes.mjs")
	if err := os.WriteFile(script, []byte(mixesViewHarness), 0o600); err != nil {
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
	var steps []mixesViewStep
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(steps) != 6 {
		t.Fatalf("the harness reported %d steps, want 6: %s", len(steps), raw)
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if strings.Contains(x, s) {
				return true
			}
		}
		return false
	}
	offAtLoad, on, analysis, off, managed, left := steps[0], steps[1], steps[2], steps[3], steps[4], steps[5]

	if !strings.Contains(offAtLoad.View, "Smart mixes are off") || has(offAtLoad.ToolbarButtons, "Regenerate all") {
		t.Errorf("%s: the view says %q and the toolbar %q, want the off state and no Regenerate all",
			offAtLoad.Step, offAtLoad.View, offAtLoad.ToolbarButtons)
	}
	if !strings.Contains(on.View, "Heavy Rotation") || strings.Contains(on.View, "Smart mixes are off") {
		t.Errorf("%s: the view says %q, want the mix the server now lists", on.Step, on.View)
	}
	if on.Fetches != 1 || !has(on.ToolbarButtons, "Regenerate all") {
		t.Errorf("%s: %d fetches and toolbar %q, want one fetch and Regenerate all",
			on.Step, on.Fetches, on.ToolbarButtons)
	}
	for _, s := range steps[:5] {
		if s.Builds != 1 || !s.GearInBar {
			t.Errorf("%s: %d trays built and the gear in the bar=%v, want the one tray, kept, "+
				"so its Saved. and the focus survive", s.Step, s.Builds, s.GearInBar)
		}
	}
	if analysis.Fetches != 0 || !strings.Contains(analysis.View, "Heavy Rotation") {
		t.Errorf("%s: %d fetches and view %q, want no redraw", analysis.Step, analysis.Fetches, analysis.View)
	}
	if !strings.Contains(off.View, "Smart mixes are off") || !strings.Contains(off.View, "gear above") ||
		has(off.ToolbarButtons, "Regenerate all") {
		t.Errorf("%s: view %q, toolbar %q, want the off state pointing at the gear, no Regenerate all",
			off.Step, off.View, off.ToolbarButtons)
	}
	if !strings.Contains(managed.View, "Smart mixes are off") || strings.Contains(managed.View, "gear") {
		t.Errorf("%s: the view says %q; a gear that leaves the switch out must not be pointed at",
			managed.Step, managed.View)
	}
	if left.View != "another page" || left.Fetches != 0 {
		t.Errorf("%s: the view says %q after %d fetches, want the other page untouched",
			left.Step, left.View, left.Fetches)
	}
}
