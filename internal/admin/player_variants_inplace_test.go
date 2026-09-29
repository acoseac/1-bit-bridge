package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The variant panel's redraw after a tray save.
//
// A save in the panel's gear that changes what the panel draws re-ran the
// whole route (views.js's rerenderView): the tray, its "Saved." and the
// focus in it went with the old DOM, and the route moved focus to the page
// title (backlog B68; measured in a browser, Chrome 152: after turning PCM
// upscaling on from the Variants tab, document.activeElement was
// #player-title and the panel had no gear left). The Smart mixes page had
// been made to redraw in place (drawMixes). The panel now does the same:
// it is given a way to fetch a fresh summary, builds its gears and their
// trays once, and repaints around them.

// variantsInPlaceHarness imports the SHIPPED variants.js under node on a DOM
// that models what matters here: the browser's focus rules (removing a node,
// or moving it, takes the focus out of its subtree, and disabling the
// focused control does too) and which nodes are in the document. The tray is
// a stand-in for app.js's, which is a classic script this harness does not
// load: it records the spec and hands back a gear and a tray holding a
// switch and the "Saved." line a real save would have written, so a scenario
// can put focus in the switch and save. setDisabled is app.js's own,
// extracted, so a control that gives focus back is the shipped one.
const variantsInPlaceHarness = `
class Node {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.parentNode = null; this.attributes = {};
    this.className = ""; this.own = ""; this.style = {}; this.dataset = {}; this.listeners = {};
    this.hidden = false; this.isDisabled = false;
    const self = this;
    this.classList = {
      add(c) { self.className = (self.className + " " + c).trim(); },
      remove(c) { self.className = self.className.split(/\s+/).filter((x) => x && x !== c).join(" "); },
    };
  }
  get disabled() { return this.isDisabled; }
  set disabled(v) {
    this.isDisabled = Boolean(v);
    if (this.isDisabled && document.activeElement === this) document.activeElement = docBody;
  }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === docRoot; }
  contains(x) { for (let n = x; n; n = n.parentNode) if (n === this) return true; return false; }
  get textContent() { return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own; }
  set textContent(v) { for (const c of [...this.children]) this.removeChild(c); this.own = String(v); }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  removeAttribute(k) { delete this.attributes[k]; }
  get firstChild() { return this.children[0] || null; }
  removeChild(c) {
    if (document.activeElement && c.contains(document.activeElement)) document.activeElement = docBody;
    this.children = this.children.filter((x) => x !== c); c.parentNode = null; return c;
  }
  appendChild(c) { return this.insertBefore(c, null); }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  insertBefore(c, ref) {
    if (c.parentNode) c.parentNode.removeChild(c);
    const i = ref ? this.children.indexOf(ref) : -1;
    if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
    c.parentNode = this; return c;
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  async click() { for (const fn of this.listeners.click || []) await fn(); }
  focus() { if (!this.isDisabled && this.isConnected) document.activeElement = this; }
}
const docRoot = new Node("root");
const docBody = new Node("body");
docBody.parentNode = docRoot;
globalThis.document = {
  body: docBody,
  activeElement: docBody,
  createElement: (tag) => new Node(tag),
  createTextNode: (s) => { const n = new Node("#text"); n.own = String(s); return n; },
};
globalThis.window = {};
globalThis.confirm = () => true;
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };

const builds = [];
window.BridgeFeatureTray = {
  build(spec) {
    const button = new Node("button"); button.own = "gear";
    const tray = new Node("div"); tray.className = "feature-tray";
    const sw = new Node("input"); tray.appendChild(sw);
    const status = new Node("p"); status.own = "Saved."; tray.appendChild(status);
    builds.push({ spec, button, tray, sw, title: spec.title });
    return { button, tray };
  },
};

const { variantPanel } = await import(process.argv[2]);
const cov = (covered, eligible) => ({ covered, eligible, exempt: 0, stale: 0 });
const base = { soxAvailable: true, sourceBytes: 0, variantBytes: 0, upscale: cov(0, 2), optimize: cov(0, 2) };
const S = {
  off: { ...base, enabled: false, optimizeActive: false },
  carplayOff: { ...base, enabled: true, optimizeActive: false },
  on: { ...base, enabled: true, optimizeActive: true },
};

let changed = 0;
const has = (n, c) => (n.className || "").split(/\s+/).includes(c);
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
// What a reader sees of the panel: the notes with words in them above the
// kinds (the block that stops both) and in each kind's row, each kind's
// Generate state, which of the trays built are in the document and which
// switch, if any, holds focus.
function observe(root) {
  const notes = [];
  const rows = [];
  for (const child of root.children) {
    if (!has(child, "variant-kind")) {
      walk(child, (n) => { if (has(n, "variants-blocked") && n.textContent) notes.push(n.textContent); });
      continue;
    }
    const row = { title: "", generateDisabled: null, notes: [], ratio: "", barNow: "", stale: "", staleIn: false };
    walk(child, (m) => {
      if (has(m, "variant-kind-title")) row.title = m.textContent;
      if (has(m, "variant-kind-ratio")) row.ratio = m.textContent;
      if (has(m, "variant-bar")) row.barNow = m.getAttribute("aria-valuenow");
      if (has(m, "variant-kind-stale")) { row.stale = m.textContent; row.staleIn = true; }
      if (m.tagName === "button" && m.textContent.startsWith("Generate")) row.generateDisabled = m.disabled;
      if (has(m, "variants-blocked") && m.textContent) row.notes.push(m.textContent);
    });
    rows.push(row);
  }
  const at = document.activeElement;
  return {
    notes, rows,
    trays: builds.map((b) => ({ title: b.title, gearIn: root.contains(b.button), trayIn: root.contains(b.tray),
      status: b.tray.children[1].textContent })),
    focus: at === docBody ? "body" : (builds.findIndex((b) => b.sw === at) >= 0
      ? "switch of " + builds.find((b) => b.sw === at).title : "elsewhere"),
    changed,
  };
}
function make(summary, opts = {}) {
  builds.length = 0; changed = 0;
  docRoot.children = []; document.activeElement = docBody;
  const root = variantPanel(summary, opts.scope || { albumIds: ["a1"] }, () => { changed++; },
    { plain: true, refresh: opts.refresh, alive: opts.alive });
  docRoot.appendChild(root);
  return root;
}
// The reader's save: focus is in the tray's switch (saveTrayField gave it
// back), and the tray calls onSaved for the field.
const save = async (b, field) => { b.sw.focus(); b.spec.onSaved(field); await settle(); };

const out = {};

// The panel's own tray: upscaling switched on from it.
{
  let refreshes = 0;
  const answers = [S.carplayOff, S.on];
  const root = make(S.off, { refresh: async () => { refreshes++; return answers.shift(); } });
  const blocked = observe(root);
  builds[0].sw.focus();
  await save(builds[0], "upscaleEnabled");
  const afterUpscale = { ...observe(root), refreshes };
  await save(builds[0], "optimizeEnabled");
  const afterCarPlay = { ...observe(root), refreshes };
  out.panelTray = { blocked, afterUpscale, afterCarPlay };
}

// The CarPlay switch saved from the panel's tray while generation is off:
// nothing the panel draws depends on it, so nothing is fetched.
{
  let refreshes = 0;
  const root = make(S.off, { refresh: async () => { refreshes++; return S.off; } });
  await save(builds[0], "optimizeEnabled");
  out.gatedField = { refreshes, changed, ...observe(root) };
}

// The CarPlay kind's own tray.
{
  const root = make(S.carplayOff, { refresh: async () => S.on });
  const before = observe(root);
  await save(builds[0], "optimizeEnabled");
  out.kindTray = { before, after: observe(root) };
}

// Coverage, the bar and the note about stale copies follow the summary, on
// the nodes that are there.
{
  const st = (stale, covered) => ({ ...S.carplayOff, upscale: { covered, eligible: 2, exempt: 0, stale } });
  const answers = [st(2, 2), st(0, 1)];
  const root = make(st(1, 2), { refresh: async () => answers.shift() });
  const first = observe(root);
  await save(builds[0], "optimizeEnabled");
  const second = observe(root);
  await save(builds[0], "optimizeEnabled");
  out.coverage = { first, second, third: observe(root) };
}

// The route moves on while the answer is out.
{
  let release;
  const gone = { now: false };
  const root = make(S.carplayOff, { refresh: () => new Promise((r) => { release = () => r(S.on); }), alive: () => !gone.now });
  builds[0].sw.focus();
  builds[0].spec.onSaved("optimizeEnabled");
  gone.now = true;
  // Optional calls: a panel that never fetches leaves nothing to release, and
  // the run should still report what it drew.
  release?.();
  await settle();
  out.movedOn = observe(root);
}

// Two saves, and the newer one's answer lands first.
{
  const releases = [];
  const root = make(S.carplayOff, { refresh: () => new Promise((r) => { releases.push(r); }) });
  builds[0].sw.focus();
  builds[0].spec.onSaved("optimizeEnabled");
  builds[0].spec.onSaved("optimizeEnabled");
  releases[1]?.(S.on);
  await settle();
  releases[0]?.(S.carplayOff);
  await settle();
  out.overtaken = observe(root);
}

// The fetch fails, or is aborted, or answers nothing.
{
  let root = make(S.carplayOff, { refresh: async () => { throw new Error("boom"); } });
  await save(builds[0], "optimizeEnabled");
  out.fails = observe(root);
  root = make(S.carplayOff, { refresh: async () => { const e = new Error("aborted"); e.name = "AbortError"; throw e; } });
  await save(builds[0], "optimizeEnabled");
  out.aborted = observe(root);
  root = make(S.carplayOff, { refresh: async () => undefined });
  await save(builds[0], "optimizeEnabled");
  out.empty = observe(root);
}

// No way to fetch: the save hands over to the route's own re-render.
{
  const root = make(S.carplayOff);
  await save(builds[0], "optimizeEnabled");
  out.noRefresh = observe(root);
}

// A Delete that fails re-enables its button and gives focus back, with the
// shipped setDisabled behind the window handshake, and without it.
{
  const covered = { ...S.on, upscale: cov(1, 2) };
  const deleteOf = (root) => { let b = null; walk(root, (n) => { if (n.tagName === "button" && n.textContent === "Delete" && !b) b = n; }); return b; };
  globalThis.fetch = async () => ({ ok: false, status: 500, json: async () => ({ message: "boom" }) });
  window.BridgeControls = { setDisabled };
  let root = make(covered);
  let del = deleteOf(root);
  del.focus();
  const startedFocused = document.activeElement === del;
  await del.click();
  await settle();
  out.deleteFails = { startedFocused, disabled: del.disabled, focusOnDelete: document.activeElement === del, changed };
  window.BridgeControls = undefined;
  root = make(covered);
  del = deleteOf(root);
  del.focus();
  await del.click();
  await settle();
  out.deleteFailsWithoutApp = { disabled: del.disabled, focusOnDelete: document.activeElement === del };
}
console.log(JSON.stringify(out));
`

