package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// toneRoute is the console's test tone (B601).
const toneRoute = "/api/output/player/tone"

// toneOn posts body (none when empty) to the tone route of a fresh bridge of
// the kind named, as the Player page's fetch does, the provider answering a
// tone that played, and answers the status, the body and the provider's Tone
// calls. The kinds: linux (the provider wired, the default), darwin (another
// host), unwired (no provider), unreadable (a player block that does not
// read), and public, public off Linux and public unwired, each asked with a
// signed-in session and the console's own Origin.
func toneOn(t *testing.T, kind, body string) (int, string, int) {
	t.Helper()
	played := PlayerToneResult{Outcome: "played", Admitted: true, Format: "S24_3LE", RateHz: 48000, ValidBits: 24, Frames: 480000}
	if strings.HasPrefix(kind, "public") {
		srv, store, _, _ := newPublicTestServerAt(t, "test-password-123")
		fake := newFakePlayerOutput()
		fake.tone = played
		srv.deps.PlayerOutput = fake
		switch kind {
		case "public off Linux":
			fake.platform = "darwin"
		case "public unwired":
			srv.deps.PlayerOutput = nil
		}
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+toneRoute, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", publicOrigin)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: signIn(t, store)})
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw), fake.called("tone")
	}
	srv, fake, cfgPath := newPlayerTestServer(t)
	fake.tone = played
	switch kind {
	case "darwin":
		fake.platform = "darwin"
	case "unwired":
		srv.deps.PlayerOutput = nil
	case "unreadable":
		writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n    enabled: true\n    device:\n      card: 5\n")
	}
	code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, body, nil)
	return code, raw, fake.called("tone")
}

// TestTheToneRouteChecksItsBodyThenItsFieldsThenThePlayer pins the tone
// route's order (B601): its body, one JSON object, then its fields (both
// there, the level a number from -90 to -30 dBFS, the length a whole number of
// seconds from 1 to 30), each refused 400 on every kind of bridge, whatever
// would refuse the player there; then the page's refusals in their own order,
// a good body answered 503 player_not_wired with no provider wired (a public
// bridge's too), 403 player_not_available on a public bridge with a session
// (off Linux too), 409 player_not_supported off Linux, 409
// player_block_unreadable over a player block that does not read; and only
// then the provider's Tone, which no refused request reaches.
func TestTheToneRouteChecksItsBodyThenItsFieldsThenThePlayer(t *testing.T) {
	bad := []struct{ name, body string }{
		{"no body", ""},
		{"not JSON", "{"},
		{"something after the object", `{"level":-40,"seconds":10}}`},
		{"a key it does not take", `{"level":-40,"seconds":10,"interrupt":true}`},
		{"no level", `{"seconds":10}`},
		{"no length", `{"level":-40}`},
		{"a level above -30", `{"level":-29.9,"seconds":10}`},
		{"a length of 31 seconds", `{"level":-40,"seconds":31}`},
		{"a length that is no whole number", `{"level":-40,"seconds":1.5}`},
	}
	for _, kind := range []string{"linux", "darwin", "unwired", "unreadable", "public", "public off Linux", "public unwired"} {
		for _, b := range bad {
			t.Run(kind+"/"+b.name, func(t *testing.T) {
				code, raw, tones := toneOn(t, kind, b.body)
				if code != http.StatusBadRequest || errorCode(raw) != errCodeBadJSON {
					t.Fatalf("= %d: %s; want 400 %s, the body's refusal, on every bridge", code, raw, errCodeBadJSON)
				}
				if tones != 0 {
					t.Errorf("a refused body reached the provider's Tone %d times", tones)
				}
			})
		}
	}
	const good = `{"level":-40,"seconds":10}`
	for _, tc := range []struct {
		kind   string
		status int
		code   string
	}{
		{"public unwired", http.StatusServiceUnavailable, playerNotWired},
		{"unwired", http.StatusServiceUnavailable, playerNotWired},
		{"public off Linux", http.StatusForbidden, playerNotAvailable},
		{"public", http.StatusForbidden, playerNotAvailable},
		{"darwin", http.StatusConflict, playerNotSupported},
		{"unreadable", http.StatusConflict, playerBlockUnread},
	} {
		t.Run(tc.kind+"/a good body", func(t *testing.T) {
			code, raw, tones := toneOn(t, tc.kind, good)
			if code != tc.status || errorCode(raw) != tc.code {
				t.Fatalf("= %d: %s; want %d %s", code, raw, tc.status, tc.code)
			}
			if tones != 0 {
				t.Errorf("a refused bridge reached the provider's Tone %d times", tones)
			}
		})
	}
	t.Run("linux/a good body", func(t *testing.T) {
		code, raw, tones := toneOn(t, "linux", good)
		if code != http.StatusOK || tones != 1 {
			t.Fatalf("= %d: %s, %d tones; want 200 and the provider's one Tone", code, raw, tones)
		}
	})
}

