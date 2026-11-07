package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The Updates panel's Probation row (backlog B660): an install of the
// running version awaits its confirmation, which a start that has answered
// and stayed up makes, and the row says where it stands in the running
// start's own words, its badge the level.

// probationWords are updater.ProbationStatus's words for a running start
// with one start before it that did not end cleanly, as the adapter hands
// them over; the apostrophe is escaped by both renderers.
const probationWords = "0.2.4 is on probation since its install, and 1 of its starts before this one did not end cleanly: " +
	"once 3 have not, the next start puts 0.2.3 back"

// TestTheSettingsPageSaysWhereAnInstallOnProbationStands: the first paint
// shows the status's probation words after a badge naming the level ("on
// probation" for ok, the level otherwise), and nothing where no install
// awaits its confirmation.
func TestTheSettingsPageSaysWhereAnInstallOnProbationStands(t *testing.T) {
	ddRe := regexp.MustCompile(`(?s)<dd id="update-probation"[^>]*>(.*?)</dd>`)
	for _, tc := range []struct {
		name string
		st   UpdateStatus
		want string
	}{
		{"on probation", UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", Probation: probationWords, ProbationLevel: "ok"},
			"on probation " + probationWords},
		{"a warning", UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", Probation: probationWords, ProbationLevel: "warn"},
			"warn " + probationWords},
		{"a failure", UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", Probation: probationWords, ProbationLevel: "fail"},
			"fail " + probationWords},
		{"no install awaiting", UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderSettingsWith(t, tc.st)
			m := ddRe.FindStringSubmatch(body)
			if m == nil {
				t.Fatal("the Settings page has no probation row")
			}
			if got := pageText(m[1]); got != tc.want {
				t.Errorf("the probation row says %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheUpdatesPanelScriptSaysWhereAnInstallOnProbationStands pins, without
// node, renderUpdateTile's reading of the status's probation fields: the row
// shows probation after its badge, the level's word through escapeHTML ("on
// probation" for ok), with its label, and hides both without one.
func TestTheUpdatesPanelScriptSaysWhereAnInstallOnProbationStands(t *testing.T) {
	body := stripJSComments(extractJSFunction(t, readConsoleJS(t, "static/app.js"), "renderUpdateTile"))
	for _, want := range []string{
		`const probation = document.getElementById("update-probation");`,
		`if (u?.probation) {`,
		`const onProbation = u.probationLevel === "ok";`,
		`const badge = onProbation ? "on probation" : escapeHTML(u.probationLevel ?? "");`,
		"probation.innerHTML = `<span class=\"badge ${onProbation ? \"idle\" : \"warn\"}\">${badge}</span> ${escapeHTML(u.probation)}`;",
		`const dt = probation.previousElementSibling;`,
		`probation.hidden = true;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderUpdateTile has no %s", want)
		}
	}
}
