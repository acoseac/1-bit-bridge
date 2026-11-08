package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/upload"
)

// One upload owns the console's upload page at a time (backlog B797). Try
// again read its session from the server before it read the files to send,
// while the pickers, the drop target and Upload stayed live: a batch picked
// meanwhile with the same path and size and other bytes was what the old
// session was then sent, a splice no chunk digest sees and no whole-file digest
// is declared to catch, and either transfer's end acted on the other's page.
// A run (an upload, the resume of an interrupted one, or its retry) now owns
// the page from before its first await, with its files and its session;
// everything that would start or replace an upload is refused until it ends,
// with words; each of its continuations asks whether it still owns the page
// before it touches the page or uploadState, and whether it may still send
// right before each request; and Cancel while it prepares, or a navigation,
// takes the page from it. Refused, not superseded: a superseded run would
// leave its session with no control on the page.
//
// The first three tests pin, without node, the shape that does it, and run
// everywhere. The node cases after them run the page itself, where node is
// installed.

// uploadRunFunctions are the functions a run is: Upload's (a new upload, or
// the resume of an interrupted one) and Try again's.
var uploadRunFunctions = []string{"startUpload", "retryUpload"}

// uploadRunAsks are what a run's continuation asks before anything else:
// whether the run may go on, or, once a commit has answered, whether the page
// is still the run's.
var uploadRunAsks = []string{"uploadCheckpoint(run);", "if (!uploadOwns(run)) return;"}

// awaitsUnasked lists each statement of body that awaits and is not followed
// by one of the run's asks (uploadRunAsks).
func awaitsUnasked(body string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(body[i:], "await ")
		if j < 0 {
			return out
		}
		at := i + j
		end := strings.Index(body[at:], ";\n")
		if end < 0 {
			return append(out, strings.TrimSpace(body[at:]))
		}
		next := at + end + 2
		rest := strings.TrimLeft(body[next:], " \t\n")
		asked := false
		for _, ask := range uploadRunAsks {
			asked = asked || strings.HasPrefix(rest, ask)
		}
		if !asked {
			out = append(out, strings.TrimSpace(body[strings.LastIndex(body[:at], "\n")+1:next]))
		}
		i = next
	}
}

// jsBlockAt returns src's block that opens at the first brace at or after at,
// through the brace that closes it. The code it is asked of holds no brace in
// a string, and its comments are stripped.
func jsBlockAt(src string, at int) string {
	open := strings.Index(src[at:], "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := at + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[at+open : i+1]
			}
		}
	}
	return ""
}

// uploadJSFunction is app.js's function name with its comments stripped,
// extracted before the strip, as the other upload pins read it.
func uploadJSFunction(t *testing.T, src, name string) string {
	t.Helper()
	body, ok := extractJSFunctionIfPresent(src, name)
	if !ok {
		t.Errorf("app.js has no function %s", name)
		return ""
	}
	return stripJSComments(body)
}

// uploadBusyWords is what the note beside the pickers says while a run owns
// the page, as upload.html ships it, white space folded; "" where the template
// has no note.
func uploadBusyWords(tmpl string) string {
	at := strings.Index(tmpl, `id="upload-busy"`)
	if at < 0 {
		return ""
	}
	open := strings.Index(tmpl[at:], ">")
	end := strings.Index(tmpl[at:], "</p>")
	if open < 0 || end < open {
		return ""
	}
	return strings.Join(strings.Fields(tmpl[at+open+1:at+end]), " ")
}

// TestAnUploadRunOwnsThePageFromItsFirstAwait: a run takes the page, its files
// and its session before its first await, works on what it took from then on,
// asks after every await whether it may go on, and says and gives back nothing
// once the page is no longer its own.
func TestAnUploadRunOwnsThePageFromItsFirstAwait(t *testing.T) {
	src := readFile(t, "static/app.js")
	for _, name := range uploadRunFunctions {
		body := uploadJSFunction(t, src, name)
		first := strings.Index(body, "await ")
		if first < 0 {
			t.Errorf("%s awaits nothing", name)
			continue
		}
		if !strings.Contains(body[:first], "const run = beginUploadRun();") {
			t.Errorf("%s does not take the page, its files and its session before its first await: a pick made while it awaits replaces what it sends", name)
		}
		if strings.Contains(body[first:], "uploadState") {
			t.Errorf("%s reads the page's upload after its first await, where a later pick may stand: a run works on what it took (run.picked, run.session, run.state)", name)
		}
		for _, stmt := range awaitsUnasked(body) {
			t.Errorf("%s: %q is not followed by the run's ask (uploadCheckpoint(run), or uploadOwns(run) once a commit has answered), so what follows acts on a page that may no longer be the run's", name, stmt)
		}
		if !strings.Contains(body, "} catch (e) {\n    showUploadRunEnd(run, e);\n  } finally {\n    endUploadRun(run);\n  }") {
			t.Errorf("%s does not end through showUploadRunEnd and endUploadRun, which say and give back nothing once the run no longer owns the page", name)
		}
	}

	begin := uploadJSFunction(t, src, "beginUploadRun")
	for _, want := range []string{"picked: [...state.picked]", "session: state.session", "state.run = run;", "setUploadBusy(true);"} {
		if !strings.Contains(begin, want) {
			t.Errorf("beginUploadRun has no %s: a run must take its files and its session, and the page, as it begins", want)
		}
	}
	if !strings.Contains(uploadJSFunction(t, src, "uploadOwns"), "return uploadState === run.state && run.state.run === run;") {
		t.Error("uploadOwns does not answer whether the page's upload is still the run's, and the run still its upload's")
	}
	if !strings.Contains(uploadJSFunction(t, src, "uploadRunLive"), "return !run.aborted && uploadOwns(run);") {
		t.Error("uploadRunLive does not answer whether the run was not stopped and still owns the page")
	}
	for _, name := range []string{"showUploadRunEnd", "endUploadRun"} {
		body := uploadJSFunction(t, src, name)
		if open := strings.Index(body, "{"); open < 0 || !strings.HasPrefix(strings.TrimSpace(body[open+1:]), "if (!uploadOwns(run)) return;") {
			t.Errorf("%s does not ask first whether its run still owns the page", name)
		}
	}

	// What a run's transfer, its chunks, its identity reads, its session's
	// create and its report work on is the run's, never the page's upload.
	for _, name := range []string{"transferAll", "putUploadChunkVerified", "putUploadChunk", "chunkDigestHeaders", "attachUploadIdentity", "createUploadSession", "reportCommit"} {
		if strings.Contains(uploadJSFunction(t, src, name), "uploadState") {
			t.Errorf("%s reads or writes uploadState, which may be another upload's by the time it runs: it works on what its run took", name)
		}
	}
	transfer := uploadJSFunction(t, src, "transferAll")
	if !strings.Contains(transfer, "new Map(run.picked.map(") {
		t.Error("transferAll does not send the files its run took at its start")
	}
	if at := strings.Index(transfer, "const paint = () => {"); at < 0 || !strings.HasPrefix(strings.TrimSpace(transfer[at+len("const paint = () => {"):]), "if (!uploadOwns(run)) return;") {
		t.Error("transferAll's paint touches the page without asking whether it is still the run's")
	}
	if !strings.Contains(uploadJSFunction(t, src, "putUploadChunk"), "if (!digest) run.state.digestUnavailable = true;") {
		t.Error("a chunk sent with no digest is not recorded on its run's upload")
	}
}

