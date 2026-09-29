package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The focus a control keeps when it disables itself around a request.
//
// A browser moves focus off a focused control that becomes disabled (the
// focus fixup rule) and does not give it back when the control is enabled
// again: measured on Chrome 152, the Jobs page's "Scan now" read
// document.activeElement as the body after its click handler disabled it,
// and still as the body once it was enabled again, so a keyboard or
// screen-reader user lost their place after every "Scan now", "Retry
// missing" and the like (backlog B68). saveTrayField gave focus back for a
// tray's switch alone; setDisabled is that rule for every other control.

// setDisabledHarness models the focus fixup rule in a small DOM (a focused
// control that becomes disabled loses focus to the body, and enabling it
// again gives none back), runs the SHIPPED setDisabled from app.js over a
// script of steps per case, and prints where focus is after each one.
//
// A control is "connected" until a step replaces it, as a redraw does.
const setDisabledHarness = `
class El {
  constructor(name) { this.name = name; this.isDisabled = false; this.isConnected = true; this.dataset = {}; }
  get disabled() { return this.isDisabled; }
  set disabled(v) {
    this.isDisabled = Boolean(v);
    if (this.isDisabled && document.activeElement === this) document.activeElement = document.body;
  }
  focus() { if (!this.isDisabled && this.isConnected) document.activeElement = this; }
}
globalThis.document = {};
document.body = new El("body");
document.activeElement = document.body;
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const where = (btn, other) => {
  const at = document.activeElement;
  return at === btn ? "control" : at === other ? "other" : at === document.body ? "body" : "?";
};
const out = [];
for (const c of cases) {
  const btn = new El("btn"), other = new El("other");
  document.activeElement = document.body;
  const seen = [];
  for (const step of c.steps) {
    switch (step) {
      case "focus": btn.focus(); break;
      case "focus-other": other.focus(); break;
      case "blur": document.activeElement = document.body; break;
      case "disable": setDisabled(btn, true); break;
      case "enable": setDisabled(btn, false); break;
      case "replace": btn.isConnected = false; break;
      default: throw new Error("unknown step " + step);
    }
    seen.push(where(btn, other));
  }
  out.push({ name: c.name, seen, disabled: btn.disabled });
}
console.log(JSON.stringify(out));
`