// TestTheToneRouteIsGuardedLikeEveryConsoleWrite pins the console's guards on
// the tone route (B601): the page's own request reaches the provider's Tone;
// a Host that names another host is 421, another Origin and a null one 403
// cross_origin, a body that is not JSON 415, and none of those reaches it.
func TestTheToneRouteIsGuardedLikeEveryConsoleWrite(t *testing.T) {
	srv, fake, _ := newPlayerTestServer(t)
	fake.tone = PlayerToneResult{Outcome: "played", Admitted: true}
	if code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, `{"level":-40,"seconds":10}`, nil); code != http.StatusOK || fake.called("tone") != 1 {
		t.Fatalf("the page's own request = %d: %s, %d tones; want the provider's one Tone", code, raw, fake.called("tone"))
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
		code   string
	}{
		{"another host", func(r *http.Request) { r.Host = "attacker.example:7789" }, http.StatusMisdirectedRequest, ""},
		{"another origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, http.StatusForbidden, errCodeCrossOrigin},
		{"a null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden, errCodeCrossOrigin},
		{"a form body", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, ""},
		{"a multipart body", func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=x") }, http.StatusUnsupportedMediaType, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, _ := newPlayerTestServer(t)
			fake.tone = PlayerToneResult{Outcome: "played", Admitted: true}
			code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, `{"level":-40,"seconds":10}`, tc.change)
			if code != tc.status || (tc.code != "" && errorCode(raw) != tc.code) {
				t.Fatalf("= %d: %s; want %d %s", code, raw, tc.status, tc.code)
			}
			if n := fake.called("tone"); n != 0 {
				t.Errorf("a guarded request reached the provider's Tone %d times", n)
			}
		})
	}
}

// TestTheToneRouteAnswersTheTonesResultAlone pins the tone route's answer
// (B601): a tone that took the DAC's slot is 200 with its result alone (its
// outcome, its refused open's cause, what the kernel committed, its frames),
// never the player's status and never whether it was admitted; a refused one
// is 409 with its word as the code; and the provider judges the tone on the
// very config the route's refusals judged, the one load of the holder: a
// config stored in the holder while the refusals ran is not the one the
// provider is handed.
func TestTheToneRouteAnswersTheTonesResultAlone(t *testing.T) {
	srv, fake, _ := newPlayerTestServer(t)
	fake.tone = PlayerToneResult{Outcome: "played", Admitted: true, Format: "S24_3LE", RateHz: 48000, ValidBits: 24, Frames: 480000}
	var judged, after *config.Config
	fake.onPlatform = func() {
		judged = srv.deps.CfgHolder.Load()
		after = srv.deps.CfgHolder.Clone()
		srv.deps.CfgHolder.Store(after)
	}
	code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, `{"level":-40,"seconds":10}`, nil)
	if code != http.StatusOK {
		t.Fatalf("= %d: %s", code, raw)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, " ") != "format frames lastError outcome rateHz validBits" {
		t.Errorf("the answer's keys are %v, want the tone's result alone", keys)
	}
	if got["outcome"] != "played" || got["format"] != "S24_3LE" || got["rateHz"] != float64(48000) || got["validBits"] != float64(24) || got["frames"] != float64(480000) {
		t.Errorf("the answer is %s, want the provider's result", raw)
	}
	fake.mu.Lock()
	live, req := fake.toneLive, fake.toneReq
	fake.mu.Unlock()
	if judged == nil || live != judged || live == after {
		t.Errorf("the provider was handed another config than the one the route's refusals judged")
	}
	if req.LevelDBFS != -40 || req.Seconds != 10 {
		t.Errorf("the provider was asked for %+v, want -40 dBFS for 10 seconds", req)
	}
	fake.tone = PlayerToneResult{Outcome: "player_sounding"}
	code, raw = playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, `{"level":-40,"seconds":10}`, nil)
	if code != http.StatusConflict || errorCode(raw) != "player_sounding" || strings.Contains(raw, `"player"`) {
		t.Errorf("a refused tone = %d: %s; want 409 with its word, and no status", code, raw)
	}
}

