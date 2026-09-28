package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// traySnapshotRaceRun drives the shipped traySettingsSnapshot and
// invalidateTraySettings with a settings fetch whose answers the script
// releases by hand, in the orders a drop can meet them: the answer to a
// request made before the drop arriving after the one made after it, or
// before it, or failing while the newer one is out. The plain case, with no
// drop, is the control that the snapshot still caches an answer.
const traySnapshotRaceRun = `
const deferred = () => { let res, rej; const p = new Promise((a, b) => { res = a; rej = b; }); return { p, res, rej }; };
let gets = [];
API.get = () => { const d = deferred(); gets.push(d); return d.p; };
const settle = () => new Promise((r) => setTimeout(r, 0));
const reset = () => { traySettings = null; traySettingsPromise = null; trayManaged = null; gets = []; };
const state = () => ({
  which: traySettings ? traySettings.which : null,
  managed: trayManaged ? [...trayManaged] : null,
  gets: gets.length,
});
const out = {};

reset();
traySettingsSnapshot().catch(() => {});
invalidateTraySettings();
traySettingsSnapshot().catch(() => {});
gets[1].res({ which: "new", managedSettings: ["b"] }); await settle();
gets[0].res({ which: "old", managedSettings: ["a"] }); await settle();
out.oldAnswerLast = state();

reset();
traySettingsSnapshot().catch(() => {});
invalidateTraySettings();
traySettingsSnapshot().catch(() => {});
gets[0].res({ which: "old", managedSettings: ["a"] }); await settle();
out.oldAnswerFirst = state();
gets[1].res({ which: "new", managedSettings: ["b"] }); await settle();
out.thenNewAnswer = state();

reset();
traySettingsSnapshot().catch(() => {});
invalidateTraySettings();
traySettingsSnapshot().catch(() => {});
gets[0].rej(new Error("the old request failed")); await settle();
traySettingsSnapshot().catch(() => {});
out.oldRequestFailed = state();
gets[1].res({ which: "new", managedSettings: ["b"] }); await settle();
out.thenNewAfterFailure = state();

reset();
traySettingsSnapshot().catch(() => {});
gets[0].res({ which: "only", managedSettings: ["m"] }); await settle();
out.noDrop = state();

console.log(JSON.stringify(out));
`

// traySnapshotState is what the snapshot held at one step: which answer it
// cached (nil for none), the managed set it took, and how many settings
// requests had been made.
type traySnapshotState struct {
	Which   *string  `json:"which"`
	Managed []string `json:"managed"`
	Gets    int      `json:"gets"`
}

// TestADroppedTraySnapshotIsNotCachedWhenItsAnswerArrives pins that a
// settings answer requested before invalidateTraySettings is never cached.
//
// A drop is a statement that anything fetched before it is stale: an
// operator page drops the snapshot in its page init, and the player on
// every route. It nulled the promise and left the request running, so its
// answer still wrote traySettings and trayManaged when it landed: over the
// newer answer when it arrived last, or as the new page's snapshot when it
// arrived first. And a failure of the old request nulled the NEWER request's
// promise, so the next tray started a third fetch. (CodeRabbit on #1088.)
func TestADroppedTraySnapshotIsNotCachedWhenItsAnswerArrives(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	src := readFile(t, "static/app.js")
	var script strings.Builder
	script.WriteString(trayHarnessPreamble)
	for _, name := range []string{"invalidateTraySettings", "traySettingsSnapshot"} {
		script.WriteString(extractJSFunction(t, src, name))
		script.WriteString("\n")
	}
	script.WriteString(traySnapshotRaceRun)
	path := filepath.Join(t.TempDir(), "snapshot.mjs")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got map[string]traySnapshotState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	which := func(s traySnapshotState) string {
		if s.Which == nil {
			return "none"
		}
		return *s.Which
	}
	check := func(step, wantWhich, wantManaged string, wantGets int) {
		t.Helper()
		s, ok := got[step]
		if !ok {
			t.Fatalf("%s: the harness reported no state", step)
		}
		if which(s) != wantWhich || strings.Join(s.Managed, ",") != wantManaged || s.Gets != wantGets {
			t.Errorf("%s: the snapshot held %q, managed [%s], after %d settings requests; want %q, [%s], %d",
				step, which(s), strings.Join(s.Managed, ","), s.Gets, wantWhich, wantManaged, wantGets)
		}
	}
	// The control: with no drop, the answer is the snapshot.
	check("noDrop", "only", "m", 1)
	// The answer to the request made before the drop, landing last, must
	// not overwrite the newer one.
	check("oldAnswerLast", "new", "b", 2)
	// Landing first, it must not stand in for the answer the new page waits
	// on; the newer answer is then the snapshot.
	check("oldAnswerFirst", "none", "", 2)
	check("thenNewAnswer", "new", "b", 2)
	// Failing while the newer request is out, it must not drop that
	// request: a caller meanwhile shares it rather than starting a third.
	check("oldRequestFailed", "none", "", 2)
	check("thenNewAfterFailure", "new", "b", 2)
}