// TestAnUploadRunSendsNothingOnceItMayNotGoOn: a run asks whether it may
// still send right before each request (each chunk, its retries included,
// after the chunk was read and hashed; its session's create), and Cancel and
// a navigation take the page from it, so a run stopped either way sends
// nothing new and its continuations touch nothing.
func TestAnUploadRunSendsNothingOnceItMayNotGoOn(t *testing.T) {
	src := readFile(t, "static/app.js")
	put := uploadJSFunction(t, src, "putUploadChunk")
	hash := strings.Index(put, "await chunkDigestHeaders(bytes)")
	send := strings.Index(put, "await fetch(")
	if hash < 0 || send < hash {
		t.Errorf("putUploadChunk does not hash its chunk before it makes the chunk's request:\n%s", put)
	} else if between := put[hash+len("await ") : send]; !strings.Contains(between, "uploadCheckpoint(run);") || strings.Contains(between, "await ") {
		t.Errorf("putUploadChunk does not ask whether its run may still send right before the chunk's request, after the chunk was read and hashed:\n%s", between)
	}
	if !strings.Contains(uploadJSFunction(t, src, "putUploadChunkVerified"), "return await putUploadChunk(run, sid, fid, offset, bytes);") {
		t.Error("a digest mismatch's re-send does not go through putUploadChunk's ask")
	}
	if !strings.Contains(uploadJSFunction(t, src, "createUploadSession"), "uploadCheckpoint(run);\n  return API.post(\"/api/upload/sessions\"") {
		t.Error("createUploadSession makes a session without asking first whether its run may still send")
	}
	if !strings.Contains(uploadJSFunction(t, src, "attachUploadIdentity"), "for (const p of picked) {\n    uploadCheckpoint(run);") {
		t.Error("attachUploadIdentity reads every file's identity whether or not its run was stopped")
	}
	transfer := uploadJSFunction(t, src, "transferAll")
	if !strings.Contains(transfer, "const stopped = () => !uploadRunLive(run) || failure !== null;") {
		t.Error("transferAll's workers go on taking chunks once their run may not")
	}
	if !strings.Contains(transfer, `if (failure === null && err?.code !== "aborted") failure = err;`) {
		t.Error("a worker that finds its run stopped before a chunk's request counts it as the transfer's failure")
	}

	// Cancel and a navigation take the page from a run.
	reset := uploadJSFunction(t, src, "resetUpload")
	if !strings.Contains(reset, "const run = uploadState?.run;\n  if (run) run.aborted = true;\n  uploadState = null;") || !strings.Contains(reset, "setUploadBusy(false);") {
		t.Errorf("resetUpload (Cancel) does not stop a run and give the page back:\n%s", reset)
	}
	wire := uploadJSFunction(t, src, "wireUpload")
	if abort := strings.Index(wire, `signal?.addEventListener("abort", () => {`); abort < 0 || !strings.Contains(jsBlockAt(wire, abort), "uploadState = null;") {
		t.Error("a navigation leaves the page's upload in place, for the continuations of a page no longer shown to act on")
	}
	if !strings.Contains(wire, `if (!signal?.aborted) setText("upload-root"`) {
		t.Error("the library root's read writes into the page a navigation put in place of the one it was for")
	}
	refresh := uploadJSFunction(t, src, "refreshResumable")
	read, asked, wrote := strings.Index(refresh, "await "), strings.Index(refresh, "if (!banner.isConnected) return;"), strings.Index(refresh, "resumableSessions = sessions;")
	if read < 0 || asked < read || wrote < asked {
		t.Error("refreshResumable writes the banner of a page a navigation replaced while it read")
	}
}

// TestTheUploadPageRefusesAnotherUploadWhileARunOwnsIt: while a run owns the
// page, a pick, a drop (one whose folder was still being read included),
// Upload, Try again and Discard it are refused, the drop target in its handler
// since a div cannot be disabled, and the note in the drop zone says why; Stop
// is offered from the retry's start, and a stopped run's session is listed
// again, as the stop's words promise.
func TestTheUploadPageRefusesAnotherUploadWhileARunOwnsIt(t *testing.T) {
	src := readFile(t, "static/app.js")
	for name, refusal := range map[string]string{
		"stageFiles":       "if (uploadState?.run) return;",
		"startUpload":      "if (!uploadState || uploadState.run) return;",
		"retryUpload":      "if (!sid || uploadState.run) return;",
		"discardResumable": "if (!s || uploadState?.run) return;",
	} {
		body := uploadJSFunction(t, src, name)
		at := strings.Index(body, refusal)
		if first := strings.Index(body, "await "); at < 0 || (first >= 0 && at > first) {
			t.Errorf("%s does not refuse while a run owns the page, before anything else it does", name)
		}
	}
	if stage := uploadJSFunction(t, src, "stageFiles"); !strings.HasPrefix(strings.TrimSpace(stage[strings.Index(stage, "{")+1:]), "if (uploadState?.run) return;") {
		t.Error("stageFiles does something before it refuses a pick while a run owns the page")
	}

	wire := uploadJSFunction(t, src, "wireUpload")
	if at := strings.Index(wire, `drop?.addEventListener("drop", async (e) => {`); at < 0 {
		t.Error("wireUpload has no drop handler")
	} else {
		handler := jsBlockAt(wire, at)
		walk := strings.Index(handler, "await collectFromDataTransfer(")
		before := strings.Index(handler, "if (uploadState?.run) return;")
		after := strings.Index(handler, "if (signal?.aborted || uploadState?.run) return;")
		stage := strings.Index(handler, "stageFiles(picked);")
		if walk < 0 || before < 0 || before > walk {
			t.Errorf("the drop target, a div no disabled state stops, takes a drop while a run owns the page:\n%s", handler)
		}
		if after < walk || stage < after {
			t.Errorf("a dropped folder read while an upload began, or while a navigation replaced the page, is staged anyway:\n%s", handler)
		}
	}
	if stop := strings.Index(wire, `getElementById("upload-abort")?.addEventListener("click", () => {`); stop < 0 || !strings.Contains(jsBlockAt(wire, stop), "const run = uploadState?.run;\n    if (run) run.aborted = true;") {
		t.Error("Stop does not stop the run that owns the page")
	}

	busy := uploadJSFunction(t, src, "setUploadBusy")
	for _, want := range []string{`"upload-files"`, `"upload-folder"`, `"upload-start"`, `"upload-retry-button"`, `"upload-resume-discard"`,
		"setDisabled(el, busy);", `drop.setAttribute("aria-disabled", "true");`, `drop.removeAttribute("aria-disabled");`,
		`getElementById("upload-busy")`, "note.hidden = !busy;"} {
		if !strings.Contains(busy, want) {
			t.Errorf("setUploadBusy has no %s", want)
		}
	}
	for _, name := range []string{"endUploadRun", "resetUpload"} {
		if !strings.Contains(uploadJSFunction(t, src, name), "setUploadBusy(false);") {
			t.Errorf("%s does not give the page back to a new upload", name)
		}
	}
	retry := uploadJSFunction(t, src, "retryUpload")
	if stop := strings.Index(retry, `document.getElementById("upload-progress").hidden = false;`); stop < 0 || stop > strings.Index(retry, "await ") {
		t.Error("Stop is not offered until Try again has read its session: a stop during that read cannot be pressed")
	}
	// A stop says its session's progress is kept and that starting again
	// resumes it, which holds only once the page has read its interrupted
	// uploads again: a session made since the page's last read is in no list.
	if stopped := uploadJSFunction(t, src, "showUploadStopped"); !strings.Contains(stopped, "start again to resume") || !strings.Contains(stopped, "refreshResumable();") {
		t.Error("a stopped upload's session is not read back into the page's list, so choosing the same files again starts over, against what the stop says")
	}

	tmpl := readFile(t, "templates/upload.html")
	zone, note := strings.Index(tmpl, `id="upload-drop"`), strings.Index(tmpl, `id="upload-busy"`)
	if zone < 0 || note < zone || strings.Count(tmpl[zone:note], "<div") < strings.Count(tmpl[zone:note], "</div>") {
		t.Error("upload.html has no busy note inside the drop zone, beside the pickers it explains")
	} else {
		tag := tmpl[strings.LastIndex(tmpl[:note], "<"):]
		if !strings.Contains(tag[:strings.Index(tag, ">")], "hidden") {
			t.Error("the busy note is shown as upload.html ships it, before any upload")
		}
	}
	if words := uploadBusyWords(tmpl); !strings.Contains(words, "An upload is running") || !strings.Contains(words, "stopped") {
		t.Errorf("the busy note says %q: it must say an upload is running and what lets the operator choose more files", words)
	}
	css := uploadCSSCommentRe.ReplaceAllString(readFile(t, "static/app.css"), "")
	if !strings.Contains(css, ".upload-pick label:has(input:disabled)") {
		t.Error("a picker refused while a run owns the page still looks live: its label is what reads as the control")
	}
}

// The node cases. Each runs the shipped upload page under node, where node is
// installed, against the console's real handlers behind ownershipRig, through
// the page's wired events (ownershipRun): a pick goes through the folder
// picker's change event, a drop through the drop target's handler, and a
// click is taken only by an element that is shown and not disabled, as in a
// browser. Sessions take chunks of eight bytes, so OLD goes in four and a
// retry has chunks left after its first.
const (
	ownershipChunk     = 8
	ownershipPath      = "New Album/01.flac"
	ownershipOld       = "old:0123old:4567old:89abold:cdef" // 32 bytes, four chunks
	ownershipNew       = "NEW:0123NEW:4567NEW:89abNEW:cdef" // OLD's path and size, other bytes
	ownershipOtherPath = "Other Album/02.flac"
	ownershipOther     = "oth:0123oth:4567oth:89ab" // 24 bytes, three chunks
	// File.lastModified in milliseconds, past 2^31: an int64, not the int an
	// untyped constant becomes in a map of any, which a 32-bit build overflows.
	ownershipModified = int64(1700000000000)
)

// ownershipReq is one request the page made, as the rig saw it.
type ownershipReq struct {
	Kind    string // create, list, read, put, commit, abandon, or other
	Method  string
	Path    string
	Session string
	File    string
	Offset  int64
	Body    []byte
	Status  int
	Answer  []byte
	Served  bool // false where the rig answered it itself
}

// ownershipRule acts on the nth request of a kind, counted from 1: it holds
// it until the script lets it go, takes the root's volume away before it is
// served, or answers it with Answer and AnswerBody, never serving it; in that
// order, where a rule names more than one.
type ownershipRule struct {
	Kind       string
	Nth        int
	Hold       string
	Away       bool
	Answer     int
	AnswerBody string
}

type ownershipHold struct {
	reached, release         chan struct{}
	reachedOnce, releaseOnce sync.Once
}

