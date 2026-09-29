package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The controls beside the feature trays that offer a field the control plane
// can own.
//
// The trays hide a managed row (backlog B35), because the settings PATCH
// refuses a managed field whole and a switch offered for one can only answer
// "Save failed". Two controls outside the trays PATCH a field that
// deployment.managedSettings can list, and were offered on such a bridge
// all the same (backlog B68, seen in a browser on a public-mode bridge whose
// managedSettings listed both): the Jobs page's fingerprint Enable button,
// which answered "Enable failed — retry", and the Duplicates page's policy
// radios, whose click answered an alert saying the setting is managed. The
// console sends only what it showed, so both are hidden there, as a tray hides
// a managed row.

// runScriptUnderNode writes the script and its JSON input to a temp dir, runs
// it under node and returns what it printed.
func runScriptUnderNode(t *testing.T, node, script string, input any) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "case.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(dir, "input.json")
	if err := os.WriteFile(inputPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, path, inputPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	return raw
}

// withFunctions appends to a script each named top-level function of app.js
// that the script does not already declare.
func withFunctions(t *testing.T, script, src string, names ...string) string {
	t.Helper()
	var out strings.Builder
	out.WriteString(script)
	for _, name := range names {
		if strings.Contains(script, "function "+name+"(") {
			continue
		}
		out.WriteString(extractJSFunction(t, src, name))
		out.WriteString("\n")
	}
	return out.String()
}

