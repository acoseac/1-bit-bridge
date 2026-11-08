package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The console's half of backlog B776's round 2. A chunk refused with 409
// root_not_mounted lost its code: putUploadChunk read every 409's body for an
// offset and then handed the response, its body read, to errorFromResponse,
// which could read nothing and answered a bare status line, so the page
// offered no Try again. And transferAll answered the first worker's failure
// at once while its siblings' chunks were still out: the retry the failure
// offered then ran beside an old worker still sending, which a late failure
// of its own turned into a stop of the retry (it set uploadState.aborted).

// TestAChunkRefusalKeepsItsCodeAndAFailedTransferSettlesFirst pins, without
// node, the shape that does it; TestTheConsoleSettlesAFailedTransferBeforeItOffersItsRetry
// runs it under node, where node is installed.
func TestAChunkRefusalKeepsItsCodeAndAFailedTransferSettlesFirst(t *testing.T) {
	js := readFile(t, "static/app.js")
	put := stripJSComments(extractJSFunction(t, js, "putUploadChunk"))
	if strings.Contains(put, "errorFromResponse(r)") {
		t.Error("putUploadChunk hands errorFromResponse a response whose body it may have read: a refusal's code is lost")
	}
	if n := strings.Count(put, "await r.json()"); n != 1 {
		t.Errorf("putUploadChunk reads a refusal's body %d times, want once", n)
	}
	transfer := stripJSComments(extractJSFunction(t, js, "transferAll"))
	if strings.Contains(transfer, "uploadState.aborted = true") {
		t.Error("a worker's failure sets uploadState.aborted, which a retry clears while that worker may still be sending")
	}
	all := strings.Index(transfer, "await Promise.all(")
	if all < 0 || !strings.Contains(transfer[all:], "throw failure") {
		t.Error("transferAll does not answer its failure only once every worker has settled")
	}
}

