package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// drainServeOnCleanup registers the shutdown of a background `serve`
// goroutine as a t.Cleanup — the only place it runs on EVERY exit path.
//
// The shape it replaces was `defer cancel()` at the top plus a
// cancel-and-assert tail at the bottom, which is correct only when the
// body runs to completion. A t.Fatalf anywhere above the tail calls
// runtime.Goexit: the deferred cancel fires and the test returns
// WITHOUT waiting for serve to notice, while t.TempDir's own cleanup —
// registered by the fixture earlier, so running later — removes the
// data dir out from under a store that is still checkpointing. What
// then gets reported is the removal, not the assertion that failed.
//
// Registering this AFTER the fixture's t.TempDir is what makes the wait
// worth anything: cleanups run LIFO, so the drain lands before the
// removal it is ordering itself against.
//
// `exited` must be a channel the serve goroutine CLOSES, never `done`
// itself. A failure path may already have consumed the exit code —
// waitForAdminReady does, to report a serve that died before the admin
// console bound — and a second bare receive on `done` would block out
// the whole grace window and then report a shutdown timeout about a
// process that exited cleanly. The closed channel answers on every
// path; `done` is then read behind a default arm, for the code, when it
// is still there to read.
//
// t.Errorf, never t.Fatalf: FailNow from a cleanup skips the cleanups
// that have not run yet, which here are the very directory removals
// this helper exists to sequence itself against.
func drainServeOnCleanup(t *testing.T, cancel context.CancelFunc, exited <-chan struct{}, done <-chan int, stderr *safeBuffer) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Errorf("serve did not shut down within grace window; stderr=%s", stderr.String())
			return
		}
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
		default: // already read by a failure path that reported it
		}
	})
}

// TestEveryBackgroundServeDrainsOnCleanup is the sweep behind the
// helper: what needs pinning is the POPULATION, not the three call
// sites. The three tests that boot `serve` on a goroutine were written
// months apart, and the shape that survives a failing assertion had
// reached only the newest of them — an enumeration is exactly how the
// other two kept the flake.
//
// Parsed with go/parser, not grepped, because a text scan is wrong in
// BOTH directions here. Looking for the compliance marker finds PROSE:
// two of these tests name drainServeOnCleanup in a comment that points
// at its reasoning, so a test mentioning the helper without calling it
// would pass — the commentary-quotes-the-fixed-string trap. Looking for
// the old `defer cancel()` shape instead flags every in-process loop
// test in this package, where it is the correct shape and there is no
// serve to strand. Parsing is also what keeps this agnostic to line
// endings on the Windows leg, where nothing pins eol and a `\n` literal
// scan would match nothing at all.
//
// The file list comes from runtime.Caller rather than the working
// directory, because one of the tests in scope chdirs mid-run.
//
// It is a SHAPE check and no more, and both limits are worth stating
// plainly, since this docblock is all a later reader has to size their
// trust by. It sees that the helper is called, not that the goroutine
// it drains is the one that was launched. And it matches only a `go`
// statement that calls run DIRECTLY: a boot factored out into a fixture
// helper would not be seen, which the floor cannot reveal either, since
// the three that exist would still satisfy it. Widen the match in the
// same change that adds such a helper.
func TestEveryBackgroundServeDrainsOnCleanup(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0): no source position, cannot locate the package")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "*_test.go"))
	if err != nil {
		t.Fatalf("glob the package's test files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("globbed no *_test.go files — the scan would pass vacuously")
	}

	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if !launchesServeOnAGoroutine(fn.Body) {
				continue
			}
			checked++
			if !callsFunc(fn.Body, "drainServeOnCleanup") {
				t.Errorf("%s: %s boots serve on a goroutine without drainServeOnCleanup. "+
					"A t.Fatalf in its body returns while serve still holds the store, and "+
					"t.TempDir's cleanup then removes the data dir under it — the removal is "+
					"what gets reported, not the assertion that failed.",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
	if checked < 3 {
		t.Fatalf("matched %d test(s) booting serve on a goroutine, want at least 3 — "+
			"the scan has stopped matching what it is meant to match", checked)
	}
}

// launchesServeOnAGoroutine reports whether body starts the CLI's entry
// point on a goroutine, i.e. holds a `go` statement that calls run.
func launchesServeOnAGoroutine(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if callsFunc(g, "run") {
			found = true
		}
		return !found
	})
	return found
}

// callsFunc reports whether n contains a call whose callee is the bare
// identifier name. Bare, so it answers for package-level funcs (run,
// drainServeOnCleanup) and never for a method selector that happens to
// share the name.
func callsFunc(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
