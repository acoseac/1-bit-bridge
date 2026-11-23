package admin

import (
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
