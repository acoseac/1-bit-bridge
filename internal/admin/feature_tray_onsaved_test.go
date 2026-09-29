package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trayHarnessPreamble stands in for what app.js's tray code reads from the
// rest of the console: the DOM, the fetch wrapper, the page scope and the
// restart latch. The tray's own functions are extracted from app.js
// verbatim and appended after it (trayHarnessFunctions).
//
// Elements keep their children, class, text, dataset, listeners and the
// checked and disabled state a switch reads and writes. dispatch awaits
// each listener, so a change handler's save has finished when it returns.
// The settings snapshot holds every field off, so checking a switch is a
// change the PATCH stand-in then answers as the case says.
//
// Focus is modelled as a browser applies the focus fixup rule: a focused
// control that becomes disabled loses focus to the body, and enabling it
// again does not give focus back (measured on Chrome 152: a focused
// checkbox reads document.activeElement as the body after disabled = true,
// and still after disabled = false). focus() moves focus to an enabled
// element; focusDuringSave, when set, takes focus while the PATCH is out.
const trayHarnessPreamble = `
class El {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.attributes = {}; this.dataset = {};
    this.listeners = {}; this.className = ""; this.own = ""; this.hidden = false;
    this.isDisabled = false; this.checked = false; this.value = ""; this.isConnected = true;
  }
  get disabled() { return this.isDisabled; }
  set disabled(v) {
    this.isDisabled = Boolean(v);
    if (this.isDisabled && document.activeElement === this) document.activeElement = document.body;
  }
  get textContent() { return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own; }
  set textContent(v) { this.children = []; this.own = String(v); }
  set innerHTML(v) { this.children = []; this.own = ""; }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  appendChild(c) { this.children.push(c); return c; }
  append(...cs) { for (const c of cs) this.appendChild(c); }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  async dispatch(type) { for (const fn of this.listeners[type] || []) await fn(); }
  querySelector() { return null; }
  focus() { if (!this.isDisabled && this.isConnected) document.activeElement = this; }
}
globalThis.document = { createElement: (tag) => new El(tag) };
document.body = new El("body");
document.activeElement = document.body;

let traySeq = 0;
let traySettings = null;
let traySettingsPromise = null;
let trayManaged = null;
const mountedTrays = new Set();
let patchAnswer = null;
let patches = [];
let focusDuringSave = null;
let settingsAnswer = { optimizeEnabled: false, upscaleEnabled: false };
const API = {
  // A fresh copy each time: a save writes into the snapshot it was given.
  get: async () => JSON.parse(JSON.stringify(settingsAnswer)),
  patch: async (url, body) => {
    patches.push(body);
    if (focusDuringSave) focusDuringSave.focus();
    if (patchAnswer instanceof Error) throw patchAnswer;
    return patchAnswer;
  },
};
function pageSignal() { return new AbortController().signal; }
function markRestartPending() {}
`

// trayHarnessFunctions are the app.js functions a tray runs from its build
// to the end of a save, extracted by name so the harness runs the shipped
// code rather than a copy of it.
var trayHarnessFunctions = []string{
	"escapeHTML", "pruneDetachedTrays", "traySettingsSnapshot", "trayFieldManaged",
	"buildFeatureTray", "buildTrayRow", "trayControlFor", "trayLabelFor", "trayValueOf",
	"trayApplyValue", "syncTray", "applyTrayManaged", "applyStatusFor", "saveTrayField", "setDisabled",
}

// trayHarnessRun builds one tray per case with the shipped buildFeatureTray,
// switches its one switch on, and reports what the save left behind: every
// call of the spec's onSaved with what the tray showed at that moment, the
// status line, the snapshot's value, whether the save's promise rejected,
// and where focus ended: on the switch, on the body, or on the element the
// case moved it to while the PATCH was out.
const trayHarnessRun = `
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const c of cases) {
  traySettings = null;
  traySettingsPromise = null;
  mountedTrays.clear();
  document.activeElement = document.body;
  const elsewhere = document.createElement("button");
  focusDuringSave = c.focus === "elsewhere" ? elsewhere : null;
  patchAnswer = c.error ? new Error(c.error) : c.answer;
  const calls = [];
  let status = null;
  let input = null;
  const spec = { title: "Tray", rows: [{ field: c.field, type: "switch", label: c.field }] };
  if (c.hook === "record") {
    spec.onSaved = (field) => calls.push({
      field, stored: traySettings?.[field] ?? null, status: status.textContent, disabled: input.disabled,
    });
  } else if (c.hook === "throw") {
    spec.onSaved = () => { throw new Error("the redraw failed"); };
  }
  const { tray } = buildFeatureTray(spec);
  walk(tray, (n) => {
    if (n.className === "tray-status") status = n;
    if (n.dataset.field === c.field) input = n;
  });
  // The mount-time snapshot lands, and syncTray enables the switch.
  await new Promise((r) => setTimeout(r, 0));
  const wasDisabled = input.disabled;
  if (c.focus) input.focus();
  const focusedBefore = document.activeElement === input;
  input.checked = true;
  let rejected = "";
  try { await input.dispatch("change"); } catch (e) { rejected = e.message; }
  const at = document.activeElement;
  out.push({
    name: c.name, calls, status: status.textContent, stored: traySettings?.[c.field] ?? null,
    rejected, wasDisabled, focusedBefore,
    focus: at === input ? "switch" : at === elsewhere ? "elsewhere" : at === document.body ? "body" : "other",
  });
}
console.log(JSON.stringify(out));
`