func (h *ownershipHold) let() { h.releaseOnce.Do(func() { close(h.release) }) }

// ownershipRig is the console's real handlers behind a test server that
// records every request the upload page makes, in order, acts on the ones the
// case's rules name, and takes the script's own requests under /__test/: put
// the volume back, wait until a held request is reached, let it go.
type ownershipRig struct {
	t    *testing.T
	srv  *Server
	h    http.Handler
	root string
	vol  *awayVolume
	ts   *httptest.Server

	mu     sync.Mutex
	reqs   []ownershipReq
	counts map[string]int
	rules  []ownershipRule
	holds  map[string]*ownershipHold
	faults []string
}

// ownershipWriter keeps the status and the body a request was answered with.
type ownershipWriter struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

func (w *ownershipWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *ownershipWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// Unwrap lets the chunk handler's read deadline reach the connection.
func (w *ownershipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ownershipKind names an upload request by what it does.
func ownershipKind(method, p string) (kind, sid, fid string) {
	const base = "/api/upload/sessions"
	switch {
	case p == base && method == http.MethodPost:
		return "create", "", ""
	case p == base && method == http.MethodGet:
		return "list", "", ""
	case strings.HasPrefix(p, base+"/"):
		rest := strings.Split(strings.TrimPrefix(p, base+"/"), "/")
		switch {
		case len(rest) == 1 && method == http.MethodGet:
			return "read", rest[0], ""
		case len(rest) == 1 && method == http.MethodDelete:
			return "abandon", rest[0], ""
		case len(rest) == 2 && rest[1] == "commit" && method == http.MethodPost:
			return "commit", rest[0], ""
		case len(rest) == 3 && rest[1] == "files" && method == http.MethodPut:
			return "put", rest[0], rest[2]
		}
	}
	return "other", "", ""
}

// newOwnershipRig is a console with uploads on, chunks of ownershipChunk bytes
// and one root holding an indexed track, so the root reads as not mounted
// while its volume is away, behind the rig.
func newOwnershipRig(t *testing.T, rules ...ownershipRule) *ownershipRig {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	srv.deps.Upload = upload.NewManager(upload.Config{ChunkBytes: ownershipChunk}, srv.deps.Scanner.Roots,
		upload.WithFreeBytes(func(string) (int64, error) { return 1 << 40, nil }),
		upload.WithRootGuard(srv.deps.Scanner.WriteGuard(srv.deps.ScanCtx)))
	root := cfg.LibraryRoots[0]
	seedLibraryFile(t, root, "Artist/Album/01.flac", "not a real flac")
	indexLibrary(t, srv)
	rig := &ownershipRig{
		t: t, srv: srv, h: srv.Handler(), root: root, vol: newAwayVolume(t, root),
		counts: map[string]int{}, rules: rules, holds: map[string]*ownershipHold{},
	}
	for _, r := range rules {
		if r.Hold != "" {
			rig.holds[r.Hold] = &ownershipHold{reached: make(chan struct{}), release: make(chan struct{})}
		}
	}
	rig.ts = httptest.NewServer(http.HandlerFunc(rig.serve))
	t.Cleanup(func() {
		for _, h := range rig.holds {
			h.let()
		}
		rig.ts.Close()
	})
	return rig
}

func (rig *ownershipRig) note(err error) {
	if err != nil {
		rig.mu.Lock()
		rig.faults = append(rig.faults, err.Error())
		rig.mu.Unlock()
	}
}

func (rig *ownershipRig) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__test/back":
		if err := rig.vol.back(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case "/__test/reached", "/__test/release":
		h := rig.holds[r.URL.Query().Get("hold")]
		if h == nil {
			http.Error(w, "no such hold", http.StatusNotFound)
			return
		}
		if r.URL.Path == "/__test/release" {
			h.let()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-h.reached:
			w.WriteHeader(http.StatusNoContent)
		case <-time.After(30 * time.Second):
			http.Error(w, "the page never made the request this hold waits for", http.StatusGatewayTimeout)
		case <-r.Context().Done():
			panic(http.ErrAbortHandler)
		}
		return
	}

	kind, sid, fid := ownershipKind(r.Method, r.URL.Path)
	// Read whole before anything is held: the record keeps it, and a held
	// request's body is drained.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	rig.mu.Lock()
	rig.counts[kind]++
	nth := rig.counts[kind]
	idx := len(rig.reqs)
	rig.reqs = append(rig.reqs, ownershipReq{Kind: kind, Method: r.Method, Path: r.URL.Path, Session: sid, File: fid, Offset: offset, Body: body})
	var rule ownershipRule
	for _, candidate := range rig.rules {
		if candidate.Kind == kind && candidate.Nth == nth {
			rule = candidate
		}
	}
	rig.mu.Unlock()

	if rule.Hold != "" {
		h := rig.holds[rule.Hold]
		h.reachedOnce.Do(func() { close(h.reached) })
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
		if r.Context().Err() != nil {
			// The page is gone (node exited): answer nothing.
			panic(http.ErrAbortHandler)
		}
	}
	if rule.Away {
		rig.note(rig.vol.away())
	}
	rw := &ownershipWriter{ResponseWriter: w}
	served := rule.Answer == 0
	if served {
		// As the console's own page reaches it: its host and its origin.
		r.Host = testConsoleHost
		r.Header.Set("Origin", "http://"+testConsoleHost)
		rig.h.ServeHTTP(rw, r)
	} else {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(rule.Answer)
		_, _ = rw.Write([]byte(rule.AnswerBody))
	}
	rig.mu.Lock()
	rig.reqs[idx].Status, rig.reqs[idx].Answer, rig.reqs[idx].Served = rw.code, rw.body.Bytes(), served
	rig.mu.Unlock()
}

// requests is a copy of what the page asked, in order.
func (rig *ownershipRig) requests() []ownershipReq {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return append([]ownershipReq(nil), rig.reqs...)
}

// sessionsMade is the ids of the sessions the page's creates made, in order.
func (rig *ownershipRig) sessionsMade() []string {
	var ids []string
	for _, r := range rig.requests() {
		if r.Kind != "create" || r.Status != http.StatusCreated {
			continue
		}
		var s struct{ ID string }
		if json.Unmarshal(r.Answer, &s) == nil && s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// of is the requests of kind the page made of session sid ("" for any).
func (rig *ownershipRig) of(kind, sid string) []ownershipReq {
	var out []ownershipReq
	for _, r := range rig.requests() {
		if r.Kind == kind && (sid == "" || r.Session == sid) {
			out = append(out, r)
		}
	}
	return out
}

// ownershipPut is a chunk's request as a case expects it: its offset and the
// status it was answered with.
type ownershipPut struct {
	Offset int64
	Status int
}

// requirePuts fails the test unless the chunks the page sent session sid were,
// in order, want, each carrying the bytes body holds at its offset: never
// another file's.
func (rig *ownershipRig) requirePuts(t *testing.T, sid, body string, want []ownershipPut) {
	t.Helper()
	got := rig.of("put", sid)
	var seen []ownershipPut
	for _, p := range got {
		seen = append(seen, ownershipPut{p.Offset, p.Status})
		end := min(p.Offset+ownershipChunk, int64(len(body)))
		if p.Offset < 0 || p.Offset >= end || string(p.Body) != body[p.Offset:end] {
			t.Errorf("session %s was sent %q at offset %d, want %q: the bytes of a file it never took", sid, p.Body, p.Offset, body[min(p.Offset, end):end])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("session %s was sent the chunks %v, want %v", sid, seen, want)
		return
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("session %s was sent the chunks %v, want %v", sid, seen, want)
			return
		}
	}
}

// requireFile fails the test unless the root holds body at path; or nothing
// there, where body is "".
func (rig *ownershipRig) requireFile(t *testing.T, path, body string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(rig.root, filepath.FromSlash(path)))
	if body == "" {
		if !os.IsNotExist(err) {
			t.Errorf("the library holds %s (%q, %v), which no upload the page owned committed", path, got, err)
		}
		return
	}
	if err != nil || string(got) != body {
		t.Errorf("the library's %s = %q, %v; want %q", path, got, err, body)
	}
}

// requireListed fails the test unless the server lists session sid as an
// interrupted upload, which choosing the same files again carries on.
func (rig *ownershipRig) requireListed(t *testing.T, sid string) {
	t.Helper()
	var listed []map[string]any
	if code := doJSON(t, rig.h, "GET", "/api/upload/sessions", nil, &listed); code != http.StatusOK {
		t.Fatalf("list sessions = %d", code)
	}
	for _, s := range listed {
		if s["id"] == sid {
			return
		}
	}
	t.Errorf("session %s is not listed as an interrupted upload: %v", sid, listed)
}

// interruptedSession makes, through the console, the session an interrupted
// upload of OLD left: OLD's identity as the page records it, its first chunk
// sent.
func (rig *ownershipRig) interruptedSession(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(ownershipOld))
	// The head and the tail are the whole of a file under 64 KiB.
	fingerprint := hex.EncodeToString(sum[:]) + hex.EncodeToString(sum[:])
	sess := createSession(t, rig.srv, []map[string]any{{
		"path": ownershipPath, "size": len(ownershipOld), "modified": ownershipModified, "fingerprint": fingerprint,
	}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)
	if code, out := putChunk(t, rig.h, sid, fid, 0, []byte(ownershipOld[:ownershipChunk]), nil); code != http.StatusOK {
		t.Fatalf("the interrupted session's first chunk = %d %v", code, out)
	}
	return sid
}

// ownershipShown is what the upload page showed at a step.
type ownershipShown struct {
	ReviewShown     bool     `json:"reviewShown"`
	ProgressShown   bool     `json:"progressShown"`
	Error           string   `json:"error"`
	RetryShown      bool     `json:"retryShown"`
	Result          string   `json:"result"`
	Busy            string   `json:"busy"`
	FilesDisabled   bool     `json:"filesDisabled"`
	FolderDisabled  bool     `json:"folderDisabled"`
	StartDisabled   bool     `json:"startDisabled"`
	RetryDisabled   bool     `json:"retryDisabled"`
	DiscardDisabled bool     `json:"discardDisabled"`
	DropDisabled    bool     `json:"dropDisabled"`
	StopClickable   bool     `json:"stopClickable"`
	CancelClickable bool     `json:"cancelClickable"`
	BannerShown     bool     `json:"bannerShown"`
	Preview         []string `json:"preview"`
}

// ownershipOut is what a case's run of the page reports.
type ownershipOut struct {
	Error        string                    `json:"error"`
	Shown        map[string]ownershipShown `json:"shown"`
	Took         map[string]bool           `json:"took"`
	Banner       string                    `json:"banner"`
	AwaitingScan bool                      `json:"awaitingScan"`
}

// requireBusy fails the test unless the page refused another upload in s: the
// note beside the pickers said words, and both pickers, Upload, Try again and
// Discard it were disabled, the drop target marked so.
func requireBusy(t *testing.T, at string, s ownershipShown, words string) {
	t.Helper()
	if words == "" || s.Busy != words {
		t.Errorf("%s: the drop zone says %q, want the busy note %q", at, s.Busy, words)
	}
	if !s.FilesDisabled || !s.FolderDisabled || !s.StartDisabled || !s.RetryDisabled || !s.DiscardDisabled || !s.DropDisabled {
		t.Errorf("%s: an upload owns the page and the page still offers another: %+v", at, s)
	}
}

// requireFree fails the test unless the page took a new upload in s.
func requireFree(t *testing.T, at string, s ownershipShown) {
	t.Helper()
	if s.Busy != "" || s.FilesDisabled || s.FolderDisabled || s.StartDisabled || s.RetryDisabled || s.DiscardDisabled || s.DropDisabled {
		t.Errorf("%s: no upload owns the page and it still refuses one: %+v", at, s)
	}
}

// requireAdded fails the test unless s reports its upload's commit, alone.
func requireAdded(t *testing.T, at string, s ownershipShown, added string) {
	t.Helper()
	if !strings.Contains(s.Result, added) || s.Error != "" || s.RetryShown || s.ProgressShown {
		t.Errorf("%s: the page shows %+v, want %q added and nothing else", at, s, added)
	}
}

// ownershipRun drives the shipped upload page through its wired events over
// the case input.case names. It reads the page's elements as upload.html nests
// them: an element shows only while it and every element around it do, and only
// a shown element that is not disabled takes a click or a pick (null where it
// took none). A request a case holds is waited for and let go through the rig.
// What the page showed at each step, and what each click took, is printed as
// JSON on the last line.
const ownershipRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const realFetch = globalThis.fetch;
globalThis.fetch = (url, init) => realFetch(new URL(url, input.base), init);
const rigCall = async (path, method) => {
  const r = await realFetch(new URL(path, input.base), { method });
  if (r.status !== 204) throw new Error(path + " answered " + r.status + ": " + (await r.text()));
};
const reached = (hold) => rigCall("/__test/reached?hold=" + hold, "GET");
const release = (hold) => rigCall("/__test/release?hold=" + hold, "POST");
const back = () => rigCall("/__test/back", "POST");

const containerOf = {
  "upload-busy": "upload-drop",
  "upload-files": "upload-drop",
  "upload-folder": "upload-drop",
  "upload-resume-text": "upload-resume",
  "upload-resume-discard": "upload-resume",
  "upload-count": "upload-review",
  "upload-count-plural": "upload-review",
  "upload-total": "upload-review",
  "upload-preview": "upload-review",
  "upload-start": "upload-review",
  "upload-cancel": "upload-review",
  "upload-overwrite": "upload-review",
  "upload-bar": "upload-progress",
  "upload-status": "upload-progress",
  "upload-abort": "upload-progress",
  "upload-retry-button": "upload-retry",
};
const pageIDs = ["upload-root", "upload-resume", "upload-resume-text", "upload-resume-discard", "upload-drop",
  "upload-files", "upload-folder", "upload-busy", "upload-review", "upload-count", "upload-count-plural",
  "upload-total", "upload-preview", "upload-start", "upload-cancel", "upload-overwrite", "upload-progress",
  "upload-bar", "upload-status", "upload-abort", "upload-error", "upload-retry", "upload-retry-button",
  "upload-dupe-note", "upload-result"];
const hiddenAsShipped = ["upload-resume", "upload-busy", "upload-review", "upload-progress", "upload-error",
  "upload-retry", "upload-dupe-note", "upload-result"];

let page = null;
class PageEl {
  constructor(id, els) {
    this.id = id;
    this.els = els;
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.value = "";
    this.className = "";
    this.href = "";
    this.dataset = {};
    this.children = [];
    this.text = "";
    this.listeners = {};
    this.attrs = {};
    this.classes = new Set();
    this.files = [];
  }
  get isConnected() {
    return page !== null && page.els === this.els;
  }
  get classList() {
    const classes = this.classes;
    return {
      add: (c) => classes.add(c),
      remove: (c) => classes.delete(c),
      contains: (c) => classes.has(c),
    };
  }
  setAttribute(name, value) {
    this.attrs[name] = String(value);
  }
  removeAttribute(name) {
    delete this.attrs[name];
  }
  getAttribute(name) {
    return Object.hasOwn(this.attrs, name) ? this.attrs[name] : null;
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
  // fire runs the listeners of type as a browser dispatches an event: each
  // runs until its first await, and what an async one returns is handed back
  // for the script to await when it chooses.
  fire(type, event) {
    return (this.listeners[type] || []).map((fn) => fn(event));
  }
  scrollIntoView() {}
  focus() {}
}
globalThis.document = {
  getElementById: (id) => (page === null ? null : page.els.get(id) ?? null),
  createElement: () => new PageEl("", page.els),
  createTextNode: (data) => ({ data }),
  activeElement: null,
  body: {},
};

let uploadState = null;
let resumableSessions = null;
let uploadAwaitingScan = false;

// newPage renders the page as upload.html ships it and wires it, as initUpload
// does with uploads on.
const newPage = () => {
  const els = new Map();
  for (const id of pageIDs) els.set(id, new PageEl(id, els));
  for (const id of hiddenAsShipped) els.get(id).hidden = true;
  els.get("upload-busy").text = input.busyWords;
  page = { els, nav: new AbortController() };
  wireUpload(page.nav.signal);
};
// navigate is a boosted navigation away and back: the page's scope aborted, as
// boostSwap and dispatchPageInit abort it, its elements replaced, and the page
// rendered and wired again. resumableSessions is cleared so that ready() waits
// for the new page's own read.
const navigate = () => {
  page.nav.abort();
  resumableSessions = null;
  newPage();
};
const el = (id) => page.els.get(id);
const shows = (e) => {
  for (let at = e; at; at = at.els.get(containerOf[at.id])) {
    if (at.hidden) return false;
  }
  return e.isConnected;
};
const click = (id) => {
  const e = el(id);
  if (!shows(e) || e.disabled) return null;
  return Promise.all(e.fire("click", {}));
};
// pick chooses files with the folder picker: through its label, which a
// hidden or disabled input does not answer, then its change event.
const pick = (files) => {
  const e = el("upload-folder");
  if (!shows(e) || e.disabled) return null;
  e.files = files;
  return Promise.all(e.fire("change", {}));
};
// drop drops on the drop target, a div, which takes every drop: its handler
// decides.
const drop = (dataTransfer) => {
  const e = el("upload-drop");
  if (!shows(e)) return null;
  return Promise.all(e.fire("drop", { preventDefault() {}, dataTransfer }));
};
// fileAt is a file at path in a picked folder, as the folder picker gives it.
const fileAt = (path, body) => {
  const f = new File([body], path.split("/").pop(), { lastModified: input.lastModified });
  Object.defineProperty(f, "webkitRelativePath", { value: path });
  return f;
};
// folderDrop is a dropped folder holding files, whose reading waits until the
// script calls open.
const folderDrop = (name, files) => {
  let open = null;
  const opened = new Promise((resolve) => {
    open = resolve;
  });
  let first = true;
  const dir = {
    isFile: false,
    name,
    createReader: () => ({
      readEntries: (ok) => {
        if (!first) {
          ok([]);
          return;
        }
        first = false;
        opened.then(() => ok(files.map((f) => ({ isFile: true, name: f.name, file: (got) => got(f) }))));
      },
    }),
  };
  return { open, dataTransfer: { items: [{ webkitGetAsEntry: () => dir }], files: [] } };
};
const waitFor = async (what, cond) => {
  const deadline = Date.now() + 30000;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error("timed out waiting for " + what);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
};
// settle waits for what a click, a pick or a drop started, where it took one.
const settle = async (p, what) => {
  if (p === null) return;
  let timer = null;
  const late = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error("timed out waiting for " + what)), 30000);
  });
  try {
    await Promise.race([p, late]);
  } finally {
    clearTimeout(timer);
  }
};
// ready waits for the page's read of the interrupted uploads.
const ready = () => waitFor("the page's read of the interrupted uploads", () => resumableSessions !== null);
const shown = () => {
  const text = (id) => (shows(el(id)) ? el(id).textContent : "");
  return {
    reviewShown: shows(el("upload-review")),
    progressShown: shows(el("upload-progress")),
    error: text("upload-error"),
    retryShown: shows(el("upload-retry")),
    result: text("upload-result"),
    busy: text("upload-busy"),
    filesDisabled: el("upload-files").disabled,
    folderDisabled: el("upload-folder").disabled,
    startDisabled: el("upload-start").disabled,
    retryDisabled: el("upload-retry-button").disabled,
    discardDisabled: el("upload-resume-discard").disabled,
    dropDisabled: el("upload-drop").getAttribute("aria-disabled") === "true",
    stopClickable: shows(el("upload-abort")) && !el("upload-abort").disabled,
    cancelClickable: shows(el("upload-cancel")) && !el("upload-cancel").disabled,
    bannerShown: shows(el("upload-resume")),
    preview: shows(el("upload-review")) ? el("upload-preview").children.map((c) => c.textContent) : [],
  };
};

