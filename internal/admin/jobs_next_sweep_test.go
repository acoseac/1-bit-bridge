package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// nextSweepPaths classifies every next-run time /api/jobs can carry, by its
// JSON path. A gated one names the card's gate, read once for the snapshot,
// and is sent only while that gate is open (nextSweepWhileOpen); an ungated
// one says why its time is a run that will happen.
//
// Backlog B156: every sweeper loop runs on every bridge whatever its gate
// says (#781), and runSweepLoop arms its next pass from the interval alone,
// so a switched-off card said "Next sweep: in 5h" beside its "off" badge,
// for a pass that stands down. TestEveryNextRunOnTheJobsCardsIsClassified
// requires a new next-run field to be classified here.
var nextSweepPaths = map[string]string{
	"analysis.sweep.nextDueAt": "gated: analysis.active",
	"fingerprint.nextDueAt":    "gated: fingerprint.active",
	"autoOptimize.nextDueAt":   "gated: autoOptimize.active",
	"smartMixes.run.nextDueAt": "gated: smartMixes.enabled",
	"scanner.nextScanDue":      "ungated: a periodic scan has no gate but its interval, and the time is omitted with it",
	"backups.run.nextDueAt":    "ungated: the interval is the backup loop's only gate, and a parked loop clears the time",
	"duplicates.run.nextDueAt": "ungated: the duplicates pass is nudge-only, and its loop never schedules a time",
}

// nextRunPaths walks rt fully (structs, pointers to structs) and returns the
// JSON path of every field whose name starts with "next": the next-run
// times a card can show.
func nextRunPaths(rt reflect.Type, prefix string) []string {
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct || rt == reflect.TypeOf(time.Time{}) {
		return nil
	}
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if strings.HasPrefix(name, "next") {
			out = append(out, path)
			continue
		}
		out = append(out, nextRunPaths(f.Type, path)...)
	}
	return out
}

// TestEveryNextRunOnTheJobsCardsIsClassified — the population guard: every
// next-run field the Jobs snapshot declares is in nextSweepPaths, and every
// entry there names a field that exists. A card added with a next-run time
// fails here until someone decides whether a closed gate withholds it.
func TestEveryNextRunOnTheJobsCardsIsClassified(t *testing.T) {
	paths := nextRunPaths(reflect.TypeOf(jobsSnapshotResponse{}), "")
	if len(paths) < 7 {
		t.Fatalf("found %d next-run field(s) in the Jobs snapshot; the walk has stopped recursing, so this test proves nothing", len(paths))
	}
	found := map[string]bool{}
	for _, p := range paths {
		found[p] = true
		if nextSweepPaths[p] == "" {
			t.Errorf("/api/jobs carries the next-run field %q and nextSweepPaths does not classify it: "+
				"decide whether its card withholds it while the gate is closed (nextSweepWhileOpen)", p)
		}
	}
	for p := range nextSweepPaths {
		if !found[p] {
			t.Errorf("nextSweepPaths classifies %q, which the Jobs snapshot no longer declares", p)
		}
	}
}

// nextSweepGates are the gates of the four gated cards, as a test moves them.
type nextSweepGates struct {
	analysisFlag, analysis   bool
	fingerprintSwitch, fp    bool
	autoOptimizeSwitches, ao bool
	smartMixes               bool
}

// wireNextSweepCards wires every card that shows a next run as serve wires
// it, each recorder holding the next run at `next`, and every gate read
// from g. The fingerprint and CarPlay closures report `active` themselves,
// as cmd/bridge's do; the analysis gate is Deps.AnalysisActive; the smart
// mixes' switch is the config's.
func wireNextSweepCards(srv *Server, next time.Time, g *nextSweepGates) {
	at := func() *time.Time { n := next; return &n }
	srv.deps.AnalysisActive = func() bool { return g.analysis }
	srv.deps.AnalysisSweep = func() *AnalysisSweepState {
		return &AnalysisSweepState{LastFinishedAt: at(), NextDueAt: at()}
	}
	srv.deps.FingerprintState = func() *FingerprintJobState {
		return &FingerprintJobState{Enabled: g.fingerprintSwitch, Active: g.fp, NextDueAt: at()}
	}
	srv.deps.AutoOptimizeState = func() *AutoOptimizeJobState {
		return &AutoOptimizeJobState{Enabled: g.autoOptimizeSwitches, Active: g.ao, NextDueAt: at()}
	}
	srv.deps.SmartMixRun = func() *JobRunState { return &JobRunState{NextDueAt: at()} }
	srv.deps.BackupRun = func() *JobRunState { return &JobRunState{NextDueAt: at()} }
	srv.deps.DuplicatesSweepRun = func() *JobRunState { return &JobRunState{NextDueAt: at()} }
}