// TestTheToneRouteRefusesALevelAboveTheCap pins the route's level and length
// to the tone's bounds by this test's own literals (B601): -30 and -90 dBFS
// and 1 and 30 seconds are taken; anything above -30 dBFS, below -90, a level
// that is no number or is missing, and 0 or 31 seconds are 400, and the
// provider is never asked.
func TestTheToneRouteRefusesALevelAboveTheCap(t *testing.T) {
	for _, body := range []string{
		`{"level":-29.9,"seconds":10}`, `{"level":0,"seconds":10}`, `{"level":6,"seconds":10}`, `{"level":-90.01,"seconds":10}`,
		`{"level":"-40","seconds":10}`, `{"seconds":10}`, `{"level":null,"seconds":10}`,
		`{"level":-40,"seconds":0}`, `{"level":-40,"seconds":31}`, `{"level":-40,"seconds":"10"}`,
	} {
		srv, fake, _ := newPlayerTestServer(t)
		fake.tone = PlayerToneResult{Outcome: "played", Admitted: true}
		code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, body, nil)
		if code != http.StatusBadRequest || errorCode(raw) != errCodeBadJSON || fake.called("tone") != 0 {
			t.Errorf("%s = %d: %s, %d tones; want 400 and no tone", body, code, raw, fake.called("tone"))
		}
	}
	for _, body := range []string{`{"level":-30,"seconds":1}`, `{"level":-90,"seconds":30}`} {
		srv, fake, _ := newPlayerTestServer(t)
		fake.tone = PlayerToneResult{Outcome: "played", Admitted: true}
		code, raw := playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, body, nil)
		if code != http.StatusOK || fake.called("tone") != 1 {
			t.Errorf("%s = %d: %s; want the tone played", body, code, raw)
		}
	}
}

