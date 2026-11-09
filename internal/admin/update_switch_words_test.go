package admin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The console's words for the update check's switch (backlog B761's round 3):
// an off a save sent and got no answer for may not have been saved, every
// claim that the rollbacks run with the checks off says what the key an off
// writes costs a release from before the switch, and (rounds 4 and 5) what
// stays open for the bridge's Windows service is named only as backlog B831,
// in a sentence of its own.

// TestTheConsolesSavesSayAnOffThatGotNoAnswerMayBeLost pins, without node,
// that both of the console's saves of the switch, the Settings form's and a
// tray's, add updateChecksOffLost's words to their failure, with the body they
// sent, and that the words say the off may not have been saved and what to do,
// claiming no cause: a save that got no answer does not say what stopped it
// (round 4). Under node, TestAnOffThatGotNoAnswerIsSaidMayBeLostOnlyThen runs
// the function.
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
	for _, says := range []string{"may not have been turned off", "once the bridge answers", "check the switch",
		"turn them off again"} {
		if !strings.Contains(fn, says) {
			t.Errorf("updateChecksOffLost does not say %q", says)
		}
	}
	for _, claim := range []string{"Windows", "SCM", "swap", "B831"} {
		if strings.Contains(fn, claim) {
			t.Errorf("updateChecksOffLost claims %q, which a save that got no answer does not show", claim)
		}
	}
}

// TestASwapThatStopsTheWindowsServiceIsNamedOnlyAsB831: an off the console
// saves while a `bridge update` of another process is past its decision waits
// for the install's swap and is saved after it; a swap that stops nothing (a
// Startup-folder install's, or one run without the rights to stop the service)
// leaves the save to land after it (backlog B761's round 4, Codex on round 3).
// What stays open, for the bridge's Windows service, is backlog B831, and
// every text that names it, README, PROTOCOL, the Settings page's hint,
// updateChecksOffLost's comment, the docs of the 503's writer and of
// updater.LockForChecksOff, and the settings matrix's row, names it as the
// bridge's Windows service and only by its id, none says it of Windows as a
// whole, and none says in a sentence that names it what a stop does to a
// save, a semicolon or a colon joining clauses of one sentence (round 5,
// Codex on round 4: LockForChecksOff's doc named it in a clause of the
// sentence that gave that contract). The words a failed save answers (the
// 503's, updateChecksOffLost's) claim no cause, since a stop does not say
// what stopped it.
func TestASwapThatStopsTheWindowsServiceIsNamedOnlyAsB831(t *testing.T) {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	readme := flat(readConsoleJS(t, filepath.Join("..", "..", "README.md")))
	i := strings.Index(readme, "swapping a new version in")
	if i < 0 {
		t.Fatal("README no longer says what an off does while a `bridge update` swaps a new version in")
	}
	readme = readme[max(0, i-400):min(len(readme), i+400)]
	var protocol string
	for _, line := range strings.Split(readConsoleJS(t, filepath.Join("..", "..", "PROTOCOL.md")), "\n") {
		if strings.HasPrefix(line, "**What goes out, and the off switch.**") {
			protocol = line
		}
	}
	page := readConsoleJS(t, filepath.Join("templates", "settings.html"))
	j := strings.Index(page, `name="updateCheckEnabled"`)
	if j < 0 || protocol == "" {
		t.Fatal("the Settings page's update check switch or PROTOCOL's paragraph on it is gone")
	}
	page = flat(page[j : j+strings.Index(page[j:], "</small>")])
	js := readConsoleJS(t, filepath.Join("static", "app.js"))
	k := strings.Index(js, "// updateChecksOffLost is")
	if k < 0 {
		t.Fatal("updateChecksOffLost's comment is gone")
	}
	lostDoc := flat(strings.ReplaceAll(js[k:strings.Index(js, "function updateChecksOffLost(")], "\n//", "\n"))
	refusalDoc, refusalWords := goFuncSource(t, "handlers_api.go", "writeUpdateChecksOffRefusal")
	lockDoc, _ := goFuncSource(t, filepath.Join("..", "updater", "checksoff.go"), "LockForChecksOff")
	var matrix string
	for _, line := range strings.Split(readConsoleJS(t, filepath.Join("..", "..", "ops", "settings-apply-semantics.md")), "\n") {
		if strings.HasPrefix(line, "| `updateCheckEnabled` |") {
			matrix = line
		}
	}
	if matrix == "" {
		t.Fatal("the settings matrix has no row for updateCheckEnabled")
	}

	onWindows := regexp.MustCompile(`(?i)\bon windows\b`)
	// What a stop does to a save: its wait ending, the save's fate, the 503.
	stopsASave := regexp.MustCompile(`(?i)\b(sav(e|es|ed|ing)|nothing|lost|los(e|es|ing)|lands?|ends?|cut short|cancel\w*|wait\w*|503|stopping)\b`)
	for where, text := range map[string]string{
		"README": readme, "PROTOCOL.md": protocol, "the Settings page's hint": page,
		"updateChecksOffLost's comment": lostDoc, "writeUpdateChecksOffRefusal's doc": flat(refusalDoc),
		"updater.LockForChecksOff's doc": flat(lockDoc), "the settings matrix's row": matrix,
	} {
		for _, says := range []string{"Windows service", "backlog B831"} {
			if !strings.Contains(text, says) {
				t.Errorf("%s does not say %q of a swap that stops the bridge's Windows service:\n%s", where, says, text)
			}
		}
		if m := onWindows.FindString(text); m != "" || strings.Contains(text, "lost with the stop") {
			t.Errorf("%s still says of Windows as a whole that an off waiting for a swap is lost:\n%s", where, text)
		}
		for _, sentence := range sentenceEnd.Split(text, -1) {
			if !strings.Contains(sentence, "B831") {
				continue
			}
			if m := stopsASave.FindString(sentence); m != "" {
				t.Errorf("%s names backlog B831 in a sentence that says what a stop does to a save (%q): %q", where, m, sentence)
			}
		}
	}
	for _, claim := range []string{"Windows", "SCM", "swap", "B831"} {
		if strings.Contains(refusalWords, claim) {
			t.Errorf("writeUpdateChecksOffRefusal's words claim %q, which a stopped wait does not show: %q", claim, refusalWords)
		}
	}
}

// sentenceEnd is where one sentence of flattened text ends: a period, a
// question mark or an exclamation mark before white space or the text's end.
// A semicolon or a colon joins clauses of one sentence.
var sentenceEnd = regexp.MustCompile(`[.!?](\s+|$)`)

// goFuncSource answers the doc comment of the function name in the Go file at
// path, and the string literals of its body, unquoted and joined.
func goFuncSource(t *testing.T, path, name string) (doc, words string) {
	t.Helper()
	src := readConsoleJS(t, path)
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != name {
			continue
		}
		if fn.Doc == nil {
			t.Fatalf("%s has no doc comment", name)
		}
		var lits []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", lit.Value, err)
				}
				lits = append(lits, s)
			}
			return true
		})
		return fn.Doc.Text(), strings.Join(lits, "")
	}
	t.Fatalf("no function %s in %s", name, path)
	return "", ""
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
		if (got[i] != "") != says || (says && !strings.Contains(got[i], "may not have been turned off")) {
			t.Errorf("case %d: updateChecksOffLost said %q, want words %v", i, got[i], says)
		}
	}
}