// TestSetDisabledGivesFocusBackToAControlThatHadIt runs the shipped
// setDisabled under node and pins where focus is at every step of a
// disable and the enable that ends it.
//
// It gives focus back to a control that had it, so a request does not cost a
// keyboard user their place; it takes none the control did not have (Safari
// does not focus a button on a click), none from what the reader moved to
// while the request was out, and none for a control a redraw replaced.
func TestSetDisabledGivesFocusBackToAControlThatHadIt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type setDisabledCase struct {
		Name  string   `json:"name"`
		Steps []string `json:"steps"`
		want  []string // where focus is after each step
	}
	cases := []setDisabledCase{
		// The reported shape: focus is lost at the disable, as the browser
		// does it, and comes back at the enable.
		{Name: "focused, disabled, enabled", Steps: []string{"focus", "disable", "enable"},
			want: []string{"control", "body", "control"}},
		// Never focused: a pointer click on a platform that focuses no
		// button. The enable must not take focus the control never had.
		{Name: "never focused", Steps: []string{"disable", "enable"},
			want: []string{"body", "body"}},
		// The reader tabbed away while the request was out.
		{Name: "focus taken meanwhile", Steps: []string{"focus", "disable", "focus-other", "enable"},
			want: []string{"control", "body", "other", "other"}},
		// A redraw replaced the control: there is nothing to focus.
		{Name: "replaced meanwhile", Steps: []string{"focus", "disable", "replace", "enable"},
			want: []string{"control", "body", "body", "body"}},
		// A second disable of a control that is already disabled sees the
		// body and must not forget that the control had focus.
		{Name: "disabled twice", Steps: []string{"focus", "disable", "disable", "enable"},
			want: []string{"control", "body", "body", "control"}},
		// The focus is owed once per disable: a control the reader has since
		// left (the body, on purpose) is not pulled back to by a later request.
		{Name: "owed once", Steps: []string{"focus", "disable", "enable", "blur", "disable", "enable"},
			want: []string{"control", "body", "control", "body", "body", "body"}},
		// And a control that got focus back is owed it again when it is next
		// disabled, since it holds focus again.
		{Name: "owed again", Steps: []string{"focus", "disable", "enable", "disable", "enable"},
			want: []string{"control", "body", "control", "body", "control"}},
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "set_disabled.mjs")
	src := readFile(t, "static/app.js")
	if err := os.WriteFile(script, []byte(extractJSFunction(t, src, "setDisabled")+"\n"+setDisabledHarness), 0o600); err != nil {
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
	raw, err := exec.Command(node, script, casesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var results []struct {
		Name     string   `json:"name"`
		Seen     []string `json:"seen"`
		Disabled bool     `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness ran %d cases for %d", len(results), len(cases))
	}
	for i, r := range results {
		c := cases[i]
		if strings.Join(r.Seen, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: focus after each of %v was %v, want %v", c.Name, c.Steps, r.Seen, c.want)
		}
		if r.Disabled {
			t.Errorf("%s: the control is still disabled after the last enable", c.Name)
		}
	}
}

// jobButtonHarness runs the SHIPPED wireJobButton (and setDisabled) under
// node, with the focus fixup rule in the DOM and the 4 s timer that
// re-enables the button under the harness's hand: the click's handler runs
// to its end, focus is read, the timer is fired, focus is read again.
const jobButtonHarness = `
class El {
  constructor(id) {
    this.id = id; this.isDisabled = false; this.isConnected = true; this.dataset = {};
    this.listeners = {}; this.text = "";
  }
  get disabled() { return this.isDisabled; }
  set disabled(v) {
    this.isDisabled = Boolean(v);
    if (this.isDisabled && document.activeElement === this) document.activeElement = document.body;
  }
  get textContent() { return this.text; }
  set textContent(v) { this.text = String(v); }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  async click() { for (const fn of this.listeners.click || []) await fn(); }
  focus() { if (!this.isDisabled && this.isConnected) document.activeElement = this; }
}
const byId = new Map();
globalThis.document = { getElementById: (id) => byId.get(id) || null };
document.body = new El("body");
document.activeElement = document.body;
const timers = [];
globalThis.setTimeout = (fn) => { timers.push(fn); return timers.length; };
console.warn = () => {};
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const c of cases) {
  const btn = new El("jobs-scan-now"), other = new El("other");
  btn.text = "Scan now";
  byId.set("jobs-scan-now", btn);
  document.activeElement = document.body;
  timers.length = 0;
  wireJobButton("jobs-scan-now", async () => { if (c.fails) throw new Error("boom"); return "started"; }, "Done");
  if (c.focus) btn.focus();
  await btn.click();
  const at = () => document.activeElement === btn ? "button" : document.activeElement === other ? "other" : document.activeElement === document.body ? "body" : "?";
  const during = { focus: at(), disabled: btn.disabled, text: btn.textContent };
  if (c.moveTo === "other") other.focus();
  for (const fn of timers.splice(0)) fn();
  out.push({ name: c.name, during, after: { focus: at(), disabled: btn.disabled, text: btn.textContent } });
}
console.log(JSON.stringify(out));
`

// TestAJobButtonGivesFocusBackAfterItsRequest runs the shipped
// wireJobButton under node and pins where focus is once the button is
// enabled again.
//
// wireJobButton is the one function behind the Jobs page's Scan now,
// Analyze now, Sweep now, Snapshot now, Regenerate and Check buttons, and
// the button re-enables four seconds after its request ends, so a keyboard
// user pressing Enter on it was left on the body (seen in a browser, Chrome
// 152, on "Scan now"). Now the button takes focus back when it is enabled,
// after a request that answered and after one that failed, unless the
// reader moved on meanwhile, and never for a button that had none.
func TestAJobButtonGivesFocusBackAfterItsRequest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type jobButtonCase struct {
		Name   string `json:"name"`
		Focus  bool   `json:"focus"`
		Fails  bool   `json:"fails"`
		MoveTo string `json:"moveTo,omitempty"`
		want   string // where focus is once the timer has enabled the button
	}
	cases := []jobButtonCase{
		{Name: "answered", Focus: true, want: "button"},
		{Name: "failed", Focus: true, Fails: true, want: "button"},
		{Name: "focus taken meanwhile", Focus: true, MoveTo: "other", want: "other"},
		{Name: "never focused", want: "body"},
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "job_button.mjs")
	src := readFile(t, "static/app.js")
	body := extractJSFunction(t, src, "setDisabled") + "\n" + extractJSFunction(t, src, "wireJobButton") + "\n" + jobButtonHarness
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
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
	raw, err := exec.Command(node, script, casesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	type snapshot struct {
		Focus    string `json:"focus"`
		Disabled bool   `json:"disabled"`
		Text     string `json:"text"`
	}
	var results []struct {
		Name   string   `json:"name"`
		During snapshot `json:"during"`
		After  snapshot `json:"after"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness ran %d cases for %d", len(results), len(cases))
	}
	for i, r := range results {
		c := cases[i]
		if !r.During.Disabled || r.During.Focus == "button" {
			t.Fatalf("%s: with the request over the button is disabled=%v with focus on %q; the harness "+
				"is not modelling a disabled button losing focus", c.Name, r.During.Disabled, r.During.Focus)
		}
		if r.After.Disabled || r.After.Text != "Scan now" {
			t.Errorf("%s: after the timer the button is disabled=%v and reads %q, want it enabled and "+
				"restored", c.Name, r.After.Disabled, r.After.Text)
		}
		if r.After.Focus != c.want {
			t.Errorf("%s: focus is on %q once the button is enabled again, want %q", c.Name, r.After.Focus, c.want)
		}
	}
}

