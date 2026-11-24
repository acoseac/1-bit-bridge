package admin

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The player bar's output (B634, PR 2), structurally, on every host: the
// player frame's way from the event stream to the bar, and every field of
// the state the bar takes rendered somewhere, the guard every console
// payload has, in both directions.

// TestThePlayerFrameIsWiredFromTheStreamToTheBar pins the frame's way: the
// stream sends a player event (handlers_events.go), app.js's stream hands
// each one to playerFrame with its EventSource, which dispatches
// bridge:player with the frame and its connection's serial, and the
// stream's open and error to playerStreamEvent, which dispatches
// bridge:player-stream; both drop a retired EventSource's; output.js's
// listen takes both window events to the output; boot.js's wireOutput calls
// listen over the page's window and document, and boot runs wireOutput.
func TestThePlayerFrameIsWiredFromTheStreamToTheBar(t *testing.T) {
	events := readFile(t, "handlers_events.go")
	if !strings.Contains(events, `publish("player", pub.Bytes)`) {
		t.Error("the event stream sends no player event from the publication's bytes")
	}
	app := stripJSComments(readConsoleJS(t, "static/app.js"))
	stream := extractJSFunction(t, app, "startEventStream")
	for _, want := range []string{
		`es.addEventListener("player",`, `playerFrame(es, e.data)`,
		`playerStreamEvent(es, "open")`, `playerStreamEvent(es, "error")`,
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("startEventStream lacks %s", want)
		}
	}
	frame := extractJSFunction(t, app, "playerFrame")
	life := extractJSFunction(t, app, "playerStreamEvent")
	for name, body := range map[string]string{"playerFrame": frame, "playerStreamEvent": life} {
		if !strings.Contains(body, "if (es !== activeEventSource) return;") {
			t.Errorf("%s takes what a retired EventSource dispatches", name)
		}
	}
	if !strings.Contains(frame, `"bridge:player"`) || !strings.Contains(frame, "serial: es.bridgeSerial") || !strings.Contains(frame, "data,") {
		t.Error("playerFrame does not dispatch bridge:player with the frame and its connection's serial")
	}
	if !strings.Contains(life, `"bridge:player-stream"`) || !strings.Contains(life, "playerStreamSerial += 1") {
		t.Error("playerStreamEvent does not give each open a serial of its own")
	}
	out := stripJSComments(readFile(t, "static/player/output.js"))
	listen := extractJSFunction(t, out, "listen")
	for _, want := range []string{
		`win.addEventListener("bridge:player", (e) => o.frame(e.detail?.data, e.detail?.serial))`,
		`win.addEventListener("bridge:player-stream"`, `o.streamOpen(e.detail.serial)`, `o.streamError(e.detail.serial)`,
		`doc.addEventListener("visibilitychange"`, `win.addEventListener("pageshow"`,
	} {
		if !strings.Contains(listen, want) {
			t.Errorf("output.js's listen lacks %s", want)
		}
	}
	boot := stripJSComments(readFile(t, "static/player/boot.js"))
	if !strings.Contains(extractJSFunction(t, boot, "wireOutput"), "listen(o, { win: window, doc: document") {
		t.Error("boot.js's wireOutput does not listen over the page's window and document")
	}
	if !strings.Contains(extractJSFunction(t, boot, "boot"), "wireOutput();") {
		t.Error("boot does not run wireOutput")
	}
}

// barPromise is the design's §3.3, the promise the bar keeps.
const barPromise = "The console never starts this browser while it knows the bridge's player plays. " +
	"Another controller can still start the bridge's player after the console's last check, and the bar then shows the bridge playing."

// TestTheBarsPromiseIsSaidWordForWord pins §3.3's promise where the bar's
// help states it, word for word: the output menu's own help (output.js's
// PROMISE) and the Player page's.
func TestTheBarsPromiseIsSaidWordForWord(t *testing.T) {
	js := readFile(t, "static/player/output.js")
	m := regexp.MustCompile(`export const PROMISE = "([^"]*)" \+\s*"([^"]*)";`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("output.js declares no PROMISE in two parts: the test has stopped reading it")
	}
	if got := m[1] + m[2]; got != barPromise {
		t.Errorf("the bar's menu says %q, want §3.3's %q", got, barPromise)
	}
	if !strings.Contains(readFile(t, "static/player/nowplaying.js"), "text: PROMISE") {
		t.Error("the output menu does not say the promise")
	}
	srv, _, _ := newPlayerTestServer(t)
	page := renderOutputPage(t, srv)
	span := regexp.MustCompile(`<span id="player-output-promise">([^<]*)</span>`).FindStringSubmatch(page)
	if span == nil || strings.ReplaceAll(span[1], "&#39;", "'") != barPromise {
		t.Errorf("the Player page's help says %v, want §3.3's %q", span, barPromise)
	}
}

// TestEveryRowDecidesByTheOutputChosen pins §7.6's one decision where the
// rows are drawn: views.js's trackRow, unplayableNote and collectionActions
// each ask playFor for the output chosen, and nothing in views.js decides a
// row's playability from its play field alone, as every list did before
// the bar had an output. TestRowsFollowTheChosenOutput runs playFor itself
// under node; this is its way into the rows, on every host.
func TestEveryRowDecidesByTheOutputChosen(t *testing.T) {
	views := stripJSComments(readFile(t, "static/player/views.js"))
	for _, fn := range []string{"trackRow", "unplayableNote", "collectionActions"} {
		body := extractJSFunction(t, views, fn)
		if !strings.Contains(body, "const output = chosenOutput();") || !strings.Contains(body, "playFor(t, output)") {
			t.Errorf("%s does not decide by playFor for the output chosen", fn)
		}
	}
	for _, re := range []string{`\.play\??\.kind`, `unplayableReason\(`} {
		if m := regexp.MustCompile(re).FindString(views); m != "" {
			t.Errorf("views.js decides a row's playability apart from playFor: %s", m)
		}
	}
}

