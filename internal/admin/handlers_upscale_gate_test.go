package admin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The live upscale gate on POST /api/upscale/batch, and the delete that
// deliberately goes without one.
//
// cmd/bridge constructs the upscale pool and its coordinator whatever
// `upscale.enabled` says (PR #781), so Deps.BatchCoordinator is never nil
// in production and a nil check on it gates nothing. Until 2026-09-28 that
// nil check was the whole of the admin submit's gate: with the feature off,
// the default, any loopback process or public-mode session could start a
// library-wide sox run. These tests pin the live gate that replaced it, its
// place ahead of the scope, and the optimize kind's own switch.

// submitCounting posts one batch body and returns the status, the error code
// of a refusal ("" for a success), and how many Submit* calls reached the
// stub, which it resets first.
func submitCounting(t *testing.T, srv *Server, stub *fakeBatchCoordinator, body string) (status int, errCode string, calls int) {
	t.Helper()
	stub.submitCalls, stub.submitOptimizeCalls, stub.submitPCMCalls = nil, nil, nil
	w := postBatch(t, srv, body)
	if w.Code >= http.StatusBadRequest {
		var envelope struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("refusal %d is not the JSON error envelope: %v: %s", w.Code, err, w.Body.String())
		}
		errCode = envelope.Error
	}
	return w.Code, errCode, len(stub.submitCalls) + len(stub.submitOptimizeCalls) + len(stub.submitPCMCalls)
}

