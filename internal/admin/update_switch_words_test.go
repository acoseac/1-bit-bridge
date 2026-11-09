package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The console's words for the update check's switch (backlog B761's round 3):
// an off a save sent and got no answer for may be lost with a stop of the
// bridge, and every claim that the rollbacks run with the checks off says
// what the key an off writes costs a release from before the switch.

// TestTheConsolesSavesSayAnOffThatGotNoAnswerMayBeLost pins, without node,
// that both of the console's saves of the switch, the Settings form's and a
// tray's, add updateChecksOffLost's words to their failure, with the body they
// sent, and that the words say what was lost and why: an install's swap,
// which on Windows stops the bridge, and the off to be turned off again. Under
// node, TestAnOffThatGotNoAnswerIsSaidMayBeLostOnlyThen runs the function.
func TestTheConsolesSavesSayAnOffThatGotNoAnswerMayBeLost(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	settings := stripJSComments(extractJSFunction(t, src, "initSettings"))
	if !strings.Contains(settings, `showMsg(msg, "err", "Save failed: " + err.message + updateChecksOffLost(body, err));`) {
		t.Error("the Settings form's failed save does not say an off that got no answer may be lost")
	}
	tray := stripJSComments(extractJSFunction(t, src, "saveTrayField"))
	if !strings.Contains(tray, `updateChecksOffLost({ [ctl.row.field]: value }, err);`) {
		t.Error("a tray's failed save does not say an off that got no answer may be lost")
	}
	fn := stripJSComments(extractJSFunction(t, src, "updateChecksOffLost"))
	if !strings.Contains(fn, `if (sent?.updateCheckEnabled !== false || err?.status) return "";`) {
		t.Error("updateChecksOffLost does not keep its words to an off that got no answer")
	}
	for _, says := range []string{"bridge update", "on Windows stops", "update checks were not turned off", "turn them off again"} {
		if !strings.Contains(fn, says) {
			t.Errorf("updateChecksOffLost does not say %q", says)
		}
	}
}

// TestEveryRollbackClaimOfTheSwitchSaysWhatTheKeyCosts: the Jobs card's tray
// and the Settings page say that Roll back and a start's rollback run with
// update checks off, and each says too that a release from before the switch
// does not start with the key an off writes, and how to keep the checks off
// without it: the variable in the service's environment, the key out of
// bridge.yaml, a restart, the older release ignoring the variable (B761's
// round 3: the tray's hint claimed the rollbacks with no caveat).
func TestEveryRollbackClaimOfTheSwitchSaysWhatTheKeyCosts(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	i := strings.Index(src, `field: "updateCheckEnabled", type: "switch"`)
	if i < 0 {
		t.Fatal("the Jobs card's tray offers no update check switch")
	}
	hint := src[i:]
	hint = hint[:strings.Index(hint, "},")]
	hint = strings.Join(strings.Fields(regexp.MustCompile(`"\s*\+\s*"`).ReplaceAllString(hint, "")), " ")
	settings, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(settings)
	j := strings.Index(page, `name="updateCheckEnabled"`)
	if j < 0 {
		t.Fatal("the Settings page offers no update check switch")
	}
	page = page[j:]
	page = strings.Join(strings.Fields(page[:strings.Index(page, "</small>")]), " ")
	for where, text := range map[string]string{"the Jobs card's tray": hint, "the Settings page": page} {
		if !strings.Contains(text, "a start's rollback") {
			t.Errorf("%s no longer claims a start's rollback runs with the checks off; this test pins the caveat beside it", where)
		}
		for _, says := range []string{"release from before this switch", "BRIDGE_UPDATE_CHECK_ENABLED=false",
			"in the service's environment", "out of bridge.yaml", "restart", "ignores the variable"} {
			if !strings.Contains(text, says) {
				t.Errorf("%s does not say %q beside its rollback claim:\n%s", where, says, text)
			}
		}
	}
}

// TestAnOffThatGotNoAnswerIsSaidMayBeLostOnlyThen runs the shipped
// updateChecksOffLost under node, where node is installed: an off that got no
// answer (fetch's TypeError, which carries no status) gets its words; an off
// the bridge answered (an error with a status, the 503 of a stop among them),
// an on, and a save that did not send the switch get none.
func TestAnOffThatGotNoAnswerIsSaidMayBeLostOnlyThen(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	fn := extractJSFunction(t, readConsoleJS(t, "static/app.js"), "updateChecksOffLost")
	script := fn + `
const noAnswer = new TypeError("Failed to fetch");
const answered = Object.assign(new Error("update checks were not turned off: the save was stopped"), { status: 503 });
console.log(JSON.stringify([
  updateChecksOffLost({ updateCheckEnabled: false }, noAnswer),
  updateChecksOffLost({ updateCheckEnabled: false, libraryName: "x" }, noAnswer),
  updateChecksOffLost({ updateCheckEnabled: false }, answered),
  updateChecksOffLost({ updateCheckEnabled: true }, noAnswer),
  updateChecksOffLost({ libraryName: "x" }, noAnswer),
  updateChecksOffLost(undefined, noAnswer),
]));
`
	var got []string
	if err := json.Unmarshal(runNodeHarness(t, node, script, nil), &got); err != nil || len(got) != 6 {
		t.Fatalf("the harness printed %q (%v)", got, err)
	}
	for i, says := range []bool{true, true, false, false, false, false} {
		if (got[i] != "") != says || (says && !strings.Contains(got[i], "update checks were not turned off")) {
			t.Errorf("case %d: updateChecksOffLost said %q, want words %v", i, got[i], says)
		}
	}
}