// inPlaceTray is one tray of the panel as the harness observed it.
type inPlaceTray struct {
	Title  string `json:"title"`
	GearIn bool   `json:"gearIn"`
	TrayIn bool   `json:"trayIn"`
	Status string `json:"status"`
}

// inPlaceRow is one kind row as the harness observed it.
type inPlaceRow struct {
	Title            string   `json:"title"`
	GenerateDisabled bool     `json:"generateDisabled"`
	Notes            []string `json:"notes"`
	Ratio            string   `json:"ratio"`
	BarNow           string   `json:"barNow"`
	Stale            string   `json:"stale"`
	StaleIn          bool     `json:"staleIn"` // whether the note is in the document at all
}

// inPlaceView is the panel at one step: what a reader sees, the trays and
// where focus is, and how many times the whole-route callback ran.
type inPlaceView struct {
	Notes     []string      `json:"notes"`
	Rows      []inPlaceRow  `json:"rows"`
	Trays     []inPlaceTray `json:"trays"`
	Focus     string        `json:"focus"`
	Changed   int           `json:"changed"`
	Refreshes int           `json:"refreshes"`
}

// row finds the kind row with the title.
func (v inPlaceView) row(t *testing.T, title string) inPlaceRow {
	t.Helper()
	for _, r := range v.Rows {
		if r.Title == title {
			return r
		}
	}
	t.Fatalf("no %q row in %+v", title, v.Rows)
	return inPlaceRow{}
}