// uploadOverlapRun drives the shipped upload page under node over two files:
// the first chunk out is held by the test, the second refused as its root's
// volume goes; the volume is put back, Try again is clicked as soon as the
// page offers it, and the held chunk is let go, answered with a failure of
// its own. What the page shows is read throughout.
const uploadOverlapRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const realFetch = globalThis.fetch;
globalThis.fetch = (url, init) => realFetch(new URL(url, input.base), init);
const waiters = new Set();
class PageEl {
  constructor(id) {
    this.id = id;
    this._hidden = false;
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
  get hidden() {
    return this._hidden;
  }
  set hidden(v) {
    this._hidden = v;
    if (!v && (this.id === "upload-retry" || this.id === "upload-error")) {
      for (const w of [...waiters]) {
        waiters.delete(w);
        w();
      }
    }
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
  pageEls.get(id)._hidden = true; // as upload.html ships them
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
// Whether the page shows an error or Try again within ms.
const showsWithin = (ms) => new Promise((resolve) => {
  if (!pageEls.get("upload-retry").hidden || !pageEls.get("upload-error").hidden) return resolve(true);
  const done = () => { clearTimeout(timer); resolve(true); };
  const timer = setTimeout(() => { waiters.delete(done); resolve(false); }, ms);
  waiters.add(done);
});
const test = (p, init) => realFetch(new URL(p, input.base), init);
wireUpload();
stageFiles([
  { file: new File([input.bodyA], "01.flac", { lastModified: 1700000000000 }), path: input.pathA },
  { file: new File([input.bodyB], "02.flac", { lastModified: 1700000000000 }), path: input.pathB },
]);
const started = pageEls.get("upload-start").click();
await test("/__test/refused");
await test("/__test/back", { method: "POST" });
const early = await showsWithin(2000);
const atHold = shown();
let retried = false;
if (!pageEls.get("upload-retry").hidden) {
  retried = true;
  await pageEls.get("upload-retry-button").click();
}
await test("/__test/release", { method: "POST" });
await started;
if (!retried && !pageEls.get("upload-retry").hidden) {
  retried = true;
  await pageEls.get("upload-retry-button").click();
}
const final = shown();
process.stdout.write(JSON.stringify({ early, atHold, final, retried }) + "\n", () => process.exit(0));
`

// TestTheConsoleSettlesAFailedTransferBeforeItOffersItsRetry runs the shipped
// upload page under node, where node is installed, against the console's real
// handlers: two files, the first chunk out held, the second refused as its
// root's volume goes (backlog B776). Nothing is offered while the held chunk
// is out; once it is let go, answered with a failure of its own, the page
// shows the refusal, which kept its code, with Try again, and Try again
// commits the same session. No file is ever sent by two requests at once.
func TestTheConsoleSettlesAFailedTransferBeforeItOffersItsRetry(t *testing.T) {
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
		inflight = map[string]int{}
		overlaps []string
		puts     int
		refused  = make(chan struct{})
		release  = make(chan struct{})
	)
	note := func(err error) {
		if err != nil {
			mu.Lock()
			faults = append(faults, err.Error())
			mu.Unlock()
		}
	}
	var releaseOnce, refusedOnce sync.Once
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/__test/refused":
			select {
			case <-refused:
				w.WriteHeader(http.StatusNoContent)
			case <-time.After(30 * time.Second):
				http.Error(w, "no chunk was refused", http.StatusInternalServerError)
			}
			return
		case "/__test/back":
			note(vol.back())
			w.WriteHeader(http.StatusNoContent)
			return
		case "/__test/release":
			releaseOnce.Do(func() { close(release) })
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// As the console's own page reaches it: its host and its origin.
		r.Host = testConsoleHost
		r.Header.Set("Origin", "http://"+testConsoleHost)
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/upload/sessions/") {
			fid := path.Base(r.URL.Path)
			mu.Lock()
			puts++
			n := puts
			inflight[fid]++
			if inflight[fid] > 1 {
				overlaps = append(overlaps, fid)
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				inflight[fid]--
				mu.Unlock()
			}()
			switch n {
			case 1:
				// The first chunk out is held, then answered with a failure of
				// its own, its root never reached.
				_, _ = io.Copy(io.Discard, r.Body)
				<-release
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"test_failure","message":"a failure the test made"}`))
				return
			case 2:
				// The second is refused: its root's volume goes as it arrives.
				note(vol.away())
				h.ServeHTTP(w, r)
				refusedOnce.Do(func() { close(refused) })
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		refusedOnce.Do(func() { close(refused) })
		ts.Close()
	})

	src := readConsoleJS(t, "static/app.js")
	prelude := extractJSConst(t, src, "API")
	for _, c := range []string{"const BYTE_UNITS = ", "const UPLOAD_PARALLEL_FILES = ", "const UPLOAD_DIGEST_RETRIES = ", "const UPLOAD_JUNK_NAMES = ", "const resumableReads = "} {
		prelude += jsStatement(t, src, c)
	}
	script := withReachedFunctions(t, prelude+uploadOverlapRun, src,
		"wireUpload", "stageFiles", "startUpload", "retryUpload", "errorFromResponse")
	const pathA, pathB = "New Album/01.flac", "New Album/02.flac"
	const bodyA, bodyB = "fresh audio bytes for the volume", "and the second file's own bytes"
	raw := runScriptUnderNode(t, node, script, map[string]any{
		"base": ts.URL, "pathA": pathA, "pathB": pathB, "bodyA": bodyA, "bodyB": bodyB,
	})
	var got struct {
		Early         bool
		AtHold, Final uploadPageShown
		Retried       bool
	}
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
	if got.Early {
		t.Errorf("the page answered the refusal while a chunk of the same transfer was still out: %+v", got.AtHold)
	}
	if len(overlaps) > 0 {
		t.Errorf("a file was sent by two requests at once (%q): the retry ran beside the old transfer:\n%q", overlaps, requests)
	}
	if !got.Retried || got.Final.Error != "" || !got.Final.RetryHidden || !strings.Contains(got.Final.Result, "2 added") {
		t.Errorf("after the transfer settled the page shows %+v (Try again clicked: %v), want both files added through Try again", got.Final, got.Retried)
	}
	creates := 0
	for _, r := range requests {
		if r == "POST /api/upload/sessions" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("the page made %d sessions, want one:\n%q", creates, requests)
	}
	for p, body := range map[string]string{pathA: bodyA, pathB: bodyB} {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err != nil || string(b) != body {
			t.Errorf("the committed %s = %q, %v", p, b, err)
		}
	}
}