// fingerprintEnableRun drives the shipped card render and Enable click over
// the cases: the jobs snapshot lands with the managed set known or not, the
// settings snapshot may land after it, and the button is clicked.
const fingerprintEnableRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
let traySettings = null;
const mountedTrays = new Set();
function syncTray() {}
async function jobsSnapshotRefresh() {}
let patches = [];
API.patch = async (url, body) => { patches.push(body); return {}; };
const out = [];
for (const c of input.cases) {
  els.clear();
  patches = [];
  el("jobs-fp-enable").hidden = true; // as the template ships it
  trayManaged = c.knownAtRender === null ? null : new Set(c.knownAtRender);
  renderJobCards(c.snapshot);
  const afterRender = el("jobs-fp-enable").hidden;
  let afterSnapshot = null;
  if (c.landsAfter) {
    trayManaged = new Set(c.landsAfter);
    syncFingerprintEnable();
    afterSnapshot = el("jobs-fp-enable").hidden;
  }
  if (c.click) await enableFingerprint(el("jobs-fp-enable"));
  out.push({ name: c.name, afterRender, afterSnapshot, sent: patches });
}
console.log(JSON.stringify(out));
`

// TestTheFingerprintEnableButtonIsOnlyOfferedWhereTheOperatorOwnsTheSwitch
// runs the shipped renderJobCards, syncFingerprintEnable and enableFingerprint
// under node and pins when the fingerprint card's Enable button shows and
// what a click of it sends.
//
// It showed whenever the switch was off, so a bridge that lists
// fingerprintEnabled in deployment.managedSettings offered it and the click
// answered "Enable failed — retry" (backlog B68; seen in a browser). It shows
// only for a switch that is off AND left to the operator, waits for a settings
// snapshot to say so (a managed bridge never flashes it, which a tray's rows
// in a closed gear can afford and a button on the card cannot), comes on when
// that snapshot lands, and a click dispatched to it anyway sends nothing for a
// managed field.
func TestTheFingerprintEnableButtonIsOnlyOfferedWhereTheOperatorOwnsTheSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	off := jobsSnapshotResponse{Fingerprint: &FingerprintJobState{Enabled: false}}
	on := jobsSnapshotResponse{Fingerprint: &FingerprintJobState{Enabled: true, Active: true}}
	none := []string{}
	managed := []string{"fingerprintEnabled", "duplicatesFilter"}
	type enableCase struct {
		Name          string               `json:"name"`
		Snapshot      jobsSnapshotResponse `json:"snapshot"`
		KnownAtRender *[]string            `json:"knownAtRender"`
		LandsAfter    []string             `json:"landsAfter,omitempty"`
		Click         bool                 `json:"click"` // a click of the button, hidden or not

		hiddenAfterRender   bool
		hiddenAfterSnapshot bool // only read where LandsAfter is set
		wantSent            int
	}
	cases := []enableCase{
		{Name: "switch off, operator's", Snapshot: off, KnownAtRender: &none, Click: true,
			hiddenAfterRender: false, wantSent: 1},
		{Name: "switch off, managed", Snapshot: off, KnownAtRender: &managed, Click: true,
			hiddenAfterRender: true, wantSent: 0},
		{Name: "switch on", Snapshot: on, KnownAtRender: &none, hiddenAfterRender: true},
		// Unknown at the first paint: hidden, and on once the snapshot says the
		// operator owns it, so a bridge that does is not left waiting for the
		// next 10 s poll.
		{Name: "unknown, then the operator's", Snapshot: off, LandsAfter: []string{"backupKeep"}, Click: true,
			hiddenAfterRender: true, hiddenAfterSnapshot: false, wantSent: 1},
		{Name: "unknown, then managed", Snapshot: off, LandsAfter: managed, Click: true,
			hiddenAfterRender: true, hiddenAfterSnapshot: true, wantSent: 0},
	}
	src := readFile(t, "static/app.js")
	// What the click reaches that the card render does not, added only where
	// the base has not already extracted it: a module refuses a function
	// declared twice.
	script := withFunctions(t, jobsToolStateBase(t), src, "enableFingerprint", "setDisabled", "trayFieldManaged") +
		fingerprintEnableRun
	raw := runScriptUnderNode(t, node, script, map[string]any{"cases": cases})
	var results []struct {
		Name          string           `json:"name"`
		AfterRender   bool             `json:"afterRender"`
		AfterSnapshot *bool            `json:"afterSnapshot"`
		Sent          []map[string]any `json:"sent"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness ran %d cases for %d", len(results), len(cases))
	}
	for i, r := range results {
		c := cases[i]
		if r.AfterRender != c.hiddenAfterRender {
			t.Errorf("%s: after the card rendered the Enable button is hidden=%v, want %v",
				c.Name, r.AfterRender, c.hiddenAfterRender)
		}
		if c.LandsAfter != nil && (r.AfterSnapshot == nil || *r.AfterSnapshot != c.hiddenAfterSnapshot) {
			t.Errorf("%s: after the settings snapshot landed the button is hidden=%v, want %v",
				c.Name, r.AfterSnapshot, c.hiddenAfterSnapshot)
		}
		// A click of a button the reader could not see is a script's: the
		// managed field goes unsent, the operator's is sent as always.
		wantSent := c.wantSent
		if len(r.Sent) != wantSent {
			t.Errorf("%s: a click sent %v, want %d PATCHes", c.Name, r.Sent, wantSent)
		}
		for _, sent := range r.Sent {
			if len(sent) != 1 || sent["fingerprintEnabled"] != true {
				t.Errorf("%s: the click sent %v, want fingerprintEnabled alone", c.Name, sent)
			}
		}
	}
}

