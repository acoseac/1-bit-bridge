package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// jobsToolStateStubs are the functions renderJobCards and
// renderSettingsPrereqs call that these tests do not look at: each answers
// a dash, or nothing, so the harness needs none of their own helpers. Every
// other top-level app.js function the two call is extracted and runs as
// shipped.
var jobsToolStateStubs = map[string]string{
	"formatInFuture":              `function formatInFuture() { return "—"; }`,
	"formatDuration":              `function formatDuration() { return "—"; }`,
	"agoOrDash":                   `function agoOrDash() { return "—"; }`,
	"renderAnalysisCoverage":      `function renderAnalysisCoverage() {}`,
	"describeAnalysisSweep":       `function describeAnalysisSweep() { return "—"; }`,
	"formatAutoOptimizeRemaining": `function formatAutoOptimizeRemaining() { return "—"; }`,
	"formatAutoOptimizeResult":    `function formatAutoOptimizeResult() { return "—"; }`,
	"renderOrphanSidecarGC":       `function renderOrphanSidecarGC() {}`,
}

// jobsToolStateDescriptions are the three cards' descriptions as the harness
// seeds them. Any text will do: the property is that a render leaves it as
// it found it.
var jobsToolStateDescriptions = map[string]string{
	"job-analysis-hint": "The analysis card's description.",
	"job-fp-hint":       "The fingerprint card's description.",
	"job-ao-hint":       "The CarPlay card's description.",
}

// jobsToolStatePreamble is the DOM the two functions reach: every element
// they look up exists, keeps its text, hidden flag and dataset, and starts
// as the template ships it (each degraded note hidden, each description
// holding its text).
const jobsToolStatePreamble = `
class El {
  constructor(id) {
    this.id = id; this.children = []; this.hidden = false;
    this.className = ""; this.dataset = {}; this.disabled = false; this.checked = false;
  }
  // Text is a child node, as in the DOM: setting it replaces every child,
  // and an element appended after keeps it.
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this.children = String(v) === "" ? [] : [{ textContent: String(v) }]; }
  appendChild(c) { this.children.push(c); return c; }
  addEventListener() {}
}
const els = new Map();
const el = (id) => { if (!els.has(id)) els.set(id, new El(id)); return els.get(id); };
globalThis.document = {
  getElementById: (id) => el(id),
  createElement: (tag) => new El(tag),
  querySelector: (sel) => (sel === 'input[name="upscaleEnabled"]' ? el("upscale-switch") : null),
};
let apiAnswers = {};
const API = { get: async (url) => apiAnswers[url] ?? null };
// The managed set a settings snapshot named, or null before one has (app.js's
// own binding, which the extracted functions read).
let trayManaged = null;
`