// TestBatchSubmitRefusesBeforeResolvingTheScope pins the gate and its place:
// ahead of the body, the kind and the scope.
//
// Every case runs twice against one server. With the gate ON it gets the
// answer its body earns; with the gate OFF it must get the gate's 503 and
// reach the coordinator zero times. The ON answers are what make the order
// visible. A traversal earns 400 and an unknown album 404, so a gate that
// ran after the scope would answer those instead of 503: "refused either
// way" cannot show the order, which refusal comes back can.
//
// The folder form never stats its path (the coordinator walks it), so a
// folder that does not exist earns 202 with the gate on, not 404. The
// unknown album and the traversal are what prove the order; the missing
// folder and the whole library are here for the property the defect broke,
// that a refused request starts no work.
func TestBatchSubmitRefusesBeforeResolvingTheScope(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	on := true
	srv.deps.UpscaleActive = func() bool { return on }
	soID := albumIDByTitle(t, srv, "So")

	cases := []struct {
		name   string
		body   string
		wantOn int
	}{
		{"folder that does not exist", `{"path":"No Such Artist/No Such Album"}`, http.StatusAccepted},
		{"whole library", `{"path":""}`, http.StatusAccepted},
		{"traversal", `{"path":"../etc"}`, http.StatusBadRequest},
		{"album by id", `{"albumIds":["` + soID + `"]}`, http.StatusAccepted},
		{"album id nothing has", `{"albumIds":["0123456789abcdef"]}`, http.StatusNotFound},
		{"optimize kind", `{"path":"Music","kind":"optimize"}`, http.StatusAccepted},
		{"unknown kind", `{"path":"Music","kind":"junk"}`, http.StatusBadRequest},
		{"body that is not JSON", `{"path":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			on = true
			if code, _, _ := submitCounting(t, srv, stub, tc.body); code != tc.wantOn {
				t.Fatalf("gate on: status %d, want %d, so this case no longer shows "+
					"what the gate is ordered against", code, tc.wantOn)
			}
			on = false
			code, errCode, calls := submitCounting(t, srv, stub, tc.body)
			if code != http.StatusServiceUnavailable || errCode != errCodeUpscaleDisabled {
				t.Errorf("gate off: %d %q, want 503 %q: the gate must answer before "+
					"the body, the kind or the scope is read", code, errCode, errCodeUpscaleDisabled)
			}
			if calls != 0 {
				t.Errorf("gate off: %d Submit calls reached the coordinator, want 0", calls)
			}
		})
	}
}

// TestBatchSubmitReadsANilUpscaleGateAsOff: a Deps built without the gate
// describes a bridge whose upscale state nobody wired, and the safe reading
// of that is off, as /v1's is (TestNilFeatureGatesReadAsOff). The refusal
// logs /v1's WARN line once, so a refused console submit is not silent.
func TestBatchSubmitReadsANilUpscaleGateAsOff(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	srv.deps.UpscaleActive = nil
	rec := loggingtest.Record(t)

	code, errCode, calls := submitCounting(t, srv, stub, `{"path":""}`)
	if code != http.StatusServiceUnavailable || errCode != errCodeUpscaleDisabled || calls != 0 {
		t.Errorf("nil gate: %d %q with %d Submit calls, want 503 %q with none",
			code, errCode, calls, errCodeUpscaleDisabled)
	}
	if got := rec.Failures("upscale batch refused: the feature is not active"); len(got) != 1 {
		t.Errorf("one refusal logged %d WARN lines, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
}

// TestBatchSubmitAnswersTheUpscaleGateLive: on, the submit goes through;
// the gate is read per request, so switching it off and on again moves the
// answer with it. That is what makes `upscale.enabled` hot on this route,
// as it is on /v1. A value captured when the server was built would keep
// answering the boot state.
func TestBatchSubmitAnswersTheUpscaleGateLive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	on := true
	srv.deps.UpscaleActive = func() bool { return on }

	for i, turnOn := range []bool{true, false, true} {
		on = turnOn
		wantCode, wantCalls := http.StatusAccepted, 1
		if !turnOn {
			wantCode, wantCalls = http.StatusServiceUnavailable, 0
		}
		code, _, calls := submitCounting(t, srv, stub, `{"path":"Music","targetRate":192000,"targetBits":24}`)
		if code != wantCode || calls != wantCalls {
			t.Errorf("request %d with the gate %v: %d with %d Submit calls, want %d with %d",
				i+1, turnOn, code, calls, wantCode, wantCalls)
		}
	}
}

// TestBatchSubmitOptimizeKindReadsItsOwnGate: with upscaling on and the
// CarPlay switch off, the optimize kind is refused before its scope is
// resolved, and the upscale kind is not. The /v1 batch refused this case
// already; the console accepted it, so one bridge answered 503 there and
// 202 here for the same kind.
//
// The switch is read as the projection endpoint reads it: nil means wired
// is active, the pre-existing meaning of a nil OptimizeActive.
func TestBatchSubmitOptimizeKindReadsItsOwnGate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub
	optimizeOn := true
	srv.deps.OptimizeActive = func() bool { return optimizeOn }

	for _, tc := range []struct {
		name   string
		body   string
		wantOn int
	}{
		{"folder that does not exist", `{"path":"No Such Album","kind":"optimize"}`, http.StatusAccepted},
		{"traversal", `{"path":"../etc","kind":"optimize"}`, http.StatusBadRequest},
		{"album id nothing has", `{"albumIds":["0123456789abcdef"],"kind":"optimize"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			optimizeOn = true
			if code, _, _ := submitCounting(t, srv, stub, tc.body); code != tc.wantOn {
				t.Fatalf("switch on: status %d, want %d, so this case no longer shows "+
					"what the switch is ordered against", code, tc.wantOn)
			}
			optimizeOn = false
			code, errCode, calls := submitCounting(t, srv, stub, tc.body)
			if code != http.StatusServiceUnavailable || errCode != "optimize-disabled" || calls != 0 {
				t.Errorf("switch off: %d %q with %d Submit calls, want 503 %q with none",
					code, errCode, calls, "optimize-disabled")
			}
		})
	}

	t.Run("upscale kind unaffected", func(t *testing.T) {
		optimizeOn = false
		for _, body := range []string{`{"path":"No Such Album"}`, `{"path":"No Such Album","kind":"upscale"}`} {
			code, _, calls := submitCounting(t, srv, stub, body)
			if code != http.StatusAccepted || len(stub.submitCalls) != 1 || calls != 1 {
				t.Errorf("%s with the CarPlay switch off: %d with %d calls, want 202 and one Submit",
					body, code, calls)
			}
		}
	})

	t.Run("nil switch reads as wired", func(t *testing.T) {
		srv.deps.OptimizeActive = nil
		code, _, _ := submitCounting(t, srv, stub, `{"path":"No Such Album","kind":"optimize"}`)
		if code != http.StatusAccepted || len(stub.submitOptimizeCalls) != 1 {
			t.Errorf("nil OptimizeActive: %d with %d SubmitOptimize calls, want 202 and one",
				code, len(stub.submitOptimizeCalls))
		}
	})
}

