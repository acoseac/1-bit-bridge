package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// playerStatusEntryFn is the app.js function the Player page hands GET
// /api/output/player's answer to, whose one parameter is the status.
const playerStatusEntryFn = "renderPlayerOutput"

// playerStatusFieldPaths walks a status DTO and returns every JSON leaf the
// page is expected to read: it recurses into this package's Player* structs,
// which exist for this page alone, an embedded one at its parent's level as
// encoding/json writes it, and stops at a list, which one read covers (the
// jobs guard's bar for a shared or repeated shape).
func playerStatusFieldPaths(rt reflect.Type, prefix string) []string {
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		tag := f.Tag.Get("json")
		if f.Anonymous && tag == "" {
			out = append(out, playerStatusFieldPaths(ft, prefix)...)
			continue
		}
		if tag == "" || tag == "-" {
			continue
		}
		path := strings.Split(tag, ",")[0]
		if prefix != "" {
			path = prefix + "." + path
		}
		if ft.Kind() == reflect.Struct && strings.HasPrefix(ft.Name(), "Player") {
			out = append(out, playerStatusFieldPaths(ft, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// TestEveryPlayerStatusFieldIsRenderedSomewhere is the Player page's twin of
// TestEveryJobsFieldIsRenderedSomewhere: every leaf GET /api/output/player
// answers is read by the page (rooted at renderPlayerOutput's parameter, its
// helpers' parameters and aliases resolved), so the endpoint computes nothing
// for nobody, and a fact added to it is shown or dropped.
func TestEveryPlayerStatusFieldIsRenderedSomewhere(t *testing.T) {
	paths := playerStatusFieldPaths(reflect.TypeOf(PlayerStatus{}), "")
	if len(paths) < 50 {
		t.Fatalf("only %d leaves found under PlayerStatus: the walk has stopped recursing, so this test proves nothing", len(paths))
	}
	sc := newSnapshotScopes(stripJSComments(readConsoleJS(t, "static/app.js")), nil)
	sc.isRoot = func(fn, ident string) bool {
		return fn == playerStatusEntryFn && ident != "" && ident == sc.param[playerStatusEntryFn]
	}
	if sc.param[playerStatusEntryFn] == "" {
		t.Fatalf("%s is not a one-parameter function in app.js: the status's root cannot be found, so this test proves nothing", playerStatusEntryFn)
	}
	sc.bindParams()
	read := sc.reads()
	nested := 0
	for p := range read {
		if strings.Count(p, ".") >= 2 {
			nested++
		}
	}
	if nested < 20 {
		t.Fatalf("only %d reads two levels deep resolved: the helpers' parameters no longer bind, so this test proves nothing", nested)
	}
	var missing []string
	for _, p := range paths {
		if !read[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("GET /api/output/player answers %q and the Player page never reads it: render it, or drop it from the status", p)
	}
}

// renderOutputPage renders GET /output as srv answers it.
func renderOutputPage(t *testing.T, srv *Server) string {
	t.Helper()
	code, body := playerRequest(t, srv.Handler(), http.MethodGet, "/output", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /output = %d", code)
	}
	return body
}

// TestThePlayerPageOffersTheRestartAsSettingsDoes pins the page's restart:
// the console's own, worded by supervision as Settings words it (Restart now
// under a service manager, Stop now without one), and none where the host owns
// restarts, which the page says instead.
func TestThePlayerPageOffersTheRestartAsSettingsDoes(t *testing.T) {
	button := regexp.MustCompile(`<button type="button" id="player-output-restart" class="btn" data-supervised="(true|false)">([^<]*)</button>`)
	for _, tc := range []struct {
		name       string
		supervised bool
		managed    bool
		want       string
	}{
		{"supervised", true, false, "true Restart now"},
		{"unsupervised", false, false, "false Stop now"},
		{"the host restarts it", true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, cfgPath := newPlayerTestServer(t)
			srv.deps.IsSupervised = tc.supervised
			if tc.managed {
				writePlayerConfig(t, srv, cfgPath, "deployment:\n  managedControls: [restart]\n")
			}
			page := renderOutputPage(t, srv)
			m := button.FindStringSubmatch(page)
			got := ""
			if m != nil {
				got = m[1] + " " + m[2]
			}
			if got != tc.want {
				t.Errorf("the restart button: %q, want %q", got, tc.want)
			}
			if managedHint := strings.Contains(page, `id="player-output-restart-managed"`); managedHint != tc.managed {
				t.Errorf("the host's restart said: %v, want %v", managedHint, tc.managed)
			}
		})
	}
	js := stripJSComments(readConsoleJS(t, "static/app.js"))
	if !strings.Contains(js, `requestBridgeRestart(restart, document.getElementById("player-output-msg")`) ||
		!strings.Contains(js, `requestBridgeRestart(restartBtn, msg, "")`) {
		t.Errorf("the Player page and Settings do not share requestBridgeRestart: the two would word a restart two ways")
	}
}

// TestThePlayerPageOffersNoDSDControl pins the owner's decision for this
// release: the page says plainly that DSD on this player comes later, offers
// no control for it and no command to copy, and its script writes nothing but
// the switch, the OpenHome switch (B612) and the DAC.
func TestThePlayerPageOffersNoDSDControl(t *testing.T) {
	srv, _, _ := newPlayerTestServer(t)
	page := renderOutputPage(t, srv)
	if !strings.Contains(page, "DSD on this player comes in a later version.") {
		t.Errorf("the page does not say that DSD on this player comes later")
	}
	content := page[strings.Index(page, `<section class="page output">`):]
	content = content[:strings.Index(content, "</section>")]
	for _, forbidden := range []string{"dsdMode", "allowDoP", "dsdQualification", "bridge output", "<code>", "<pre", "<input", "<form"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the page carries %q: no DSD control, no command to copy", forbidden)
		}
	}
	js := stripJSComments(readConsoleJS(t, "static/app.js"))
	writes := regexp.MustCompile(`(?:API\.(?:post|patch|put|del|delete)|playerOutputPost)\("(/api/output/player[^"]*)"`).FindAllStringSubmatch(js, -1)
	var got []string
	for _, w := range writes {
		got = append(got, w[1])
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "/api/output/player/dac /api/output/player/enabled /api/output/player/openhome" {
		t.Errorf("the page's script writes %v, want the DAC, the switch and the OpenHome switch alone", got)
	}
}

// TestThePlayerPageAsksOnlyInTheServersWords pins where the page decides to
// warn before a write that stops playback: never from its last status, which
// can predate a play a controller started since, or be missing when a poll
// failed. Both writes that can stop playback go through playerOutputPost,
// whose one confirm answers the server's would_interrupt in the server's
// words and sends the write again with the go-ahead; the switch and the DAC
// choice ask nothing of their own. The OpenHome switch (B612) stops nothing,
// so its route takes no go-ahead: it asks nothing either, and posts its
// write once (API.post), never through the helper that would send one.
// TestAWriteThatWouldStopPlaybackAsksFirst pins the server's half, and
// TestThePlayerPageAsksBeforeAWriteThatStopsPlayback runs the helper under
// node where node is installed.
func TestThePlayerPageAsksOnlyInTheServersWords(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	post := stripJSComments(extractJSFunction(t, src, "playerOutputPost"))
	for _, want := range []string{`err.code !== "would_interrupt"`, `confirm(`, `err.message`, `interrupt: true`} {
		if !strings.Contains(post, want) {
			t.Errorf("playerOutputPost has no %s: the server's refusal is not what the page asks from", want)
		}
	}
	for _, name := range []string{"wirePlayerOutputSwitch", "wirePlayerOutputDACs"} {
		body := stripJSComments(extractJSFunction(t, src, name))
		if strings.Contains(body, "confirm(") {
			t.Errorf("%s asks the operator itself: whether a write stops playback is the server's to say, in its turn", name)
		}
		if !strings.Contains(body, `playerOutputPost("/api/output/player/`) {
			t.Errorf("%s does not send its write through playerOutputPost", name)
		}
	}
	openhome := stripJSComments(extractJSFunction(t, src, "wirePlayerOutputOpenHome"))
	if strings.Contains(openhome, "confirm(") || strings.Contains(openhome, "playerOutputPost(") || strings.Contains(openhome, "interrupt") {
		t.Errorf("the OpenHome switch asks, or sends a go-ahead: it stops nothing, and its route takes none")
	}
	if !strings.Contains(openhome, `API.post("/api/output/player/openhome", { openhome: on })`) ||
		!strings.Contains(openhome, "const on = !st.configured.openHome;") {
		t.Errorf("the OpenHome switch does not send the opposite of the opt-in the config holds to its route")
	}
}

// TestThePlayerPageAsksBeforeAWriteThatStopsPlayback runs the shipped
// playerOutputPost under node: a write the server saves is sent once and asks
// nothing; one the server refuses as would_interrupt asks the operator in the
// server's words and, with their go-ahead, is sent again with interrupt, or,
// without it, is not, and saves nothing; any other refusal is the caller's,
// asking nothing.
func TestThePlayerPageAsksBeforeAWriteThatStopsPlayback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	fn := extractJSFunction(t, readConsoleJS(t, "static/app.js"), "playerOutputPost")
	const stops = "The player is playing, or holds a pause, now. Turning it off stops it at once."
	driver := `const calls = [];
const confirms = [];
let answers = [];
let goAhead = true;
globalThis.API = {
  post: async (path, body) => {
    calls.push({ path, body });
    const next = answers.shift();
    if (next && next.refuse) {
      const e = new Error(next.refuse.message);
      e.code = next.refuse.code;
      throw e;
    }
    return next ? next.answer : null;
  },
};
globalThis.confirm = (text) => {
  confirms.push(text);
  return goAhead;
};
` + fn + `
const out = [];
async function run(name, steps, answer) {
  calls.length = 0;
  confirms.length = 0;
  answers = steps;
  goAhead = answer;
  let result = null;
  let refused = "";
  try {
    result = await playerOutputPost("/api/output/player/enabled", { enabled: false });
  } catch (e) {
    refused = e.code || e.message;
  }
  out.push({ name, calls: JSON.parse(JSON.stringify(calls)), confirms: [...confirms], result: result === undefined ? null : result, refused });
}
(async () => {
  await run("saved", [{ answer: { status: "restart" } }], true);
  await run("asked, then saved", [{ refuse: { code: "would_interrupt", message: ` + strconv.Quote(stops) + ` } }, { answer: { status: "restart", interrupted: true } }], true);
  await run("asked, called off", [{ refuse: { code: "would_interrupt", message: ` + strconv.Quote(stops) + ` } }], false);
  await run("refused otherwise", [{ refuse: { code: "player_refused", message: "the player cannot run" } }], true);
  process.stdout.write(JSON.stringify(out));
})();
`
	dir := t.TempDir()
	script := filepath.Join(dir, "player_output_post.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	type call struct {
		Path string         `json:"path"`
		Body map[string]any `json:"body"`
	}
	var runs []struct {
		Name     string         `json:"name"`
		Calls    []call         `json:"calls"`
		Confirms []string       `json:"confirms"`
		Result   map[string]any `json:"result"`
		Refused  string         `json:"refused"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 4 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	withoutGoAhead := func(c call) bool { _, ok := c.Body["interrupt"]; return !ok && c.Body["enabled"] == false }
	saved, asked, calledOff, refused := runs[0], runs[1], runs[2], runs[3]
	if len(saved.Calls) != 1 || !withoutGoAhead(saved.Calls[0]) || len(saved.Confirms) != 0 || saved.Result["status"] != "restart" {
		t.Errorf("a write the server saves: %+v; want one send without the go-ahead, nothing asked", saved)
	}
	if len(asked.Calls) != 2 || !withoutGoAhead(asked.Calls[0]) || asked.Calls[1].Body["interrupt"] != true || asked.Calls[1].Body["enabled"] != false ||
		len(asked.Confirms) != 1 || !strings.Contains(asked.Confirms[0], stops) || asked.Result["interrupted"] != true {
		t.Errorf("a write the server refuses as would_interrupt, the operator going ahead: %+v; want it asked in the server's words, then sent again with interrupt", asked)
	}
	if len(calledOff.Calls) != 1 || len(calledOff.Confirms) != 1 || calledOff.Result != nil || calledOff.Refused != "" {
		t.Errorf("a write the operator calls off: %+v; want one send, one question, and nothing more", calledOff)
	}
	if len(refused.Calls) != 1 || len(refused.Confirms) != 0 || refused.Refused != "player_refused" {
		t.Errorf("a write refused otherwise: %+v; want the refusal passed on, nothing asked", refused)
	}
}

// TestThePlayerPageOnAPublicBridge: a public bridge's nav offers no Player
// entry, and the page, reached by a bookmark with a session, says why and
// carries no control.
func TestThePlayerPageOnAPublicBridge(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "test-password-123")
	srv.deps.PlayerOutput = newFakePlayerOutput()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	code, page := consoleRequest(t, ts, "", http.MethodGet, "/output", signIn(t, store))
	if code != http.StatusOK {
		t.Fatalf("GET /output on a public bridge = %d", code)
	}
	if !strings.Contains(page, `id="player-output-public"`) {
		t.Errorf("the public page does not say why the player is not here")
	}
	for _, control := range []string{`id="player-output-switch"`, `id="player-output-openhome-switch"`, `id="player-output-list"`, `id="player-output-restart"`, `href="/output"`} {
		if strings.Contains(page, control) {
			t.Errorf("a public bridge's page carries %s", control)
		}
	}
}

// The OpenHome switch's words the owner chose (2026-10-31, B612): its label,
// and what it costs a listener with an older app.
const (
	openHomeSwitchLabel = "Let the 1-bit app cast to this player"
	openHomeOlderApps   = "1-bit 2.1 and earlier stop listing this box's own MPD player while this is on, and 1-bit 2.2 lists both."
)

// htmlByID is the element of the parsed page whose id is id, nil for none.
func htmlByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := htmlByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

// htmlText is the text inside n, as the browser shows it before any script.
func htmlText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(htmlText(c))
	}
	return b.String()
}

// htmlHasAttr reports whether n carries the attribute key.
func htmlHasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// TestThePlayerPageOffersTheOpenHomeSwitchBelowThePlayersOwn pins the
// OpenHome switch's place and words (B612): inside the switch panel, below
// the player's on/off, so that what hides that panel (the player's switch
// managed, a bridge where the player cannot run, a block that does not read)
// hides it too; hidden until the status paints it; a button, labelled in the
// owner's words, with the consequence for older apps; and a description and a
// note the script builds from the status alone, the name and the port
// included, as text.
func TestThePlayerPageOffersTheOpenHomeSwitchBelowThePlayersOwn(t *testing.T) {
	srv, _, _ := newPlayerTestServer(t)
	doc, err := html.Parse(strings.NewReader(renderOutputPage(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	panel := htmlByID(doc, "player-output-switch-panel")
	if panel == nil {
		t.Fatal("the page has no switch panel")
	}
	box := htmlByID(panel, "player-output-openhome")
	if box == nil {
		t.Fatal("the OpenHome switch is not inside the switch panel: hiding the panel would not hide it")
	}
	if !htmlHasAttr(box, "hidden") {
		t.Errorf("the OpenHome switch shows before the status says it may")
	}
	if htmlByID(box, "player-output-switch") != nil {
		t.Errorf("the player's own switch is inside the OpenHome switch's box")
	}
	// Below the player's on/off: the player's button comes first in the
	// panel, and the OpenHome box after it.
	var order []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && (a.Val == "player-output-switch" || a.Val == "player-output-openhome") {
					order = append(order, a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(panel)
	if strings.Join(order, " ") != "player-output-switch player-output-openhome" {
		t.Errorf("the panel's order is %v, want the player's switch, then the OpenHome switch below it", order)
	}
	btn := htmlByID(box, "player-output-openhome-switch")
	if btn == nil || btn.Data != "button" {
		t.Fatalf("the OpenHome switch is not a button: %+v", btn)
	}
	if title := htmlByID(box, "player-output-openhome-title"); title == nil || strings.TrimSpace(htmlText(title)) != openHomeSwitchLabel {
		t.Errorf("the OpenHome switch's label is not %q", openHomeSwitchLabel)
	}
	if older := htmlByID(box, "player-output-openhome-older"); older == nil || strings.TrimSpace(htmlText(older)) != openHomeOlderApps {
		t.Errorf("the OpenHome switch does not say what it costs an older app: want %q", openHomeOlderApps)
	}
	for _, id := range []string{"player-output-openhome-about", "player-output-openhome-configured", "player-output-openhome-note"} {
		if htmlByID(box, id) == nil {
			t.Errorf("the OpenHome switch has no %s", id)
		}
	}
	if note := htmlByID(box, "player-output-openhome-note"); note != nil && !htmlHasAttr(note, "hidden") {
		t.Errorf("the OpenHome switch's note shows before the status says what it is")
	}

	src := readConsoleJS(t, "static/app.js")
	render := stripJSComments(extractJSFunction(t, src, "renderPlayerOutput"))
	for _, want := range []string{
		`show("player-output-switch-panel", st.available && !st.managed.enabled);`,
		`show("player-output-openhome", !st.managed.openHome);`,
		`renderPlayerOutputOpenHome(st.configured);`,
		`playerOpenHomeNote(st.configured, st.running.started, st.pending)`,
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlayerOutput has no %s", want)
		}
	}
	about := stripJSComments(extractJSFunction(t, src, "renderPlayerOutputOpenHome"))
	for _, want := range []string{
		"OpenHome renderer named ${conf.openHomeName}",
		"no sign-in", "Like DLNA", "UDP port 1900",
		"conf.openHomePort > 0 ? `TCP port ${conf.openHomePort}` : \"a TCP port chosen for each address\"",
	} {
		if !strings.Contains(about, want) {
			t.Errorf("the OpenHome switch's description has no %s", want)
		}
	}
	note := stripJSComments(extractJSFunction(t, src, "playerOpenHomeNote"))
	if !strings.Contains(note, "`Saved. The 1-bit app can cast to ${conf.openHomeName} once the player is on.`") {
		t.Errorf("the note for the opt-in on with the player off is not the owner's words")
	}
	for _, name := range []string{"renderPlayerOutput", "renderPlayerOutputOpenHome", "playerOpenHomeNote", "wirePlayerOutputOpenHome"} {
		body := stripJSComments(extractJSFunction(t, src, name))
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML"} {
			if strings.Contains(body, sink) {
				t.Errorf("%s writes %s: every string goes in as text", name, sink)
			}
		}
	}
}

// playerPageHarnessRun drives the shipped renderPlayerOutput over the cases
// and prints what each left of the switch panel, the OpenHome switch and the
// status line. The functions it calls that these cases do not look at are
// stubs (the transports' table, the pending list), and every other one is
// extracted from app.js as shipped; the panel, the OpenHome box and its note
// start hidden, as the template ships them.
const playerPageHarnessRun = `
let playerOutputLatest = null;
function renderPlayerOutputTransports() {}
function renderPlayerOutputPending() {}
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const out = [];
for (const c of input.cases) {
  els.clear();
  for (const id of ["player-output-switch-panel", "player-output-openhome", "player-output-openhome-note"]) el(id).hidden = true;
  renderPlayerOutput(c.status);
  const panel = !el("player-output-switch-panel").hidden;
  const note = el("player-output-openhome-note");
  out.push({
    name: c.name,
    panel,
    openHome: panel && !el("player-output-openhome").hidden,
    button: el("player-output-openhome-switch").textContent,
    about: el("player-output-openhome-about").textContent,
    configured: el("player-output-openhome-configured").textContent,
    note: note.hidden ? null : note.textContent,
    state: el("player-output-state").textContent,
  });
}
console.log(JSON.stringify(out));
`

// TestThePlayerPageDrawsTheOpenHomeSwitchStateByState runs the shipped
// renderPlayerOutput under node, where node is installed, over the states the
// owner worded (B612): which of the two switches shows for each managed
// setting (the whole panel hidden where playerEnabled is managed, which owns
// playerOpenHome too; the OpenHome switch alone where playerOpenHome alone
// is; both where neither is); the description by the status's name and port,
// a port chosen for each address for 0; the note: the opt-in on with the
// player off, with the player switch's restart line when it waits for one;
// the opt-in changed since boot, in the server's words; nothing in a steady
// state; and a Source the bridge started with that is not on the LAN, whose
// reason is the status line's and no note offers a restart for it.
func TestThePlayerPageDrawsTheOpenHomeSwitchStateByState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	const (
		starts      = "The player starts when the bridge restarts."
		stops       = "The player is turned off: it plays nothing now, and it stops when the bridge restarts."
		turnedOn    = "Casting from the 1-bit app is turned on: the bridge puts the player on the LAN as Study when it restarts."
		turnedOff   = "Casting from the 1-bit app is turned off: the bridge takes the player off the LAN when it restarts. Until then the 1-bit app can still cast to it."
		noAddress   = "The box has no LAN address for its OpenHome source yet (an interface that is up, with a private IPv4 address): it goes on the LAN once one comes up."
		onTheLAN    = "Its OpenHome source is on the LAN."
		castOnceOn  = "Saved. The 1-bit app can cast to Study once the player is on."
		letsItCast  = "The config lets the 1-bit app cast to this player."
		keepsItOff  = "The config keeps the player off the LAN for the 1-bit app."
		portOfItsOn = "TCP port 7792"
		perAddress  = "a TCP port chosen for each address"
	)
	type status = PlayerStatus
	running := func(on bool) status {
		return status{Available: true, PlayerReport: PlayerReport{
			Running:    PlayerRunning{Started: true, State: "stopped", Transport: "stopped", OpenHome: on},
			Configured: PlayerConfigured{Enabled: true, Starts: true, OpenHome: on, OpenHomePort: 7792, OpenHomeName: "Study"},
			Pending:    []PlayerPending{}, Transports: []PlayerTransport{},
		}}
	}
	with := func(st status, change func(*status)) status { change(&st); return st }
	type want struct {
		panel, openHome bool
		button          string
		about           []string
		configured      string
		note            *string
		state           []string
	}
	text := func(s string) *string { return &s }
	cases := []struct {
		name   string
		status status
		want   want
	}{
		{"neither switch managed", running(false), want{panel: true, openHome: true, button: "Turn on", about: []string{"named Study", portOfItsOn}, configured: keepsItOff}},
		{"playerOpenHome managed alone", with(running(false), func(s *status) { s.Managed.OpenHome = true }),
			want{panel: true, openHome: false, button: "Turn on", configured: keepsItOff}},
		{"playerEnabled managed, which owns playerOpenHome", with(running(false), func(s *status) { s.Managed.Enabled, s.Managed.OpenHome = true, true }),
			want{panel: false, openHome: false, button: "Turn on", configured: keepsItOff}},
		{"a port the kernel picks for each address", with(running(false), func(s *status) { s.Configured.OpenHomePort = 0 }),
			want{panel: true, openHome: true, button: "Turn on", about: []string{perAddress}, configured: keepsItOff}},
		{"on, the player off", with(running(false), func(s *status) {
			s.Running = PlayerRunning{State: "off"}
			s.Configured.Enabled, s.Configured.Starts, s.Configured.OpenHome = false, false, true
		}), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, note: text(castOnceOn)}},
		{"on, the player turned on for the next start", with(running(false), func(s *status) {
			s.Running = PlayerRunning{State: "off"}
			s.Configured.OpenHome = true
			s.Pending = []PlayerPending{{Change: "start", Text: starts}}
		}), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, note: text(castOnceOn + " " + starts)}},
		{"on, the player turned off for the next start", with(running(true), func(s *status) {
			s.Configured.Enabled, s.Configured.Starts = false, false
			s.Pending = []PlayerPending{{Change: "stop", Text: stops}}
		}), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, note: text(castOnceOn + " " + stops), state: []string{onTheLAN}}},
		{"turned on since the bridge started", with(running(false), func(s *status) {
			s.Configured.OpenHome = true
			s.Pending = []PlayerPending{{Change: "openHome", Text: turnedOn}}
		}), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, note: text(turnedOn)}},
		{"turned off since the bridge started", with(running(true), func(s *status) {
			s.Configured.OpenHome = false
			s.Pending = []PlayerPending{{Change: "openHome", Text: turnedOff}}
		}), want{panel: true, openHome: true, button: "Turn on", configured: keepsItOff, note: text(turnedOff), state: []string{onTheLAN}}},
		{"on and on the LAN", running(true), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, state: []string{onTheLAN}}},
		{"on at boot and not on the LAN", with(running(true), func(s *status) {
			s.Running.OpenHome, s.Running.OpenHomeDetail = false, noAddress
		}), want{panel: true, openHome: true, button: "Turn off", configured: letsItCast, state: []string{noAddress}}},
	}
	type harnessCase struct {
		Name   string `json:"name"`
		Status status `json:"status"`
	}
	var input struct {
		Cases []harnessCase `json:"cases"`
	}
	for _, c := range cases {
		input.Cases = append(input.Cases, harnessCase{c.name, c.status})
	}
	src := readConsoleJS(t, "static/app.js")
	script := withFunctions(t, jobsToolStatePreamble+playerPageHarnessRun, src,
		"renderPlayerOutput", "renderPlayerOutputRunning", "renderPlayerOutputConfigured", "renderPlayerOutputOpenHome", "playerOpenHomeNote", "playerOutputText")
	raw := runScriptUnderNode(t, node, script, input)
	var got []struct {
		Name       string  `json:"name"`
		Panel      bool    `json:"panel"`
		OpenHome   bool    `json:"openHome"`
		Button     string  `json:"button"`
		About      string  `json:"about"`
		Configured string  `json:"configured"`
		Note       *string `json:"note"`
		State      string  `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for i, c := range cases {
		g, w := got[i], c.want
		if g.Panel != w.panel || g.OpenHome != w.openHome {
			t.Errorf("%s: the switch panel shows %v and the OpenHome switch %v; want %v and %v", c.name, g.Panel, g.OpenHome, w.panel, w.openHome)
		}
		if g.Button != w.button || g.Configured != w.configured {
			t.Errorf("%s: the button says %q and the config line %q; want %q and %q", c.name, g.Button, g.Configured, w.button, w.configured)
		}
		for _, s := range w.about {
			if !strings.Contains(g.About, s) {
				t.Errorf("%s: the description %q has no %q", c.name, g.About, s)
			}
		}
		if strings.Contains(g.About, "TCP port 0") {
			t.Errorf("%s: the description names port 0: %q", c.name, g.About)
		}
		switch {
		case (g.Note == nil) != (w.note == nil):
			t.Errorf("%s: the note is %v, want %v", c.name, deref(g.Note), deref(w.note))
		case g.Note != nil && *g.Note != *w.note:
			t.Errorf("%s: the note says %q, want %q", c.name, *g.Note, *w.note)
		}
		if g.Note != nil && strings.Contains(*g.Note, "restart") && w.note == nil {
			t.Errorf("%s: a note offers a restart: %q", c.name, *g.Note)
		}
		for _, s := range w.state {
			if !strings.Contains(g.State, s) {
				t.Errorf("%s: the status line %q has no %q", c.name, g.State, s)
			}
		}
	}
}

// deref is *s, or "none" for nil.
func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}