// TestAVariantTraySaveRedrawsThePanelInPlace runs the shipped variantPanel
// under node and pins what a save in the panel's gear does to the panel, the
// gear and the focus.
//
// The panel repaints from a fresh summary and leaves the gears and their
// trays where they are: they are still in the document with their "Saved.",
// the switch the reader toggled still holds focus (a node that is removed or
// moved takes the focus out with it, so a repaint that rebuilt them would
// fail here), and the whole-route callback that used to run does not. The
// panel's tray stays after generation comes on, so it answers for the
// CarPlay switch too then, and a save it makes for that switch while
// generation is off fetches nothing. A redraw refuses to paint once the route
// has moved on, or once a newer redraw has been started, and when the fetch
// fails the panel hands over to the route's re-render (an abort is not a
// failure). Without a fetch it does as it always did.
func TestAVariantTraySaveRedrawsThePanelInPlace(t *testing.T) {
	out := runVariantsInPlaceHarness(t)
	checkPanelTraySave(t, out)
	checkGatedField(t, out)
	checkCoverageFollowsTheSummary(t, out)
	checkKindTraySave(t, out)
	checkRedrawsThatPaintNothing(t, out)
	checkFailedDelete(t, out)
}

// inPlaceOutcome is what the harness printed: the panel at each step of each
// scenario it ran.
type inPlaceOutcome struct {
	PanelTray struct {
		Blocked      inPlaceView `json:"blocked"`
		AfterUpscale inPlaceView `json:"afterUpscale"`
		AfterCarPlay inPlaceView `json:"afterCarPlay"`
	} `json:"panelTray"`
	GatedField inPlaceView `json:"gatedField"`
	Coverage   struct {
		First  inPlaceView `json:"first"`
		Second inPlaceView `json:"second"`
		Third  inPlaceView `json:"third"`
	} `json:"coverage"`
	KindTray struct {
		Before inPlaceView `json:"before"`
		After  inPlaceView `json:"after"`
	} `json:"kindTray"`
	MovedOn    inPlaceView `json:"movedOn"`
	Overtaken  inPlaceView `json:"overtaken"`
	Fails      inPlaceView `json:"fails"`
	Aborted    inPlaceView `json:"aborted"`
	Empty      inPlaceView `json:"empty"`
	NoRefresh  inPlaceView `json:"noRefresh"`
	DeleteFail struct {
		StartedFocused bool `json:"startedFocused"`
		Disabled       bool `json:"disabled"`
		FocusOnDelete  bool `json:"focusOnDelete"`
		Changed        int  `json:"changed"`
	} `json:"deleteFails"`
	DeleteFailNoApp struct {
		Disabled      bool `json:"disabled"`
		FocusOnDelete bool `json:"focusOnDelete"`
	} `json:"deleteFailsWithoutApp"`
}