// applyNextSweepGates stores g's config halves (the analysis flag and the
// smart mixes' switch) into srv's live config.
func applyNextSweepGates(srv *Server, g *nextSweepGates) {
	next := config.Clone(srv.deps.CfgHolder.Load())
	next.Analysis.Enabled = g.analysisFlag
	on := g.smartMixes
	next.SmartPlaylists.Enabled = &on
	srv.deps.CfgHolder.Store(next)
}

// nextSweepSteps move each gate open, closed over a switch that is on (the
// degraded card: a tool is missing), and closed with the switch off.
var nextSweepSteps = []struct {
	name string
	g    nextSweepGates
}{
	{"every switch off", nextSweepGates{}},
	{"every gate open", nextSweepGates{analysisFlag: true, analysis: true, fingerprintSwitch: true, fp: true,
		autoOptimizeSwitches: true, ao: true, smartMixes: true}},
	{"switched on over a missing tool", nextSweepGates{analysisFlag: true, fingerprintSwitch: true,
		autoOptimizeSwitches: true}},
}

// jsonPathPresent reports whether the dotted path names a key in the decoded
// JSON object m.
func jsonPathPresent(m map[string]any, path string) bool {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		obj, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = obj[p]; !ok {
			return false
		}
	}
	return true
}

// TestAJobsCardSendsItsNextSweepOnlyWhileItsGateIsOpen serves /api/jobs and
// GET /api/analysis/stats (the SSE `analysis` frame, which paints the same
// analysis line) over every card wired as serve wires it, each recorder
// holding a next run, and requires each gated next run to be sent exactly
// while its gate is open, and each ungated one always. The steps move each
// switch apart from its gate, so a handler that read a switch rather than
// the gate fails on the degraded step.
func TestAJobsCardSendsItsNextSweepOnlyWhileItsGateIsOpen(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	g := &nextSweepGates{}
	wireNextSweepCards(srv, time.Now().Add(5*time.Hour), g)
	gateOf := map[string]func() bool{
		"analysis.sweep.nextDueAt": func() bool { return g.analysis },
		"fingerprint.nextDueAt":    func() bool { return g.fp },
		"autoOptimize.nextDueAt":   func() bool { return g.ao },
		"smartMixes.run.nextDueAt": func() bool { return g.smartMixes },
	}
	for _, step := range nextSweepSteps {
		*g = step.g
		applyNextSweepGates(srv, g)
		var jobs map[string]any
		if code := doJSON(t, h, "GET", "/api/jobs", nil, &jobs); code != http.StatusOK {
			t.Fatalf("%s: /api/jobs answered %d", step.name, code)
		}
		for path, open := range gateOf {
			if got := jsonPathPresent(jobs, path); got != open() {
				t.Errorf("%s: /api/jobs carries %s: %v, want %v (the gate is open: %v)",
					step.name, path, got, open(), open())
			}
		}
		for _, path := range []string{"backups.run.nextDueAt", "duplicates.run.nextDueAt"} {
			if !jsonPathPresent(jobs, path) {
				t.Errorf("%s: /api/jobs dropped the ungated %s", step.name, path)
			}
		}
		if !jsonPathPresent(jobs, "analysis.sweep.lastFinishedAt") {
			t.Errorf("%s: the analysis card lost its last sweep, which stays whatever the gate says", step.name)
		}

		var stats map[string]any
		if code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &stats); code != http.StatusOK {
			t.Fatalf("%s: /api/analysis/stats answered %d", step.name, code)
		}
		if got := jsonPathPresent(stats, "sweep.nextDueAt"); got != g.analysis {
			t.Errorf("%s: /api/analysis/stats carries sweep.nextDueAt: %v, want %v", step.name, got, g.analysis)
		}
	}
}