// dupesPolicyRun builds the policy fieldset's DOM as the template ships it
// and drives the shipped functions over the cases.
const dupesPolicyRun = `
class El {
  constructor(id) {
    this.id = id; this.hidden = false; this.isDisabled = false; this.isConnected = true;
    this.dataset = {}; this.textContent = ""; this.children = [];
  }
  get disabled() { return this.isDisabled; }
  set disabled(v) { this.isDisabled = Boolean(v); }
  focus() {}
}
const ids = ["dupes-policy", "dupes-policy-hint", "dupes-policy-managed", "dupes-policy-name"];
const els = new Map();
const radios = ["highest-quality", "same-format", "off"].map((v) => { const r = new El(v); r.value = v; return r; });
globalThis.document = {
  body: new El("body"), activeElement: null,
  getElementById: (id) => els.get(id) || null,
  querySelectorAll: (sel) => (sel === "#dupes-policy input" ? radios : []),
};
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
let trayManaged = null;
let refreshes = 0;
function refreshDupesSummary() { refreshes++; }
function scheduleDupesRefresh() {}
globalThis.alert = () => {};
let patches = [];
const API = { patch: async (url, body) => { patches.push(body); return {}; } };
const out = [];
for (const c of input.cases) {
  els.clear(); patches = []; refreshes = 0;
  for (const id of ids) els.set(id, new El(id));
  els.get("dupes-policy-managed").hidden = true; // as the template ships it
  for (const r of radios) r.isDisabled = false;
  trayManaged = c.known === null ? null : new Set(c.known);
  applyDupesPolicyManaged();
  const state = () => ({
    fieldsetHidden: els.get("dupes-policy").hidden, hintHidden: els.get("dupes-policy-hint").hidden,
    readoutHidden: els.get("dupes-policy-managed").hidden, inputsDisabled: radios.map((r) => r.disabled),
  });
  const first = state();
  let landed = null;
  if (c.landsAfter) { trayManaged = new Set(c.landsAfter); applyDupesPolicyManaged(); landed = state(); }
  await saveDupesPolicy("same-format");
  const named = dupesPolicyName({ closest: () => ({ querySelector: () => ({ textContent: "Highest quality" }) }) }, "highest-quality");
  out.push({ name: c.name, first, landed, sent: patches, refreshes, named,
    bare: dupesPolicyName(null, "highest-quality"), none: dupesPolicyName(null, "") });
}
console.log(JSON.stringify(out));
`

