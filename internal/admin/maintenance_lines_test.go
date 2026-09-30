package admin

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
)

// maintenanceLinesRun drives the shipped renderJobCards over each
// /api/jobs snapshot and prints the two maintenance lines that can refuse,
// and the warning under the card for each.
const maintenanceLinesRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const read = (id) => ({ hidden: el(id).hidden, text: el(id).textContent });
const line = (id) => ({ text: el(id).textContent, badge: el(id).children.map((c) => c.className || "").join(" ").trim() });
const out = [];
for (const jobs of input) {
  renderJobCards(jobs);
  out.push({
    integrity: line("job-maint-integrity"), integrityWhy: read("job-maint-integrity-refusal"),
    gc: line("job-maint-gc"), gcWhy: read("job-maint-gc-refusal"),
  });
}
console.log(JSON.stringify(out));
`

// maintenanceLine is one line as the harness read it.
type maintenanceLine struct {
	Text  string `json:"text"`
	Badge string `json:"badge"`
}

// maintenanceNote is one warning as the harness read it.
type maintenanceNote struct {
	Hidden bool   `json:"hidden"`
	Text   string `json:"text"`
}

// maintenanceLinesStep is what one render left on the card.
type maintenanceLinesStep struct {
	Integrity    maintenanceLine `json:"integrity"`
	IntegrityWhy maintenanceNote `json:"integrityWhy"`
	GC           maintenanceLine `json:"gc"`
	GCWhy        maintenanceNote `json:"gcWhy"`
}

// TestTheMaintenanceLinesSayWhenASweepRefuses runs the shipped
// renderJobCards under node over the /api/jobs payloads the handler serves
// for the variant integrity watcher and the orphan sidecar GC, each off,
// on, and refusing as each kind, and reads the two lines and their
// warnings. The watcher's line said "on" while every tick refused until
// 2026-09-29 (backlog B131); the orphan GC's is the control that the
// shared renderer (renderMaintenanceLine) paints it as before.
func TestTheMaintenanceLinesSayWhenASweepRefuses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	since := time.Now().Add(-2 * time.Hour)
	var variant integrity.VariantSweepStatus
	var orphan integrity.OrphanSweepStatus
	srv.deps.VariantSweepStatus = func() integrity.VariantSweepStatus { return variant }
	srv.deps.OrphanSweepStatus = func() integrity.OrphanSweepStatus { return orphan }
	intervals := func(integritySec, gcSec int) {
		next := config.Clone(srv.deps.CfgHolder.Load())
		next.Integrity.VariantSweepIntervalSec = &integritySec
		next.Integrity.OrphanSidecarSweepIntervalSec = &gcSec
		srv.deps.CfgHolder.Store(next)
	}
	type step struct {
		name                  string
		integritySec, gcSec   int
		variant               integrity.VariantSweepStatus
		orphan                integrity.OrphanSweepStatus
		wantIntegrity, wantGC string // the line's text
		integrityWhy, gcWhy   string // a phrase the warning holds; "" = hidden
	}
	steps := []step{
		{name: "both on, neither refusing", integritySec: 3600, gcSec: 3600,
			wantIntegrity: "on", wantGC: "on"},
		{name: "the watcher refuses a relocation", integritySec: 3600, gcSec: 3600,
			variant:       integrity.VariantSweepStatus{Refusing: integrity.VariantRefusalRelocation, Since: since},
			wantIntegrity: "refusing since 2h ago", wantGC: "on",
			integrityWhy: "Variant integrity is refusing. More of the catalog's renditions are missing"},
		{name: "the watcher skips an unmounted variants directory", integritySec: 3600, gcSec: 0,
			variant:       integrity.VariantSweepStatus{Refusing: integrity.VariantRefusalVariantsDir, Since: since},
			wantIntegrity: "refusing since 2h ago", wantGC: "off (default)",
			integrityWhy: "Variant integrity is refusing. The variants directory is missing, empty or cannot be read"},
		{name: "both refuse", integritySec: 3600, gcSec: 3600,
			variant:       integrity.VariantSweepStatus{Refusing: integrity.VariantRefusalVariantsDir, Since: since},
			orphan:        integrity.OrphanSweepStatus{Refusing: integrity.OrphanRefusalMassOrphans, Since: since},
			wantIntegrity: "refusing since 2h ago", wantGC: "refusing since 2h ago",
			integrityWhy: "The variants directory is missing", gcWhy: "Orphan sidecar GC is refusing. The variant catalog is far smaller"},
		{name: "the watcher off, its latch left refusing", integritySec: 0, gcSec: 3600,
			variant:       integrity.VariantSweepStatus{Refusing: integrity.VariantRefusalRelocation, Since: since},
			wantIntegrity: "off (integrity.variantSweepIntervalSec: 0)", wantGC: "on"},
	}
	var payloads []json.RawMessage
	for _, s := range steps {
		variant, orphan = s.variant, s.orphan
		intervals(s.integritySec, s.gcSec)
		var jobs json.RawMessage
		if code := doJSON(t, h, "GET", "/api/jobs", nil, &jobs); code != http.StatusOK {
			t.Fatalf("%s: /api/jobs answered %d", s.name, code)
		}
		payloads = append(payloads, jobs)
	}
	stubs := map[string]string{
		"renderAnalysisCoverage": `function renderAnalysisCoverage() {}`,
		"describeAnalysisSweep":  `function describeAnalysisSweep() { return "—"; }`,
	}
	out := runNodeHarness(t, node, consoleCardsHarness(t, stubs, "renderJobCards")+maintenanceLinesRun, payloads)
	var got []maintenanceLinesStep
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(steps) {
		t.Fatalf("the harness printed %q (err %v), want %d steps", out, err, len(steps))
	}
	for i, s := range steps {
		requireMaintenanceLine(t, s.name+", variant integrity", got[i].Integrity, got[i].IntegrityWhy, s.wantIntegrity, s.integrityWhy)
		requireMaintenanceLine(t, s.name+", orphan sidecar GC", got[i].GC, got[i].GCWhy, s.wantGC, s.gcWhy)
	}
}

// requireMaintenanceLine checks one painted line and its warning: the
// line's text, a "badge warn" on it exactly when it reads as refusing, and
// the warning shown holding why when there is a why, hidden and empty when
// there is none.
func requireMaintenanceLine(t *testing.T, what string, line maintenanceLine, note maintenanceNote, wantText, why string) {
	t.Helper()
	if line.Text != wantText {
		t.Errorf("%s: the line reads %q, want %q", what, line.Text, wantText)
	}
	refusing := strings.HasPrefix(wantText, "refusing")
	if got := line.Badge == "badge warn"; got != refusing {
		t.Errorf("%s: the line's badge is %q, want a warn badge: %v", what, line.Badge, refusing)
	}
	switch {
	case why == "" && (!note.Hidden || note.Text != ""):
		t.Errorf("%s: the warning is hidden=%v with %q, want it hidden and empty", what, note.Hidden, note.Text)
	case why != "" && (note.Hidden || !strings.Contains(note.Text, why)):
		t.Errorf("%s: the warning is hidden=%v with %q, want it shown, holding %q", what, note.Hidden, note.Text, why)
	}
}
