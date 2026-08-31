package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	// The constant lives outside the extracted function, so the harness
	// supplies it. Long here: this test is about the status mapping, and a
	// deadline that could fire would make it about timing instead.
	script := "const PROBE_TIMEOUT_MS = 60000;\n" + fn + `
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

// TestStalePlaybackFailuresTouchNothing pins that a failure handler
// which resumes after the reader has moved on writes no shared state.
//
// The probe is a round trip, and everything after it — the error text,
// the skip-storm counter, the queue position — belongs to whatever is
// playing NOW. Gemini caught the last line of this (the advance);
// CodeRabbit caught the whole stretch, which is the one that matters:
// guarding only the timer still let a stale handler overwrite the
// message and count a skip against the wrong track.
func TestStalePlaybackFailuresTouchNothing(t *testing.T) {
	src := readFile(t, filepath.Join("static", "player", "audio.js"))
	fn := extractJSFunction(t, src, "handleSourceError")

	// Captured BEFORE the await, or it describes the state it was meant
	// to detect a change in.
	at := strings.Index(fn, "const at = playbackGen")
	await := strings.Index(fn, "await classifySourceFailure(")
	if at < 0 || await < 0 || at > await {
		t.Fatal("handleSourceError does not capture the playback generation " +
			"before awaiting the probe")
	}
	if !strings.Contains(fn, "if (at !== playbackGen) return;") {
		t.Error("handleSourceError no longer discards a stale result; the error " +
			"text and the skip counter would be written against another track")
	}
	// A counter, not track identity: playQueue clones its tracks, so
	// identity catches a replaced queue and a different selection but NOT
	// a re-load of the same track — a reader retrying the row that just
	// failed would have the fresh attempt clobbered by the stale one.
	for _, site := range []string{"function load(", "export function clearQueue("} {
		i := strings.Index(src, site)
		if i < 0 {
			t.Fatalf("%s not found — this test has stopped checking anything", site)
		}
		end := strings.Index(src[i:], "\n}\n")
		if end < 0 || !strings.Contains(src[i:i+end], "playbackGen += 1") {
			t.Errorf("%s does not bump the playback generation; a suspended "+
				"failure handler would not notice the track changed", site)
		}
	}
}

// TestFailureAutoAdvanceIsGuarded pins that the post-failure skip cannot
// act on a decision the reader has already changed.
//
// 1200ms is long enough to pause, pick another track, or replace the
// queue. Both conditions are needed: the generation catches a track
// change, `playing` catches a plain pause, which changes no track at all.
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
	if !strings.Contains(body, "at === playbackGen") {
		t.Error("the auto-advance does not check the playback generation; " +
			"picking another track would be skipped past")
	}
}

// TestSourceProbeCannotHangForever pins the deadline.
//
// A source that accepts the connection and then sends no headers leaves
// fetch pending — and with it the error message, the skip and the
// advance. The player would simply stop, with nothing on screen to say
// why. The abort turns that into an ordinary "error", which the caller
// already handles.
func TestSourceProbeCannotHangForever(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	src := readFile(t, filepath.Join("static", "player", "audio.js"))
	fn := extractJSFunction(t, src, "classifySourceFailure")
	if !strings.Contains(src, "PROBE_TIMEOUT_MS =") {
		t.Fatal("no probe deadline constant in audio.js")
	}

	// A fetch that NEVER settles on its own; only the abort can end it.
	script := "const PROBE_TIMEOUT_MS = 50;\n" + fn + `
globalThis.fetch = (url, opts) => new Promise((_, reject) => {
  opts.signal.addEventListener("abort", () => reject(new Error("aborted")));
});
globalThis.AbortController = class {
  constructor() { this.signal = new EventTarget(); }
  abort() { this.signal.dispatchEvent(new Event("abort")); }
};
const t0 = Date.now();
const reason = await classifySourceFailure("/x");
console.log(JSON.stringify({ reason, elapsedUnderASecond: Date.now() - t0 < 1000 }));
`
	dir := t.TempDir()
	path := filepath.Join(dir, "deadline.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hard ceiling of our own: without the deadline under test the
	// script's await never settles, and while node happens to detect that
	// and exit, relying on it would make a HANG look like an ordinary
	// failure — or hang the suite on a runtime that does not.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, path).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the probe never returned — a source that sends no headers "+
			"would stall the player with nothing on screen:\n%s", out)
	}
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Reason string `json:"reason"`
		Quick  bool   `json:"elapsedUnderASecond"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("client returned %q: %v", out, err)
	}
	if got.Reason != "error" || !got.Quick {
		t.Errorf("a never-answering source gave %q after a long wait (quick=%v); "+
			"the player would stall with nothing on screen", got.Reason, got.Quick)
	}
}