// The functions of audio.js that play the element, each named for why: a
// new play anywhere else fails TestEveryStartOfThisBrowserAsksTheGate, and
// is to be routed through the gate or added here with its reason.
var audioPlaySites = map[string]string{
	"playEl": "the one play the bar makes (round 4, T1), marked for the play event it causes, called by a start the gate let through, a continuation continueStart let go on, or an admitted resume of the browser's own",
}

// The exported functions of audio.js that reach a play of the element, by
// how: gated asks the start gate first (requestStart); afterBarrier is
// called by output.js alone, on a barrier's done, with its permit;
// continued goes on by itself only through continueStart (round 3, S1).
// A function whose every play is handed to a gate (removeAt's successor,
// through continueStart) is no start: reach finds none in it.
var audioStarts = map[string]string{
	"playQueue": "gated",
	"toggle":    "gated",
	"advance":   "gated",
	"take":      "afterBarrier",
	"resume":    "afterBarrier",
	"start":     "afterBarrier",
	"init":      "continued",
}

// audioGateCallRe starts a call that hands what it runs to a gate, a start
// (requestStart) or what playback does by itself (continueStart), and
// audioStoppedLoadRe a load that plays nothing: what a gate call defers is
// the gate's, a stopped load no play, and reach follows neither.
var (
	audioGateCallRe    = regexp.MustCompile(`(?:^|[^\w$.])(?:requestStart|continueStart)\(`)
	audioStoppedLoadRe = regexp.MustCompile(`(?:^|[^\w$.])load\([^()]*\{\s*autoplay:\s*false\s*\}\)`)
	// jsFuncHeadRe starts a function literal: an arrow's parameters, in
	// parentheses or one name, or a function expression's keyword.
	jsFuncHeadRe = regexp.MustCompile(`^\s*(?:async\s+)?(?:(\()|[A-Za-z_$][\w$]*\s*=>|(function)\b[^(]*\()`)
)

// withoutGatedCalls is body as reach reads it: every stopped load taken
// out, every string, template and regular-expression literal's text blanked
// (jsMaskLiterals), so a parenthesis or a call inside one is none, and the
// body of every function literal handed to a gate call as an argument
// blanked, since the gate runs it when it lets it. Everything else a gate
// call's arguments hold runs as the call is made, and stays.
func withoutGatedCalls(body string) string {
	code := []byte(jsMaskLiterals(audioStoppedLoadRe.ReplaceAllString(body, " ")))
	for at := 0; ; {
		loc := audioGateCallRe.FindIndex(code[at:])
		if loc == nil {
			return string(code)
		}
		open := at + loc[1] - 1
		end := jsMatch(code, open)
		for s := open + 1; s < end; {
			e := jsArgEnd(code, s, end)
			jsBlankFuncBody(code, s, e)
			s = e + 1
		}
		at = open + 1
	}
}

// jsMaskLiterals is src with the text of every string, template and
// regular-expression literal blanked to spaces, newlines kept, and a
// template's substitutions kept as the code they are. A slash starts a
// regular expression after a byte that cannot end an operand (an operator,
// an opening bracket, a comma or the start), and divides otherwise; a slash
// after a keyword (return /x/) reads as a division.
func jsMaskLiterals(src string) string {
	b := []byte(src)
	blank := func(from, to int) {
		for k := from; k < to && k < len(b); k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	var subst []int
	depth, inTemplate := 0, false
	prev := byte(0)
	for i := 0; i < len(b); {
		c := b[i]
		if inTemplate {
			switch {
			case c == '\\':
				blank(i, i+2)
				i += 2
			case c == '`':
				inTemplate, prev = false, '`'
				i++
			case c == '$' && i+1 < len(b) && b[i+1] == '{':
				subst = append(subst, depth)
				depth++
				inTemplate, prev = false, '{'
				i += 2
			default:
				blank(i, i+1)
				i++
			}
			continue
		}
		switch {
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(b) && b[j] != c && b[j] != '\n' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			blank(i+1, j)
			i, prev = j+1, c
		case c == '`':
			inTemplate = true
			i++
		case c == '/' && (prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0):
			j, class := i+1, false
			for j < len(b) && b[j] != '\n' && (class || b[j] != '/') {
				switch b[j] {
				case '\\':
					j++
				case '[':
					class = true
				case ']':
					class = false
				}
				j++
			}
			blank(i+1, j)
			i, prev = j+1, '/'
		case c == '{':
			depth++
			i, prev = i+1, c
		case c == '}':
			depth--
			if n := len(subst); n > 0 && subst[n-1] == depth {
				subst, inTemplate = subst[:n-1], true
			}
			i, prev = i+1, c
		default:
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prev = c
			}
			i++
		}
	}
	return string(b)
}

// jsMatch is the index of the bracket that closes the one at code[open],
// counting every kind of bracket, in code whose literals are masked; the
// end of code where none does.
func jsMatch(code []byte, open int) int {
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(code)
}

// jsArgEnd is the end of the argument that starts at s: the next comma
// outside brackets before end, or end.
func jsArgEnd(code []byte, s, end int) int {
	for i := s; i < end; i++ {
		switch code[i] {
		case '(', '[', '{':
			i = jsMatch(code, i)
		case ',':
			return i
		}
	}
	return end
}