// TestTheDuplicatesPolicyIsOnlyOfferedWhereTheOperatorOwnsIt runs the
// shipped applyDupesPolicyManaged, saveDupesPolicy and dupesPolicyName under
// node and pins what the Duplicates page shows and sends for the policy.
//
// The radios were offered on a bridge that lists duplicatesFilter in
// deployment.managedSettings, and a click answered an alert saying so (backlog
// B68). There they are hidden with the prose about saving one, disabled so
// nothing can send them, and the policy in force is said in their place; a
// change dispatched to them anyway sends nothing and snaps the radios back
// to what the bridge holds. Where the operator owns the policy nothing
// changes, and an unknown managed set reads as not managed, as a tray reads
// it, since the radios also show the policy in force.
func TestTheDuplicatesPolicyIsOnlyOfferedWhereTheOperatorOwnsIt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type policyCase struct {
		Name       string    `json:"name"`
		Known      *[]string `json:"known"`
		LandsAfter []string  `json:"landsAfter,omitempty"`
		managed    bool      // whether the control plane owns the policy once known
	}
	none := []string{"backupKeep"}
	owned := []string{"duplicatesFilter"}
	cases := []policyCase{
		{Name: "the operator's", Known: &none, managed: false},
		{Name: "managed", Known: &owned, managed: true},
		{Name: "unknown at the first paint, then managed", LandsAfter: owned, managed: true},
		{Name: "unknown at the first paint, then the operator's", LandsAfter: none, managed: false},
	}
	src := readFile(t, "static/app.js")
	script := strings.Join([]string{
		extractJSFunction(t, src, "applyDupesPolicyManaged"), extractJSFunction(t, src, "saveDupesPolicy"),
		extractJSFunction(t, src, "dupesPolicyName"), extractJSFunction(t, src, "trayFieldManaged"),
		extractJSFunction(t, src, "setDisabled"),
	}, "\n") + "\n" + dupesPolicyRun
	raw := runScriptUnderNode(t, node, script, map[string]any{"cases": cases})
	type state struct {
		FieldsetHidden bool   `json:"fieldsetHidden"`
		HintHidden     bool   `json:"hintHidden"`
		ReadoutHidden  bool   `json:"readoutHidden"`
		InputsDisabled []bool `json:"inputsDisabled"`
	}
	var results []struct {
		Name      string           `json:"name"`
		First     state            `json:"first"`
		Landed    *state           `json:"landed"`
		Sent      []map[string]any `json:"sent"`
		Refreshes int              `json:"refreshes"`
		Named     string           `json:"named"`
		Bare      string           `json:"bare"`
		None      string           `json:"none"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness ran %d cases for %d", len(results), len(cases))
	}
	check := func(name, when string, s state, managed bool) {
		t.Helper()
		if s.FieldsetHidden != managed || s.HintHidden != managed || s.ReadoutHidden == managed {
			t.Errorf("%s, %s: radios hidden=%v, saving prose hidden=%v, policy line hidden=%v; want the radios "+
				"and the prose hidden=%v and the line hidden=%v", name, when, s.FieldsetHidden, s.HintHidden,
				s.ReadoutHidden, managed, !managed)
		}
		for i, d := range s.InputsDisabled {
			if d != managed {
				t.Errorf("%s, %s: radio %d is disabled=%v, want %v: nothing may send a managed field",
					name, when, i, d, managed)
			}
		}
	}
	for i, r := range results {
		c := cases[i]
		if c.LandsAfter == nil {
			check(c.Name, "at the first paint", r.First, c.managed)
		} else {
			// Unknown reads as not managed: the radios show, they carry the policy in force.
			check(c.Name, "before the snapshot", r.First, false)
			check(c.Name, "after the snapshot", *r.Landed, c.managed)
		}
		switch {
		case c.managed && len(r.Sent) != 0:
			t.Errorf("%s: a change of the policy sent %v; the PATCH refuses a managed field whole", c.Name, r.Sent)
		case c.managed && r.Refreshes == 0:
			t.Errorf("%s: a refused change did not snap the radios back to what the bridge holds", c.Name)
		case !c.managed && (len(r.Sent) != 1 || r.Sent[0]["duplicatesFilter"] != "same-format"):
			t.Errorf("%s: a change of the policy sent %v, want duplicatesFilter alone", c.Name, r.Sent)
		}
		if r.Named != "Highest quality" || r.Bare != "highest-quality" || r.None != "—" {
			t.Errorf("%s: the policy is named %q by its radio, %q with no radio and %q with no policy; want "+
				"the radio's own name, else the raw value, else a dash", c.Name, r.Named, r.Bare, r.None)
		}
	}
}

// TestTheDuplicatesTemplateCarriesWhatTheManagedPolicyLineNeeds pins the
// markup applyDupesPolicyManaged and refreshDupesSummary reach into: the
// fieldset, the prose about saving, the line that says which policy is set
// for the bridge (hidden until it is shown), and a name for each radio's
// policy that the line quotes.
func TestTheDuplicatesTemplateCarriesWhatTheManagedPolicyLineNeeds(t *testing.T) {
	tmpl := readFile(t, "templates/duplicates.html")
	for _, want := range []string{
		`id="dupes-policy"`, `id="dupes-policy-hint"`, `id="dupes-policy-name"`,
		`<p class="hint" id="dupes-policy-managed" hidden>`,
		`<strong>Highest quality</strong>`, `<strong>Same format only</strong>`, `<strong>Off</strong>`,
	} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("templates/duplicates.html lacks %s", want)
		}
	}
}

// carPlayBlurbRun mounts the Jobs page's trays with the shipped
// mountJobTrays on a settings answer that names the case's managed fields,
// lets the snapshot land, and reads back the CarPlay pre-generation tray:
// its blurb and the rows the reader can see.
const carPlayBlurbRun = `
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const c of input.cases) {
  traySettings = null; traySettingsPromise = null; trayManaged = null; mountedTrays.clear();
  settingsAnswer = { managedSettings: c.managed, upscaleEnabled: false, optimizeEnabled: false,
    autoOptimizeEnabled: false, dsdRenderEnabled: false };
  const trays = [];
  const heads = new Map();
  document.querySelector = (sel) => {
    if (!heads.has(sel)) {
      const head = new El("div");
      head.parentNode = { insertBefore: (tray) => trays.push(tray) };
      heads.set(sel, head);
    }
    return heads.get(sel);
  };
  mountJobTrays();
  await new Promise((r) => setTimeout(r, 0));
  const tray = trays.find((t) => { let title = ""; walk(t, (n) => { if (n.className === "tray-title") title = n.textContent; }); return title === "CarPlay pre-generation"; });
  let blurb = null; const rows = [];
  walk(tray, (n) => {
    if (n.className === "tray-blurb") blurb = n.textContent;
    if (n.className === "tray-row" && !n.hidden) { walk(n, (m) => { if (m.dataset.field) rows.push(m.dataset.field); }); }
  });
  out.push({ name: c.name, blurb, rows });
}
console.log(JSON.stringify(out));
`

// TestTheCarPlayTrayBlurbCountsOnlySwitchesTheReaderCanSee runs the shipped
// mountJobTrays under node and pins the CarPlay pre-generation tray's blurb
// against the rows the reader can see.
//
// The blurb said "All three switches have to be on for anything to run"
// whatever the tray showed, so where the control plane owns PCM upscaling and
// CarPlay-optimized variants (the hosted product's case) it stood beside the
// two rows that are left, the second of them not one of the three (backlog
// B68). It still says so while all three are on the screen, and otherwise
// says how many are set for the bridge, without naming a switch that is not
// there to be found.
func TestTheCarPlayTrayBlurbCountsOnlySwitchesTheReaderCanSee(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type blurbCase struct {
		Name    string   `json:"name"`
		Managed []string `json:"managed"`
		want    string   // the sentence after "anything to run", or "" for none
	}
	cases := []blurbCase{
		{Name: "nothing managed", Managed: nil, want: "."},
		{Name: "one of the three", Managed: []string{"upscaleEnabled"}, want: "; the one not shown here is set for this bridge."},
		{Name: "two of the three", Managed: []string{"upscaleEnabled", "optimizeEnabled"},
			want: "; the two not shown here are set for this bridge."},
		{Name: "all three", Managed: []string{"upscaleEnabled", "optimizeEnabled", "autoOptimizeEnabled"},
			want: "; all three are set for this bridge."},
		// The DSD row is not one of the three, so managing it changes no count.
		{Name: "only the DSD row", Managed: []string{"dsdRenderEnabled"}, want: "."},
	}
	src := readFile(t, "static/app.js")
	functions := append([]string{}, trayHarnessFunctions...)
	functions = append(functions, "mountJobTrays", "attachFeatureTray", "carPlayBlurb")
	var script strings.Builder
	script.WriteString(trayHarnessPreamble)
	for _, name := range functions {
		script.WriteString(extractJSFunction(t, src, name))
		script.WriteString("\n")
	}
	script.WriteString(carPlayBlurbRun)
	raw := runScriptUnderNode(t, node, script.String(), map[string]any{"cases": cases})
	var results []struct {
		Name  string   `json:"name"`
		Blurb *string  `json:"blurb"`
		Rows  []string `json:"rows"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness ran %d cases for %d", len(results), len(cases))
	}
	const lead = "Builds the optimized copies ahead of time instead of waiting for a device to ask for one. " +
		"All three switches have to be on for anything to run"
	for i, r := range results {
		c := cases[i]
		if r.Blurb == nil {
			t.Fatalf("%s: the CarPlay tray has no blurb; the harness did not reach it", c.Name)
		}
		if want := lead + c.want; *r.Blurb != want {
			t.Errorf("%s: the blurb reads %q, want %q", c.Name, *r.Blurb, want)
		}
		// The sentence must agree with the screen: how many of the three the
		// reader can see is what it says are not shown.
		visible := 0
		for _, f := range r.Rows {
			switch f {
			case "upscaleEnabled", "optimizeEnabled", "autoOptimizeEnabled":
				visible++
			}
		}
		if got := strings.Count(*r.Blurb, "set for this bridge") == 1; got != (visible < 3) {
			t.Errorf("%s: %d of the three switches are shown and the blurb says some are set for the bridge=%v",
				c.Name, visible, got)
		}
	}
}