// TestRoutedTracksReportBeingOnAnUpstream pins the reason a routed track
// can never gain a variant.
//
// DIDL-Lite has no bit-depth element — 0 of 15,283 items on a real Chord
// 2Go published one — so a routed FLAC has no bitsPerSample and falls
// into the geometry test, which told the reader "Format unreadable" about
// a file whose format is perfectly well known. On the reference library
// that was 13,519 tracks, the overwhelming majority of what a 2Go user
// browses.
//
// The routed case has to come FIRST rather than merely be added: it
// dominates. The bridge has no local file to hand sox, so even a pristine
// 24/96 FLAC with complete geometry is still unable to gain a variant,
// and reporting its geometry as the blocker would be a second wrong
// answer rather than the first one fixed.
func TestRoutedTracksReportBeingOnAnUpstream(t *testing.T) {
	rate, bits := 44100.0, 16

	// The shape the 2Go actually publishes: FLAC, sample rate, no depth.
	if got := fundamentalSkipReason(true, false, "FLAC", &rate, nil, "2go/a.flac", nil); got != "routed_upstream" {
		t.Errorf("routed FLAC with no bit depth: got %q, want routed_upstream — "+
			"the reader is told the format is unreadable when it is simply remote", got)
	}
	// Complete geometry, still routed: nothing about the file is the
	// blocker, so a reason derived from the file would be wrong.
	if got := fundamentalSkipReason(true, false, "FLAC", &rate, &bits, "2go/a.flac", nil); got != "routed_upstream" {
		t.Errorf("routed FLAC with full geometry: got %q, want routed_upstream", got)
	}
	// Routed AND DSD: still routed. Both are true; the one the operator
	// can act on is neither, but "on an upstream" is the one that would
	// remain true if the file were transcodable.
	if got := fundamentalSkipReason(true, true, "DSF", nil, nil, "2go/a.dsf", nil); got != "routed_upstream" {
		t.Errorf("routed DSD: got %q, want routed_upstream", got)
	}

	// And local tracks are untouched — the whole existing truth table
	// still has to hold, or this fix traded one wrong badge for another.
	for _, tc := range []struct {
		name  string
		isDSD bool
		codec string
		rate  *float64
		bits  *int
		want  string
	}{
		{"local dsd", true, "DSF", nil, nil, "dsd_bitstream"},
		{"local lossy", false, "MP3", &rate, &bits, "lossy_source"},
		{"local no geometry", false, "FLAC", nil, nil, "unknown_format"},
		{"local fine", false, "FLAC", &rate, &bits, ""},
	} {
		if got := fundamentalSkipReason(false, tc.isDSD, tc.codec, tc.rate, tc.bits, "a", nil); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEverySkipReasonHasALabel pins the Go -> JS half.
//
// An unlabelled reason renders as the raw identifier: the reader sees
// "routed_upstream" where a sentence belongs. Nothing else connects the
// two files.
func TestEverySkipReasonHasALabel(t *testing.T) {
	labels := readFile(t, filepath.Join("static", "player", "format.js"))
	i := strings.Index(labels, "const SKIP_LABELS = {")
	if i < 0 {
		t.Fatal("SKIP_LABELS not found — this test has stopped checking anything")
	}
	block := labels[i:]
	if end := strings.Index(block, "};"); end > 0 {
		block = block[:end]
	}
	for _, reason := range []string{
		"routed_upstream", "dsd_bitstream", "lossy_source", "unknown_format", "no_decoder",
	} {
		if !strings.Contains(block, reason+":") {
			t.Errorf("skip reason %q has no label; it would render as the raw identifier", reason)
		}
	}
}