// traySaveCase is one save the harness makes through a tray: the field its
// switch saves, what the PATCH answers (or the error it fails with), the
// spec's onSaved, "record" or "throw", or none when empty, and where focus
// is: on the switch when the change is made ("switch"), taken by another
// element while the PATCH is out ("elsewhere"), or never on the switch
// (empty). The unexported fields are the expectation, which the harness
// never sees.
type traySaveCase struct {
	Name   string         `json:"name"`
	Field  string         `json:"field"`
	Answer map[string]any `json:"answer,omitempty"`
	Error  string         `json:"error,omitempty"`
	Hook   string         `json:"hook"`
	Focus  string         `json:"focus,omitempty"`

	wantCall   bool   // whether onSaved is called, once, with Field
	wantStatus string // the tray's status line afterwards, as a prefix
	wantStored bool   // the snapshot's value for Field afterwards
	wantFocus  string // where focus is once the save is over
}

// traySaveCall is one call of a spec's onSaved, with what the tray showed
// when it was made.
type traySaveCall struct {
	Field    string `json:"field"`
	Stored   any    `json:"stored"`
	Status   string `json:"status"`
	Disabled bool   `json:"disabled"`
}

// traySaveResult is what one case's save left behind.
type traySaveResult struct {
	Name          string         `json:"name"`
	Calls         []traySaveCall `json:"calls"`
	Status        string         `json:"status"`
	Stored        any            `json:"stored"`
	Rejected      string         `json:"rejected"`
	WasDisabled   bool           `json:"wasDisabled"`
	FocusedBefore bool           `json:"focusedBefore"`
	Focus         string         `json:"focus"`
}

// runTraySavesUnderNode runs trayHarnessRun over the cases, with the tray
// functions extracted from the shipped app.js, and returns the results in
// order.
func runTraySavesUnderNode(t *testing.T, node string, cases []traySaveCase) []traySaveResult {
	t.Helper()
	src := readFile(t, "static/app.js")
	var script strings.Builder
	script.WriteString(trayHarnessPreamble)
	for _, name := range trayHarnessFunctions {
		script.WriteString(extractJSFunction(t, src, name))
		script.WriteString("\n")
	}
	script.WriteString(trayHarnessRun)
	dir := t.TempDir()
	path := filepath.Join(dir, "tray.mjs")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
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
	raw, err := exec.Command(node, path, casesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var results []traySaveResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness reported %d saves for %d cases", len(results), len(cases))
	}
	return results
}