// TestVariantDeleteStaysOpenWithUpscaleOff pins the repo owner's decision
// (2026-09-28): with upscaling switched off, the console's delete still
// reaches the deleter, so an operator can reclaim the disk renditions use.
// DELETE /v1/upscale/variants refuses in the same state; this route
// deliberately does not, while the batch submit beside it does
// (TestBatchSubmitRefusesBeforeResolvingTheScope).
func TestVariantDeleteStaysOpenWithUpscaleOff(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	soID := albumIDByTitle(t, srv, "So")

	for _, gate := range []struct {
		name string
		fn   func() bool
	}{
		{"gate off", func() bool { return false }},
		{"no gate wired", nil},
	} {
		srv.deps.UpscaleActive = gate.fn
		for _, tc := range []struct {
			name  string
			query string
			ok    func(AdminVariantDeleteRequest) bool
		}{
			{"folder", "prefix=Music/Jazz&kind=optimize", func(r AdminVariantDeleteRequest) bool {
				return r.Prefix == "Music/Jazz" && r.Kind == "optimize"
			}},
			{"album", "albumId=" + soID, func(r AdminVariantDeleteRequest) bool { return len(r.Paths) == 2 }},
			{"every variant", "confirm=true", func(r AdminVariantDeleteRequest) bool { return r.All }},
		} {
			stub := &stubVariantDeleter{}
			srv.deps.VariantDeleter = stub
			w := deleteVariants(t, srv, tc.query)
			if w.Code != http.StatusOK || stub.gotCalls != 1 || !tc.ok(stub.gotReq) {
				t.Errorf("%s, %s: %d with %d deleter calls (%+v), want 200 reaching the "+
					"deleter once; an operator with upscaling off could not reclaim the disk",
					gate.name, tc.name, w.Code, stub.gotCalls, stub.gotReq)
			}
		}
	}
}

// TestVariantDeleteReadsAPlusInAPathLiterally: the delete reads its query
// through safeQuery, so a "+" in a path stays a plus. Through r.URL.Query()
// the first prefix below arrived as "AC DC/A B Album", a different album,
// and the delete answered 200 having reclaimed nothing of the one named.
func TestVariantDeleteReadsAPlusInAPathLiterally(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		query, wantPrefix, wantPath string
	}{
		{"prefix=AC%20DC/A+B%20Album", "AC DC/A+B Album", ""},
		{"prefix=AC+DC/Live&kind=upscale", "AC+DC/Live", ""},
		{"path=AC%20DC/A+B%20Album/01+Intro.flac&kind=optimize", "", "AC DC/A+B Album/01+Intro.flac"},
	} {
		stub := &stubVariantDeleter{}
		srv.deps.VariantDeleter = stub
		w := deleteVariants(t, srv, tc.query)
		if w.Code != http.StatusOK || stub.gotCalls != 1 {
			t.Errorf("%s: %d with %d deleter calls, want 200 and one: %s",
				tc.query, w.Code, stub.gotCalls, w.Body.String())
			continue
		}
		if stub.gotReq.Prefix != tc.wantPrefix || stub.gotReq.Path != tc.wantPath {
			t.Errorf("%s reached the deleter as prefix %q path %q, want prefix %q path %q",
				tc.query, stub.gotReq.Prefix, stub.gotReq.Path, tc.wantPrefix, tc.wantPath)
		}
	}
}

