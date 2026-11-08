package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The console's half of backlog B776 item 5: a refusal of a write into a
// root that reads as not mounted must reach the operator where they can act
// on it. startUpload hides the review, which holds the Upload button, before
// it sends a byte, and its catch only showed the error: a commit refused there
// left a message and no control, the Upload button re-enabled inside a hidden
// panel. The refusal now shows a Try again beside the error, outside the
// review, which carries on the same session: it reads the session again from
// the server, sends what is not complete from the offsets the server holds,
// and commits it, never creating a session or sending a finished file again.

// TestARefusalTheReviewHidOffersARetryOfTheSameSession pins, without node, the
// shape that does it; TestTheConsoleRetriesARefusedCommitWithItsSessionOnceTheVolumeIsBack
// runs it under node, where node is installed.
func TestARefusalTheReviewHidOffersARetryOfTheSameSession(t *testing.T) {
	tmpl := readFile(t, "templates/upload.html")
	progress := strings.Index(tmpl, `id="upload-progress"`)
	retry := strings.Index(tmpl, `id="upload-retry"`)
	button := strings.Index(tmpl, `id="upload-retry-button"`)
	if progress < 0 || retry < 0 || button < retry {
		t.Fatalf("upload.html has no retry block holding its button (progress %d, retry %d, button %d)", progress, retry, button)
	}
	if retry < progress {
		t.Error("the retry block is inside #upload-review, which a refusal after the review collapsed leaves hidden")
	}

	js := readFile(t, "static/app.js")
	start := stripJSComments(extractJSFunction(t, js, "startUpload"))
	catchAt := strings.Index(start, "} catch (e) {")
	if catchAt < 0 || !strings.Contains(start[catchAt:], "showUploadFailure(e);") {
		t.Error("startUpload's catch does not show its error through showUploadFailure, which offers the retry")
	}

	failure, ok := extractJSFunctionIfPresent(js, "showUploadFailure")
	if !ok {
		t.Fatal("app.js has no showUploadFailure")
	}
	failure = stripJSComments(failure)
	for _, want := range []string{
		"showUploadError(",
		`e?.code === "root_not_mounted"`,
		"uploadState?.session",
		`getElementById("upload-retry").hidden = false;`,
	} {
		if !strings.Contains(failure, want) {
			t.Errorf("showUploadFailure has no %s", want)
		}
	}

	retryFn, ok := extractJSFunctionIfPresent(js, "retryUpload")
	if !ok {
		t.Fatal("app.js has no retryUpload")
	}
	retryFn = stripJSComments(retryFn)
	for _, want := range []string{
		"uploadState?.session?.id",
		"API.get(`/api/upload/sessions/${encodeURIComponent(sid)}`)",
		"await transferAll(session);",
		"API.post(`/api/upload/sessions/${encodeURIComponent(sid)}/commit`)",
		"showUploadFailure(e);",
	} {
		if !strings.Contains(retryFn, want) {
			t.Errorf("retryUpload has no %s", want)
		}
	}
	if strings.Contains(retryFn, "createUploadSession(") {
		t.Error("retryUpload creates a session: the retry carries on the one the refusal stopped")
	}
	if !strings.Contains(stripJSComments(extractJSFunction(t, js, "transferAll")), "session.files.filter((f) => !f.complete)") {
		t.Error("transferAll no longer passes over the files the server holds complete, so a retry would send them again")
	}
	if !strings.Contains(stripJSComments(extractJSFunction(t, js, "wireUpload")),
		`document.getElementById("upload-retry-button")?.addEventListener("click", retryUpload);`) {
		t.Error("wireUpload does not wire the retry button to retryUpload")
	}
}