// TestATrayCallsOnSavedOnlyAfterASaveTheServerAppliedLive runs the shipped
// buildFeatureTray and its save under node, and pins when a tray calls its
// spec's onSaved.
//
// A tray saves one switch and redraws nothing else, so a page whose drawing
// depends on that switch went on showing the old state beside the tray's
// "Saved.": the variant panel's CarPlay row said "switched off" until the
// next render (CodeRabbit on #1068). onSaved is how the page learns to
// redraw. It is called with the field, once, for a save the server applied
// live, and for nothing else: a "restart" or "unchanged" answer has moved
// nothing on the page, and a redraw would take away the tray and the
// restart instruction in it. It runs once the tray has recorded the save
// (the snapshot holds the new value, the status says "Saved.", the switch
// is enabled again), and a callback that throws leaves the save reported
// as saved, since the save landed.
func TestATrayCallsOnSavedOnlyAfterASaveTheServerAppliedLive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	answer := func(field, status, reason string) map[string]any {
		entry := map[string]any{"status": status}
		if reason != "" {
			entry["reason"] = reason
		}
		return map[string]any{"restartRequired": status == "restart", "fields": map[string]any{field: entry}}
	}
	const optimize, upscale = "optimizeEnabled", "upscaleEnabled"
	cases := []traySaveCase{
		{Name: "live", Field: optimize, Answer: answer(optimize, "live", ""), Hook: "record",
			wantCall: true, wantStatus: "Saved.", wantStored: true},
		// upscaleEnabled answers live with a reason when sox is unusable,
		// and the variant panel still has to redraw: its block changes.
		{Name: "live with a reason", Field: upscale,
			Answer: answer(upscale, "live", "sox is not usable on this host"), Hook: "record",
			wantCall: true, wantStatus: "Saved.", wantStored: true},
		{Name: "restart", Field: optimize, Answer: answer(optimize, "restart", ""), Hook: "record",
			wantStatus: "Saved — restart", wantStored: true},
		{Name: "unchanged", Field: optimize, Answer: answer(optimize, "unchanged", ""), Hook: "record",
			wantStatus: "Already set.", wantStored: true},
		{Name: "refused", Field: optimize, Error: "HTTP 400: validate", Hook: "record",
			wantStatus: "Save failed: HTTP 400: validate", wantStored: false},
		{Name: "no hook", Field: optimize, Answer: answer(optimize, "live", ""),
			wantStatus: "Saved.", wantStored: true},
		// The save landed, so a redraw that fails must not report it as failed.
		{Name: "a hook that throws", Field: optimize, Answer: answer(optimize, "live", ""), Hook: "throw",
			wantStatus: "Saved.", wantStored: true},
	}
	results := runTraySavesUnderNode(t, node, cases)

	for i, r := range results {
		c := cases[i]
		if r.WasDisabled {
			t.Fatalf("%s: the switch was still disabled when the harness changed it, so the "+
				"settings snapshot never reached the tray and the save measured nothing", r.Name)
		}
		if !strings.HasPrefix(r.Status, c.wantStatus) || r.Stored != c.wantStored {
			t.Errorf("%s: the status says %q and the snapshot holds %v, want %q… and %v",
				r.Name, r.Status, r.Stored, c.wantStatus, c.wantStored)
		}
		switch {
		case c.Hook == "throw" && r.Rejected != "the redraw failed":
			t.Errorf("%s: the save's promise rejected with %q, want the callback's own error, "+
				"which is how this case knows the callback ran", r.Name, r.Rejected)
		case c.Hook != "throw" && r.Rejected != "":
			t.Errorf("%s: the save's promise rejected with %q", r.Name, r.Rejected)
		}
		wantCalls := 0
		if c.wantCall {
			wantCalls = 1
		}
		if len(r.Calls) != wantCalls {
			t.Errorf("%s: onSaved was called %d times, want %d: %+v", r.Name, len(r.Calls), wantCalls, r.Calls)
			continue
		}
		for _, call := range r.Calls {
			if call.Field != c.Field {
				t.Errorf("%s: onSaved was called with %q, want the saved field %q", r.Name, call.Field, c.Field)
			}
			if call.Stored != true || call.Status != "Saved." || call.Disabled {
				t.Errorf("%s: onSaved ran before the tray had recorded the save: the snapshot held %v, "+
					"the status said %q and the switch was disabled=%v", r.Name, call.Stored, call.Status, call.Disabled)
			}
		}
	}
}

// TestATraySaveGivesFocusBackToItsSwitch runs the shipped tray save under
// node and pins where focus is once a save is over.
//
// A save disables its switch while the PATCH is out, and a browser moves
// focus off a focused control that becomes disabled (the focus fixup rule)
// and does not give it back when the control is enabled again. So every
// tray save left a keyboard user's focus on the body, whatever the answer:
// seen in a browser on the Smart mixes page (Chrome 152) for the switch
// whose save redraws the page and for one whose save redraws nothing. The
// save gives focus back to its switch, unless something else took focus
// while it was out, and never takes focus the switch did not have.
func TestATraySaveGivesFocusBackToItsSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	const optimize = "optimizeEnabled"
	answer := func(status string) map[string]any {
		return map[string]any{"restartRequired": status == "restart",
			"fields": map[string]any{optimize: map[string]any{"status": status}}}
	}
	cases := []traySaveCase{
		{Name: "live", Field: optimize, Answer: answer("live"), Hook: "record", Focus: "switch",
			wantFocus: "switch"},
		{Name: "restart", Field: optimize, Answer: answer("restart"), Focus: "switch", wantFocus: "switch"},
		{Name: "unchanged", Field: optimize, Answer: answer("unchanged"), Focus: "switch", wantFocus: "switch"},
		{Name: "refused", Field: optimize, Error: "HTTP 400: validate", Focus: "switch", wantFocus: "switch"},
		// The reader moved on while the save was out: the save must not
		// pull focus back from where they went.
		{Name: "focus taken during the save", Field: optimize, Answer: answer("live"), Focus: "elsewhere",
			wantFocus: "elsewhere"},
		// The switch never had focus (a pointer on a platform that does not
		// focus a checkbox on click): the save takes none.
		{Name: "never focused", Field: optimize, Answer: answer("live"), wantFocus: "body"},
	}
	results := runTraySavesUnderNode(t, node, cases)
	for i, r := range results {
		c := cases[i]
		if r.WasDisabled {
			t.Fatalf("%s: the switch was still disabled when the harness changed it, so the "+
				"save measured nothing", r.Name)
		}
		if c.Focus != "" && !r.FocusedBefore {
			t.Fatalf("%s: the switch did not take focus before the change, so the harness's "+
				"focus model measures nothing", r.Name)
		}
		if r.Focus != c.wantFocus {
			t.Errorf("%s: focus ended on %q after the save, want %q", r.Name, r.Focus, c.wantFocus)
		}
	}
}
