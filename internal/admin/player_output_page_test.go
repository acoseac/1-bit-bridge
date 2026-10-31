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
// the switch and the DAC.
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
	if strings.Join(got, " ") != "/api/output/player/dac /api/output/player/enabled" {
		t.Errorf("the page's script writes %v, want the DAC and the switch alone", got)
	}
}

// TestThePlayerPageAsksOnlyInTheServersWords pins where the page decides to
// warn before a write that stops playback: never from its last status, which
// can predate a play a controller started since, or be missing when a poll
// failed. Both writes go through playerOutputPost, whose one confirm answers
// the server's would_interrupt in the server's words and sends the write again
// with the go-ahead; the switch and the DAC choice ask nothing of their own.
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
	for _, control := range []string{`id="player-output-switch"`, `id="player-output-list"`, `id="player-output-restart"`, `href="/output"`} {
		if strings.Contains(page, control) {
			t.Errorf("a public bridge's page carries %s", control)
		}
	}
}
