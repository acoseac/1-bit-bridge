package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// MEDIA_ERR_SRC_NOT_SUPPORTED does not mean "this browser cannot decode
// it": the element raises the same code when the source could not be
// FETCHED. So a missing file and an unreachable upstream both reported as
// a format problem — and worse, marked the track permanently unplayable
// for the session, so an upstream coming back would not fix it.

// TestSourceFailureClassifierMapsTheServersOwnCodes runs the SHIPPED
// classifier under node against the status codes this server actually
// emits, rather than against a Go replica of what it emits.
func TestSourceFailureClassifierMapsTheServersOwnCodes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "audio.js")), "classifySourceFailure")

	script := fn + `
globalThis.AbortController = class { abort() {} get signal() { return null; } };
const run = async (status) => {
  globalThis.fetch = async () => ({ ok: status >= 200 && status < 300, status });
  return classifySourceFailure("/x");
};
const out = {};
for (const s of [200, 206, 404, 410, 503, 500]) out[s] = await run(s);
globalThis.fetch = async () => { throw new Error("network"); };
out.threw = await classifySourceFailure("/x");
out.noURL = await classifySourceFailure("");
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	path := filepath.Join(dir, "classify.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("client returned %q: %v", out, err)
	}
	// The codes are the ones player_audio.go writes: 404 for a track that
	// is not there, 410 for a stale variant, 503 for a routed track whose
	// upstream is down, and a 2xx for anything it served — which leaves
	// decoding as the only remaining explanation.
	for status, want := range map[string]string{
		"200": "decode", "206": "decode",
		"404": "missing", "410": "missing",
		"503": "offline",
		"500": "error", "threw": "error", "noURL": "error",
	} {
		if got[status] != want {
			t.Errorf("status %s classified as %q, want %q", status, got[status], want)
		}
	}
}

// TestOnlyADecodeFailureMarksATrackUnplayable is the half that matters
// beyond the wording.
//
// markUnplayable is permanent for the session. Applying it to a fetch
// failure keeps a track dead over a condition that may already have
// cleared — an upstream that came back up, a file restored by the next
// scan — and no reload short of a new tab would recover it.
func TestOnlyADecodeFailureMarksATrackUnplayable(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "audio.js")), "handleSourceError")
	if !strings.Contains(fn, `if (reason === "decode") markUnplayable(track)`) {
		t.Error("handleSourceError no longer gates markUnplayable on a genuine " +
			"decode failure; a missing file or an offline upstream would mark the " +
			"track dead for the rest of the session")
	}
	if !strings.Contains(fn, "await classifySourceFailure(") {
		t.Error("handleSourceError no longer asks the server what went wrong")
	}
}

// TestUPnPGroupLinksToItsManagementPage pins the connection between the
// two things the sidebar calls UPnP.
//
// The group browses an upstream's music; Server > UPnP adds and rescans
// them. Without a link between them a reader looking for "add a server"
// has no reason to look under Server — which is exactly how the existing
// discovery page went unfound.
func TestUPnPGroupLinksToItsManagementPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	withTestUpstream(srv, true)

	req := httptest.NewRequest(http.MethodGet, "/albums", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()

	i := strings.Index(body, `class="nav-group nav-group-row"`)
	if i < 0 {
		t.Fatal("the UPnP group heading carries no action; the management page is " +
			"reachable only from the Server group, which is where it went unfound")
	}
	end := strings.Index(body[i:], "</p>")
	if end < 0 {
		t.Fatal("unterminated nav-group heading")
	}
	heading := body[i : i+end]
	if !strings.Contains(heading, `href="/upnp"`) {
		t.Errorf("the UPnP group action does not link to /upnp: %q", heading)
	}
	// It must NOT be markable: Server > UPnP is the canonical entry for
	// that page, and exactly one nav entry may carry aria-current — a rule
	// TestPrimaryNavHighlightsEveryEntry enforces across the whole rail.
	if strings.Contains(heading, "aria-current") {
		t.Error("the group action carries aria-current; two entries would light " +
			"on /upnp at once")
	}
}

// TestFailureAutoAdvanceIsGuarded pins that the post-failure skip cannot
// act on a decision the reader has already changed.
//
// 1200ms is long enough to pause, pick another track, or replace the
// queue — and advance() steps from the CURRENT index, so a timer firing
// afterwards skips past whatever they chose. Predates this change (the
// timer was unguarded where it stood before), but the probe added ahead
// of it widens the window, which is reason enough to close it here.
func TestFailureAutoAdvanceIsGuarded(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "audio.js")), "handleSourceError")
	i := strings.Index(fn, "setTimeout(")
	if i < 0 {
		t.Fatal("handleSourceError no longer schedules the post-failure advance")
	}
	body := fn[i:]
	if !strings.Contains(body, "state.playing") {
		t.Error("the auto-advance does not check that playback is still running; " +
			"pausing after a failure would be undone 1200ms later")
	}
	// Identity on the TRACK, not the index: a replaced queue can hold a
	// different object at the same position, and an index check would
	// pass while pointing at something else entirely.
	if !strings.Contains(body, "state.queue[state.index] === track") {
		t.Error("the auto-advance does not check that the failed track is still " +
			"the current one; picking another track would be skipped past")
	}
}