const out = { shown: {}, took: {} };
const oldFile = () => fileAt(input.path, input.old);
// refusedThenRetried uploads OLD, which the volume's going stops after its
// first chunk, and presses Try again once the volume is back; while the
// retry's request held as hold is out, a batch with OLD's path and size and
// NEW's bytes is picked and Upload pressed, and then the hold is let go.
const refusedThenRetried = async (hold) => {
  newPage();
  await ready();
  await settle(pick([oldFile()]), "the pick");
  await settle(click("upload-start"), "the upload");
  out.shown.refused = shown();
  await back();
  const retry = click("upload-retry-button");
  out.took.retry = retry !== null;
  await reached(hold);
  out.shown.during = shown();
  const repick = pick([fileAt(input.path, input.new)]);
  out.took.repick = repick !== null;
  await settle(repick, "the second pick");
  const reupload = click("upload-start");
  out.took.reupload = reupload !== null;
  await release(hold);
  await settle(retry, "Try again");
  await settle(reupload, "the second upload");
  out.shown.final = shown();
};
const steps = {
  "splice-read": () => refusedThenRetried("retry-read"),
  "splice-put": () => refusedThenRetried("retry-put"),
  "splice-digest": () => refusedThenRetried("digest-resend"),
  "stop-read": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    await settle(click("upload-start"), "the upload");
    out.shown.refused = shown();
    await back();
    const retry = click("upload-retry-button");
    await reached("retry-read");
    out.shown.during = shown();
    const stop = click("upload-abort");
    out.took.stop = stop !== null;
    await settle(stop, "Stop");
    await release("retry-read");
    await settle(retry, "Try again");
    out.shown.stopped = shown();
    await waitFor("the banner to list the stopped upload", () => shows(el("upload-resume")));
    out.banner = el("upload-resume-text").textContent;
    await settle(pick([oldFile()]), "the pick of the same files");
    await settle(click("upload-start"), "the resume");
    out.shown.final = shown();
  },
  "cancel-prepare": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const first = click("upload-start");
    await reached("create");
    out.shown.during = shown();
    const cancel = click("upload-cancel");
    out.took.cancel = cancel !== null;
    await settle(cancel, "Cancel");
    out.shown.cancelled = shown();
    const repick = pick([fileAt(input.otherPath, input.other)]);
    out.took.repick = repick !== null;
    await settle(repick, "the second pick");
    const second = click("upload-start");
    out.took.second = second !== null;
    await settle(second, "the second upload");
    out.shown.second = shown();
    await release("create");
    await settle(first, "the cancelled upload");
    out.shown.final = shown();
  },
  "cancel-overlap": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const first = click("upload-start");
    await reached("create");
    await settle(click("upload-cancel"), "Cancel");
    await settle(pick([fileAt(input.otherPath, input.other)]), "the second pick");
    const second = click("upload-start");
    out.took.second = second !== null;
    await reached("second-put");
    await release("create");
    await settle(first, "the cancelled upload");
    out.shown.during = shown();
    await release("second-put");
    await settle(second, "the second upload");
    out.shown.final = shown();
  },
  "drop-pending": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const pending = folderDrop("Dropped", [new File([input.other], "01.flac", { lastModified: input.lastModified })]);
    const dropped = drop(pending.dataTransfer);
    out.took.drop = dropped !== null;
    const run = click("upload-start");
    await reached("create");
    pending.open();
    await settle(dropped, "the pending drop");
    out.shown.during = shown();
    const now = folderDrop("Also Dropped", [new File([input.other], "02.flac", { lastModified: input.lastModified })]);
    now.open();
    await settle(drop(now.dataTransfer), "the drop made while the upload ran");
    out.shown.droppedNow = shown();
    await release("create");
    await settle(run, "the upload");
    out.shown.final = shown();
  },
  "drop-navigate": async () => {
    newPage();
    await ready();
    const pending = folderDrop("Dropped", [new File([input.other], "01.flac", { lastModified: input.lastModified })]);
    const dropped = drop(pending.dataTransfer);
    navigate();
    await ready();
    pending.open();
    await settle(dropped, "the pending drop");
    out.shown.after = shown();
    await settle(pick([oldFile()]), "the pick");
    await settle(click("upload-start"), "the upload");
    out.shown.final = shown();
  },
  "navigate-failure": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const run = click("upload-start");
    await reached("put");
    navigate();
    await ready();
    out.shown.fresh = shown();
    // The new page's own upload is under way when the left one's chunk fails.
    await settle(pick([fileAt(input.otherPath, input.other)]), "the new page's pick");
    const second = click("upload-start");
    out.took.second = second !== null;
    await reached("second-put");
    await release("put");
    await settle(run, "the upload a navigation left");
    out.shown.after = shown();
    await release("second-put");
    await settle(second, "the new page's upload");
    out.shown.final = shown();
  },
  "navigate-commit": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const run = click("upload-start");
    await reached("commit");
    navigate();
    await ready();
    await settle(pick([fileAt(input.otherPath, input.other)]), "the new page's pick");
    out.shown.staged = shown();
    await release("commit");
    await settle(run, "the upload a navigation left");
    out.shown.after = shown();
    out.awaitingScan = uploadAwaitingScan;
    await settle(click("upload-start"), "the new page's upload");
    out.shown.final = shown();
  },
  "resume-retry": async () => {
    newPage();
    await ready();
    out.shown.loaded = shown();
    await settle(pick([oldFile()]), "the pick");
    await settle(click("upload-start"), "the resume");
    out.shown.refused = shown();
    await back();
    const retry = click("upload-retry-button");
    await reached("retry-read");
    out.shown.during = shown();
    const repick = pick([oldFile()]);
    out.took.repick = repick !== null;
    await settle(repick, "the second pick");
    const reupload = click("upload-start");
    out.took.reupload = reupload !== null;
    await release("retry-read");
    await settle(retry, "Try again");
    await settle(reupload, "the second resume");
    out.shown.final = shown();
  },
  "ordinary": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const run = click("upload-start");
    await reached("put");
    out.shown.during = shown();
    await release("put");
    await settle(run, "the upload");
    out.shown.final = shown();
  },
  "retry": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    await settle(click("upload-start"), "the upload");
    out.shown.refused = shown();
    await back();
    await settle(click("upload-retry-button"), "Try again");
    out.shown.final = shown();
  },
  "resume": async () => {
    newPage();
    await ready();
    out.shown.loaded = shown();
    await settle(pick([oldFile()]), "the pick");
    await settle(click("upload-start"), "the resume");
    out.shown.final = shown();
  },
  "stop-transfer": async () => {
    newPage();
    await ready();
    await settle(pick([oldFile()]), "the pick");
    const run = click("upload-start");
    await reached("put");
    const stop = click("upload-abort");
    out.took.stop = stop !== null;
    await settle(stop, "Stop");
    await release("put");
    await settle(run, "the upload");
    out.shown.final = shown();
  },
};
try {
  await steps[input.case]();
} catch (e) {
  out.error = String(e && e.stack ? e.stack : e);
}
if (out.awaitingScan === undefined) out.awaitingScan = uploadAwaitingScan;
process.stdout.write(JSON.stringify(out) + "\n", () => process.exit(0));
`

// jsTopBindingRe is a top-level let, const or var of a script.
var jsTopBindingRe = regexp.MustCompile(`(?m)^(?:let|const|var) ([A-Za-z_$][A-Za-z0-9_$]*)`)

// jsBareCallRe is a call of a lowercase name, wherever it stands: unlike
// jsCallRe it consumes nothing before the name, so a call written directly
// inside another call's parentheses (stageFiles(collectFromInput(…))) is
// found too.
var jsBareCallRe = regexp.MustCompile(`[a-z][A-Za-z0-9_$]*\s*\(`)

// requireCarried fails the test, before node is looked for, where node would
// answer the script with a ReferenceError rather than the defect's red: the
// functions it takes from app.js read a top-level binding of app.js that its
// own part (harness) does not declare, or it calls a name it neither declares
// nor has as a global (a function withReachedFunctions did not carry, whose
// call scan misses a call inside another call's parentheses).
func requireCarried(t *testing.T, harness, functions, src string) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range jsTopBindingRe.FindAllStringSubmatch(harness, -1) {
		declared[m[1]] = true
	}
	code := stripJSComments(functions)
	var missing []string
	for _, m := range jsTopBindingRe.FindAllStringSubmatch(src, -1) {
		if name := m[1]; !declared[name] && regexp.MustCompile(`(^|[^.\w$])`+regexp.QuoteMeta(name)+`\b`).MatchString(code) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the upload page's functions read app.js's %v, which the harness does not declare: declare them beside uploadState", missing)
	}
	body := stripJSNoise(harness + "\n" + functions)
	decls := jsDeclarations(body)
	uncarried := map[string]bool{}
	for _, loc := range jsBareCallRe.FindAllStringIndex(body, -1) {
		if loc[0] > 0 && regexp.MustCompile(`[.\w$]`).MatchString(body[loc[0]-1:loc[0]]) {
			continue
		}
		name := strings.TrimRight(body[loc[0]:loc[1]-1], " \t\n")
		if !decls[name] && !jsCallKeywords[name] && !jsKnownGlobals[name] && name != "btoa" && name != "import" {
			uncarried[name] = true
		}
	}
	if len(uncarried) > 0 {
		t.Fatalf("the upload page's script calls %v, which it neither declares nor carries from app.js: name them as entries of withReachedFunctions", uncarried)
	}
}

// ownershipScript is ownershipRun after app.js's API, the upload page's
// constants and every function of app.js the page's wiring reaches. The
// pickers' collectFromInput is named: it is called inside stageFiles' own
// parentheses, which withReachedFunctions' scan does not see. What the script
// carries is checked (requireCarried) before node is looked for.
func ownershipScript(t *testing.T) (script, node string) {
	t.Helper()
	src := readConsoleJS(t, "static/app.js")
	harness := extractJSConst(t, src, "API")
	for _, c := range []string{"const BYTE_UNITS = ", "const UPLOAD_PARALLEL_FILES = ", "const UPLOAD_DIGEST_RETRIES = ", "const UPLOAD_JUNK_NAMES = "} {
		harness += jsStatement(t, src, c)
	}
	harness += ownershipRun
	script = withReachedFunctions(t, harness, src, "wireUpload", "stageFiles", "collectFromInput",
		"startUpload", "retryUpload", "discardResumable", "resetUpload", "errorFromResponse")
	requireCarried(t, harness, script[len(harness):], src)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	return script, node
}

// runOwnershipCase runs the script's case under node against rig, bounded,
// and returns what the page reported.
func runOwnershipCase(t *testing.T, rig *ownershipRig, script, node, name string) ownershipOut {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "case.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(map[string]any{
		"base": rig.ts.URL, "case": name, "busyWords": uploadBusyWords(readFile(t, "templates/upload.html")),
		"path": ownershipPath, "old": ownershipOld, "new": ownershipNew,
		"otherPath": ownershipOtherPath, "other": ownershipOther, "lastModified": ownershipModified,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(dir, "input.json")
	if err := os.WriteFile(inputPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw, err := exec.CommandContext(ctx, node, path, inputPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got ownershipOut
	// The last line: a node that warns writes before it.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	srvBgScansWait(t, rig.srv)
	rig.mu.Lock()
	faults := rig.faults
	rig.mu.Unlock()
	if len(faults) > 0 {
		t.Fatalf("the volume could not be taken away or put back: %q", faults)
	}
	if got.Error != "" {
		t.Fatalf("the page's run stopped at a step: %s\nwhat it showed: %+v\nwhat it took: %v\nrequests: %v", got.Error, got.Shown, got.Took, rig.requests())
	}
	return got
}

// The refusal that stops OLD's first upload after its first chunk: the volume
// goes as the second chunk arrives.
var ownershipSecondChunkAway = ownershipRule{Kind: "put", Nth: 2, Away: true}

// TestARetrySendsOnlyTheFilesItBeganWith: an upload of OLD is refused after
// its first chunk, its volume away; once the volume is back, Try again is
// pressed, and while one of its requests is held (its session's read, its
// second chunk, or the re-send of a chunk the server refused as corrupt) a
// batch with OLD's path and size and other bytes is picked and Upload pressed.
// The pick and the Upload are refused, the page saying why, and the retry
// sends its session only OLD's bytes, each missing chunk once, and commits
// OLD. Before B797 the pick was staged and its bytes were what the retry sent
// after a held read: a splice of two files under one session, which no chunk
// digest sees and no whole-file digest is declared to catch.
func TestARetrySendsOnlyTheFilesItBeganWith(t *testing.T) {
	script, node := ownershipScript(t)
	words := uploadBusyWords(readFile(t, "templates/upload.html"))
	for _, tc := range []struct {
		name, script string
		rules        []ownershipRule
		puts         []ownershipPut
	}{
		{"the retry's read of its session held", "splice-read",
			[]ownershipRule{ownershipSecondChunkAway, {Kind: "read", Nth: 1, Hold: "retry-read"}},
			[]ownershipPut{{0, 200}, {8, 409}, {8, 200}, {16, 200}, {24, 200}}},
		{"the retry's second chunk held, after its first", "splice-put",
			[]ownershipRule{ownershipSecondChunkAway, {Kind: "put", Nth: 4, Hold: "retry-put"}},
			[]ownershipPut{{0, 200}, {8, 409}, {8, 200}, {16, 200}, {24, 200}}},
		{"the re-send of a chunk refused as corrupt held", "splice-digest",
			[]ownershipRule{ownershipSecondChunkAway,
				{Kind: "put", Nth: 3, Answer: http.StatusBadRequest, AnswerBody: `{"error":"digest_mismatch","message":"the chunk did not match its digest"}`},
				{Kind: "put", Nth: 4, Hold: "digest-resend"}},
			[]ownershipPut{{0, 200}, {8, 409}, {8, 400}, {8, 200}, {16, 200}, {24, 200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOwnershipRig(t, tc.rules...)
			got := runOwnershipCase(t, rig, script, node, tc.script)

			if refused := got.Shown["refused"]; !strings.Contains(refused.Error, notMountedWords) || !refused.RetryShown {
				t.Fatalf("the refused upload shows %+v, want the refusal and Try again", refused)
			}
			// OLD's session is the first the page made: what it was sent comes
			// first, since a splice is what this test is for.
			made := rig.sessionsMade()
			if len(made) == 0 {
				t.Fatalf("the page made no session:\n%v", rig.requests())
			}
			rig.requirePuts(t, made[0], ownershipOld, tc.puts)
			rig.requireFile(t, ownershipPath, ownershipOld)

			during := got.Shown["during"]
			requireBusy(t, "while the retry's request was held", during, words)
			if !during.StopClickable {
				t.Errorf("while the retry's request was held Stop could not be pressed: %+v", during)
			}
			if got.Took["repick"] || got.Took["reupload"] {
				t.Errorf("while the retry ran, the pick was taken (%v) and Upload was taken (%v): a second upload ran beside it", got.Took["repick"], got.Took["reupload"])
			}
			requireAdded(t, "after the retry", got.Shown["final"], "1 added")
			requireFree(t, "after the retry", got.Shown["final"])
			if len(made) != 1 {
				t.Errorf("the page made the sessions %q, want OLD's alone:\n%v", made, rig.requests())
			}
			if all, own := len(rig.of("put", "")), len(rig.of("put", made[0])); all != own {
				t.Errorf("the page sent %d chunks to sessions other than OLD's", all-own)
			}
			if commits := rig.of("commit", ""); len(commits) != 1 || commits[0].Session != made[0] || commits[0].Status != http.StatusOK {
				t.Errorf("the page committed %v, want OLD's session once", commits)
			}
		})
	}
}

// TestStopAndCancelEndARunWhereverItIs: Stop is offered from Try again's start
// and ends it during its session's read, sending nothing; the session stays
// resumable, the page says so and lists it, and choosing the same files again
// carries it on. Cancel while an upload prepares (its session's create held)
// ends it there: the page is clear at once for a new upload, which runs and
// reports alone, and the cancelled upload, its create answered (after the new
// upload, or while its chunk is held), sends nothing and says nothing, its
// session left on the server as an interrupted upload.
func TestStopAndCancelEndARunWhereverItIs(t *testing.T) {
	script, node := ownershipScript(t)
	words := uploadBusyWords(readFile(t, "templates/upload.html"))

	t.Run("Stop during the retry's read of its session", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipSecondChunkAway, ownershipRule{Kind: "read", Nth: 1, Hold: "retry-read"})
		got := runOwnershipCase(t, rig, script, node, "stop-read")
		if during := got.Shown["during"]; !during.StopClickable || !got.Took["stop"] {
			t.Fatalf("Stop could not be pressed while Try again read its session: %+v", during)
		}
		stopped := got.Shown["stopped"]
		if !strings.Contains(stopped.Result, "Upload stopped") || stopped.Error != "" || stopped.ProgressShown || stopped.RetryShown {
			t.Errorf("after Stop the page shows %+v, want the stop said and nothing else", stopped)
		}
		requireFree(t, "after Stop", stopped)
		if !strings.Contains(got.Banner, "An upload was interrupted") {
			t.Errorf("the banner says %q, want the stopped upload listed", got.Banner)
		}
		requireAdded(t, "after the same files were chosen again", got.Shown["final"], "1 added")

		made := rig.sessionsMade()
		if len(made) != 1 {
			t.Fatalf("the page made the sessions %q, want one, carried on after the stop:\n%v", made, rig.requests())
		}
		reqs := rig.requests()
		var reads []int
		for i, r := range reqs {
			if r.Kind == "read" {
				reads = append(reads, i)
			}
		}
		if len(reads) != 2 {
			t.Fatalf("the page read its session %d times, want the retry's read and the resume's:\n%v", len(reads), reqs)
		}
		for _, r := range reqs[reads[0]:reads[1]] {
			if r.Kind == "put" || r.Kind == "commit" {
				t.Errorf("the stopped retry sent %s %s: Stop sends nothing more", r.Method, r.Path)
			}
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 409}, {8, 200}, {16, 200}, {24, 200}})
		if commits := rig.of("commit", made[0]); len(commits) != 1 || commits[0].Status != http.StatusOK {
			t.Errorf("the session was committed %v, want once, by the resume", commits)
		}
		rig.requireFile(t, ownershipPath, ownershipOld)
	})

	t.Run("Cancel while an upload prepares", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "create", Nth: 1, Hold: "create"})
		got := runOwnershipCase(t, rig, script, node, "cancel-prepare")
		during := got.Shown["during"]
		requireBusy(t, "while the upload prepared", during, words)
		if !during.ReviewShown || !during.CancelClickable || !got.Took["cancel"] {
			t.Fatalf("Cancel could not be pressed while the upload prepared: %+v", during)
		}
		cancelled := got.Shown["cancelled"]
		if cancelled.ReviewShown || cancelled.ProgressShown || cancelled.Error != "" {
			t.Errorf("after Cancel the page shows %+v, want it clear", cancelled)
		}
		requireFree(t, "after Cancel", cancelled)
		if !got.Took["repick"] || !got.Took["second"] {
			t.Fatalf("after Cancel the page refused a new upload (pick %v, Upload %v)", got.Took["repick"], got.Took["second"])
		}
		requireAdded(t, "after the new upload", got.Shown["second"], "1 added")
		if final := got.Shown["final"]; final.Result != got.Shown["second"].Result || final.Error != "" || final.ProgressShown || final.RetryShown || final.ReviewShown {
			t.Errorf("once the cancelled upload's create answered, the page shows %+v, want the new upload's report alone, as before (%q)", final, got.Shown["second"].Result)
		}

		// In the order asked: the held create first, answered after the new
		// upload's.
		made := rig.sessionsMade()
		if len(made) != 2 {
			t.Fatalf("the page made the sessions %q, want the cancelled upload's and the new one's:\n%v", made, rig.requests())
		}
		cancelledSID, newSID := made[0], made[1]
		if puts, commits := rig.of("put", cancelledSID), rig.of("commit", cancelledSID); len(puts) != 0 || len(commits) != 0 {
			t.Errorf("the cancelled upload sent %d chunks and %d commits to its session", len(puts), len(commits))
		}
		rig.requireListed(t, cancelledSID)
		rig.requirePuts(t, newSID, ownershipOther, []ownershipPut{{0, 200}, {8, 200}, {16, 200}})
		rig.requireFile(t, ownershipOtherPath, ownershipOther)
		rig.requireFile(t, ownershipPath, "")
	})

	t.Run("Cancel, the cancelled upload's create answering while the next upload runs", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "create", Nth: 1, Hold: "create"}, ownershipRule{Kind: "put", Nth: 1, Hold: "second-put"})
		got := runOwnershipCase(t, rig, script, node, "cancel-overlap")
		if !got.Took["second"] {
			t.Fatal("after Cancel the page refused a new upload")
		}
		// The next upload owns the page: the cancelled one's end says
		// nothing there and gives back nothing it does not own.
		during := got.Shown["during"]
		if !during.ProgressShown || during.Error != "" || during.RetryShown || during.Result != "" {
			t.Errorf("once the cancelled upload's create answered, the page shows %+v, want the running upload's progress and nothing else", during)
		}
		requireBusy(t, "once the cancelled upload's create answered", during, words)
		requireAdded(t, "after the next upload", got.Shown["final"], "1 added")
		made := rig.sessionsMade()
		if len(made) != 2 {
			t.Fatalf("the page made the sessions %q, want the cancelled upload's and the next one's:\n%v", made, rig.requests())
		}
		if puts := rig.of("put", made[0]); len(puts) != 0 {
			t.Errorf("the cancelled upload sent %d chunks", len(puts))
		}
		rig.requirePuts(t, made[1], ownershipOther, []ownershipPut{{0, 200}, {8, 200}, {16, 200}})
		rig.requireFile(t, ownershipOtherPath, ownershipOther)
	})
}

// TestNoContinuationTouchesAPageItNoLongerOwns: a folder whose reading was
// still under way when an upload began, or when a navigation replaced the
// page, is not staged over either; a drop while an upload runs is refused at
// once; and an upload a navigation left (its chunk held, then failing while
// the new page's own upload runs; or its commit held, then answered over the
// new page's staged pick) sends nothing more and touches nothing on the page
// shown since, whose own upload runs and reports alone. The commit already
// asked for lands.
func TestNoContinuationTouchesAPageItNoLongerOwns(t *testing.T) {
	script, node := ownershipScript(t)
	words := uploadBusyWords(readFile(t, "templates/upload.html"))

	t.Run("a folder still being read as an upload begins", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "create", Nth: 1, Hold: "create"})
		got := runOwnershipCase(t, rig, script, node, "drop-pending")
		if !got.Took["drop"] {
			t.Fatal("the drop target took no drop before the upload began")
		}
		for _, at := range []string{"during", "droppedNow"} {
			s := got.Shown[at]
			requireBusy(t, at, s, words)
			if len(s.Preview) != 1 || s.Preview[0] != ownershipPath {
				t.Errorf("%s: the review lists %q, want the upload's own %q: a drop was staged over the upload", at, s.Preview, ownershipPath)
			}
		}
		requireAdded(t, "after the upload", got.Shown["final"], "1 added")
		made := rig.sessionsMade()
		if len(made) != 1 {
			t.Fatalf("the page made the sessions %q, want one:\n%v", made, rig.requests())
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 200}, {16, 200}, {24, 200}})
		rig.requireFile(t, ownershipPath, ownershipOld)
		rig.requireFile(t, "Dropped/01.flac", "")
		rig.requireFile(t, "Also Dropped/02.flac", "")
	})

	t.Run("a folder still being read as a navigation replaces the page", func(t *testing.T) {
		rig := newOwnershipRig(t)
		got := runOwnershipCase(t, rig, script, node, "drop-navigate")
		if after := got.Shown["after"]; after.ReviewShown || len(after.Preview) != 0 || after.Error != "" || after.Result != "" {
			t.Errorf("once the folder dropped on the page before was read, the page shown since shows %+v, want nothing staged", after)
		}
		requireAdded(t, "after the new page's upload", got.Shown["final"], "1 added")
		if made := rig.sessionsMade(); len(made) != 1 {
			t.Errorf("the page made the sessions %q, want the new page's one", made)
		}
		rig.requireFile(t, ownershipPath, ownershipOld)
		rig.requireFile(t, "Dropped/01.flac", "")
	})

	t.Run("an upload whose chunk fails after a navigation, the new page's upload under way", func(t *testing.T) {
		rig := newOwnershipRig(t,
			ownershipRule{Kind: "put", Nth: 2, Hold: "put", Answer: http.StatusInternalServerError,
				AnswerBody: `{"error":"test_failure","message":"a failure the test made"}`},
			ownershipRule{Kind: "put", Nth: 3, Hold: "second-put"})
		got := runOwnershipCase(t, rig, script, node, "navigate-failure")
		fresh := got.Shown["fresh"]
		if fresh.ReviewShown || fresh.ProgressShown || fresh.Error != "" || fresh.RetryShown || fresh.Result != "" {
			t.Errorf("the page shown since shows %+v, want nothing of the upload a navigation left", fresh)
		}
		requireFree(t, "the page shown since", fresh)
		if !got.Took["second"] {
			t.Fatal("the new page refused its own upload")
		}
		// The new page's upload owns it: the left one's failure says nothing
		// there and gives back nothing it does not own.
		after := got.Shown["after"]
		if !after.ProgressShown || after.Error != "" || after.RetryShown || after.Result != "" {
			t.Errorf("once the left upload's chunk failed, the new page shows %+v, want its own upload's progress and nothing else", after)
		}
		requireBusy(t, "once the left upload's chunk failed", after, words)
		requireAdded(t, "after the new page's upload", got.Shown["final"], "1 added")
		made := rig.sessionsMade()
		if len(made) != 2 {
			t.Fatalf("the page made the sessions %q, want the left upload's and the new page's:\n%v", made, rig.requests())
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 500}})
		if commits := rig.of("commit", made[0]); len(commits) != 0 {
			t.Errorf("the upload a navigation left committed %v", commits)
		}
		rig.requireListed(t, made[0])
		rig.requirePuts(t, made[1], ownershipOther, []ownershipPut{{0, 200}, {8, 200}, {16, 200}})
		rig.requireFile(t, ownershipOtherPath, ownershipOther)
	})

	t.Run("an upload whose commit answers after a navigation", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "commit", Nth: 1, Hold: "commit"})
		got := runOwnershipCase(t, rig, script, node, "navigate-commit")
		staged, after := got.Shown["staged"], got.Shown["after"]
		if !staged.ReviewShown || len(staged.Preview) != 1 || staged.Preview[0] != ownershipOtherPath {
			t.Fatalf("the new page staged %+v, want %q", staged, ownershipOtherPath)
		}
		if !after.ReviewShown || len(after.Preview) != 1 || after.Preview[0] != ownershipOtherPath || after.Result != "" || after.Error != "" {
			t.Errorf("once the left upload's commit answered, the new page shows %+v, want its own pick staged and nothing else", after)
		}
		if got.AwaitingScan {
			t.Error("the left upload's commit set the scan note of the page shown since")
		}
		requireAdded(t, "after the new page's upload", got.Shown["final"], "1 added")
		if made := rig.sessionsMade(); len(made) != 2 {
			t.Errorf("the page made the sessions %q, want two", made)
		}
		// The commit was asked for before the navigation: it lands.
		rig.requireFile(t, ownershipPath, ownershipOld)
		rig.requireFile(t, ownershipOtherPath, ownershipOther)
	})
}

// TestTheResumePathIsRefusedWhileARetryRuns: an interrupted upload of OLD is
// carried on (choosing OLD again), refused at its commit with its volume away;
// once the volume is back Try again is pressed, and while it reads its session
// OLD is chosen again and Upload pressed, the resume path of the same session.
// Both are refused, and the session is read twice (the resume, the retry),
// sent each missing chunk once and committed once more.
func TestTheResumePathIsRefusedWhileARetryRuns(t *testing.T) {
	script, node := ownershipScript(t)
	words := uploadBusyWords(readFile(t, "templates/upload.html"))
	rig := newOwnershipRig(t, ownershipRule{Kind: "commit", Nth: 1, Away: true}, ownershipRule{Kind: "read", Nth: 2, Hold: "retry-read"})
	sid := rig.interruptedSession(t)
	got := runOwnershipCase(t, rig, script, node, "resume-retry")
	if !got.Shown["loaded"].BannerShown {
		t.Fatalf("the page does not list the interrupted upload: %+v", got.Shown["loaded"])
	}
	if refused := got.Shown["refused"]; !strings.Contains(refused.Error, notMountedWords) || !refused.RetryShown {
		t.Fatalf("the refused resume shows %+v, want the refusal and Try again", refused)
	}
	requireBusy(t, "while the retry read its session", got.Shown["during"], words)
	if got.Took["repick"] || got.Took["reupload"] {
		t.Errorf("while the retry ran, the pick was taken (%v) and Upload was taken (%v)", got.Took["repick"], got.Took["reupload"])
	}
	requireAdded(t, "after the retry", got.Shown["final"], "1 added")
	if made := rig.sessionsMade(); len(made) != 0 {
		t.Errorf("the page made the sessions %q, want none: it carries on the interrupted one", made)
	}
	if reads := rig.of("read", sid); len(reads) != 2 {
		t.Errorf("the page read the session %d times, want the resume's read and the retry's", len(reads))
	}
	rig.requirePuts(t, sid, ownershipOld, []ownershipPut{{8, 200}, {16, 200}, {24, 200}})
	var codes []int
	for _, c := range rig.of("commit", sid) {
		codes = append(codes, c.Status)
	}
	if len(codes) != 2 || codes[0] != http.StatusConflict || codes[1] != http.StatusOK {
		t.Errorf("the session's commits answered %v, want the refusal and then one commit", codes)
	}
	rig.requireFile(t, ownershipPath, ownershipOld)
}

// TestAnOrdinaryUploadRetryResumeAndStopAreUnchanged: what B797 must leave as
// it was. An upload sends its four chunks and commits; a retry after a refused
// chunk sends what is missing and commits; an interrupted upload chosen again
// is carried on from its offset; and Stop during a transfer lets the chunk out
// finish, sends nothing more and commits nothing.
func TestAnOrdinaryUploadRetryResumeAndStopAreUnchanged(t *testing.T) {
	script, node := ownershipScript(t)

	t.Run("an upload", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "put", Nth: 2, Hold: "put"})
		got := runOwnershipCase(t, rig, script, node, "ordinary")
		if during := got.Shown["during"]; !during.ProgressShown || !during.StopClickable || during.ReviewShown {
			t.Errorf("during the transfer the page shows %+v, want its progress and Stop", during)
		}
		requireAdded(t, "after the upload", got.Shown["final"], "1 added")
		made := rig.sessionsMade()
		if len(made) != 1 {
			t.Fatalf("the page made the sessions %q, want one", made)
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 200}, {16, 200}, {24, 200}})
		if commits := rig.of("commit", made[0]); len(commits) != 1 {
			t.Errorf("the page committed %d times, want once", len(commits))
		}
		rig.requireFile(t, ownershipPath, ownershipOld)
	})

	t.Run("a retry", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipSecondChunkAway)
		got := runOwnershipCase(t, rig, script, node, "retry")
		if refused := got.Shown["refused"]; !strings.Contains(refused.Error, notMountedWords) || !refused.RetryShown {
			t.Fatalf("the refused upload shows %+v, want the refusal and Try again", refused)
		}
		requireAdded(t, "after Try again", got.Shown["final"], "1 added")
		made := rig.sessionsMade()
		if len(made) != 1 {
			t.Fatalf("the page made the sessions %q, want one", made)
		}
		if reads := rig.of("read", made[0]); len(reads) != 1 {
			t.Errorf("Try again read its session %d times, want once", len(reads))
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 409}, {8, 200}, {16, 200}, {24, 200}})
		rig.requireFile(t, ownershipPath, ownershipOld)
	})

	t.Run("a resume", func(t *testing.T) {
		rig := newOwnershipRig(t)
		sid := rig.interruptedSession(t)
		got := runOwnershipCase(t, rig, script, node, "resume")
		if !got.Shown["loaded"].BannerShown {
			t.Fatalf("the page does not list the interrupted upload: %+v", got.Shown["loaded"])
		}
		requireAdded(t, "after the resume", got.Shown["final"], "1 added")
		if made := rig.sessionsMade(); len(made) != 0 {
			t.Errorf("the page made the sessions %q, want none", made)
		}
		if reads := rig.of("read", sid); len(reads) != 1 {
			t.Errorf("the resume read the session %d times, want once", len(reads))
		}
		rig.requirePuts(t, sid, ownershipOld, []ownershipPut{{8, 200}, {16, 200}, {24, 200}})
		rig.requireFile(t, ownershipPath, ownershipOld)
	})

	t.Run("Stop during a transfer", func(t *testing.T) {
		rig := newOwnershipRig(t, ownershipRule{Kind: "put", Nth: 2, Hold: "put"})
		got := runOwnershipCase(t, rig, script, node, "stop-transfer")
		if !got.Took["stop"] {
			t.Fatal("Stop could not be pressed during the transfer")
		}
		if final := got.Shown["final"]; !strings.Contains(final.Result, "Upload stopped") || final.Error != "" || final.ProgressShown {
			t.Errorf("after Stop the page shows %+v, want the stop said", final)
		}
		made := rig.sessionsMade()
		if len(made) != 1 {
			t.Fatalf("the page made the sessions %q, want one", made)
		}
		rig.requirePuts(t, made[0], ownershipOld, []ownershipPut{{0, 200}, {8, 200}})
		if commits := rig.of("commit", ""); len(commits) != 0 {
			t.Errorf("the stopped upload committed %v", commits)
		}
		rig.requireFile(t, ownershipPath, "")
	})
}
