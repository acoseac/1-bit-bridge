package admin

import (
	"encoding/json"
	"fmt"
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

// requireInOrder fails the test unless code holds each of parts, in that
// order: where a call stands against another is what the test pins.
func requireInOrder(t *testing.T, what, code string, parts ...string) {
	t.Helper()
	at := 0
	for _, p := range parts {
		i := strings.Index(code[at:], p)
		if i < 0 {
			t.Errorf("%s has no %s after what comes before it (%q)", what, p, parts)
			return
		}
		at += i + len(p)
	}
}

// TestThePageStopsItsToneByItsRequestAlone pins the page's tone (B601):
// Play posts the tone's one route through API.post with a signal of the
// tone's own; Stop aborts that signal and posts nothing, no route stopping a
// tone; the page's scope (a navigation inside the console, once the next page
// is in) and its being hidden (pagehide) abort it too; and after a Stop the
// page reads the status at once and every 250 ms while the tone reads playing
// or stopping, for at most three seconds, each read bounded by what is left of
// them and by the page's scope (API.get takes the read's signal), saying
// whether the stop was confirmed. Where node is installed it runs the shipped
// functions on a clock only the run moves: a stop the status confirms through
// playing and stopping to idle, one it reads playing or stopping to the end of
// its three seconds, said not confirmed, one whose read never answers, said
// not confirmed at the three seconds and no later (B601's round 2: the read
// was unbounded, and the deadline was asked only after it), and the request
// aborted by pagehide and by the page's scope, with no read and no second
// post.
func TestThePageStopsItsToneByItsRequestAlone(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	code := stripJSComments(src)
	if n := strings.Count(code, `"`+toneRoute); n != 1 {
		t.Errorf("app.js names the tone's route %d times, want once: the Play's post, and no second route", n)
	}
	wire := stripJSComments(extractJSFunction(t, src, "wirePlayerOutputTone"))
	for _, want := range []string{
		`API.post("/api/output/player/tone", body, controller.signal)`,
		`scope.addEventListener("abort", () => playerTone.controller?.abort(), { once: true });`,
		`window.addEventListener("pagehide", hidePlayerTone, { signal: scope });`,
		`stop.addEventListener("click", () => stopPlayerTone(scope));`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wirePlayerOutputTone has no %s", want)
		}
	}
	stop := stripJSComments(extractJSFunction(t, src, "stopPlayerTone"))
	requireInOrder(t, "stopPlayerTone", stop, "controller.abort();", "confirmPlayerToneStop(scope);")
	for _, sink := range []string{"API.post", "API.patch", "API.delete", "fetch("} {
		if strings.Contains(stop, sink) {
			t.Errorf("Stop sends a request (%s): it ends the tone by its request's end alone", sink)
		}
	}
	confirm := stripJSComments(extractJSFunction(t, src, "confirmPlayerToneStop"))
	for _, want := range []string{"Date.now() + 3000", "readPlayerOutputForTone(scope, deadline - Date.now()", "setTimeout(resolve, 250)",
		`state !== "playing" && state !== "stopping"`, "The stop is not confirmed"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmPlayerToneStop has no %s", want)
		}
	}
	read := stripJSComments(extractJSFunction(t, src, "readPlayerOutputForTone"))
	requireInOrder(t, "readPlayerOutputForTone", read, "setTimeout(end, Math.max(0, ms))", `scope.addEventListener("abort", end`,
		`API.get("/api/output/player", bound.signal)`, "clearTimeout(timer)", `scope.removeEventListener("abort", end)`)
	if api := stripJSComments(extractJSConst(t, src, "API")); !strings.Contains(api, "async get(path, signal)") ||
		!strings.Contains(api, `fetch(path, { headers: { accept: "application/json" }, signal })`) {
		t.Errorf("API.get does not hand its fetch the signal it is given: %s", api)
	}
	// The script node runs is assembled, and checked for what it would not
	// carry, on every host: the shipped functions the tone reaches, after the
	// harness and its run.
	script, assembled := tonePageScript(t, src, tonePageRun)
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
		LineAt  int      `json:"lineAt"`
		PlayOff bool     `json:"playDisabled"`
		StopOff bool     `json:"stopDisabled"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 6 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs {
		if strings.Join(r.Posts, " ") != toneRoute || !r.Aborted {
			t.Errorf("%s: posted %v, aborted %v; want the tone's one post, aborted", r.Name, r.Posts, r.Aborted)
		}
	}
	confirmed, playing, stopping, stalled, hidden, scoped := runs[0], runs[1], runs[2], runs[3], runs[4], runs[5]
	if confirmed.Gets != 3 || confirmed.Line != "The tone stopped." || confirmed.LineAt != 500 || confirmed.PlayOff || !confirmed.StopOff {
		t.Errorf("a stop the status confirms through playing and stopping: %+v; want three reads, the stop confirmed at 500 ms, Play back and Stop off", confirmed)
	}
	for _, r := range []struct {
		run   any
		gets  int
		line  string
		at    int
		state string
	}{{playing, playing.Gets, playing.Line, playing.LineAt, "playing"}, {stopping, stopping.Gets, stopping.Line, stopping.LineAt, "stopping"}} {
		if r.gets != 12 || r.at != 3000 || !strings.Contains(r.line, "The stop is not confirmed") || !strings.Contains(r.line, r.state) {
			t.Errorf("a stop the status reads %s throughout: %+v; want a read every 250 ms for three seconds, then the stop said not confirmed at 3 s", r.state, r.run)
		}
	}
	if stalled.Gets != 1 || stalled.LineAt != 3000 || stalled.Line != "The stop is not confirmed: the bridge's status could not be read." {
		t.Errorf("a stop whose read never answers: %+v; want its one read ended at the three seconds, said not confirmed then", stalled)
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

// tonePageScript is tonePageHarness and the playerTone line and the tone's
// constants of app.js, then run, and the assembled script: that, then the
// shipped functions the tone's page reaches (with the router's when run asks
// for it).
func tonePageScript(t *testing.T, src, run string, entries ...string) (script, assembled string) {
	t.Helper()
	line := toneLineRe.FindString(src)
	if line == "" {
		t.Fatal("app.js declares no one-line playerTone")
	}
	script = tonePageHarness + line + "\n" + jsStatement(t, src, "const pageLeaving = ") + "\n" +
		extractJSConst(t, src, "PLAYER_TONE_WORDS") + extractJSConst(t, src, "PLAYER_TONE_LAST_ERRORS") +
		extractJSConst(t, src, "PLAYER_TONE_STATES") + run
	entries = append([]string{"wirePlayerOutputTone", "stopPlayerTone", "hidePlayerTone", "showPlayerToneAgain", "confirmPlayerToneStop",
		"readPlayerOutputForTone", "loadPlayerOutput", "renderPlayerOutput", "renderPlayerOutputTone", "showPlayerToneLine", "showMsg",
		"playerToneStatusText", "playerToneEndText", "playerToneFormatText", "playerRateText", "setDisabled", "pageSignal"}, entries...)
	return script, withReachedFunctions(t, script, src, entries...)
}

// TestANavigationEndsTheToneAsItBegins (B601's round 2): a navigation inside
// the console ends the page's tone as it begins, as Stop does, before the
// next page's fetch answers: boostSwap tells the page (pageLeaving) before its
// fetch, and tears the page's scope down only once the next page's body is in,
// as it did, so the page stays live while a navigation stalls. Until then the
// tone played on until the swap, its whole length where the fetch stalled.
// The router's refusals start no navigation and end nothing. Where node is
// installed it runs the shipped router and page: a link and a popstate to
// another page, each held at the fetch's headers and at its body, end the
// tone before either answers, the page's scope standing and Stop's
// confirmation reading the status; a modified click, another target, a
// download, another origin, an anchor on the page, the page's own address and
// a popstate within the page start no fetch and leave the tone playing.
func TestANavigationEndsTheToneAsItBegins(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	swap := stripJSComments(extractJSFunction(t, src, "boostSwap"))
	requireInOrder(t, "boostSwap", swap, `pageLeaving.dispatchEvent(new Event("leave"));`, "await fetch(", "await resp.text()", "pageAbort.abort()")
	if !regexp.MustCompile(`(?m)^const pageLeaving = new EventTarget\(\);$`).MatchString(src) {
		t.Error("app.js declares no pageLeaving at the top level")
	}
	wire := stripJSComments(extractJSFunction(t, src, "wirePlayerOutputTone"))
	if !strings.Contains(wire, `pageLeaving.addEventListener("leave", () => stopPlayerTone(scope), { signal: scope });`) {
		t.Error("wirePlayerOutputTone does not end its tone as a navigation begins, with the page's scope")
	}
	router := stripJSComments(extractJSFunction(t, src, "wireBoostRouter"))
	requireInOrder(t, "wireBoostRouter's click", router, `if (a.hasAttribute("download")) return;`, `if (a.target && a.target !== "_self") return;`,
		"if (url.origin !== location.origin) return;", "if (url.pathname === location.pathname && url.hash) return;",
		"if (url.href === location.href) { e.preventDefault(); return; }", "boostNavigate(url.href);")
	if strings.Contains(router, "pageLeaving") || strings.Contains(router, "stopPlayerTone") {
		t.Error("the router ends the tone itself: a navigation's start is boostSwap's alone, past the router's refusals")
	}
	script, assembled := tonePageScript(t, src, toneRouterRun, "wireBoostRouter", "boostNavigate", "boostSwap", "boostIsPlayerPath")
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped router and page")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name     string   `json:"name"`
		Started  bool     `json:"started"`
		Aborted  bool     `json:"aborted"`
		Scope    bool     `json:"scopeAborted"`
		Pending  bool     `json:"pending"`
		Gets     int      `json:"gets"`
		Line     string   `json:"line"`
		Assigned []string `json:"assigned"`
		Fetches  int      `json:"fetches"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 11 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs[:4] {
		if !r.Started || !r.Pending || !r.Aborted || r.Scope || r.Gets != 1 || r.Line != "Stopping the tone…" || r.Fetches != 1 || len(r.Assigned) != 1 {
			t.Errorf("%s: %+v; want the tone's request aborted while the navigation's fetch is held, the page's scope standing, Stop's confirmation reading the status, then the fallback's load", r.Name, r)
		}
	}
	for _, r := range runs[4:] {
		if r.Started || r.Aborted || r.Fetches != 0 || r.Gets != 0 || len(r.Assigned) != 0 {
			t.Errorf("%s: %+v; want no navigation, no fetch, and the tone playing on", r.Name, r)
		}
	}
}