// TestDeleteVariantsClientRoundTripsThroughTheServer runs the SHIPPED
// deleteVariants under node, captures the URL it fetches, and sends exactly
// that URL to the handler.
//
// The two halves moved together, the client from URLSearchParams to
// encodeURIComponent and the handler from r.URL.Query() to safeQuery, and a
// test of either half alone passes with the other one wrong. URLSearchParams
// writes a space as "+", which safeQuery keeps as a plus, so a folder with a
// space in its name reached the deleter as a folder that does not exist.
func TestDeleteVariantsClientRoundTripsThroughTheServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t, readFile(t, filepath.Join("static", "player", "api.js")), "deleteVariants")
	// An .mjs, so the extracted `export` and the top-level awaits parse.
	script := fn + `
const calls = [];
globalThis.fetch = async (url, init) => {
  calls.push({ url, method: init && init.method });
  return { ok: true, status: 200, json: async () => ({ deletedCount: 0, deletedPaths: [] }) };
};
const out = {};
await deleteVariants({ path: "AC DC/A+B Album" }, "optimize");
out.folder = calls[calls.length - 1];
await deleteVariants({ albumIds: ["0123456789abcdef", "fedcba9876543210"] }, "upscale");
out.albums = calls[calls.length - 1];
await deleteVariants({ artistId: "00112233445566ff" });
out.artist = calls[calls.length - 1];
const before = calls.length;
try {
  await deleteVariants({ path: "" }, "optimize");
  out.empty = "no error";
} catch (e) {
  out.empty = e.message;
}
out.emptyFetched = calls.length !== before;
console.log(JSON.stringify(out));
`
	path := filepath.Join(t.TempDir(), "delete.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	type fetchCall struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	}
	var got struct {
		Folder       fetchCall `json:"folder"`
		Albums       fetchCall `json:"albums"`
		Artist       fetchCall `json:"artist"`
		Empty        string    `json:"empty"`
		EmptyFetched bool      `json:"emptyFetched"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("client printed %q: %v", raw, err)
	}
	for _, c := range []fetchCall{got.Folder, got.Albums, got.Artist} {
		if !strings.HasPrefix(c.URL, "/api/upscale/variants?") {
			t.Fatalf("the client fetched %q, want /api/upscale/variants?…: %s", c.URL, raw)
		}
	}

	// The folder scope, through the real handler: the path must arrive
	// byte for byte, the space and the plus both.
	srv, _, _ := newTestServer(t)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub
	if got.Folder.Method != http.MethodDelete {
		t.Errorf("client sent %q, want DELETE", got.Folder.Method)
	}
	req := httptest.NewRequest(http.MethodDelete, got.Folder.URL, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || stub.gotCalls != 1 {
		t.Fatalf("the client's URL %q: %d with %d deleter calls: %s",
			got.Folder.URL, w.Code, stub.gotCalls, w.Body.String())
	}
	if stub.gotReq.Prefix != "AC DC/A+B Album" || stub.gotReq.Kind != "optimize" {
		t.Errorf("the client's URL %q reached the deleter as prefix %q kind %q, "+
			"want \"AC DC/A+B Album\" and \"optimize\"", got.Folder.URL, stub.gotReq.Prefix, stub.gotReq.Kind)
	}

	// The identity forms carry ids, which the handler expands against a
	// catalog; what matters here is that the server's own parser reads the
	// client's spelling back, repeated albumId included.
	albums := httptest.NewRequest(http.MethodDelete, got.Albums.URL, nil)
	scope, present := identityParams(safeQuery(albums))
	if !present || !slices.Equal(scope.AlbumIDs, []string{"0123456789abcdef", "fedcba9876543210"}) ||
		safeQuery(albums).Get("kind") != "upscale" {
		t.Errorf("the client's URL %q parsed to %+v (present %v), want both album ids and kind upscale",
			got.Albums.URL, scope, present)
	}
	artist := httptest.NewRequest(http.MethodDelete, got.Artist.URL, nil)
	if scope, present := identityParams(safeQuery(artist)); !present || scope.ArtistID != "00112233445566ff" {
		t.Errorf("the client's URL %q parsed to %+v (present %v), want the artist id",
			got.Artist.URL, scope, present)
	}

	// The empty folder scope is still refused before anything is fetched:
	// an empty prefix is every variant the bridge has.
	if got.Empty != "Refusing to delete every variant from a folder scope." || got.EmptyFetched {
		t.Errorf("empty folder scope: error %q, fetched %v; want the refusal and no request",
			got.Empty, got.EmptyFetched)
	}
}

// batchSubmitter is one function that submits variant work through the batch
// coordinator, with where it first reads each gate and first does work.
// token.NoPos means it never does.
type batchSubmitter struct {
	name         string
	pos          token.Pos
	upscaleGate  token.Pos // first s.upscaleActive() call
	optimizeGate token.Pos // first reference to OptimizeActive
	resolve      token.Pos // first resolveVariantScope call
	submit       token.Pos // first Submit* call on a BatchCoordinator
	submitOpt    token.Pos // first SubmitOptimize* call on a BatchCoordinator
}

// batchSubmitters finds every function in f that calls a Submit* method on a
// `….BatchCoordinator` selector, and records where each reads its gates and
// does its work. Syntactic: a call through a local alias of the coordinator
// is not seen, which is why the handlers spell it through the field.
func batchSubmitters(f *ast.File) []batchSubmitter {
	var out []batchSubmitter
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		b := batchSubmitter{name: fn.Name.Name, pos: fn.Pos()}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				recordBatchCall(&b, x)
			case *ast.SelectorExpr:
				if x.Sel.Name == "OptimizeActive" {
					earliestPos(&b.optimizeGate, x.Pos())
				}
			}
			return true
		})
		if b.submit.IsValid() {
			out = append(out, b)
		}
	}
	return out
}

// recordBatchCall notes one call the sweep cares about: the upscale gate, the
// scope resolution, or a submit through the coordinator.
func recordBatchCall(b *batchSubmitter, call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	switch name := sel.Sel.Name; {
	case name == "upscaleActive":
		earliestPos(&b.upscaleGate, call.Pos())
	case name == "resolveVariantScope":
		earliestPos(&b.resolve, call.Pos())
	case strings.HasPrefix(name, "Submit") && onBatchCoordinator(sel.X):
		earliestPos(&b.submit, call.Pos())
		if strings.HasPrefix(name, "SubmitOptimize") {
			earliestPos(&b.submitOpt, call.Pos())
		}
	}
}

// onBatchCoordinator reports whether x is a selector naming the
// BatchCoordinator field, as in `s.deps.BatchCoordinator`.
func onBatchCoordinator(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "BatchCoordinator"
}

// earliestPos keeps the smaller of *at and p, treating NoPos as unset.
func earliestPos(at *token.Pos, p token.Pos) {
	if !at.IsValid() || p < *at {
		*at = p
	}
}

// problems lists how b fails to read a gate ahead of its work, or nothing.
func (b batchSubmitter) problems() []string {
	var out []string
	if msg := gateAheadOfWork("the live upscale gate (s.upscaleActive())", b.upscaleGate, b.resolve, b.submit); msg != "" {
		out = append(out, msg)
	}
	if b.submitOpt.IsValid() {
		if msg := gateAheadOfWork("the CarPlay switch (OptimizeActive)", b.optimizeGate, b.resolve, b.submitOpt); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

// gateAheadOfWork describes how a gate read at `at` fails to come before the
// scope resolution and the submit, or returns "".
func gateAheadOfWork(gate string, at, resolve, submit token.Pos) string {
	switch {
	case !at.IsValid():
		return "submits without reading " + gate
	case resolve.IsValid() && at > resolve:
		return "reads " + gate + " only after resolving the scope"
	case at > submit:
		return "reads " + gate + " only after submitting"
	}
	return ""
}

// TestEveryBatchSubmitReadsTheUpscaleGateFirst sweeps this package's source
// for every function that submits variant work through the batch coordinator,
// and requires each to read the live upscale gate before it resolves a scope
// or submits, and one that submits the optimize kind to read OptimizeActive
// before either too.
//
// The behavioural tests pin apiUpscaleBatchSubmit; this pins the NEXT
// submitter. A handler added beside it without the gate is the shape of this
// defect, and every earlier pass on the class stopped short of one: #852
// restored two /v1 gates and missed both batches, and the /v1 batch's fix
// missed this one. AST, not a text scan, because this package's commentary
// names what it discusses.
func TestEveryBatchSubmitReadsTheUpscaleGateFirst(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// A name beginning with "." or "_" is not the package's source (the
		// go tool ignores it), and emacs's `.#name.go` lock is one.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, b := range batchSubmitters(f) {
			found++
			for _, p := range b.problems() {
				t.Errorf("%s: %s %s, so a bridge with the feature off can be made "+
					"to start variant work", fset.Position(b.pos), b.name, p)
			}
		}
	}
	// The floor: a sweep that finds no submitter passes whatever the code
	// does, which is how a renamed field would retire it without a word.
	if found == 0 {
		t.Fatal("no function in this package submits through BatchCoordinator; " +
			"the sweep found nothing to check and would pass vacuously")
	}
}