// uploadRetryRun drives the shipped upload page under node: the page wired
// (wireUpload), a pick staged (stageFiles), Upload clicked, and, once the
// test has put the volume back, Try again clicked. The page's requests go to
// the console's real handlers over HTTP (input.base); what the page shows is
// read after each click.
const uploadRetryRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const realFetch = globalThis.fetch;
globalThis.fetch = (url, init) => realFetch(new URL(url, input.base), init);
class PageEl {
  constructor(id) {
    this.id = id;
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.value = 0;
    this.className = "";
    this.href = "";
    this.dataset = {};
    this.isConnected = true;
    this.children = [];
    this.text = "";
    this.listeners = {};
  }
  get textContent() {
    return this.text + this.children.map((c) => (c.data !== undefined ? c.data : c.textContent)).join("");
  }
  set textContent(v) {
    this.text = String(v);
    this.children = [];
  }
  appendChild(node) {
    this.children.push(node);
    return node;
  }
  replaceChildren(...nodes) {
    this.text = "";
    this.children = nodes;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  async click() {
    for (const fn of this.listeners.click || []) await fn();
  }
  scrollIntoView() {}
  focus() {}
}
const pageEls = new Map();
for (const id of ["upload-review", "upload-preview", "upload-count", "upload-count-plural", "upload-total",
  "upload-progress", "upload-bar", "upload-status", "upload-start", "upload-overwrite", "upload-error",
  "upload-dupe-note", "upload-result", "upload-retry", "upload-retry-button"]) {
  pageEls.set(id, new PageEl(id));
}
for (const id of ["upload-review", "upload-progress", "upload-error", "upload-dupe-note", "upload-result", "upload-retry"]) {
  pageEls.get(id).hidden = true; // as upload.html ships them
}
globalThis.document = {
  getElementById: (id) => pageEls.get(id) ?? null,
  createElement: () => new PageEl(""),
  createTextNode: (data) => ({ data }),
  activeElement: null,
  body: {},
};
let uploadState = null;
let resumableSessions = [];
let uploadAwaitingScan = false;
const shown = () => ({
  reviewHidden: pageEls.get("upload-review").hidden,
  error: pageEls.get("upload-error").hidden ? "" : pageEls.get("upload-error").textContent,
  retryHidden: pageEls.get("upload-retry").hidden,
  result: pageEls.get("upload-result").hidden ? "" : pageEls.get("upload-result").textContent,
});
wireUpload();
const file = new File([input.bytes], "01.flac", { lastModified: 1700000000000 });
stageFiles([{ file, path: input.path }]);
await pageEls.get("upload-start").click();
const refused = shown();
const back = await realFetch(new URL("/__test/back", input.base), { method: "POST" });
await pageEls.get("upload-retry-button").click();
const retried = shown();
process.stdout.write(JSON.stringify({ refused, retried, back: back.status }) + "\n", () => process.exit(0));
`

// uploadPageShown is what the upload page showed after a click.
type uploadPageShown struct {
	ReviewHidden bool   `json:"reviewHidden"`
	Error        string `json:"error"`
	RetryHidden  bool   `json:"retryHidden"`
	Result       string `json:"result"`
}

// jsStatement slices a top-level statement of app.js that begins with prefix
// (a const that is no object, so extractJSConst cannot), through its first
// line ending in a semicolon.
func jsStatement(t *testing.T, src, prefix string) string {
	t.Helper()
	start := strings.Index(src, "\n"+prefix)
	if start < 0 {
		t.Fatalf("app.js has no %s", prefix)
	}
	start++
	end := strings.Index(src[start:], ";\n")
	if end < 0 {
		t.Fatalf("unterminated %s", prefix)
	}
	return src[start:start+end+2] + "\n"
}

// TestTheConsoleRetriesARefusedCommitWithItsSessionOnceTheVolumeIsBack runs
// the shipped upload page under node, where node is installed, against the
// console's real handlers: a pick is uploaded whole, its root's volume goes as
// the commit arrives, and the commit is refused (backlog B776). The refusal is
// shown with a Try again outside the review the upload hid; once the volume is
// back, Try again commits the same session, with no session created and no
// chunk sent again, and the file lands on the volume.
func TestTheConsoleRetriesARefusedCommitWithItsSessionOnceTheVolumeIsBack(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	root := cfg.LibraryRoots[0]
	seedLibraryFile(t, root, "Artist/Album/01.flac", "not a real flac")
	indexLibrary(t, srv)
	vol := newAwayVolume(t, root)

	var (
		mu       sync.Mutex
		requests []string
		faults   []string
		awayOnce sync.Once
	)
	note := func(err error) {
		if err != nil {
			mu.Lock()
			faults = append(faults, err.Error())
			mu.Unlock()
		}
	}
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/__test/back" {
			note(vol.back())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commit") {
			// The volume goes as the first commit arrives, every chunk landed.
			awayOnce.Do(func() { note(vol.away()) })
		}
		// As the console's own page reaches it: its host and its origin.
		r.Host = testConsoleHost
		r.Header.Set("Origin", "http://"+testConsoleHost)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	src := readConsoleJS(t, "static/app.js")
	prelude := extractJSConst(t, src, "API")
	for _, c := range []string{"const BYTE_UNITS = ", "const UPLOAD_PARALLEL_FILES = ", "const UPLOAD_DIGEST_RETRIES = ", "const UPLOAD_JUNK_NAMES = "} {
		prelude += jsStatement(t, src, c)
	}
	script := withReachedFunctions(t, prelude+uploadRetryRun, src,
		"wireUpload", "stageFiles", "startUpload", "retryUpload", "errorFromResponse")
	// Not the seeded track's size: the page leaves out a pick whose name and
	// size a track has, as already in the library.
	const path, body = "New Album/01.flac", "fresh audio bytes for the volume"
	raw := runScriptUnderNode(t, node, script, map[string]any{"base": ts.URL, "path": path, "bytes": body})
	var got struct {
		Refused, Retried uploadPageShown
		Back             int
	}
	// The last line: a node that warns writes before it.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	srvBgScansWait(t, srv)

	mu.Lock()
	defer mu.Unlock()
	if len(faults) > 0 {
		t.Fatalf("the volume could not be taken away or put back: %q", faults)
	}
	if !got.Refused.ReviewHidden || got.Refused.RetryHidden || !strings.Contains(got.Refused.Error, notMountedWords) {
		t.Errorf("after the refused commit the page shows %+v, want the review hidden, the refusal in the hold's words and Try again shown", got.Refused)
	}
	if !got.Retried.RetryHidden || got.Retried.Error != "" || !strings.Contains(got.Retried.Result, "1 added") {
		t.Errorf("after Try again the page shows %+v, want the file added, no error and Try again hidden", got.Retried)
	}
	var creates, chunks, reads int
	var commits []string
	for _, r := range requests {
		switch {
		case r == "POST /api/upload/sessions":
			creates++
		case strings.HasPrefix(r, "PUT /api/upload/sessions/"):
			chunks++
		case strings.HasPrefix(r, "POST /api/upload/sessions/") && strings.HasSuffix(r, "/commit"):
			commits = append(commits, r)
		case strings.HasPrefix(r, "GET /api/upload/sessions/"):
			reads++
		}
	}
	if creates != 1 || chunks != 1 || reads != 1 || len(commits) != 2 || commits[0] != commits[1] {
		t.Errorf("the page made %d session(s), sent %d chunk(s), read a session %d time(s) and committed %q, "+
			"want one session, its one chunk, one read by Try again and two commits of that session:\n%q",
			creates, chunks, reads, commits, requests)
	}
	if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))); err != nil || string(got) != body {
		t.Errorf("the committed file = %q, %v", got, err)
	}
}