// handshakeHarness runs app.js's own publication of setDisabled and the
// player's ui.js wrapper against each other: the wrapper is called the way a
// player control calls it, on a control that has focus, and must reach the
// shipped helper; then with nothing published, where it must still disable.
const handshakeHarness = `
globalThis.window = {};
globalThis.document = { activeElement: null, body: { name: "body" } };
` + "%s" + `
const { setDisabled: fromThePlayer } = await import(process.argv[2]);
let refocused = false;
const control = { dataset: {}, disabled: false, isConnected: true, focus() { refocused = true; } };
document.activeElement = control;
fromThePlayer(control, true);
const marked = control.dataset.refocus === "1";
document.activeElement = document.body;
fromThePlayer(control, false);
const shared = { marked, refocused, disabledAfterEnable: control.disabled };
window.BridgeControls = undefined;
const plain = { dataset: {}, disabled: false };
fromThePlayer(plain, true);
console.log(JSON.stringify({ shared, plainDisabled: plain.disabled }));
`

// TestThePlayerReachesSetDisabledThroughTheWindowHandshake runs app.js's
// publication of setDisabled and the player's ui.js wrapper against each
// other under node.
//
// The player's modules cannot import app.js, so every one of their controls
// gives focus back only if app.js publishes the helper on window and ui.js
// reads it from there: a harness that installs the handshake itself (the
// panel's does) passes with either half missing. Here the publication is the
// statement app.js ships, cut out of it, and the wrapper is ui.js's own. A
// page without app.js still disables its controls, and only loses the focus.
func TestThePlayerReachesSetDisabledThroughTheWindowHandshake(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	src := readFile(t, "static/app.js")
	publication := regexp.MustCompile(`window\.BridgeControls = \{[^}]*\};`).FindString(src)
	if publication == "" {
		t.Fatal("app.js does not publish window.BridgeControls, so no player control can give focus back")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "handshake.mjs")
	body := fmt.Sprintf(handshakeHarness, extractJSFunction(t, src, "setDisabled")+"\n"+publication)
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Join("static", "player", "ui.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script, fileURL(module)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var out struct {
		Shared struct {
			Marked              bool `json:"marked"`
			Refocused           bool `json:"refocused"`
			DisabledAfterEnable bool `json:"disabledAfterEnable"`
		} `json:"shared"`
		PlainDisabled bool `json:"plainDisabled"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if !out.Shared.Marked || !out.Shared.Refocused || out.Shared.DisabledAfterEnable {
		t.Errorf("a player control disabled and enabled through ui.js: focus noted=%v, given back=%v, still "+
			"disabled=%v; want the shipped helper's behaviour (noted, given back, enabled)",
			out.Shared.Marked, out.Shared.Refocused, out.Shared.DisabledAfterEnable)
	}
	if !out.PlainDisabled {
		t.Errorf("with nothing published, ui.js's setDisabled left the control enabled: the fallback is a plain assignment")
	}
}

// jsFunctionStartRe is where a top-level function of the console's scripts
// starts: its name is what points a reader at a site.
var jsFunctionStartRe = regexp.MustCompile(`(?m)^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z0-9_$]+)`)

// enclosingJSFunction names the top-level function a position falls in, given
// the source up to it: the last function that started before it.
func enclosingJSFunction(before string) string {
	all := jsFunctionStartRe.FindAllStringSubmatch(before, -1)
	if len(all) == 0 {
		return "no function"
	}
	return all[len(all)-1][1] + "()"
}

// consoleScripts are the console's own scripts: app.js, and the player's
// modules beside it, whose controls reach the same helper through ui.js.
func consoleScripts(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{"static/app.js": readFile(t, "static/app.js")}
	entries, err := os.ReadDir("static/player")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") || isEditorDetritus(e.Name()) {
			continue
		}
		files["static/player/"+e.Name()] = readFile(t, "static/player/"+e.Name())
	}
	return files
}

// disabledLiteralRe is a control disabled or enabled by a literal: the
// assignment that loses focus, in every shape the scripts write it (a
// statement, an arrow's body, a comma expression).
var disabledLiteralRe = regexp.MustCompile(`\.disabled\s*=\s*(?:true|false)\b`)

// disabledAttributeRe is the same act through the attribute.
var disabledAttributeRe = regexp.MustCompile(`(?:setAttribute|removeAttribute|toggleAttribute)\(\s*["']disabled["']`)

// TestNoConsoleControlDisablesItselfOutsideSetDisabled sweeps the console's
// scripts for a control that disables or enables itself by any route but
// setDisabled.
//
// Thirty-odd controls did, and every one left a keyboard user on the body
// (backlog B68): the sweep exists because the fix that closed the first of
// them (the tray's switch) was a private copy of the rule, and the next
// control written was not going to know it. So a literal `.disabled = true`
// or `= false`, and the attribute forms, are refused outside the helper
// itself: app.js's setDisabled holds the only two. ui.js's forwarding
// wrapper assigns a variable, which a control that is only ever passed
// through it never reads as a literal. A value computed from state
// (`gen.disabled = !actionable`) is not a request in flight and is not
// the sweep's subject.
//
// The scan reads the scripts with their comments stripped, since this
// repo's commentary names the very assignment it explains.
func TestNoConsoleControlDisablesItselfOutsideSetDisabled(t *testing.T) {
	files := consoleScripts(t)
	if len(files) < 8 {
		t.Fatalf("the sweep read %d scripts, want app.js and the player's modules", len(files))
	}
	helperBodies := 0
	for name, raw := range files {
		src := stripJSComments(raw)
		if name == "static/app.js" {
			body := extractJSFunction(t, src, "setDisabled")
			if n := len(disabledLiteralRe.FindAllString(body, -1)); n != 2 {
				t.Fatalf("setDisabled holds %d literal assignments, want its 2: the sweep would pass "+
					"vacuously if it could not see the helper's own", n)
			}
			helperBodies++
			src = strings.Replace(src, body, "", 1)
		}
		for _, re := range []*regexp.Regexp{disabledLiteralRe, disabledAttributeRe} {
			for _, loc := range re.FindAllStringIndex(src, -1) {
				t.Errorf("%s, in %s: %q disables or enables a control without setDisabled, so a focused "+
					"control loses focus and never gets it back", name, enclosingJSFunction(src[:loc[0]]),
					strings.TrimSpace(src[max(0, loc[0]-40):min(len(src), loc[1]+10)]))
			}
		}
	}
	if helperBodies != 1 {
		t.Fatalf("the sweep found the helper in %d scripts, want app.js's alone", helperBodies)
	}
}