// The two switches the harness's tray stand-ins put focus in, as observe
// names them.
const (
	inPlacePanelSwitch = "switch of Variant generation"
	inPlaceKindSwitch  = "switch of CarPlay-optimized variants"
)

// runVariantsInPlaceHarness runs variantsInPlaceHarness against the shipped
// variants.js, with app.js's own setDisabled, and returns what it printed. It
// skips the test where node is not installed.
func runVariantsInPlaceHarness(t *testing.T) inPlaceOutcome {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped panel")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "inplace.mjs")
	app := readFile(t, "static/app.js")
	if err := os.WriteFile(script, []byte(extractJSFunction(t, app, "setDisabled")+"\n"+variantsInPlaceHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Join("static", "player", "variants.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script, fileURL(module)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var out inPlaceOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	return out
}

// checkPanelTraySave checks the panel's own tray: generation off, the reader
// turns PCM upscaling on from its gear, and then the CarPlay switch.
func checkPanelTraySave(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	b := out.PanelTray.Blocked
	if len(b.Notes) != 1 || len(b.Trays) != 1 || !b.row(t, "Hi-res upscale").GenerateDisabled {
		t.Fatalf("blocked: notes %q, trays %+v, want one note, one tray and a disabled Generate", b.Notes, b.Trays)
	}
	checkAfterUpscalingFromThePanel(t, out.PanelTray.AfterUpscale)
	checkAfterCarPlayFromThePanel(t, out.PanelTray.AfterCarPlay)
}

// checkAfterUpscalingFromThePanel checks the panel once PCM upscaling has
// been saved from its gear: one fetch, no whole-route callback, focus and the
// gear kept, the block lifted, and the CarPlay row saying for itself that its
// switch is off, with a tray of its own.
func checkAfterUpscalingFromThePanel(t *testing.T, up inPlaceView) {
	t.Helper()
	if up.Refreshes != 1 || up.Changed != 0 {
		t.Errorf("upscaling on: %d fetches and %d whole-route callbacks, want one fetch and none", up.Refreshes, up.Changed)
	}
	if up.Focus != inPlacePanelSwitch {
		t.Errorf("upscaling on: focus is %q, want it kept on the switch the reader toggled (%q)", up.Focus, inPlacePanelSwitch)
	}
	if len(up.Trays) != 2 || !up.Trays[0].GearIn || !up.Trays[0].TrayIn || up.Trays[0].Status != "Saved." {
		t.Errorf("upscaling on: trays %+v, want the panel's gear and tray still in the document with their Saved.", up.Trays)
	}
	if hiRes := up.row(t, "Hi-res upscale"); hiRes.GenerateDisabled || len(up.Notes) != 0 {
		t.Errorf("upscaling on: the hi-res Generate is disabled=%v and the panel says %q, want the block lifted",
			hiRes.GenerateDisabled, up.Notes)
	}
	// The redrawn panel reads the CarPlay switch from the server: still off, so
	// the CarPlay row says so itself, with a tray of its own.
	if carPlay := up.row(t, "CarPlay-optimized"); !carPlay.GenerateDisabled || len(carPlay.Notes) != 1 ||
		len(up.Trays) != 2 || up.Trays[1].Title != "CarPlay-optimized variants" || !up.Trays[1].GearIn {
		t.Errorf("upscaling on: the CarPlay row has Generate disabled=%v, notes %q and trays %+v, want its own note "+
			"and gear", carPlay.GenerateDisabled, carPlay.Notes, up.Trays)
	}
}

// checkAfterCarPlayFromThePanel checks the panel once the CarPlay switch has
// been saved from the panel's own tray, which stays and answers for it once
// generation is on.
func checkAfterCarPlayFromThePanel(t *testing.T, cp inPlaceView) {
	t.Helper()
	if cp.Refreshes != 2 || cp.Changed != 0 || cp.Focus != inPlacePanelSwitch {
		t.Errorf("CarPlay on from the panel's tray: %d fetches, %d whole-route callbacks, focus %q; want a second "+
			"fetch, none, and focus kept", cp.Refreshes, cp.Changed, cp.Focus)
	}
	if carPlay := cp.row(t, "CarPlay-optimized"); carPlay.GenerateDisabled || len(carPlay.Notes) != 0 {
		t.Errorf("CarPlay on: Generate disabled=%v and notes %q, want a live row", carPlay.GenerateDisabled, carPlay.Notes)
	}
	if len(cp.Trays) == 0 || !cp.Trays[0].GearIn || !cp.Trays[0].TrayIn {
		t.Errorf("CarPlay on: the panel's tray left the document: %+v", cp.Trays)
	}
}

// checkGatedField checks that the CarPlay switch saved from the panel's tray
// while generation is off decides nothing the panel draws, so nothing is
// fetched.
func checkGatedField(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	if g := out.GatedField; g.Refreshes != 0 || g.Changed != 0 {
		t.Errorf("the CarPlay switch saved while generation is off: %d fetches, %d whole-route callbacks, want none",
			g.Refreshes, g.Changed)
	}
}

// checkCoverageFollowsTheSummary checks what a redraw does to a kind's numbers:
// the ratio, the bar's value and the note about copies whose source changed
// follow the summary, the note appearing, changing its count and going as the
// stale count does.
func checkCoverageFollowsTheSummary(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	steps := []struct {
		name  string
		view  inPlaceView
		ratio string
		bar   string
		stale string
	}{
		{"as drawn", out.Coverage.First, "2 / 2", "2", "1 copy is out of date"},
		{"one save later", out.Coverage.Second, "2 / 2", "2", "2 copies are out of date"},
		{"two saves later", out.Coverage.Third, "1 / 2", "1", ""},
	}
	for _, st := range steps {
		row := st.view.row(t, "Hi-res upscale")
		if row.Ratio != st.ratio || row.BarNow != st.bar {
			t.Errorf("%s: the hi-res row reads %q with the bar at %q, want %q and %q",
				st.name, row.Ratio, row.BarNow, st.ratio, st.bar)
		}
		if !strings.HasPrefix(row.Stale, st.stale) || row.StaleIn != (st.stale != "") {
			t.Errorf("%s: the stale-copy note reads %q (in the document: %v), want it to start %q and to be in the "+
				"document only when that is not empty", st.name, row.Stale, row.StaleIn, st.stale)
		}
	}
}

// checkKindTraySave checks the CarPlay kind's own tray: built for a row whose
// switch is off, and staying, with its focus and its Saved., once the row is
// live.
func checkKindTraySave(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	before := out.KindTray.Before
	if len(before.Trays) != 1 || len(before.row(t, "CarPlay-optimized").Notes) != 1 {
		t.Fatalf("carplay off: trays %+v and notes %q, want the kind's tray and its note",
			before.Trays, before.row(t, "CarPlay-optimized").Notes)
	}
	k := out.KindTray.After
	if k.Focus != inPlaceKindSwitch || k.Changed != 0 || len(k.Trays) != 1 || !k.Trays[0].GearIn || !k.Trays[0].TrayIn ||
		k.Trays[0].Status != "Saved." {
		t.Errorf("the CarPlay kind's tray saved: focus %q, %d whole-route callbacks, trays %+v; want focus kept, "+
			"none, and the gear and tray still there with their Saved.", k.Focus, k.Changed, k.Trays)
	}
	if carPlay := k.row(t, "CarPlay-optimized"); carPlay.GenerateDisabled || len(carPlay.Notes) != 0 {
		t.Errorf("the CarPlay kind's tray saved: Generate disabled=%v and notes %q, want a live row",
			carPlay.GenerateDisabled, carPlay.Notes)
	}
}

// checkRedrawsThatPaintNothing checks the redraws that leave the panel as it
// was or hand over to the route: refused once the route has moved on,
// overtaken by a newer one, failed, aborted, answering nothing, and made by a
// panel with no fetch to redraw from.
func checkRedrawsThatPaintNothing(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	if m := out.MovedOn; m.Changed != 0 || len(m.row(t, "CarPlay-optimized").Notes) != 1 {
		t.Errorf("an answer after the route moved on: %d whole-route callbacks and CarPlay notes %q, want none "+
			"and the panel untouched", m.Changed, m.row(t, "CarPlay-optimized").Notes)
	}
	if o := out.Overtaken; len(o.row(t, "CarPlay-optimized").Notes) != 0 || o.Changed != 0 {
		t.Errorf("the older answer landed last: CarPlay notes %q, %d whole-route callbacks; want the newer answer's "+
			"state and none", o.row(t, "CarPlay-optimized").Notes, o.Changed)
	}
	if f := out.Fails; f.Changed != 1 {
		t.Errorf("a failed fetch ran the whole-route callback %d times, want once", f.Changed)
	}
	if a := out.Aborted; a.Changed != 0 {
		t.Errorf("an aborted fetch ran the whole-route callback %d times, want none: an abort is a navigation", a.Changed)
	}
	if e := out.Empty; e.Changed != 0 || len(e.row(t, "CarPlay-optimized").Notes) != 1 {
		t.Errorf("an answer with no summary: %d whole-route callbacks and CarPlay notes %q, want none and the panel kept",
			e.Changed, e.row(t, "CarPlay-optimized").Notes)
	}
	if n := out.NoRefresh; n.Changed != 1 {
		t.Errorf("a save with no fetch to redraw from ran the whole-route callback %d times, want once", n.Changed)
	}
}

// checkFailedDelete checks that a failed Delete gives focus back through the
// shipped helper, and only there.
func checkFailedDelete(t *testing.T, out inPlaceOutcome) {
	t.Helper()
	d := out.DeleteFail
	if !d.StartedFocused || d.Disabled || !d.FocusOnDelete || d.Changed != 0 {
		t.Errorf("a failed Delete: focused before=%v, disabled after=%v, focus back=%v, whole-route callbacks %d; "+
			"want it enabled with focus back and no redraw", d.StartedFocused, d.Disabled, d.FocusOnDelete, d.Changed)
	}
	if n := out.DeleteFailNoApp; n.Disabled || n.FocusOnDelete {
		t.Errorf("a failed Delete on a page without app.js: disabled=%v, focus back=%v; want it enabled, focus left "+
			"on the body (the fallback is a plain assignment)", n.Disabled, n.FocusOnDelete)
	}
}