// nextSweepRun drives the shipped renderJobCards over each /api/jobs
// snapshot and applyAnalysisStats over each /api/analysis/stats one, and
// prints what the next-run lines read.
const nextSweepRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const step of input) {
  for (const id of ["job-analysis-next", "job-fp-next", "job-ao-next", "job-mix-next"]) el(id).textContent = "";
  renderJobCards(step.jobs);
  const lines = {};
  for (const id of ["job-analysis-next", "job-fp-next", "job-ao-next", "job-mix-next"]) lines[id] = el(id).textContent;
  el("job-analysis-next").textContent = "";
  applyAnalysisStats(step.stats);
  lines["sse:job-analysis-next"] = el("job-analysis-next").textContent;
  out.push(lines);
}
console.log(JSON.stringify(out));
`

// TestTheJobsCardsSayNoNextSweepWhileTheirGateIsClosed runs the shipped
// renderJobCards and applyAnalysisStats under node over the payloads the
// handlers serve, gate by gate, and reads the next-run lines: "in 5h" while
// the gate is open, "—" while it is closed, on each of the four cards and on
// the SSE frame's analysis line. Seen in a browser on a real serve before
// the fix: "off" beside "Next sweep: in 5h" on the analysis, fingerprint
// and CarPlay cards, and "Next run: in 23h" on the smart mixes card.
func TestTheJobsCardsSayNoNextSweepWhileTheirGateIsClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	g := &nextSweepGates{}
	// Five and a half hours, so the rendered hour is 5 however long node
	// takes to start.
	wireNextSweepCards(srv, time.Now().Add(5*time.Hour+30*time.Minute), g)
	var steps []map[string]json.RawMessage
	for _, step := range nextSweepSteps {
		*g = step.g
		applyNextSweepGates(srv, g)
		var jobs, stats json.RawMessage
		if code := doJSON(t, h, "GET", "/api/jobs", nil, &jobs); code != http.StatusOK {
			t.Fatalf("%s: /api/jobs answered %d", step.name, code)
		}
		if code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &stats); code != http.StatusOK {
			t.Fatalf("%s: /api/analysis/stats answered %d", step.name, code)
		}
		steps = append(steps, map[string]json.RawMessage{"jobs": jobs, "stats": stats})
	}
	stubs := map[string]string{
		"renderAnalysisCoverage": `function renderAnalysisCoverage() {}`,
		"describeAnalysisSweep":  `function describeAnalysisSweep() { return "—"; }`,
	}
	script := consoleCardsHarness(t, stubs, "renderJobCards", "applyAnalysisStats") + nextSweepRun
	out := runNodeHarness(t, node, script, steps)
	var got []map[string]string
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(nextSweepSteps) {
		t.Fatalf("the harness printed %q (err %v), want %d steps", out, err, len(nextSweepSteps))
	}
	for i, step := range nextSweepSteps {
		open := map[string]bool{
			"job-analysis-next": step.g.analysis, "sse:job-analysis-next": step.g.analysis,
			"job-fp-next": step.g.fp, "job-ao-next": step.g.ao, "job-mix-next": step.g.smartMixes,
		}
		ids := make([]string, 0, len(open))
		for id := range open {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			want := "—"
			if open[id] {
				want = "in 5h"
			}
			if got[i][id] != want {
				t.Errorf("%s: %s reads %q, want %q", step.name, id, got[i][id], want)
			}
		}
	}
}

// runNodeHarness writes script and its JSON input to a temporary directory,
// runs it under node, and returns what it printed.
func runNodeHarness(t *testing.T, node, script string, input any) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.mjs")
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
	out, err := exec.Command(node, path, inputPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return out
}