// TestAPageTheCacheShowsAgainSaysWhatTheStatusSays (B601's round 2): a page
// the back/forward cache shows again (pageshow, persisted) after pagehide
// says nothing of a tone before it has read the status: its tone and a Stop's
// confirmation were ended as it was hidden (hidePlayerTone), and once shown
// again its tone's line is emptied, Play and Stop are those of a page with no
// tone out, a confirmation still out is ended, and the status is read at once,
// whose tone the shipped renderPlayerOutputTone then says, idle included
// (showPlayerToneAgain). Until then the line said "Playing…" over an idle
// tone, which the five-second paint left as it was. Where node is installed it
// runs the shipped page on one DOM, its listeners wired once: hidden with a
// Stop's confirmation out (its read held across the cache, then answering a
// tone still playing, late), and with its tone out, each shown again on an
// idle first status; shown again where the status cannot be read; and a
// pageshow that is not the cache's, which changes nothing.
func TestAPageTheCacheShowsAgainSaysWhatTheStatusSays(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	wire := stripJSComments(extractJSFunction(t, src, "wirePlayerOutputTone"))
	requireInOrder(t, "wirePlayerOutputTone", wire, `window.addEventListener("pageshow", (e) => {`,
		"if (e.persisted) showPlayerToneAgain(play, stop);", "}, { signal: scope });")
	hide := stripJSComments(extractJSFunction(t, src, "hidePlayerTone"))
	for _, want := range []string{"playerTone.controller?.abort();", "playerTone.stopSeq++;", "playerTone.confirming = false;", "playerTone.busyShown = true;"} {
		if !strings.Contains(hide, want) {
			t.Errorf("hidePlayerTone has no %s", want)
		}
	}
	show := stripJSComments(extractJSFunction(t, src, "showPlayerToneAgain"))
	requireInOrder(t, "showPlayerToneAgain", show, "playerTone.controller = null;", "playerTone.stopSeq++;", "playerTone.confirming = false;",
		"playerTone.busyShown = true;", `showPlayerToneLine("ok", "");`, "setDisabled(stop, true);", "setDisabled(play, false);", "loadPlayerOutput();")
	script, assembled := tonePageScript(t, src, toneShownAgainRun)
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped page")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name      string   `json:"name"`
		Posts     []string `json:"posts"`
		Aborted   bool     `json:"aborted"`
		Line      string   `json:"line"`
		Idle      string   `json:"idle"`
		Msg       string   `json:"msg"`
		PlayOff   bool     `json:"playDisabled"`
		StopOff   bool     `json:"stopDisabled"`
		Listeners []int    `json:"listeners"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 4 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs {
		if strings.Join(r.Posts, " ") != toneRoute {
			t.Errorf("%s: posted %v; want the tone's one post", r.Name, r.Posts)
		}
		if fmt.Sprint(r.Listeners) != "[1 1 1 1]" {
			t.Errorf("%s: Play, Stop, pagehide and pageshow have %v listeners; want one each, never wired again", r.Name, r.Listeners)
		}
	}
	confirming, playing, unread, notCached := runs[0], runs[1], runs[2], runs[3]
	for _, r := range []struct {
		name string
		got  any
		ok   bool
	}{
		{"hidden with a Stop's confirmation out", confirming, confirming.Aborted && confirming.Idle != "" && confirming.Line == confirming.Idle && !confirming.PlayOff && confirming.StopOff},
		{"hidden with its tone out", playing, playing.Aborted && playing.Idle != "" && playing.Line == playing.Idle && !playing.PlayOff && playing.StopOff},
		{"shown again where the status cannot be read", unread, unread.Aborted && unread.Line == "" && strings.HasPrefix(unread.Msg, "Couldn't read the player") && !unread.PlayOff && unread.StopOff},
		{"a pageshow that is not the cache's", notCached, !notCached.Aborted && strings.HasPrefix(notCached.Line, "Playing a 1 kHz tone") && notCached.PlayOff && !notCached.StopOff},
	} {
		if !r.ok {
			t.Errorf("%s: %+v", r.name, r.got)
		}
	}
}

// TestNoStaleStatusReadPaintsOverANewerTone (B601's round 3): a status read
// that answers after the page has moved on paints nothing. Play ends every read
// still out as it starts the tone and again as it says the tone's result (the
// paint's sequence, playerOutputSeq), so neither a Stop's confirmation nor the
// five-second paint, held across a newer tone, writes "A test tone is
// playing" over what the page said of that tone; and a Stop's confirmation
// paints only while it is current (readPlayerOutputForTone's predicate), so
// one whose page was hidden, which moves no sequence, paints nothing either.
// Where node is installed it runs the shipped page, each held read released
// with a status still playing and before its three seconds are up, nothing
// else read in between: a confirmation's read and a poll's, each held past a
// newer one-second tone that played to its end, and a confirmation's read
// held past the page's pagehide.
func TestNoStaleStatusReadPaintsOverANewerTone(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	wire := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "wirePlayerOutputTone"))
	requireInOrder(t, "Play", wire, `play.addEventListener("click", async () => {`, "playerOutputSeq++;",
		`await API.post("/api/output/player/tone"`, "if (!scope.aborted) {", "playerOutputSeq++;", `showPlayerToneLine(r.outcome === "played"`,
		"if (!controller.signal.aborted && !scope.aborted) {", "playerOutputSeq++;", `showPlayerToneLine("err"`)
	read := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "readPlayerOutputForTone"))
	requireInOrder(t, "readPlayerOutputForTone", read, "async function readPlayerOutputForTone(scope, ms, current) {",
		"if (current() && seq === playerOutputSeq) renderPlayerOutput(st);")
	confirm := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "confirmPlayerToneStop"))
	requireInOrder(t, "confirmPlayerToneStop", confirm, "readPlayerOutputForTone(scope, deadline - Date.now(), current)")
	script, assembled := tonePageScript(t, src, toneStaleRun)
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped page")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name    string   `json:"name"`
		Posts   []string `json:"posts"`
		Line    string   `json:"line"`
		Painted bool     `json:"painted"`
		Now     int      `json:"now"`
		Held    int      `json:"held"`
		Played  string   `json:"playedLine"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 3 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs {
		if r.Painted || r.Now != 0 || r.Held != 0 {
			t.Errorf("%s: %+v; want the stale read, released before its three seconds, to paint nothing", r.Name, r)
		}
	}
	confirmation, poll, hidden := runs[0], runs[1], runs[2]
	if len(confirmation.Posts) != 2 || confirmation.Played == "" || confirmation.Line != confirmation.Played {
		t.Errorf("%s: the line says %q; want the newer tone's result, %q", confirmation.Name, confirmation.Line, confirmation.Played)
	}
	if len(poll.Posts) != 1 || poll.Played == "" || poll.Line != poll.Played {
		t.Errorf("%s: the line says %q; want the newer tone's result, %q", poll.Name, poll.Line, poll.Played)
	}
	if hidden.Line != "Stopping the tone…" {
		t.Errorf("%s: the line says %q; want the hidden page's line as its pagehide left it", hidden.Name, hidden.Line)
	}
}

// extractJSFunctionIfPresentOrEmpty is a top-level function of src, or "" for
// none, so a structural check fails by what it looks for rather than stopping
// the test before the cases node runs.
func extractJSFunctionIfPresentOrEmpty(src, name string) string {
	body, _ := extractJSFunctionIfPresent(src, name)
	return body
}

// TestAStopsConfirmationKeepsTheLastToneItRead (B601's round 3): a Stop's
// confirmation remembers the tone of the last status it read: a read that
// fails or is cut off at the three seconds keeps it, and a read that answers
// replaces it, with none where no tone runs. So a read cut off after one that
// read playing says the bridge still reads the tone as playing, where it said
// the status could not be read; a status with no tone after one that read
// playing says so, never that the tone still plays; and every stop said not
// confirmed leaves its line for the next status to say again (busyShown), so
// an ordinary idle status replaces the warning. Where node is installed it runs
// the shipped page: playing then a read held to the deadline, playing then a
// status with no tone, and a read that never answers, each followed by an
// idle status.
func TestAStopsConfirmationKeepsTheLastToneItRead(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	confirm := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "confirmPlayerToneStop"))
	requireInOrder(t, "confirmPlayerToneStop", confirm, "for (;;) {", "if (st) tone = t;", "playerTone.busyShown = true;", "The stop is not confirmed")
	if strings.Contains(confirm, "if (t) tone = t;") {
		t.Error("confirmPlayerToneStop keeps a tone across a status that read none")
	}
	script, assembled := tonePageScript(t, src, toneLastReadRun)
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped page")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name   string `json:"name"`
		Warned string `json:"warned"`
		After  string `json:"after"`
		Idle   string `json:"idle"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 3 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for i, want := range []string{
		"The stop is not confirmed: three seconds on, the bridge still reads the tone as playing.",
		"The stop is not confirmed: the bridge's player is not running, and its status says nothing of the tone.",
		"The stop is not confirmed: the bridge's status could not be read.",
	} {
		r := runs[i]
		if r.Warned != want {
			t.Errorf("%s: the stop's line says %q; want %q", r.Name, r.Warned, want)
		}
		if r.Idle == "" || r.After != r.Idle {
			t.Errorf("%s: after an idle status the line says %q; want the status's own words, %q", r.Name, r.After, r.Idle)
		}
	}
}

// TestAStopNotConfirmedGivesWayOnceTheDACCloses (B601's round 4): a line that
// says a tone's stop is not confirmed (a Stop's confirmation that read
// stop_unconfirmed, or the tone's own answer stop_unconfirmed) is left for the
// next status to say again, so once the DAC closes an ordinary idle status
// replaces the warning with the idle tone's words; until then the warning
// stayed under every idle status. A newer message still wins: Play takes the
// flag back, so an idle status after a refused Play leaves the refusal as it
// is. Where node is installed it runs the shipped page: a confirmation that
// read stop_unconfirmed, then an idle status; the tone's answer
// stop_unconfirmed, then an idle status; and a confirmation that read
// stop_unconfirmed, a new Play the bridge refused (409 player_sounding), then
// an idle status.
func TestAStopNotConfirmedGivesWayOnceTheDACCloses(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	confirm := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "confirmPlayerToneStop"))
	requireInOrder(t, "confirmPlayerToneStop", confirm, `if (t && state !== "playing" && state !== "stopping") {`,
		`playerTone.busyShown = state !== "idle";`, `if (state === "idle") showPlayerToneLine("ok", "The tone stopped.");`)
	wire := stripJSComments(extractJSFunctionIfPresentOrEmpty(src, "wirePlayerOutputTone"))
	requireInOrder(t, "Play", wire, `play.addEventListener("click", async () => {`, "playerTone.confirming = false;",
		"playerTone.busyShown = false;", `await API.post("/api/output/player/tone"`, `showPlayerToneLine(r.outcome === "played"`,
		`playerTone.busyShown = r.outcome === "stop_unconfirmed";`)
	script, assembled := tonePageScript(t, src, toneWarningRun)
	requireCarried(t, script, assembled[len(script):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the rest runs the shipped page")
	}
	raw := runScriptUnderNode(t, node, assembled, struct{}{})
	var runs []struct {
		Name   string `json:"name"`
		Warned string `json:"warned"`
		After  string `json:"after"`
		Idle   string `json:"idle"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 3 {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, r := range runs[:2] {
		if !strings.Contains(r.Warned, "has not confirmed that it let go") || r.Idle == "" || r.After != r.Idle {
			t.Errorf("%s: %+v; want the stop's warning, then the idle status's own words", r.Name, r)
		}
	}
	if r := runs[2]; !strings.HasPrefix(r.Warned, "The tone did not play: ") || r.After != r.Warned {
		t.Errorf("%s: %+v; want the newer Play's refusal kept under the idle status", r.Name, r)
	}
}

// tonePageHarness is the tone's page as the shipped functions see it: buttons
// and selects by id, whose click runs their listeners; a document and a window
// whose listeners a scope's signal removes and whose events the run
// dispatches; the page's scope (pageAbort, as dispatchPageInit keeps it); the
// console's API, whose post answers when the run resolves or rejects it
// (posts[i].resolve, posts[i].reject) or its signal aborts it, and whose get
// answers the statuses queued, the last
// one again and again, a held one when the run releases it or its signal
// aborts it, a failed one with an error; fetch, which a navigation's run holds
// and answers; a clock that the run's advance alone moves, firing each timer
// due in turn; and the Player page's status painted by the shipped
// renderPlayerOutput, what it paints beside the tone stubbed.
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
const docListeners = [];
const windowListeners = [];
const listen = (list) => (type, handler, opts) => { list.push({ type, handler, signal: opts && opts.signal }); };
globalThis.document = { getElementById: (id) => el(id), activeElement: null, body: { dataset: {} }, addEventListener: listen(docListeners) };
globalThis.window = { addEventListener: listen(windowListeners) };
function dispatchTo(list, target, type, ev) {
  for (const l of [...list]) if (l.type === type && !(l.signal && l.signal.aborted)) l.handler.call(target, ev);
}
const dispatchWindow = (type, ev) => dispatchTo(windowListeners, globalThis.window, type, ev || { type });
const liveListeners = (list, type) => list.filter((l) => l.type === type && !(l.signal && l.signal.aborted)).length;
const realSetTimeout = setTimeout;
let now = 0;
const RealDate = Date;
globalThis.Date = class extends RealDate { static now() { return now; } };
let timers = [];
let timerSeq = 0;
globalThis.setTimeout = (fn, ms) => { const id = ++timerSeq; timers.push({ id, at: now + Math.max(0, Number(ms) || 0), fn }); return id; };
globalThis.clearTimeout = (id) => { timers = timers.filter((x) => x.id !== id); };
const settle = () => new Promise((resolve) => realSetTimeout(resolve, 0));
async function advance(until) {
  for (;;) {
    await settle();
    timers.sort((a, b) => a.at - b.at || a.id - b.id);
    const due = timers[0];
    if (!due || due.at > until) break;
    timers.shift();
    now = Math.max(now, due.at);
    due.fn();
  }
  now = Math.max(now, until);
  await settle();
}
const posts = [];
const gets = [];
let statuses = [];
const HOLD = "hold";
const FAIL = "fail";
const held = [];
function release(st) {
  const h = held.shift();
  if (h) h.resolve(st);
}
const abortError = () => { const e = new Error("aborted"); e.name = "AbortError"; return e; };
const API = {
  post(path, body, signal) {
    return new Promise((resolve, reject) => {
      posts.push({ path, body, signal, resolve, reject });
      if (signal) signal.addEventListener("abort", () => reject(abortError()));
    });
  },
  get(path, signal) {
    gets.push(path);
    const next = statuses.length > 1 ? statuses.shift() : statuses[0];
    if (next === FAIL) return Promise.reject(new Error("the bridge did not answer"));
    if (next !== HOLD) return Promise.resolve(next);
    return new Promise((resolve, reject) => {
      held.push({ resolve });
      if (signal) signal.addEventListener("abort", () => reject(abortError()));
    });
  },
};
const fetches = [];
globalThis.fetch = (url) => new Promise((resolve, reject) => { fetches.push({ url, resolve, reject }); });
let pageAbort = null;
let playerOutputLatest = null;
let playerOutputSeq = 0;
let lineAt = -1;
function renderPlayerOutputRunning() {}
function renderPlayerOutputTransports() {}
function renderPlayerOutputConfigured() {}
function renderPlayerOutputOpenHome() {}
function playerOpenHomeNote() { return ""; }
function renderPlayerOutputPending() {}
const status = (state) => ({ available: true, managed: {}, running: { tone: { state, levelDBFS: -40, seconds: 10,
  format: "S24_3LE", rateHz: 48000, validBits: 24, outcome: state === "idle" ? "request_ended" : "", lastError: "" } } });
// freshPage wires the tone on a page of its own: a line that notes when it
// was last set, its controls, and its scope.
function freshPage() {
  els.clear(); posts.length = 0; gets.length = 0; held.length = 0; fetches.length = 0;
  docListeners.length = 0; windowListeners.length = 0; timers = []; now = 0; lineAt = -1;
  if (pageAbort) pageAbort.abort();
  pageAbort = new AbortController();
  const line = el("player-output-tone-msg");
  let text = "";
  Object.defineProperty(line, "textContent", { get: () => text, set: (v) => { text = String(v); lineAt = now; } });
  el("player-output-tone-level").value = "-40";
  el("player-output-tone-seconds").value = "10";
  el("player-output-tone-stop").disabled = true;
  playerOutputLatest = { available: true, managed: {}, running: {} };
  wirePlayerOutputTone();
}
const out = [];
`

// tonePageRun plays a tone on a fresh page for each case and ends it as the
// case does, the clock run on past every timer, printing what the page posted
// and read, what its line says and when it was last set.
const tonePageRun = `
async function run(name, queued, end) {
  freshPage();
  statuses = queued;
  el("player-output-tone-play").click();
  await settle();
  const signal = posts[0] && posts[0].signal;
  end();
  await advance(10000);
  out.push({ name, posts: posts.map((p) => p.path), aborted: Boolean(signal && signal.aborted), gets: gets.length,
    line: el("player-output-tone-msg").textContent, lineAt, playDisabled: el("player-output-tone-play").disabled,
    stopDisabled: el("player-output-tone-stop").disabled });
}
const stop = () => el("player-output-tone-stop").click();
await run("confirmed", [status("playing"), status("stopping"), status("idle")], stop);
await run("playing to the end", [status("playing")], stop);
await run("stopping to the end", [status("stopping")], stop);
await run("a read that never answers", [HOLD], stop);
await run("pagehide", [status("playing")], () => dispatchWindow("pagehide"));
await run("the page's scope", [status("playing")], () => pageAbort.abort());
console.log(JSON.stringify(out));
`

// toneRouterRun plays a tone on a fresh page with the console's router wired
// for each case, starts a navigation as the case does (a click on a link the
// router takes, or a popstate to another page, held at the fetch's headers or
// at its body; or one of the router's refusals), and prints, while the
// navigation is held, whether the tone's request was aborted and the page's
// scope stands, what Stop's confirmation read and said, then what the
// navigation's fallback loaded once its fetch failed.
const toneRouterRun = `
const BOOST_ENABLED = true;
let boostGen = 0;
let lastBoostPath = "/output";
const assigned = [];
const origin = "http://127.0.0.1:7789";
globalThis.location = { origin, pathname: "/output", hash: "", search: "", href: origin + "/output", assign: (h) => { assigned.push(h); } };
globalThis.history = { pushState() {} };
function boostUpdateTopNav() {}
function runInlineScripts() {}
function boostFocusMain() {}
function dispatchPageInit() {}
function recycleEventStream() {}
const goTo = (path) => { location.pathname = path; location.href = origin + path; };
function clickLink(href, opts) {
  const o = opts || {};
  const a = { href: new URL(href, origin).href, target: o.target || "", hasAttribute: (n) => n === "download" && Boolean(o.download) };
  const ev = { defaultPrevented: false, button: 0, metaKey: Boolean(o.metaKey), ctrlKey: false, shiftKey: false, altKey: false,
    target: { closest: () => a }, preventDefault() { this.defaultPrevented = true; } };
  dispatchTo(docListeners, globalThis.document, "click", ev);
}
const bodies = [];
const answerHeaders = () => fetches[0].resolve({ ok: true, redirected: false,
  headers: { get: (h) => ({ "X-Bridge-Active": "settings", "X-Bridge-Section": "operator" })[h] || null },
  text: () => new Promise((resolve, reject) => { bodies.push({ resolve, reject }); }) });
async function navigate(name, start, holdBody) {
  freshPage();
  goTo("/output"); lastBoostPath = "/output"; assigned.length = 0; bodies.length = 0;
  wireBoostRouter();
  statuses = [status("playing")];
  el("player-output-tone-play").click();
  await settle();
  const signal = posts[0].signal;
  const scope = pageAbort.signal;
  start();
  await settle();
  const started = fetches.length > 0;
  if (started && holdBody) { answerHeaders(); await settle(); }
  const pending = started && (holdBody ? bodies.length === 1 : true);
  const seen = { name, started, pending, aborted: signal.aborted, scopeAborted: scope.aborted, gets: gets.length,
    line: el("player-output-tone-msg").textContent, fetches: fetches.length };
  if (started) {
    if (holdBody) bodies[0].reject(new Error("cut")); else fetches[0].reject(new Error("cut"));
    await settle();
  }
  out.push({ ...seen, assigned: [...assigned] });
}
await navigate("a link, its headers held", () => clickLink("/settings"), false);
await navigate("a link, its body held", () => clickLink("/settings"), true);
await navigate("a popstate to another page, its headers held", () => { goTo("/settings"); dispatchWindow("popstate"); }, false);
await navigate("a popstate to another page, its body held", () => { goTo("/settings"); dispatchWindow("popstate"); }, true);
await navigate("a modified click", () => clickLink("/settings", { metaKey: true }), false);
await navigate("another target", () => clickLink("/settings", { target: "_blank" }), false);
await navigate("a download", () => clickLink("/settings", { download: true }), false);
await navigate("another origin", () => clickLink("https://elsewhere.example/settings"), false);
await navigate("an anchor on the page", () => clickLink("/output#tone"), false);
await navigate("the page's own address", () => clickLink("/output"), false);
await navigate("a popstate within the page", () => dispatchWindow("popstate"), false);
console.log(JSON.stringify(out));
`

// toneShownAgainRun hides a page and shows it again from the back/forward
// cache for each case, on the DOM and listeners its one wiring made, and
// prints what its line says against what the shipped words say of the idle
// tone the status reads, the page's message line, its controls, and how many
// listeners Play, Stop, pagehide and pageshow have.
const toneShownAgainRun = `
const shownAgain = { type: "pageshow", persisted: true };
async function cached(name, queued, before, after) {
  freshPage();
  statuses = queued;
  el("player-output-tone-play").click();
  await settle();
  const signal = posts[0].signal;
  before();
  await settle();
  after();
  await settle();
  release(status("playing"));
  await advance(10000);
  out.push({ name, posts: posts.map((p) => p.path), aborted: signal.aborted, line: el("player-output-tone-msg").textContent,
    idle: playerToneStatusText(status("idle").running.tone), msg: el("player-output-msg").textContent,
    playDisabled: el("player-output-tone-play").disabled, stopDisabled: el("player-output-tone-stop").disabled,
    listeners: [(el("player-output-tone-play").listeners.click || []).length, (el("player-output-tone-stop").listeners.click || []).length,
      liveListeners(windowListeners, "pagehide"), liveListeners(windowListeners, "pageshow")] });
}
const hideAndShow = () => { dispatchWindow("pagehide"); dispatchWindow("pageshow", shownAgain); };
await cached("hidden with a Stop's confirmation out", [HOLD, status("idle")], () => el("player-output-tone-stop").click(), hideAndShow);
await cached("hidden with its tone out", [status("idle")], () => {}, hideAndShow);
await cached("shown again where the status cannot be read", [FAIL], () => {}, hideAndShow);
await cached("a pageshow that is not the cache's", [status("idle")], () => {}, () => dispatchWindow("pageshow", { type: "pageshow", persisted: false }));
console.log(JSON.stringify(out));
`

// toneStaleRun holds one status read for each case, lets the page move on
// (a newer tone that plays to its end, or the page's pagehide), then releases
// the held read with a status still playing, before its three seconds are up
// and with nothing else read in between, and prints what the line says,
// whether the released status was painted, the clock, the reads still held,
// and the words of the newer tone's result.
const toneStaleRun = `
const played = { outcome: "played", admitted: true, format: "S24_3LE", rateHz: 48000, validBits: 24, frames: 48000 };
const playedLine = playerToneEndText(played.outcome, "") + playerToneFormatText(played.format, played.rateHz, played.validBits, "played");
const play = () => el("player-output-tone-play").click();
const stop = () => el("player-output-tone-stop").click();
async function stale(name, setup) {
  freshPage();
  statuses = [HOLD];
  await setup();
  const old = status("playing");
  release(old);
  await settle();
  out.push({ name, posts: posts.map((p) => p.path), line: el("player-output-tone-msg").textContent,
    painted: playerOutputLatest === old, now, held: held.length, playedLine });
}
await stale("a Stop's confirmation read answering after a newer tone's result", async () => {
  play(); await settle();
  stop(); await settle();
  play(); await settle();
  posts[1].resolve(played); await settle();
});
await stale("a poll's read answering after a newer tone's result", async () => {
  loadPlayerOutput(); await settle();
  play(); await settle();
  posts[0].resolve(played); await settle();
});
await stale("a Stop's confirmation read answering after the page was hidden", async () => {
  play(); await settle();
  stop(); await settle();
  dispatchWindow("pagehide"); await settle();
});
console.log(JSON.stringify(out));
`

// toneLastReadRun plays a tone and stops it for each case, the confirmation's
// reads answering the statuses queued, runs the clock past its three seconds,
// then paints an ordinary idle status as the five-second paint does, and
// prints what the stop's line said, what the line says after the idle status,
// and the shipped words of that idle tone.
const toneLastReadRun = `
const noTone = { available: true, managed: {}, running: {} };
async function lastRead(name, queued) {
  freshPage();
  statuses = queued;
  el("player-output-tone-play").click();
  await settle();
  el("player-output-tone-stop").click();
  await advance(10000);
  const warned = el("player-output-tone-msg").textContent;
  statuses = [status("idle")];
  loadPlayerOutput();
  await settle();
  out.push({ name, warned, after: el("player-output-tone-msg").textContent, idle: playerToneStatusText(status("idle").running.tone) });
}
await lastRead("a read cut off at the three seconds after one that read playing", [status("playing"), HOLD]);
await lastRead("a status with no tone after one that read playing", [status("playing"), noTone]);
await lastRead("a read that never answers", [HOLD]);
console.log(JSON.stringify(out));
`

// toneWarningRun leaves the tone's line on a stop not confirmed for each case
// (a confirmation that read stop_unconfirmed, the tone's own answer
// stop_unconfirmed) or on a refusal of a newer Play after one, then paints an
// ordinary idle status as the five-second paint does, and prints what the
// line said before the idle status, what it says after, and the shipped words
// of that idle tone.
const toneWarningRun = `
const play = () => el("player-output-tone-play").click();
const stop = () => el("player-output-tone-stop").click();
const unconfirmed = { outcome: "stop_unconfirmed", admitted: true, format: "S24_3LE", rateHz: 48000, validBits: 24, frames: 4800 };
const sounding = () => Object.assign(new Error("the player sounds"), { status: 409, code: "player_sounding" });
async function warning(name, setup) {
  freshPage();
  statuses = [status("stop_unconfirmed")];
  await setup();
  const warned = el("player-output-tone-msg").textContent;
  statuses = [status("idle")];
  loadPlayerOutput();
  await settle();
  out.push({ name, warned, after: el("player-output-tone-msg").textContent, idle: playerToneStatusText(status("idle").running.tone) });
}
await warning("a confirmation that read stop_unconfirmed, then an idle status", async () => {
  play(); await settle();
  stop(); await settle();
});
await warning("the tone's answer stop_unconfirmed, then an idle status", async () => {
  play(); await settle();
  posts[0].resolve(unconfirmed); await settle();
});
await warning("a confirmation that read stop_unconfirmed, a newer Play refused, then an idle status", async () => {
  play(); await settle();
  stop(); await settle();
  play(); await settle();
  posts[1].reject(sounding()); await settle();
});
console.log(JSON.stringify(out));
`