// TestTheTonesLastErrorIsItsOwn pins the tone's refused open's cause apart
// from the player's (B601): running.tone.lastError and the tone's answer carry
// it, and running.lastError, the engine's last play attempt alone, stays as
// the provider gives it.
func TestTheTonesLastErrorIsItsOwn(t *testing.T) {
	srv, fake, _ := newPlayerTestServer(t)
	fake.report.Running.LastError = ""
	fake.report.Running.Tone = &PlayerTone{State: "idle", Format: "S24_3LE", RateHz: 48000, ValidBits: 24, Outcome: "open_refused", LastError: "device_busy"}
	code, raw := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d: %s", code, raw)
	}
	var st struct {
		Running struct {
			LastError string         `json:"lastError"`
			Tone      map[string]any `json:"tone"`
		} `json:"running"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st.Running.LastError != "" || st.Running.Tone["lastError"] != "device_busy" || st.Running.Tone["outcome"] != "open_refused" {
		t.Errorf("the status: running.lastError %q, running.tone %v; want the tone's cause in the tone alone", st.Running.LastError, st.Running.Tone)
	}
	for _, key := range []string{"state", "levelDBFS", "seconds", "format", "rateHz", "validBits", "outcome", "lastError"} {
		if _, ok := st.Running.Tone[key]; !ok {
			t.Errorf("running.tone has no %q: %s", key, raw)
		}
	}
	fake.tone = PlayerToneResult{Outcome: "open_refused", Admitted: true, LastError: "permission_denied"}
	code, raw = playerRequest(t, srv.Handler(), http.MethodPost, toneRoute, `{"level":-40,"seconds":10}`, nil)
	if code != http.StatusOK || !strings.Contains(raw, `"lastError":"permission_denied"`) {
		t.Errorf("a tone whose open was refused = %d: %s; want its cause in its answer", code, raw)
	}
	fake.report.Running.Tone = nil
	_, raw = playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player", "", nil)
	if !strings.Contains(raw, `"tone":null`) {
		t.Errorf("a player with no tone to offer: %s; want running.tone null", raw)
	}
}

// toneLineRe is a one-line top-level const of app.js.
var toneLineRe = regexp.MustCompile(`(?m)^const playerTone = \{[^\n]*\};$`)

// TestThePageStopsItsToneByItsRequestAlone pins the page's tone (B601):
// Play posts the tone's one route through API.post with a signal of the
// tone's own; Stop aborts that signal and posts nothing, no route stopping a
// tone; the page's scope (a navigation inside the console) and its leaving
// (pagehide) abort it too; and after a Stop the page reads the status at once
// and every 250 ms while the tone reads playing or stopping, for at most three
// seconds, saying whether the stop was confirmed. Where node is installed it
// runs the shipped functions: a stop the status confirms through playing and
// stopping to idle, one it reads playing or stopping to the end of its three
// seconds, said not confirmed, and the request aborted by pagehide and by the
// page's scope, with no read and no second post.
func TestThePageStopsItsToneByItsRequestAlone(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	code := stripJSComments(src)
	if n := strings.Count(code, `"`+toneRoute); n != 1 {
		t.Errorf("app.js names the tone's route %d times, want once: the Play's post, and no second route", n)
	}
	wire := stripJSComments(extractJSFunction(t, src, "wirePlayerOutputTone"))
	for _, want := range []string{
		`API.post("/api/output/player/tone", body, controller.signal)`,
		`scope.addEventListener("abort", abortTone, { once: true });`,
		`window.addEventListener("pagehide", abortTone, { signal: scope });`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wirePlayerOutputTone has no %s", want)
		}
	}
	_, stop, ok := strings.Cut(wire, `stop.addEventListener("click"`)
	if !ok {
		t.Fatal("wirePlayerOutputTone arms no Stop")
	}
	if !strings.Contains(stop, "controller.abort();") || !strings.Contains(stop, "confirmPlayerToneStop(scope);") {
		t.Errorf("Stop does not abort the tone's request and confirm it: %s", stop)
	}
	for _, sink := range []string{"API.post", "API.patch", "API.delete", "fetch("} {
		if strings.Contains(stop, sink) {
			t.Errorf("Stop sends a request (%s): it ends the tone by its request's end alone", sink)
		}
	}
	confirm := stripJSComments(extractJSFunction(t, src, "confirmPlayerToneStop"))
	for _, want := range []string{"Date.now() + 3000", "setTimeout(resolve, 250)", `state !== "playing" && state !== "stopping"`, "The stop is not confirmed"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmPlayerToneStop has no %s", want)
		}
	}
	// The script node runs is assembled, and checked for what it would not
	// carry, on every host: the shipped functions the tone reaches, after the
	// harness and its run.
	line := toneLineRe.FindString(src)
	if line == "" {
		t.Fatal("app.js declares no one-line playerTone")
	}
	script := tonePageHarness + line + "\n" + extractJSConst(t, src, "PLAYER_TONE_WORDS") + extractJSConst(t, src, "PLAYER_TONE_LAST_ERRORS") +
		extractJSConst(t, src, "PLAYER_TONE_STATES") + tonePageRun
	assembled := withReachedFunctions(t, script, src, "wirePlayerOutputTone", "confirmPlayerToneStop", "readPlayerOutputForTone",
		"showPlayerToneLine", "playerToneStatusText", "playerToneEndText", "playerToneFormatText", "playerRateText", "setDisabled", "pageSignal")
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped page script")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name    string   `json:"name"`
		Posts   []string `json:"posts"`
		Aborted bool     `json:"aborted"`
		Gets    int      `json:"gets"`
		Line    string   `json:"line"`
		PlayOff bool     `json:"playDisabled"`
		StopOff bool     `json:"stopDisabled"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 5 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs {
		if strings.Join(r.Posts, " ") != toneRoute || !r.Aborted {
			t.Errorf("%s: posted %v, aborted %v; want the tone's one post, aborted", r.Name, r.Posts, r.Aborted)
		}
	}
	confirmed, playing, stopping, hidden, scoped := runs[0], runs[1], runs[2], runs[3], runs[4]
	if confirmed.Gets != 3 || confirmed.Line != "The tone stopped." || confirmed.PlayOff || !confirmed.StopOff {
		t.Errorf("a stop the status confirms through playing and stopping: %+v; want three reads, the stop confirmed, Play back and Stop off", confirmed)
	}
	for _, r := range []struct {
		run   any
		gets  int
		line  string
		state string
	}{{playing, playing.Gets, playing.Line, "playing"}, {stopping, stopping.Gets, stopping.Line, "stopping"}} {
		if r.gets < 12 || r.gets > 14 || !strings.Contains(r.line, "The stop is not confirmed") || !strings.Contains(r.line, r.state) {
			t.Errorf("a stop the status reads %s throughout: %+v; want a read every 250 ms for three seconds, then the stop said not confirmed", r.state, r.run)
		}
	}
	for _, r := range []struct {
		what string
		got  int
	}{{"pagehide", hidden.Gets}, {"the page's scope", scoped.Gets}} {
		if r.got != 0 {
			t.Errorf("a tone ended by %s read the status %d times: the page is going, and nothing confirms for it", r.what, r.got)
		}
	}
}

// tonePageHarness is the tone's page as the shipped functions see it: buttons
// and selects by id, whose click runs their listeners; a window whose
// listeners a scope's signal removes; the page's scope (pageAbort, as
// dispatchPageInit keeps it); the console's API, whose post never answers
// until its signal aborts it and whose get answers the statuses queued, the
// last one again and again; and a clock that each timer moves by its delay,
// at once.
const tonePageHarness = `
class El {
  constructor(id) {
    this.id = id; this.listeners = {}; this.disabled = false; this.hidden = false;
    this.textContent = ""; this.className = ""; this.value = ""; this.dataset = {}; this.isConnected = true;
  }
  addEventListener(type, handler) { (this.listeners[type] ||= []).push(handler); }
  click() { for (const handler of this.listeners.click || []) handler.call(this); }
  focus() {}
}
const els = new Map();
const el = (id) => { if (!els.has(id)) els.set(id, new El(id)); return els.get(id); };
globalThis.document = { getElementById: (id) => el(id), activeElement: null, body: {} };
const windowListeners = [];
globalThis.window = { addEventListener: (type, handler, opts) => { windowListeners.push({ type, handler, signal: opts && opts.signal }); } };
function dispatchWindow(type) {
  for (const l of windowListeners) if (l.type === type && !(l.signal && l.signal.aborted)) l.handler.call(globalThis.window);
}
const realSetTimeout = setTimeout;
let now = 0;
const RealDate = Date;
globalThis.Date = class extends RealDate { static now() { return now; } };
globalThis.setTimeout = (fn, ms) => { now += ms; queueMicrotask(fn); return 0; };
const settle = () => new Promise((resolve) => realSetTimeout(resolve, 0));
const posts = [];
const gets = [];
let statuses = [];
const API = {
  post(path, body, signal) {
    posts.push({ path, body, signal });
    return new Promise((resolve, reject) => {
      if (signal) signal.addEventListener("abort", () => { const e = new Error("aborted"); e.name = "AbortError"; reject(e); });
    });
  },
  async get(path) {
    gets.push(path);
    return statuses.length > 1 ? statuses.shift() : statuses[0];
  },
};
let pageAbort = null;
let playerOutputLatest = null;
let playerOutputSeq = 0;
function renderPlayerOutput(st) { playerOutputLatest = st; }
`

// tonePageRun plays a tone on a fresh page for each case and ends it as the
// case does, printing what the page posted and read and what its line says.
const tonePageRun = `
const status = (state) => ({ available: true, running: { tone: { state, levelDBFS: -40, seconds: 10,
  format: "S24_3LE", rateHz: 48000, validBits: 24, outcome: state === "idle" ? "request_ended" : "", lastError: "" } } });