// jsBlankFuncBody blanks the body of the function literal that code[s:e]
// is, a block's inside or an arrow's expression; an argument that is no
// function literal is left as it is.
func jsBlankFuncBody(code []byte, s, e int) {
	m := jsFuncHeadRe.FindSubmatchIndex(code[s:e])
	if m == nil {
		return
	}
	body := s + m[1]
	if m[2] >= 0 || m[4] >= 0 {
		// The parameters' parentheses, then an arrow (or a function's block).
		close := jsMatch(code, body-1)
		body = close + 1
		if m[2] >= 0 {
			arrow := regexp.MustCompile(`^\s*=>`).FindIndex(code[body:e])
			if arrow == nil {
				return
			}
			body += arrow[1]
		}
	}
	for body < e && (code[body] == ' ' || code[body] == '\t' || code[body] == '\n' || code[body] == '\r') {
		body++
	}
	to := e
	if body < e && code[body] == '{' {
		body, to = body+1, jsMatch(code, body)
	}
	for k := body; k < to; k++ {
		if code[k] != '\n' {
			code[k] = ' '
		}
	}
}

// The continuations of audio.js, each by the function that makes it: what
// playback does by itself, through continueStart alone (round 3, S1).
var audioContinuations = map[string]string{
	"init":              "a track's end, the element's ended",
	"handleSourceError": "the skip past a source that cannot play",
	"removeAt":          "a removed row's successor",
	"onError":           "the retry after a network or decode error",
}

// jsTopFunctions is every top-level function of src by name: its body (to
// the next one's start) and whether it is exported.
func jsTopFunctions(src string) map[string]struct {
	body     string
	exported bool
} {
	out := map[string]struct {
		body     string
		exported bool
	}{}
	locs := barFnRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		head := src[loc[0]:loc[1]]
		out[src[loc[2]:loc[3]]] = struct {
			body     string
			exported bool
		}{src[loc[0]:end], strings.HasPrefix(head, "export")}
	}
	return out
}

// TestEveryStartOfThisBrowserAsksTheGate pins R1 at the source, on every
// host: in audio.js every exported function that reaches a play of the
// element (its body, or a function of the module it calls) asks the start
// gate first, or is one of the barrier's continuations output.js calls with
// a permit, or keeps playing what plays; every play is in a function
// audioPlaySites names; the MediaSession's handlers start through toggle and
// advance alone; no other player module plays an element or calls take,
// resume or start of audio.js; output.js hands each of those a permit, or
// takes a queue stopped; and boot.js wires the gate before the bar mounts.
// TestEveryStartOfThisBrowserGoesThroughTheGate runs the wiring under node.
func TestEveryStartOfThisBrowserAsksTheGate(t *testing.T) {
	src := stripJSComments(readFile(t, "static/player/audio.js"))
	fns := jsTopFunctions(src)
	if len(fns) < 30 {
		t.Fatalf("only %d functions found in audio.js: the reader has stopped finding them", len(fns))
	}
	for _, v := range audioStartViolations(src) {
		t.Error(v)
	}
	checkTheContinuationsAndTheGatesWiring(t, src, fns)
}