// TestBatchSubmitGateSweepReportsEveryMisorder runs the sweep over synthetic
// source. On a clean tree it reports nothing, so the tree alone cannot show
// that it still reports anything; this shows each shape it exists to catch.
func TestBatchSubmitGateSweepReportsEveryMisorder(t *testing.T) {
	const src = `package admin

func gated(s *Server) {
	if !s.upscaleActive() {
		return
	}
	if s.deps.OptimizeActive != nil && !s.deps.OptimizeActive() {
		return
	}
	scope, _ := s.resolveVariantScope(nil, scopeRequest{})
	s.deps.BatchCoordinator.SubmitOptimize(nil, scope.Prefix)
}

func ungated(s *Server) {
	s.deps.BatchCoordinator.Submit(nil, "", 0, 0)
}

func gateAfterScope(s *Server) {
	scope, _ := s.resolveVariantScope(nil, scopeRequest{})
	if !s.upscaleActive() {
		return
	}
	s.deps.BatchCoordinator.Submit(nil, scope.Prefix, 0, 0)
}

func gateAfterSubmit(s *Server) {
	s.deps.BatchCoordinator.SubmitPaths(nil, "", nil, 0, 0)
	_ = s.upscaleActive()
}

func optimizeUngated(s *Server) {
	if !s.upscaleActive() {
		return
	}
	s.deps.BatchCoordinator.SubmitOptimizePaths(nil, "", nil)
}

func optimizeGateAfterScope(s *Server) {
	if !s.upscaleActive() {
		return
	}
	scope, _ := s.resolveVariantScope(nil, scopeRequest{})
	if !s.deps.OptimizeActive() {
		return
	}
	s.deps.BatchCoordinator.SubmitOptimize(nil, scope.Prefix)
}

func notASubmitter(s *Server) {
	s.deps.BatchCoordinator.Cancel("x")
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, b := range batchSubmitters(f) {
		got[b.name] = b.problems()
	}
	want := map[string]string{
		"gated":                  "",
		"ungated":                "submits without reading the live upscale gate",
		"gateAfterScope":         "reads the live upscale gate (s.upscaleActive()) only after resolving the scope",
		"gateAfterSubmit":        "reads the live upscale gate (s.upscaleActive()) only after submitting",
		"optimizeUngated":        "submits without reading the CarPlay switch",
		"optimizeGateAfterScope": "reads the CarPlay switch (OptimizeActive) only after resolving the scope",
	}
	if len(got) != len(want) {
		t.Errorf("the sweep found %d submitters, want %d: %v", len(got), len(want), got)
	}
	for name, wantProblem := range want {
		problems, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s: not found as a submitter", name)
		case wantProblem == "" && len(problems) != 0:
			t.Errorf("%s: reported %q, want nothing", name, problems)
		case wantProblem != "" && (len(problems) != 1 || !strings.HasPrefix(problems[0], wantProblem)):
			t.Errorf("%s: reported %q, want one problem starting %q", name, problems, wantProblem)
		}
	}
}
