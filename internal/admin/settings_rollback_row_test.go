package admin

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The Updates panel's rollback row (backlog B707): where the rollback would
// refuse to put back the backup beside the binary, the panel offers no Roll
// back and says why, as it says why an update is held.

// rollbackUnavailable is the badge the rollback row's reason follows.
const rollbackUnavailable = "unavailable"

// The reasons these tests carry, in the rollback's own words for each of its
// refusals: the version installed opened the data dir, and the install marker
// cannot be read (with an apostrophe, which both renderers escape).
const (
	reasonOpened     = "0.2.4 has opened the data dir since the install, so its store may hold changes 0.2.3 does not read, and a rollback would be a downgrade past them"
	reasonUnreadable = "whether a version other than the backup's has opened the data dir cannot be told, since the install marker cannot be read"
)

// renderSettingsWith renders the Settings page over an updater whose status
// is st.
func renderSettingsWith(t *testing.T, st UpdateStatus) string {
	t.Helper()
	srv, _, _ := newTestServer(t)
	srv.deps.Updater = stubUpdater{st: st}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d", resp.StatusCode)
	}
	return string(raw)
}

// pageText is a fragment of a page as a reader sees it: its tags dropped, its
// entities read, its white space one space.
func pageText(fragment string) string {
	return strings.Join(strings.Fields(html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(fragment, ""))), " ")
}

// TestTheSettingsPageSaysWhyRollBackIsNotOffered: the Settings page's first
// paint shows the status's rollback refusal in the Updates panel's rollback
// row, as text after its badge, beside a hidden Roll back; a status that
// offers the rollback leaves the row empty, and the status frame reveals the
// button (renderUpdateTile).
func TestTheSettingsPageSaysWhyRollBackIsNotOffered(t *testing.T) {
	ddRe := regexp.MustCompile(`(?s)<dd id="update-rollback-refused"[^>]*>(.*?)</dd>`)
	for _, tc := range []struct {
		name string
		st   UpdateStatus
		want string
	}{
		{"the version installed opened the data dir",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", RollbackRefusal: reasonOpened},
			rollbackUnavailable + " " + reasonOpened},
		{"an install marker that cannot be read",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", RollbackRefusal: reasonUnreadable},
			rollbackUnavailable + " " + reasonUnreadable},
		{"a rollback that would run",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", CanRollback: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderSettingsWith(t, tc.st)
			m := ddRe.FindStringSubmatch(body)
			if m == nil {
				t.Fatal("the Settings page has no rollback row")
			}
			if got := pageText(m[1]); got != tc.want {
				t.Errorf("the rollback row says %q, want %q", got, tc.want)
			}
			if !strings.Contains(body, `id="update-rollback" class="btn" hidden`) {
				t.Error("Roll back is not hidden at first paint")
			}
		})
	}
}

// TestTheUpdatesPanelScriptShowsWhyRollBackIsNotOffered pins, without node,
// renderUpdateTile's reading of the status's rollback fields: Roll back is
// revealed from canRollback alone, and the rollback row shows rollbackRefusal
// after its badge, through escapeHTML, with its label, and hides both without
// one. TestTheUpdatesPanelShowsRollBackOrWhyNot runs the function under node
// where node is installed.
func TestTheUpdatesPanelScriptShowsWhyRollBackIsNotOffered(t *testing.T) {
	body := stripJSComments(extractJSFunction(t, readConsoleJS(t, "static/app.js"), "renderUpdateTile"))
	for _, want := range []string{
		`if (rollbackBtn) rollbackBtn.hidden = !u?.canRollback;`,
		`const refusal = document.getElementById("update-rollback-refused");`,
		`if (u?.rollbackRefusal) {`,
		"refusal.innerHTML = `<span class=\"badge idle\">" + rollbackUnavailable + "</span> ${escapeHTML(u.rollbackRefusal)}`;",
		`const dt = refusal.previousElementSibling;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderUpdateTile has no %s", want)
		}
	}
}

// updatesPanelRun is the DOM renderUpdateTile reaches, as the Settings page
// ships it (Roll back and every row hidden, each row's label the element
// before it; no Jobs line, no last-check cell and no actions, which this test
// does not read), and a run that renders each step's status in turn and
// prints what Roll back and the rollback row show.
const updatesPanelRun = `
class El {
  constructor(id, prev) { this.id = id; this.hidden = false; this.innerHTML = ""; this.previousElementSibling = prev || null; }
}
const els = new Map();
els.set("update-status", new El("update-status"));
els.set("update-rollback", new El("update-rollback"));
els.get("update-rollback").hidden = true;
for (const id of ["update-last-error", "update-deferred", "update-rollback-refused"]) {
  const label = new El(id + "-label");
  label.hidden = true;
  const row = new El(id, label);
  row.hidden = true;
  els.set(id, row);
}
globalThis.document = {
  getElementById: (id) => els.get(id) ?? null,
  querySelector: () => null,
};
const text = (h) => h.replace(/<[^>]*>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">")
  .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&").replace(/\s+/g, " ").trim();
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const step of input.steps) {
  renderUpdateTile(step.status);
  const row = els.get("update-rollback-refused");
  out.push({
    buttonHidden: els.get("update-rollback").hidden,
    rowHidden: row.hidden,
    labelHidden: row.previousElementSibling.hidden,
    text: text(row.innerHTML),
  });
}
console.log(JSON.stringify(out));
`

// TestTheUpdatesPanelShowsRollBackOrWhyNot runs the shipped renderUpdateTile
// under node, where node is installed, over statuses one after another, as
// the updates frames bring them: a rollback refused (Roll back hidden, the
// row and its label shown with the reason after its badge, its apostrophe
// read back from the escape), then offered (Roll back shown, the row hidden
// and emptied), refused again, and no backup (nothing shown). Each status
// travels through json.Marshal of UpdateStatus, so a Go tag and a JS read
// that disagree about a name fail here too.
func TestTheUpdatesPanelShowsRollBackOrWhyNot(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type shown struct {
		ButtonHidden bool   `json:"buttonHidden"`
		RowHidden    bool   `json:"rowHidden"`
		LabelHidden  bool   `json:"labelHidden"`
		Text         string `json:"text"`
	}
	steps := []struct {
		name string
		st   UpdateStatus
		want shown
	}{
		{"refused: the version installed opened the data dir",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", RollbackRefusal: reasonOpened},
			shown{ButtonHidden: true, Text: rollbackUnavailable + " " + reasonOpened}},
		{"offered",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", CanRollback: true},
			shown{RowHidden: true, LabelHidden: true}},
		{"refused: the install marker cannot be read",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", RollbackRefusal: reasonUnreadable},
			shown{ButtonHidden: true, Text: rollbackUnavailable + " " + reasonUnreadable}},
		{"no backup",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable"},
			shown{ButtonHidden: true, RowHidden: true, LabelHidden: true}},
	}
	var input struct {
		Steps []struct {
			Status UpdateStatus `json:"status"`
		} `json:"steps"`
	}
	for _, s := range steps {
		input.Steps = append(input.Steps, struct {
			Status UpdateStatus `json:"status"`
		}{s.st})
	}
	script := withFunctions(t, updatesPanelRun, readConsoleJS(t, "static/app.js"),
		"renderUpdateTile", "escapeHTML", "isUpdateLastCheckZero")
	raw := runScriptUnderNode(t, node, script, input)
	var got []shown
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(steps) {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for i, s := range steps {
		if got[i] != s.want {
			t.Errorf("%s: Roll back and the rollback row show %+v, want %+v", s.name, got[i], s.want)
		}
	}
}