// jobsToolStateRun drives both functions over the cases and prints what
// each left on the page.
const jobsToolStateRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const read = (id) => ({ hidden: el(id).hidden, text: el(id).textContent });
const cards = [];
for (const [id, text] of Object.entries(input.descriptions)) el(id).textContent = text;
for (const id of ["job-analysis-degraded", "job-fp-degraded", "job-ao-degraded"]) el(id).hidden = true;
for (const snap of input.jobs || []) {
  renderJobCards(snap);
  const out = {};
  for (const card of ["analysis", "fp", "ao"]) {
    out[card] = {
      badge: el("job-" + card + "-state").textContent,
      note: read("job-" + card + "-degraded"),
      description: el("job-" + card + "-hint").textContent,
    };
  }
  out.enableHidden = el("jobs-fp-enable").hidden;
  cards.push(out);
}
const chips = [];
for (const c of input.chips || []) {
  els.delete("prereq-fingerprint"); els.delete("prereq-analysis"); els.delete("prereq-upscale");
  el("upscale-switch").checked = !!c.upscaleSwitch;
  apiAnswers = { "/api/jobs": c.jobs, "/api/doctor": c.doctor, "/api/upscale/stats": c.upscale };
  await renderSettingsPrereqs();
  chips.push({
    fingerprint: { state: el("prereq-fingerprint").dataset.state, text: el("prereq-fingerprint").textContent },
    analysis: { state: el("prereq-analysis").dataset.state, text: el("prereq-analysis").textContent },
  });
}
console.log(JSON.stringify({ cards, chips }));
`

// jobsToolStateScript assembles the harness: the preamble, the stubs, the
// constant table the cards word their keys from, and every top-level
// function the two entry points reach, extracted from the shipped app.js.
func jobsToolStateScript(t *testing.T) string {
	t.Helper()
	return jobsToolStateBase(t) + jobsToolStateRun
}

// jobsToolStateBase is the harness without its run: what a test that drives
// the same shipped functions its own way appends its own script to.
func jobsToolStateBase(t *testing.T) string {
	t.Helper()
	src := readFile(t, "static/app.js")
	var script strings.Builder
	script.WriteString(jobsToolStatePreamble)
	for _, stub := range jobsToolStateStubs {
		script.WriteString(stub + "\n")
	}
	script.WriteString(extractJSConst(t, src, "JOB_DEGRADED_LABELS"))
	seen := map[string]bool{}
	queue := []string{"renderJobCards", "renderSettingsPrereqs"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] || jobsToolStateStubs[name] != "" {
			continue
		}
		seen[name] = true
		body, ok := extractJSFunctionIfPresent(src, name)
		if !ok {
			continue
		}
		script.WriteString(body + "\n")
		// jsCallRe (js_reference_parity_test.go) finds a bare call's callee.
		for _, m := range jsCallRe.FindAllStringSubmatch(body, -1) {
			queue = append(queue, m[2])
		}
	}
	for _, need := range []string{"renderJobCards", "renderSettingsPrereqs", "setBadge", "setText"} {
		if !seen[need] {
			t.Fatalf("the harness did not reach %s, so it no longer runs the shipped cards", need)
		}
	}
	return script.String()
}

// extractJSFunctionIfPresent is extractJSFunction for a name that may not be
// a top-level function at all (a method, a builtin): false, rather than a
// failed test, when it is not one.
func extractJSFunctionIfPresent(src, name string) (string, bool) {
	for _, anchor := range jsFunctionAnchors {
		i := strings.Index(src, "\n"+anchor+name+"(")
		if i < 0 {
			continue
		}
		start := i + 1
		end := strings.Index(src[start:], "\n}\n")
		if end < 0 {
			return "", false
		}
		return src[start : start+end+3], true
	}
	return "", false
}

// extractJSConst slices a top-level `const NAME = {…};` out of app.js.
func extractJSConst(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "\nconst "+name+" = {")
	if start < 0 {
		t.Fatalf("no const %s in the source", name)
	}
	start++
	end := strings.Index(src[start:], "\n};\n")
	if end < 0 {
		t.Fatalf("unterminated const %s", name)
	}
	return src[start:start+end+4] + "\n"
}

// jobsToolStateCard is one card as a render left it.
type jobsToolStateCard struct {
	Badge string `json:"badge"`
	Note  struct {
		Hidden bool   `json:"hidden"`
		Text   string `json:"text"`
	} `json:"note"`
	Description string `json:"description"`
}

// jobsToolStateChip is one Settings chip as renderSettingsPrereqs left it.
type jobsToolStateChip struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// jobsToolStateResult is what the harness printed.
type jobsToolStateResult struct {
	Cards []map[string]json.RawMessage `json:"cards"`
	Chips []struct {
		Fingerprint jobsToolStateChip `json:"fingerprint"`
		Analysis    jobsToolStateChip `json:"analysis"`
	} `json:"chips"`
}

// runJobsToolStateUnderNode runs the harness over the given jobs snapshots
// and chip cases.
func runJobsToolStateUnderNode(t *testing.T, node string, jobs []jobsSnapshotResponse, chips []map[string]any) jobsToolStateResult {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "jobs.mjs")
	if err := os.WriteFile(script, []byte(jobsToolStateScript(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(map[string]any{
		"descriptions": jobsToolStateDescriptions, "jobs": jobs, "chips": chips,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script, input).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var out jobsToolStateResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	return out
}

// jobsCard decodes one card out of a rendered step.
func jobsCard(t *testing.T, step map[string]json.RawMessage, card string) jobsToolStateCard {
	t.Helper()
	var c jobsToolStateCard
	if err := json.Unmarshal(step[card], &c); err != nil {
		t.Fatalf("card %s: %v", card, err)
	}
	return c
}

// TestAJobCardSaysWhyItIsInactiveBesideItsDescriptionAndClearsWhenActive
// runs the shipped renderJobCards under node, through a switched-on card
// losing its tool and getting it back, for the three cards whose gate
// probes one: audio analysis (sox), fingerprinting (fpcalc) and CarPlay
// pre-generation (sox).
//
// The analysis and fingerprint cards wrote "Enabled but inactive: … Restart
// after fixing." over their own description until 2026-09-28. Both gates
// have been live since #781, so the restart was never needed, and nothing
// wrote the description back: in a browser, the badge read "active" beside
// "sox is not installed … Restart after fixing." until a reload. The
// CarPlay card has had a note of its own since #1067; all three share it
// now. The payload travels through json.Marshal of jobsSnapshotResponse,
// so a Go tag and a JS read that disagree about a name fail here too.
func TestAJobCardSaysWhyItIsInactiveBesideItsDescriptionAndClearsWhenActive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	degraded := jobsSnapshotResponse{
		Analysis:     jobsAnalysis{Enabled: true, DegradedReason: "sox_missing"},
		Fingerprint:  &FingerprintJobState{Enabled: true, DegradedReason: "fpcalc_missing"},
		AutoOptimize: &AutoOptimizeJobState{Enabled: true, DegradedReason: "sox_missing"},
	}
	active := jobsSnapshotResponse{
		Analysis:     jobsAnalysis{Enabled: true, Active: true},
		Fingerprint:  &FingerprintJobState{Enabled: true, Active: true},
		AutoOptimize: &AutoOptimizeJobState{Enabled: true, Active: true},
	}
	out := runJobsToolStateUnderNode(t, node, []jobsSnapshotResponse{degraded, active}, nil)
	if len(out.Cards) != 2 {
		t.Fatalf("the harness rendered %d snapshots, want 2", len(out.Cards))
	}
	reasons := map[string]string{
		"analysis": "sox is not installed",
		"fp":       "fpcalc is not installed",
		"ao":       "sox is not installed",
	}
	descriptions := map[string]string{
		"analysis": jobsToolStateDescriptions["job-analysis-hint"],
		"fp":       jobsToolStateDescriptions["job-fp-hint"],
		"ao":       jobsToolStateDescriptions["job-ao-hint"],
	}
	cards := make([]string, 0, len(reasons))
	for card := range reasons {
		cards = append(cards, card)
	}
	sort.Strings(cards)
	for _, card := range cards {
		down := jobsCard(t, out.Cards[0], card)
		if down.Note.Hidden || !strings.Contains(down.Note.Text, reasons[card]) {
			t.Errorf("%s, degraded: the note is hidden=%v with %q, want it shown, naming %q",
				card, down.Note.Hidden, down.Note.Text, reasons[card])
		}
		if !strings.Contains(down.Note.Text, "No restart is needed") ||
			strings.Contains(strings.ToLower(down.Note.Text), "restart after") {
			t.Errorf("%s, degraded: the note says %q; the gate is live, so it must say no restart is needed",
				card, down.Note.Text)
		}
		if down.Description != descriptions[card] {
			t.Errorf("%s, degraded: the description reads %q, want it left as %q",
				card, down.Description, descriptions[card])
		}
		up := jobsCard(t, out.Cards[1], card)
		if !up.Note.Hidden || up.Note.Text != "" {
			t.Errorf("%s, active again: the note is hidden=%v with %q, want it hidden and empty",
				card, up.Note.Hidden, up.Note.Text)
		}
		if up.Description != descriptions[card] {
			t.Errorf("%s, active again: the description reads %q, want %q back",
				card, up.Description, descriptions[card])
		}
		if up.Badge != "active" && up.Badge != "on" {
			t.Errorf("%s, active again: the badge reads %q, so the step did not render an active card",
				card, up.Badge)
		}
	}
	var enableHidden bool
	if err := json.Unmarshal(out.Cards[0]["enableHidden"], &enableHidden); err != nil || !enableHidden {
		t.Errorf("fingerprinting switched on: the Enable button is hidden=%v, want hidden (err %v)",
			enableHidden, err)
	}
}

// TestTheFingerprintChipReadsTheGateNotTheSwitch runs the shipped
// renderSettingsPrereqs under node and pins the chip beside "Identify
// untagged tracks by audio".
//
// It read the switch (`jobs.fingerprint.enabled`) as running until
// 2026-09-28, so on a bridge without fpcalc it said "active" while the Jobs
// card beside the same switch said degraded: seen in a browser. The
// analysis chip is the control: its reading and its words did not change.
func TestTheFingerprintChipReadsTheGateNotTheSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	doctor := func(fpStatus, fpSummary string) map[string]any {
		return map[string]any{"available": true, "report": map[string]any{"checks": []map[string]string{
			{"name": "fingerprint-toolchain", "status": fpStatus, "summary": fpSummary},
			{"name": "audio-toolchain", "status": "ok", "summary": "sox v14.4.2 (FLAC ok)"},
		}}}
	}
	jobs := func(fp FingerprintJobState) jobsSnapshotResponse {
		return jobsSnapshotResponse{
			Analysis:    jobsAnalysis{Enabled: true, DegradedReason: "sox_missing"},
			Fingerprint: &fp,
		}
	}
	chips := []map[string]any{
		{"jobs": jobs(FingerprintJobState{Enabled: true, DegradedReason: "fpcalc_missing"}),
			"doctor": doctor("fail", "fpcalc not found")},
		{"jobs": jobs(FingerprintJobState{Enabled: true, DegradedReason: "fpcalc_missing"}),
			"doctor": doctor("ok", "fpcalc 1.5.1, API key configured")},
		{"jobs": jobs(FingerprintJobState{Enabled: true, Active: true}),
			"doctor": doctor("ok", "fpcalc 1.5.1, API key configured")},
		{"jobs": jobs(FingerprintJobState{}), "doctor": doctor("ok", "not enabled (fpcalc not required)")},
	}
	want := []jobsToolStateChip{
		{State: "warn", Text: "not running — fpcalc not found"},
		{State: "warn", Text: "not running yet — fpcalc was just found; picked up within a minute"},
		{State: "ok", Text: "active"},
		{State: "off", Text: "off"},
	}
	out := runJobsToolStateUnderNode(t, node, nil, chips)
	if len(out.Chips) != len(want) {
		t.Fatalf("the harness painted %d chip cases, want %d", len(out.Chips), len(want))
	}
	for i, w := range want {
		if got := out.Chips[i].Fingerprint; got != w {
			t.Errorf("case %d: the fingerprint chip is %+v, want %+v", i, got, w)
		}
		analysis := jobsToolStateChip{State: "warn",
			Text: "not running yet — sox was just found; picked up within a minute"}
		if got := out.Chips[i].Analysis; got != analysis {
			t.Errorf("case %d: the analysis chip is %+v, want %+v, unchanged", i, got, analysis)
		}
	}
}