// audioStartViolations is what the start guard finds wrong in src, audio.js
// with its comments stripped: a play outside the sites audioPlaySites names,
// an exported function that reaches a play and is no start audioStarts
// names, and a start audioStarts names that does not do as it says, a gated
// one's own code included, read beside what it hands the gate.
func audioStartViolations(src string) []string {
	var out []string
	fns := jsTopFunctions(src)
	plays := map[string]bool{}
	for name, f := range fns {
		if strings.Contains(f.body, ".play(") {
			plays[name] = true
			if _, ok := audioPlaySites[name]; !ok {
				out = append(out, fmt.Sprintf("audio.js's %s plays the element and is no play site this test names: route it through the gate, or name it with its reason", name))
			}
		}
	}
	for name := range audioPlaySites {
		if !plays[name] {
			out = append(out, fmt.Sprintf("audioPlaySites names %s, which plays nothing now: take it off the list", name))
		}
	}
	// reach is whether a function reaches a play through the module's calls,
	// outside what it hands a gate. A call of a gated start is accounted for
	// where that start is checked (it asks the gate, and its own code reaches
	// no play), and init's listeners are the element's own continuations, so
	// reach does not go through either; it does go through a start output.js
	// alone calls after its barrier, so a function that calls one without a
	// permit (start(), take(h, true)) reaches a play.
	memo := map[string]int{}
	var reach func(string) bool
	reach = func(name string) bool {
		switch memo[name] {
		case 1:
			return false
		case 2:
			return true
		}
		memo[name] = 1
		if plays[name] {
			memo[name] = 2
			return true
		}
		body := withoutGatedCalls(fns[name].body)
		for callee := range fns {
			if how := audioStarts[callee]; how == "gated" || callee == "init" || callee == name {
				continue
			}
			if regexp.MustCompile(`(?:^|[^\w$.])`+regexp.QuoteMeta(callee)+`\(`).MatchString(body) && reach(callee) {
				memo[name] = 2
				return true
			}
		}
		return false
	}
	for name, f := range fns {
		how, named := audioStarts[name]
		if !named {
			if f.exported && reach(name) {
				out = append(out, fmt.Sprintf("audio.js's %s reaches a play and is no start this test names: ask the start gate in it", name))
			}
			continue
		}
		if !f.exported {
			out = append(out, fmt.Sprintf("audioStarts names %s, which audio.js does not export", name))
			continue
		}
		switch how {
		case "gated":
			if !strings.Contains(f.body, "requestStart(") {
				out = append(out, fmt.Sprintf("audio.js's %s starts the element without asking the start gate", name))
			}
			if reach(name) {
				out = append(out, fmt.Sprintf("audio.js's %s reaches a play outside what it hands the start gate", name))
			}
		case "afterBarrier":
			if !regexp.MustCompile(`function\s+` + name + `\([^)]*\bpermit\b`).MatchString(f.body) {
				out = append(out, fmt.Sprintf("audio.js's %s takes no permit for the plays it defers", name))
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestTheGateGuardReadsWhatAGateCallRunsAtOnce holds the start guard to what
// a gate call runs as it is made: only the body of a function handed to the
// gate is its own, a bracket inside a literal bounds nothing, and a gated
// start's own code is read beside its call of the gate. Each planting, in a
// copy of the shipped audio.js, is a start outside the gate that the guard
// must name, or a deferred callback whose literals hold brackets, which it
// must not; the shipped file, whose removed row's successor goes to
// continueStart in a closure, passes it.
func TestTheGateGuardReadsWhatAGateCallRunsAtOnce(t *testing.T) {
	src := stripJSComments(readFile(t, "static/player/audio.js"))
	if v := audioStartViolations(src); len(v) != 0 {
		t.Fatalf("the shipped audio.js fails the guard: %v", v)
	}
	successor := "continueStart((permit) => playNow(permit), { fresh: true });"
	brackets := "continueStart((permit) => { const s = `)}${\"((\"}`; playNow(permit); }, { fresh: true });"
	next := "requestStart((permit) => advanceNow(delta, { permit }));"
	for _, tc := range []struct {
		name, in, old, planted string
		named                  bool
	}{
		{"an eager argument", "removeAt", successor, "continueStart(playNow(null), { fresh: true });", true},
		{"a bare start after a callback holding a parenthesis", "removeAt", successor,
			"continueStart((permit) => { const open = \"(\"; playNow(permit); }, { fresh: true });\n      start();", true},
		{"a deferred callback whose literals hold brackets", "removeAt", successor, brackets, false},
		{"a bare start after a callback whose literals hold brackets", "removeAt", successor, brackets + "\n      start();", true},
		{"a bare start beside a gated start's call of its gate", "advance", next, next + "\n  start();", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n := strings.Count(src, tc.old); n != 1 {
				t.Fatalf("audio.js holds %d of %q: the planting has stopped finding its place", n, tc.old)
			}
			got := audioStartViolations(strings.Replace(src, tc.old, tc.planted, 1))
			named := false
			for _, v := range got {
				named = named || strings.HasPrefix(v, "audio.js's "+tc.in+" ")
			}
			if named != tc.named {
				t.Errorf("%s planted in %s: the guard names %q, want %s named: %v", tc.name, tc.in, got, tc.in, tc.named)
			}
		})
	}
}

// checkTheContinuationsAndTheGatesWiring is the rest of
// TestEveryStartOfThisBrowserAsksTheGate, over the shipped audio.js (src,
// its comments stripped, and its functions) and the modules beside it.
func checkTheContinuationsAndTheGatesWiring(t *testing.T, src string, fns map[string]struct {
	body     string
	exported bool
}) {
	t.Helper()
	// Round 3 (S1, Q1, Q3): every continuation goes on through continueStart,
	// which asks the continuation gate with its start's permit (none for a
	// fresh one) and asks again, when the gate lets it go on, whether the
	// playback it continues is still the one this browser means to play; and
	// nothing calls the old way on (advance with auto).
	for name, why := range audioContinuations {
		if !strings.Contains(fns[name].body, "continueStart(") {
			t.Errorf("audio.js's %s (%s) goes on without continueStart", name, why)
		}
	}
	cont := fns["continueStart"].body
	for _, want := range []string{"const permit = fresh ? null : startPermit;", "continueGate(permit, go, drop)", "if (gen !== playbackGen || !intent || asked !== readerGen()) return;"} {
		if !strings.Contains(cont, want) {
			t.Errorf("continueStart lacks %s", want)
		}
	}
	if m := regexp.MustCompile(`[^\w$.]advance\([^()]*,`).FindString(src); m != "" {
		t.Errorf("audio.js calls advance with an option (%s): what playback does by itself goes on through continueStart", m)
	}
	// Round 4 (T1): a play the bar did not mark is the browser's own, which
	// the play listener hands to ownPlay whatever the gate (round 5); ownPlay
	// pauses the element before it asks the gate, as a fresh start (a null
	// permit) with a drop.
	if !strings.Contains(fns["init"].body, "if (!marks.length) {\n      ownPlay();") {
		t.Error("audio.js's play listener does not hand every play it did not mark to ownPlay, whatever the gate")
	}
	own := fns["ownPlay"].body
	pauseAt, askAt := strings.Index(own, "state.el.pause();"), strings.Index(own, "continueGate(null, go, drop);")
	if pauseAt < 0 || askAt < 0 || pauseAt > askAt {
		t.Error("ownPlay does not pause the element before it asks the gate as a fresh start")
	}
	// Round 5: a missing gate refuses. Each way a start asks its gate asks it
	// only where it is wired, refusing otherwise, and runs nothing but
	// through it.
	for name, ask := range map[string][2]string{
		"requestStart":  {"if (startGate) startGate(run, { drop: () => startDropped(gen) });\n}", "run"},
		"continueStart": {"if (continueGate) continueGate(permit, go, drop);\n  else drop();", "go"},
		"ownPlay":       {"if (continueGate) continueGate(null, go, drop);\n  else drop();", "go"},
	} {
		body := fns[name].body
		if !strings.Contains(body, ask[0]) {
			t.Errorf("audio.js's %s does not refuse where its gate is missing: it lacks %q", name, ask[0])
		}
		if m := regexp.MustCompile(`(?:^|[^\w$.])` + ask[1] + `\(`).FindString(strings.Replace(body, ask[0], "", 1)); m != "" {
			t.Errorf("audio.js's %s calls %s other than through its gate", name, strings.TrimSpace(m))
		}
	}
	ms := fns["wireMediaSession"].body
	for _, want := range []string{`set("play", () => toggle({ play: true }))`, `set("pause", () => pauseNow())`, `set("previoustrack", () => advance(-1))`, `set("nexttrack", () => advance(1))`} {
		if !strings.Contains(ms, want) {
			t.Errorf("the MediaSession's handlers lack %s", want)
		}
	}
	for _, f := range []string{"views.js", "nowplaying.js", "trackmarks.js", "ui.js", "variants.js", "api.js", "format.js", "boot.js", "output.js"} {
		other := stripJSComments(readFile(t, "static/player/"+f))
		if strings.Contains(other, ".play(") {
			t.Errorf("%s plays an element: every start of this browser is audio.js's, through the gate", f)
		}
		if m := regexp.MustCompile(`audio\.(?:take|resume|start)\(`).FindString(other); m != "" {
			t.Errorf("%s calls %s: only output.js, on a barrier's done, starts the browser that way", f, m)
		}
	}
	out := stripJSComments(readFile(t, "static/player/output.js"))
	for _, m := range regexp.MustCompile(`env\.browser\?\.(take|resume|start)\?\.\(([^)]*)\)`).FindAllStringSubmatch(out, -1) {
		args := m[2]
		if strings.HasSuffix(args, "permit") || (m[1] == "take" && strings.HasSuffix(args, ", false")) {
			continue
		}
		t.Errorf("output.js calls the browser's %s(%s): a start hands its permit, and a queue taken without one is taken stopped", m[1], args)
	}
	boot := extractJSFunction(t, stripJSComments(readFile(t, "static/player/boot.js")), "boot")
	gateAt, mountAt := strings.Index(boot, "audio.setStartGate(output.gateStart, output.cancelStarts)"), strings.Index(boot, "mountBar();")
	if gateAt < 0 || mountAt < 0 || gateAt > mountAt {
		t.Error("boot.js does not wire the start gate and its cancel before the bar mounts")
	}
	contAt := strings.Index(boot, "audio.setContinueGate(output.gateContinue)")
	if contAt < 0 || contAt > mountAt {
		t.Error("boot.js does not wire the continuation gate before the bar mounts")
	}
}

// consoleSoundPatterns are what can make the console sound, by name (round
// 4, T4, W10): a media element's play() (a property read such as track.play
// takes no parenthesis), autoplay by property, attribute or markup (never an
// option key such as { autoplay: … }), an audio or video element made, an
// Audio object, a Web Audio context, and a MediaSession handler.
var consoleSoundPatterns = map[string]*regexp.Regexp{
	"play":            regexp.MustCompile(`\.play\(`),
	"autoplay":        regexp.MustCompile(`\.autoplay\s*=|setAttribute\(\s*["']autoplay["']|<(?:audio|video)\b[^>]*\bautoplay\b`),
	"media element":   regexp.MustCompile(`createElement\(\s*["'](?:audio|video)["']`),
	"Audio object":    regexp.MustCompile(`\bnew\s+Audio\s*\(`),
	"audio context":   regexp.MustCompile(`\b(?:webkit)?AudioContext\b`),
	"session handler": regexp.MustCompile(`\bsetActionHandler\s*\(`),
	"element markup":  regexp.MustCompile(`<(?:audio|video)\b`),
}

// soundSite is a site the census allows: how many finds it holds, and why.
type soundSite struct {
	n   int
	why string
}

// consoleSoundSites is every site the census allows, by pattern, as
// file:function with its count of finds and its reason (round 5); a site
// found and not named fails the census, and so does a site named and found
// another number of times, none included.
var consoleSoundSites = map[string]map[string]soundSite{
	"play": {
		"player/audio.js:playEl": {1, "the one play the bar makes, marked for the play event it causes; a play with no mark is the browser's own and asks the gate (ownPlay)"},
	},
	"autoplay": {},
	"media element": {
		"player/audio.js:init":           {2, "the bar's two elements: the one that plays, and the preloader, which is never played"},
		"player/format.js:engineCanPlay": {1, "canPlayType's probe, never played"},
	},
	"Audio object":  {},
	"audio context": {},
	"session handler": {
		"player/audio.js:setMediaSession":  {1, "clears every handler while the bridge's player is the output"},
		"player/audio.js:wireMediaSession": {1, "the browser's handlers, each start through toggle or advance (sessionHandlers)"},
	},
	"element markup": {},
}

// sessionHandlers is every MediaSession handler wireMediaSession sets, as
// written: its starts go through toggle and advance, which ask the gate, its
// Pause pauses and never starts (pauseNow, never toggle, which is a start over
// a failed track), and nothing else it does plays.
var sessionHandlers = []string{
	`set("play", () => toggle({ play: true }));`,
	`set("pause", () => pauseNow());`,
	`set("previoustrack", () => advance(-1));`,
	`set("nexttrack", () => advance(1));`,
	`set("stop", () => clearQueue());`,
	`set("seekto", (d) => { if (d && typeof d.seekTime === "number") seek(d.seekTime); });`,
	`set("seekbackward", (d) => seek(state.el.currentTime - (d?.seekOffset || 10)));`,
	`set("seekforward", (d) => seek(state.el.currentTime + (d?.seekOffset || 10)));`,
}

// TestTheHistoryListsTheMediaSessionsStartsAsTheyAreSet holds the long form
// of the bar's output rule to what wireMediaSession sets: the MediaSession
// handlers it lists as starts, which audio.js routes through requestStart,
// are exactly those whose handler calls toggle or advance, so its Pause,
// which calls pauseNow, is never listed among them.
func TestTheHistoryListsTheMediaSessionsStartsAsTheyAreSet(t *testing.T) {
	history := readFile(t, filepath.Join("..", "..", "ops", "History-rules-admin-console.md"))
	m := regexp.MustCompile(`the MediaSession's ([a-z, ]+) handlers, which call those`).FindStringSubmatch(history)
	if m == nil {
		t.Fatal("the History's bar output entry lists no MediaSession handlers that call the bar's starts: the test has stopped reading it")
	}
	listed := regexp.MustCompile(`, | and `).Split(m[1], -1)
	sort.Strings(listed)
	words := map[string]string{"play": "play", "previoustrack": "previous", "nexttrack": "next"}
	var set []string
	ms := jsTopFunctions(stripJSComments(readFile(t, "static/player/audio.js")))["wireMediaSession"].body
	for _, h := range regexp.MustCompile(`set\("(\w+)", \(\) => (?:toggle|advance)\(`).FindAllStringSubmatch(ms, -1) {
		word, ok := words[h[1]]
		if !ok {
			t.Fatalf("wireMediaSession starts through its %s handler, which the test has no word for", h[1])
		}
		set = append(set, word)
	}
	sort.Strings(set)
	if len(set) < 3 || !reflect.DeepEqual(listed, set) {
		t.Errorf("the History lists the MediaSession's %v handlers as starts; wireMediaSession sets %v to start: list those alone", listed, set)
	}
}

// TestNoSoundSourceOfTheConsoleBypassesTheGate is round 4's census (T4,
// W10), on every host: every file of the console's scripts under static/,
// comments stripped, and the templates' markup, searched for what can make
// the console sound, each find placed in the top-level function holding it;
// only the sites consoleSoundSites names may hold one, each as many times as
// it says (round 5); and wireMediaSession sets exactly sessionHandlers.
func TestNoSoundSourceOfTheConsoleBypassesTheGate(t *testing.T) {
	found := map[string]map[string]int{}
	for name := range consoleSoundPatterns {
		found[name] = map[string]int{}
	}
	files := 0
	err := filepath.WalkDir("static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		files++
		rel := filepath.ToSlash(strings.TrimPrefix(path, "static"+string(filepath.Separator)))
		src := stripJSComments(readConsoleJS(t, path))
		locs := barFnRe.FindAllStringSubmatchIndex(src, -1)
		siteOf := func(at int) string {
			fn := "(top level)"
			for _, loc := range locs {
				if loc[0] > at {
					break
				}
				fn = src[loc[2]:loc[3]]
			}
			return rel + ":" + fn
		}
		for name, re := range consoleSoundPatterns {
			for _, m := range re.FindAllStringIndex(src, -1) {
				found[name][siteOf(m[0])]++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("only %d scripts found under static/: the census has stopped reading them", files)
	}
	templates, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("no templates found (%v): the census has stopped reading their markup", err)
	}
	for _, path := range templates {
		for _, name := range []string{"element markup", "autoplay"} {
			if n := len(consoleSoundPatterns[name].FindAllStringIndex(readFile(t, path), -1)); n > 0 {
				found[name]["templates/"+filepath.Base(path)] += n
			}
		}
	}
	for name, sites := range found {
		for site, n := range sites {
			allowed, ok := consoleSoundSites[name][site]
			if !ok {
				t.Errorf("%s at %s: no site the console's census allows; route it through audio.js's playEl and the gate, or name it with its reason", name, site)
			} else if n != allowed.n {
				t.Errorf("%s at %s: %d found, the census allows %d (%s): route the new one through audio.js's playEl and the gate, or count it with its reason", name, site, n, allowed.n, allowed.why)
			}
		}
		for site := range consoleSoundSites[name] {
			if sites[site] == 0 {
				t.Errorf("the census names %s at %s, which holds none now: take it off the list", name, site)
			}
		}
	}
	ms := jsTopFunctions(stripJSComments(readFile(t, "static/player/audio.js")))["wireMediaSession"].body
	sets := regexp.MustCompile(`(?m)^\s*set\(.*$`).FindAllString(ms, -1)
	if len(sets) != len(sessionHandlers) {
		t.Errorf("wireMediaSession sets %d handlers, the census names %d: %q", len(sets), len(sessionHandlers), sets)
	}
	for _, want := range sessionHandlers {
		if !strings.Contains(ms, want) {
			t.Errorf("wireMediaSession does not set %s", want)
		}
	}
}

// TestThePhoneBarKeepsItsWordsWithTheBridgeChosen pins R10 at the source: in
// a max-width 640px block, with the bridge chosen, the bar draws neither the
// cover, which the bridge never paints, nor the Mute, which it disables
// (the level is fixed), so the words keep about 63px at 375px; the bar says
// which output it shows (data-output) and the Mute carries np-mute. The
// browser session measures .np-meta's width.
func TestThePhoneBarKeepsItsWordsWithTheBridgeChosen(t *testing.T) {
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	found := false
	for _, block := range cssMediaBlocks(css, "(max-width: 640px)") {
		rule := regexp.MustCompile(`\.npbar\[data-output="bridge"\] \.np-art,\s*\.npbar\[data-output="bridge"\] \.np-mute\s*\{\s*display:\s*none;\s*\}`)
		if rule.MatchString(block) {
			found = true
		}
	}
	if !found {
		t.Error("no max-width 640px block hides the cover and the Mute with the bridge chosen")
	}
	np := stripJSComments(readFile(t, "static/player/nowplaying.js"))
	for _, want := range []string{`refs.mute.classList.add("np-mute")`, `bar.dataset.output = "bridge"`, `bar.dataset.output = "browser"`, `refs.art = el("div", { class: "np-art" })`} {
		if !strings.Contains(np, want) {
			t.Errorf("nowplaying.js lacks %s", want)
		}
	}
}

// TestTheResumeWaitsCancelCueShowsUnderReducedMotion holds the glyph the bar's
// button shows while a resume of the browser's own waits (B1015), which
// TestTheResumeWaitsButtonSaysCancelResume reads off the button under node,
// to what ships: the sprite in layout.html holds that symbol, the button
// carries the busy class while it waits, and the reduced-motion rules show
// the glyph in place of the ring, so the reader who asks for no motion sees
// the cancel cue and no Play.
func TestTheResumeWaitsCancelCueShowsUnderReducedMotion(t *testing.T) {
	np := stripJSComments(readFile(t, "static/player/nowplaying.js"))
	m := regexp.MustCompile(`s\.resumeWaits \? "([a-z][\w-]*)"`).FindStringSubmatch(np)
	if m == nil {
		t.Fatal("nowplaying.js names no glyph for a resume's wait (s.resumeWaits ? \"…\"): the test has stopped reading it")
	}
	if !strings.Contains(readFile(t, filepath.Join("templates", "layout.html")), `<g id="i-np-`+m[1]+`">`) {
		t.Errorf("the sprite holds no i-np-%s, the glyph the button shows while a resume waits", m[1])
	}
	if !strings.Contains(np, `refs.play.classList.toggle("np-btn-busy", s.loading)`) {
		t.Error("the bar's button no longer carries the busy class while it waits, which the reduced-motion rules read")
	}
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	reveal := regexp.MustCompile(`\.np-btn-busy \.np-ico\s*\{\s*visibility:\s*visible;\s*\}`)
	noRing := regexp.MustCompile(`\.np-btn-busy::after\s*\{\s*content:\s*none;\s*\}`)
	found := false
	for _, block := range cssMediaBlocks(css, "(prefers-reduced-motion: reduce)") {
		found = found || (reveal.MatchString(block) && noRing.MatchString(block))
	}
	if !found {
		t.Error("no reduced-motion block shows the busy button's glyph and hides its ring")
	}
}

// TestTheChosenPickCarriesAMarkTheFocusRingLacks pins at the source that the
// output menu's chosen pick says so by a mark of its own, a check drawn
// after its words, which the focus ring does not draw: the menu opens on the
// bridge's pick, so a ring alike to the chosen pick's border read as the
// choice. The browser session sees the two apart.
func TestTheChosenPickCarriesAMarkTheFocusRingLacks(t *testing.T) {
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	chosen := regexp.MustCompile(`\.np-pick\[aria-pressed="true"\]::after\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if chosen == nil || !regexp.MustCompile(`\bcontent:\s*"[^"]+"`).MatchString(chosen[1]) {
		t.Fatal("the chosen pick draws no mark of its own: a pick that holds the focus reads as the chosen one")
	}
	for _, m := range regexp.MustCompile(`\.np-pick:focus-visible[^{]*\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[0], "::after") || strings.Contains(m[1], "content:") {
			t.Errorf("the focus ring draws the chosen pick's mark: %s", m[0])
		}
	}
}

// TestTheOutputMenuTakesTheFocusOfAPressOnItsWords pins at the source that
// the output menu is focusable (tabindex -1), so a pointer press on its
// words, whose focus a browser gives the nearest focusable ancestor, keeps
// the focus inside the menu, and that its focusout closes it only for a
// focus that leaves it. TestTheOutputMenuClosesAsTheFocusLeavesIt runs the
// focusout under node with the focus moved by hand; the browser check of a
// real press is the record's.
func TestTheOutputMenuTakesTheFocusOfAPressOnItsWords(t *testing.T) {
	menu := extractJSFunction(t, stripJSComments(readFile(t, "static/player/nowplaying.js")), "buildOutputMenu")
	if !regexp.MustCompile(`refs\.menu = el\("div", \{[^}]*attrs: \{[^}]*\btabindex: "-1"`).MatchString(menu) {
		t.Error(`the output menu is not focusable: a press on its words moves the focus out of it, and its focusout closes it`)
	}
	if !strings.Contains(menu, "if (!refs.menu.hidden && !refs.menu.contains(e.relatedTarget)) closeMenu();") {
		t.Error("the output menu's focusout does not close it for a focus that leaves it alone")
	}
}

// cssMediaBlocks is the body of every @media block of css whose query is
// query, found by its braces.
func cssMediaBlocks(css, query string) []string {
	var out []string
	head := "@media " + query
	for i := strings.Index(css, head); i >= 0; {
		open := strings.Index(css[i:], "{")
		if open < 0 {
			break
		}
		depth, j := 0, i+open
		for ; j < len(css); j++ {
			if css[j] == '{' {
				depth++
			} else if css[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		out = append(out, css[i+open:j])
		next := strings.Index(css[j:], head)
		if next < 0 {
			break
		}
		i = j + next
	}
	return out
}

// barStateExempt are the state's fields the bar must not act on, each
// forbidden as an input by the design: at, a capture's time, never the
// clock's offset (M1: the offset is GET now's X-Bridge-Clock samples
// alone); token, the queue's token, never what the queue is read by (G4:
// queueRev is); anchor.id and anchor.occurrence, the publisher's identity
// of an anchor, which moves only where its position does: the bar
// extrapolates from the anchor's seconds, time and advance, for the entry
// it names. The list is exact: a field added to it without a reason, or
// left on it once the bar reads it, fails the guard.
var barStateExempt = map[string]string{
	"at":                "M1: a capture's time is never the clock's offset",
	"token":             "G4: the queue is read against queueRev, never its token",
	"anchor.id":         "the publisher's identity of an anchor, which moves only with its position",
	"anchor.occurrence": "the publisher's identity of an anchor's occurrence, which its entry names to the bar",
}

// barSources are the bar's modules whose reads the guard resolves.
var barSources = []string{"static/player/output.js", "static/player/nowplaying.js", "static/player/format.js"}

// TestEveryPlayerControlStateFieldIsRenderedSomewhere pins both directions of
// the state the bar takes (PlayerState, a player frame's and every answer's
// bytes): every leaf the server sends is read by the bar's modules, but the
// exempt few, and every path the bar reads off a state, a record or one of
// their parts names a field the server sends. Reads resolve through aliases
// inside each top-level function (const t = st.track), from the state's
// names in these modules (st, state, o.state), a record's (rec) and
// signalChip's two parameters; a read the resolver cannot follow is
// reported missing, the safe direction.
func TestEveryPlayerControlStateFieldIsRenderedSomewhere(t *testing.T) {
	leaves := map[string]bool{}
	all := map[string]bool{}
	barStatePaths(reflect.TypeOf(PlayerState{}), "", leaves, all)
	if len(leaves) < 60 {
		t.Fatalf("only %d leaves under PlayerState: the walk has stopped recursing, so this test proves nothing", len(leaves))
	}
	read := map[string]bool{}
	for _, f := range barSources {
		for p := range barReads(stripJSComments(readFile(t, f))) {
			read[p] = true
		}
	}
	nested := 0
	for p := range read {
		if strings.Count(p, ".") >= 1 {
			nested++
		}
	}
	if nested < 25 {
		t.Fatalf("only %d reads a level deep resolved: the aliases no longer bind, so this test proves nothing", nested)
	}
	var missing, unknown, stale []string
	for p := range leaves {
		if _, ok := barStateExempt[p]; ok {
			if read[p] {
				stale = append(stale, p)
			}
			continue
		}
		if !read[p] {
			missing = append(missing, p)
		}
	}
	for p := range read {
		if !all[p] {
			unknown = append(unknown, p)
		}
	}
	for p := range barStateExempt {
		if !leaves[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	sort.Strings(stale)
	for _, p := range missing {
		t.Errorf("the player state carries %q and the bar never reads it: render it, or drop it from the state", p)
	}
	for _, p := range unknown {
		t.Errorf("the bar reads %q of the player state, which carries no such field", p)
	}
	for _, p := range stale {
		t.Errorf("%q is exempt but the bar reads it, or the state no longer carries it: take it off the list", p)
	}
}

// barStatePaths collects PlayerState's JSON paths: every leaf, and every
// path, containers included. A slice of structs (operations) is walked as
// its element; a record's handover is walked too, which an answer and a
// read back carry though a publication never does.
func barStatePaths(rt reflect.Type, prefix string, leaves, all map[string]bool) {
	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice {
		rt = rt.Elem()
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		all[p] = true
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			barStatePaths(ft, p, leaves, all)
			continue
		}
		leaves[p] = true
	}
}

// barFnRe starts a top-level function of a module, exported or not.
var barFnRe = regexp.MustCompile(`(?m)^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(`)

// barRoots are the bar's names for the state and its parts, by the path
// they stand for: the state (st, state), the view whose state it is (o),
// and a record (rec), which is a publication's operations' element.
var barRoots = map[string]string{"st": "", "state": "", "o": "\x00view", "rec": "operations"}

// barParams are parameters bound by hand, where a function takes a part of
// the state under a name of its own: format.js's signalChip.
var barParams = map[string]map[string]string{
	"signalChip": {"track": "track", "signal": "signal"},
}

// barForOfRe is a for-of loop's binding, an alias of its collection's
// element: for (const t of out).
var barForOfRe = regexp.MustCompile(`\bfor\s*\(\s*(?:const|let|var)\s+([A-Za-z_]\w*)\s+of\s+([A-Za-z_]\w*(?:\??\.[A-Za-z_]\w*)*)`)

// barBuiltins are what a read of a value's member can end on that is the
// language's, never a field: an array's, a number's.
var barBuiltins = map[string]bool{
	"length": true, "map": true, "forEach": true, "filter": true, "find": true, "some": true, "every": true,
	"includes": true, "has": true, "join": true, "slice": true, "split": true, "toLocaleString": true, "push": true,
}

// barReads is every state path a module reads, with every prefix of a path
// counted.
func barReads(src string) map[string]bool {
	out := map[string]bool{}
	locs := barFnRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := src[loc[0]:end]
		local := map[string]string{}
		for k, v := range barRoots {
			local[k] = v
		}
		for k, v := range barParams[name] {
			local[k] = v
		}
		aliases := append(jsAliasRe.FindAllStringSubmatch(body, -1), barForOfRe.FindAllStringSubmatch(body, -1)...)
		for round := 0; round < 3; round++ {
			for _, a := range aliases {
				if _, isRoot := barRoots[a[1]]; isRoot {
					continue
				}
				if p, ok := barResolve(local, a[2]); ok {
					local[a[1]] = p
				}
			}
		}
		for _, m := range jsMemberReadRe.FindAllStringSubmatch(body, -1) {
			p, ok := barResolve(local, m[1]+m[2])
			if !ok || p == "" {
				continue
			}
			parts := strings.Split(p, ".")
			// The state's own length is a field: a built-in is trimmed only
			// past the path's first segment.
			for len(parts) > 1 && barBuiltins[parts[len(parts)-1]] {
				parts = parts[:len(parts)-1]
			}
			for j := 1; j <= len(parts); j++ {
				out[strings.Join(parts[:j], ".")] = true
			}
		}
	}
	return out
}

// barResolve is expr's state path through local's names: a view's reads
// count only through its state.
func barResolve(local map[string]string, expr string) (string, bool) {
	root, rest, _ := strings.Cut(jsNormPath(expr), ".")
	base, ok := local[root]
	if !ok {
		return "", false
	}
	if base == "\x00view" {
		after, found := strings.CutPrefix(rest, "state")
		if !found || (after != "" && !strings.HasPrefix(after, ".")) {
			return "", false
		}
		return strings.TrimPrefix(after, "."), true
	}
	return joinPath(base, rest), true
}
