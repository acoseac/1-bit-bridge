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
	"time"
)

// The Updates panel's rollback row (backlog B707): where the rollback would
// refuse to put back the backup beside the binary, the panel offers no Roll
// back and says why, as it says why an update is held. And a Check now whose
// request fails (backlog B720): the panel keeps every row as the last status
// rendered it and shows the error beside them.

// rollbackUnavailable is the badge the rollback row's reason follows.
const rollbackUnavailable = "unavailable"

// The reasons these tests carry, in the rollback's own words for each of its
// refusals: the version installed opened the data dir, and the install marker
// cannot be read (with an apostrophe, which both renderers escape); and the
// auto-installer's reason for holding an update back.
const (
	reasonOpened     = "0.2.4 has opened the data dir since the install, so its store may hold changes 0.2.3 does not read, and a rollback would be a downgrade past them"
	reasonUnreadable = "whether a version other than the backup's has opened the data dir cannot be told, since the install marker cannot be read"
	reasonHeld       = "would orphan device(s): Kitchen iPad (1-bit 2.0)"
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

// updatesPanelRun is the Updates panel as the Settings page ships it (Roll
// back and every row hidden, each row's label the element before it, Last
// check "never", Check now, and the actions span Install & restart is made
// in; no Jobs line), and a run that wires the panel as initSettings does
// (wireUpdatePanel), opens the stream as every page does (startEventStream,
// over an EventSource the run holds), and takes each step in turn: an updates
// frame handed to the stream's own updates listener, or a click on Check now
// whose POST, through the console's API, gets the answer the step names from
// fetch. After each step it prints what the panel shows and the requests the
// step made. Date.now is fixed, so Last check reads the same in every run.
// API is the shipped const, put before this run by runUpdatesPanel: a click
// made here would find a const declared after the run in its dead zone.
const updatesPanelRun = `
class PanelEl {
  constructor(id, label) {
    this.id = id; this.hidden = false; this.innerHTML = ""; this.textContent = "";
    this.previousElementSibling = label || null;
    this.dataset = {}; this.disabled = false; this.isConnected = true; this.listeners = {};
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  async click() { for (const fn of this.listeners.click || []) await fn(); }
  remove() { if (panelEls.get(this.id) === this) panelEls.delete(this.id); this.isConnected = false; }
  appendChild(child) { return child; }
  focus() {}
}
const panelEls = new Map();
const panelAdd = (el) => { panelEls.set(el.id, el); return el; };
panelAdd(new PanelEl("update-status"));
panelAdd(new PanelEl("update-last-check")).textContent = "never";
panelAdd(new PanelEl("update-check")).textContent = "Check now";
panelAdd(new PanelEl("update-rollback")).hidden = true;
for (const id of ["update-last-error", "update-deferred", "update-rollback-refused"]) {
  const label = new PanelEl(id + "-label");
  label.hidden = true;
  panelAdd(new PanelEl(id, label)).hidden = true;
}
const panelActions = new PanelEl("update-actions");
panelActions.dataset.supervised = "true";
panelActions.firstChild = null;
panelActions.insertBefore = (node) => panelAdd(node);
globalThis.document = {
  getElementById: (id) => panelEls.get(id) ?? null,
  querySelector: (sel) => (sel === ".panel-head .panel-actions" ? panelActions : null),
  createElement: () => new PanelEl(""),
  createTextNode: (data) => ({ data }),
  activeElement: null,
};
const panelNow = Date.parse("2026-11-04T12:00:00Z");
Date.now = () => panelNow;
const panelStreams = [];
globalThis.EventSource = class {
  constructor(url) { this.url = url; this.listeners = {}; panelStreams.push(this); }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
};
let activeEventSource = null;
let lastEventSourceSeenAt = 0;
let panelAnswer = null;
const panelPosts = [];
globalThis.fetch = async (url, init) => {
  panelPosts.push((init?.method ?? "GET") + " " + url);
  if (panelAnswer.network) throw new TypeError(panelAnswer.network);
  return {
    ok: panelAnswer.status >= 200 && panelAnswer.status < 300,
    status: panelAnswer.status,
    statusText: panelAnswer.statusText ?? "",
    json: async () => JSON.parse(panelAnswer.body),
  };
};
const panelText = (h) => h.replace(/<[^>]*>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">")
  .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&").replace(/\s+/g, " ").trim();
const panelRow = (id) => {
  const row = panelEls.get(id);
  return { hidden: row.hidden, labelHidden: row.previousElementSibling.hidden, text: panelText(row.innerHTML) };
};
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
wireUpdatePanel();
startEventStream();
const out = [];
for (const step of input.steps) {
  panelPosts.length = 0;
  if (step.frame) {
    for (const fn of panelStreams[0].listeners.updates || []) fn({ data: step.frame });
  }
  if (step.check) {
    panelAnswer = step.check;
    await panelEls.get("update-check").click();
  }
  const check = panelEls.get("update-check");
  out.push({
    status: panelText(panelEls.get("update-status").innerHTML),
    lastCheck: panelEls.get("update-last-check").textContent,
    install: panelEls.has("update-install"),
    rollbackHidden: panelEls.get("update-rollback").hidden,
    lastError: panelRow("update-last-error"),
    deferred: panelRow("update-deferred"),
    refusal: panelRow("update-rollback-refused"),
    checkButton: check.textContent + (check.disabled ? " (disabled)" : ""),
    posts: panelPosts.join(" | "),
  });
}
console.log(JSON.stringify(out));
`

// updatesPanelStep is one thing that happens to the Updates panel: an updates
// frame the stream delivers (its data, as the server marshals the status), or
// a click on Check now and the answer its POST gets. A step with neither
// reads the panel as it stands.
type updatesPanelStep struct {
	Frame string            `json:"frame,omitempty"`
	Check *updatesPanelPOST `json:"check,omitempty"`
}

// updatesPanelPOST is how Check now's POST ends: with no answer (Network, the
// message of the TypeError fetch rejects with), or with an answer's status
// and body.
type updatesPanelPOST struct {
	Network    string `json:"network,omitempty"`
	Status     int    `json:"status,omitempty"`
	StatusText string `json:"statusText,omitempty"`
	Body       string `json:"body,omitempty"`
}

// updatesPanelRow is one of the panel's conditional rows: the value, its
// label, and the value's text as a reader sees it.
type updatesPanelRow struct {
	Hidden      bool   `json:"hidden"`
	LabelHidden bool   `json:"labelHidden"`
	Text        string `json:"text"`
}

// updatesPanelShown is what the Updates panel shows after a step, and the
// requests the step made.
type updatesPanelShown struct {
	Status         string          `json:"status"`
	LastCheck      string          `json:"lastCheck"`
	Install        bool            `json:"install"`
	RollbackHidden bool            `json:"rollbackHidden"`
	LastError      updatesPanelRow `json:"lastError"`
	Deferred       updatesPanelRow `json:"deferred"`
	Refusal        updatesPanelRow `json:"refusal"`
	CheckButton    string          `json:"checkButton"`
	Posts          string          `json:"posts"`
}

// rows is what a step shows but for the Last error row and the requests it
// made: the rows a Check now that fails must leave as the last status
// rendered them.
func (p updatesPanelShown) rows() updatesPanelShown {
	p.LastError = updatesPanelRow{}
	p.Posts = ""
	return p
}

// updatesFrame is the data of the updates frame the stream sends for st:
// st's json.Marshal, as marshalAndPublish writes it.
func updatesFrame(t *testing.T, st UpdateStatus) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkNowAnswer is the answer POST /api/updates/check gives on a console
// whose updater's check returns st, or with no updater (st nil) the 503 the
// handler refuses with. It is taken from the handler itself, so the run's
// fetch hands the console the bytes the bridge sends.
func checkNowAnswer(t *testing.T, st *UpdateStatus) *updatesPanelPOST {
	t.Helper()
	srv, _, _ := newTestServer(t)
	srv.deps.Updater = nil
	if st != nil {
		srv.deps.Updater = stubUpdater{st: *st}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/updates/check", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &updatesPanelPOST{Status: resp.StatusCode, StatusText: http.StatusText(resp.StatusCode), Body: string(body)}
}

// withReachedFunctions appends to a script the top-level functions of app.js
// that the entry points reach by bare calls, transitively, as
// consoleCardsHarness does for the Jobs cards, over a harness of the caller's
// own: the run executes whatever the shipped handlers call, by whatever name.
// A name the script declares itself is its stub.
func withReachedFunctions(t *testing.T, script, src string, entries ...string) string {
	t.Helper()
	var out strings.Builder
	out.WriteString(script)
	seen := map[string]bool{}
	queue := append([]string(nil), entries...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] || strings.Contains(script, "function "+name+"(") {
			continue
		}
		seen[name] = true
		body, ok := extractJSFunctionIfPresent(src, name)
		if !ok {
			continue
		}
		out.WriteString(body + "\n")
		for _, m := range jsCallRe.FindAllStringSubmatch(body, -1) {
			queue = append(queue, m[2])
		}
	}
	return out.String()
}

// runUpdatesPanel runs updatesPanelRun under node over the steps, after the
// console's API, with every top-level function of app.js that the panel's
// wiring (wireUpdatePanel), the stream (startEventStream) and API's reading
// of a refusal (errorFromResponse) reach, and returns what the panel showed
// after each step.
func runUpdatesPanel(t *testing.T, node string, steps []updatesPanelStep) []updatesPanelShown {
	t.Helper()
	src := readConsoleJS(t, "static/app.js")
	script := withReachedFunctions(t, extractJSConst(t, src, "API")+updatesPanelRun, src,
		"wireUpdatePanel", "startEventStream", "errorFromResponse")
	raw := runScriptUnderNode(t, node, script, map[string]any{"steps": steps})
	var got []updatesPanelShown
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(steps) {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	return got
}

// TestTheUpdatesPanelShowsRollBackOrWhyNot runs the shipped panel under node,
// where node is installed, over statuses one after another, each an updates
// frame handed to the stream's own listener (startEventStream, safeApply,
// renderUpdateTile): a rollback refused (Roll back hidden, the row and its
// label shown with the reason after its badge, its apostrophe read back from
// the escape), then offered (Roll back shown, the row hidden and emptied),
// refused again, refused with a reason that carries markup (shown as text: an
// unescaped reason loses its tags to the page), and no backup (nothing
// shown). Each status travels through json.Marshal of UpdateStatus, so a Go
// tag and a JS read that disagree about a name fail here too.
func TestTheUpdatesPanelShowsRollBackOrWhyNot(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type shown struct {
		ButtonHidden bool
		RowHidden    bool
		LabelHidden  bool
		Text         string
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
		{"refused: a reason that carries markup",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", RollbackRefusal: reasonOpened + " <b>&</b>"},
			shown{ButtonHidden: true, Text: rollbackUnavailable + " " + reasonOpened + " <b>&</b>"}},
		{"no backup",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable"},
			shown{ButtonHidden: true, RowHidden: true, LabelHidden: true}},
	}
	run := make([]updatesPanelStep, 0, len(steps))
	for _, s := range steps {
		run = append(run, updatesPanelStep{Frame: updatesFrame(t, s.st)})
	}
	got := runUpdatesPanel(t, node, run)
	for i, s := range steps {
		g := got[i]
		if seen := (shown{g.RollbackHidden, g.Refusal.Hidden, g.Refusal.LabelHidden, g.Refusal.Text}); seen != s.want {
			t.Errorf("%s: Roll back and the rollback row show %+v, want %+v", s.name, seen, s.want)
		}
	}
}

// TestACheckNowThatFailsKeepsTheRowsTheLastStatusRendered runs the shipped
// Check now click and updates stream under node, where node is installed
// (backlog B720). A Check now whose request fails, with no answer (fetch's
// TypeError, in Chrome's words and in Safari's) or with an answer that is not
// a 2xx (the handler's own 503 with no updater, and a 500 whose message
// carries markup, shown as text), leaves every row as the last status
// rendered it: the first paint, before any frame; Roll back and a Held
// update; the Rollback row; and a status's own Last error, whose row the
// click's error takes. It shows the error in the Last error row, with its
// label, at once. The stream sends an updates frame only when the status's
// bytes change (TestEventsStreamDiffSuppression), so a row the click hid
// would stay hidden while the status stood. The next status rendered writes
// the row from what it carries: a frame delivered again, as a reconnect's
// first frame brings it, and a Check now answered 200, whose status renders
// whole.
func TestACheckNowThatFailsKeepsTheRowsTheLastStatusRendered(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	checked := time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)
	offered := UpdateStatus{
		CurrentVersion: "0.2.4", LatestVersion: "0.2.5", UpdateAvailable: true, Channel: "stable",
		LastCheck: &checked, CanInstall: true, CanRollback: true, DeferredReason: reasonHeld,
	}
	refused := offered
	refused.CanRollback, refused.RollbackRefusal = false, reasonOpened
	pollFailed := refused
	pollFailed.LastError = "github: 403 rate limit exceeded"

	const failed = "Check now failed: "
	const post = "POST /api/updates/check"
	shownAs := func(text string) updatesPanelRow { return updatesPanelRow{Text: text} }
	none := updatesPanelRow{Hidden: true, LabelHidden: true}
	// like is the step whose rows a step shows; -1 for a status's own, which
	// the checks after the run read.
	steps := []struct {
		name      string
		step      updatesPanelStep
		like      int
		lastError updatesPanelRow
		posts     string
	}{
		{"0 the first paint", updatesPanelStep{}, -1, none, ""},
		{"1 a request that fails before any frame",
			updatesPanelStep{Check: &updatesPanelPOST{Network: "Failed to fetch"}}, 0, shownAs(failed + "Failed to fetch"), post},
		{"2 a frame: Roll back offered, an update held",
			updatesPanelStep{Frame: updatesFrame(t, offered)}, -1, none, ""},
		{"3 a request that fails beside Roll back and the Held update",
			updatesPanelStep{Check: &updatesPanelPOST{Network: "Failed to fetch"}}, 2, shownAs(failed + "Failed to fetch"), post},
		{"4 the same status delivered again",
			updatesPanelStep{Frame: updatesFrame(t, offered)}, 2, none, ""},
		{"5 a frame: the rollback refused, an update held",
			updatesPanelStep{Frame: updatesFrame(t, refused)}, -1, none, ""},
		{"6 an answer that refuses: no updater",
			updatesPanelStep{Check: checkNowAnswer(t, nil)}, 5, shownAs(failed + errMsgUpdaterNotConfig), post},
		{"7 an answer whose message carries markup",
			updatesPanelStep{Check: &updatesPanelPOST{Status: http.StatusInternalServerError, StatusText: "Internal Server Error",
				Body: `{"error":"boom","message":"<b>&</b> went wrong"}`}}, 5, shownAs(failed + "<b>&</b> went wrong"), post},
		{"8 a frame: the poll failed upstream",
			updatesPanelStep{Frame: updatesFrame(t, pollFailed)}, 5, shownAs(pollFailed.LastError), ""},
		{"9 a request that fails beside the status's own Last error",
			updatesPanelStep{Check: &updatesPanelPOST{Network: "Load failed"}}, 8, shownAs(failed + "Load failed"), post},
		{"10 the same status delivered again",
			updatesPanelStep{Frame: updatesFrame(t, pollFailed)}, 8, shownAs(pollFailed.LastError), ""},
		{"11 a Check now answered 200",
			updatesPanelStep{Check: checkNowAnswer(t, &offered)}, 2, none, post},
	}
	run := make([]updatesPanelStep, 0, len(steps))
	for _, s := range steps {
		run = append(run, s.step)
	}
	got := runUpdatesPanel(t, node, run)

	for i, s := range steps {
		g := got[i]
		if s.like >= 0 && g.rows() != got[s.like].rows() {
			t.Errorf("%s: the panel shows %+v, want the rows of step %d, %+v", s.name, g.rows(), s.like, got[s.like].rows())
		}
		if g.LastError != s.lastError {
			t.Errorf("%s: the Last error row shows %+v, want %+v", s.name, g.LastError, s.lastError)
		}
		if g.Posts != s.posts {
			t.Errorf("%s: the step requested %q, want %q", s.name, g.Posts, s.posts)
		}
		if g.CheckButton != "Check now" {
			t.Errorf("%s: Check now reads %q after the step", s.name, g.CheckButton)
		}
	}
	// The rows each status renders, which the steps above hold the others to.
	firstPaint := updatesPanelShown{LastCheck: "never", RollbackHidden: true, LastError: none, Deferred: none, Refusal: none, CheckButton: "Check now"}
	if got[0] != firstPaint {
		t.Errorf("the first paint shows %+v, want %+v", got[0], firstPaint)
	}
	held := shownAs("deferred " + reasonHeld)
	for _, c := range []struct {
		step    int
		refused bool
	}{{2, false}, {5, true}} {
		g := got[c.step]
		if !strings.Contains(g.Status, "update available") || !strings.Contains(g.Status, "0.2.5") ||
			g.LastCheck != "1h ago" || !g.Install || g.Deferred != held {
			t.Errorf("step %d: the panel shows %+v, want the update offered, Install, Last check 1h ago and the update held", c.step, g)
		}
		if c.refused && (!g.RollbackHidden || g.Refusal != shownAs(rollbackUnavailable+" "+reasonOpened)) {
			t.Errorf("step %d: Roll back hidden %v and the Rollback row %+v, want hidden and the refusal", c.step, g.RollbackHidden, g.Refusal)
		}
		if !c.refused && (g.RollbackHidden || g.Refusal != none) {
			t.Errorf("step %d: Roll back hidden %v and the Rollback row %+v, want Roll back offered and no row", c.step, g.RollbackHidden, g.Refusal)
		}
	}
}

// TestACheckNowThatFailsRendersNoStatus pins, without node, the Check now
// click's failure path: its catch shows the error through
// showUpdateCheckFailure and renders no status (renderUpdateTile writes every
// row from the status it is given, so a partial one hides every row it
// omits), while its answer still renders whole; and showUpdateCheckFailure
// reaches the Last error row alone, writes the error through escapeHTML, and
// reveals the row with its label.
// TestACheckNowThatFailsKeepsTheRowsTheLastStatusRendered runs the click and
// the stream under node where node is installed.
func TestACheckNowThatFailsRendersNoStatus(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	wire := stripJSComments(extractJSFunction(t, src, "wireUpdatePanel"))
	at := strings.Index(wire, `document.getElementById("update-check")`)
	if at < 0 {
		t.Fatal("wireUpdatePanel no longer looks Check now up")
	}
	handler := wire[at:]
	if end := strings.Index(handler, "bindInstallButton("); end >= 0 {
		handler = handler[:end]
	}
	catchAt := strings.Index(handler, "} catch (err) {")
	finallyAt := strings.Index(handler, "} finally {")
	if catchAt < 0 || finallyAt < catchAt {
		t.Fatalf("Check now's handler has no catch before its finally:\n%s", handler)
	}
	answered, failed := handler[:catchAt], handler[catchAt:finallyAt]
	if !strings.Contains(answered, "renderUpdateTile(u);") {
		t.Error("Check now's answer no longer renders the status it carries")
	}
	if strings.Contains(failed, "renderUpdateTile(") {
		t.Error("a Check now whose request fails renders a status: renderUpdateTile writes every row from it, " +
			"and the stream may send no frame that puts the rows it omits back")
	}
	if !strings.Contains(failed, "showUpdateCheckFailure(err?.message);") {
		t.Error("a Check now whose request fails does not show its error through showUpdateCheckFailure")
	}

	body, ok := extractJSFunctionIfPresent(src, "showUpdateCheckFailure")
	if !ok {
		t.Fatal("app.js has no showUpdateCheckFailure")
	}
	body = stripJSComments(body)
	ids := regexp.MustCompile(`getElementById\("([^"]*)"\)`).FindAllStringSubmatch(body, -1)
	if len(ids) != 1 || ids[0][1] != "update-last-error" {
		t.Errorf("showUpdateCheckFailure reaches %v, want the Last error row (update-last-error) alone", ids)
	}
	for _, want := range []string{
		"${escapeHTML(message)}",
		"lastError.hidden = false;",
		"const dt = lastError.previousElementSibling;",
		"if (dt) dt.hidden = false;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("showUpdateCheckFailure has no %s", want)
		}
	}
	if strings.Contains(body, "renderUpdateTile(") {
		t.Error("showUpdateCheckFailure renders a status")
	}
}
