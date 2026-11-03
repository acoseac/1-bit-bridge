package admin

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The Player page's sentences for the cause of the player's last play that
// failed (B676): the DAC another program holds, and the DAC's node this
// process may not open, the second of which names the cause and where the fix
// is said, never a bare failure.
const (
	pageCauseBusy   = "Its last play found the DAC held by another program."
	pageCauseDenied = "Its last play was refused the DAC: the bridge's user may not open the DAC's node, which the audio group owns on most boxes. The checks on the Diagnostics page (bridge doctor) name the user and the group, and the commands that let it open the node."
)

// TestThePlayerPageNamesTheCauseOfAPermissionRefusal pins, without node, the
// status line's reading of running.lastError (B676): device_busy and
// permission_denied each have their sentence, written as text, and the
// permission sentence says what refused the play and where the fix is
// named. TestThePlayerPageSaysWhyItsLastPlayFailed runs the same function
// under node where node is installed.
func TestThePlayerPageNamesTheCauseOfAPermissionRefusal(t *testing.T) {
	body := stripJSComments(extractJSFunction(t, readConsoleJS(t, "static/app.js"), "renderPlayerOutputRunning"))
	for _, want := range []string{
		`if (run.lastError === "device_busy") state += " ` + pageCauseBusy + `";`,
		`else if (run.lastError === "permission_denied") state += " ` + pageCauseDenied + `";`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderPlayerOutputRunning has no %s", want)
		}
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML"} {
		if strings.Contains(body, sink) {
			t.Errorf("renderPlayerOutputRunning writes %s: every string goes in as text", sink)
		}
	}
}

// playerCauseHarnessRun drives the shipped renderPlayerOutputRunning over the
// cases and prints each status line.
const playerCauseHarnessRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const c of input.cases) {
  els.clear();
  renderPlayerOutputRunning(c.running);
  out.push({ name: c.name, state: el("player-output-state").textContent, reason: el("player-output-state").dataset.reason });
}
console.log(JSON.stringify(out));
`

// TestThePlayerPageSaysWhyItsLastPlayFailed runs the shipped
// renderPlayerOutputRunning under node, where node is installed, over a
// running player's last attempt: none, a busy DAC, a node this process may
// not open (B676), and a word of a later release, which says nothing rather
// than guess. The permission sentence is the page's and the doctor names the
// rest; neither sentence shows beside the other.
func TestThePlayerPageSaysWhyItsLastPlayFailed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type harnessCase struct {
		Name    string        `json:"name"`
		Running PlayerRunning `json:"running"`
	}
	stopped := func(last string) PlayerRunning {
		return PlayerRunning{Started: true, State: "stopped", Transport: "stopped", LastError: last}
	}
	cases := []struct {
		c        harnessCase
		has, not []string
	}{
		{harnessCase{"no failure", stopped("")}, nil, []string{pageCauseBusy, pageCauseDenied}},
		{harnessCase{"a busy DAC", stopped("device_busy")}, []string{pageCauseBusy}, []string{pageCauseDenied}},
		{harnessCase{"a node this process may not open", stopped("permission_denied")}, []string{pageCauseDenied}, []string{pageCauseBusy}},
		{harnessCase{"a word of a later release", stopped("some_later_word")}, nil, []string{pageCauseBusy, pageCauseDenied, "some_later_word"}},
	}
	var input struct {
		Cases []harnessCase `json:"cases"`
	}
	for _, c := range cases {
		input.Cases = append(input.Cases, c.c)
	}
	script := withFunctions(t, jobsToolStatePreamble+playerCauseHarnessRun, readConsoleJS(t, "static/app.js"),
		"renderPlayerOutputRunning", "playerOutputText", "playerBindingText", "playerRateText", "playerDSDRateText", "playerTimeText")
	raw := runScriptUnderNode(t, node, script, input)
	var got []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for i, c := range cases {
		for _, s := range c.has {
			if !strings.Contains(got[i].State, s) {
				t.Errorf("%s: the status line %q does not say %q", c.c.Name, got[i].State, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(got[i].State, s) {
				t.Errorf("%s: the status line %q says %q", c.c.Name, got[i].State, s)
			}
		}
	}
}