function freshPage() {
  els.clear(); posts.length = 0; gets.length = 0; windowListeners.length = 0; now = 0;
  if (pageAbort) pageAbort.abort();
  pageAbort = new AbortController();
  el("player-output-tone-level").value = "-40";
  el("player-output-tone-seconds").value = "10";
  el("player-output-tone-stop").disabled = true;
  playerOutputLatest = { available: true, running: {} };
  wirePlayerOutputTone();
}
const out = [];
async function run(name, queued, end) {
  freshPage();
  statuses = queued;
  el("player-output-tone-play").click();
  await settle();
  const signal = posts[0] && posts[0].signal;
  end();
  await settle();
  out.push({ name, posts: posts.map((p) => p.path), aborted: Boolean(signal && signal.aborted), gets: gets.length,
    line: el("player-output-tone-msg").textContent, playDisabled: el("player-output-tone-play").disabled,
    stopDisabled: el("player-output-tone-stop").disabled });
}
const stop = () => el("player-output-tone-stop").click();
await run("confirmed", [status("playing"), status("stopping"), status("idle")], stop);
await run("playing to the end", [status("playing")], stop);
await run("stopping to the end", [status("stopping")], stop);
await run("pagehide", [status("playing")], () => dispatchWindow("pagehide"));
await run("the page's scope", [status("playing")], () => pageAbort.abort());
console.log(JSON.stringify(out));
`
