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
	// Refuse at the CALL SITE rather than inside the cleanup. The closure
	// calls `cancel` and reports with `stderr`, and a nil-dereference
	// raised from a cleanup surfaces as a stack trace laid over whatever
	// the test was really failing for — which is the diagnostic problem,
	// not the nil itself. Tolerating a nil stderr is the other wrong
	// answer: it is the ONLY diagnostic either branch below has, so an
	// empty `stderr=` would quietly remove the reason the grace window was
	// worth reporting. Unreachable from the three current callers, which
	// all pass the &safeBuffer{} they also hand to run, and the sweep test
	// means a new boot test arrives through here too. Both are checked, not
	// just the one that was raised. (Gemini, PR #944.)
	if cancel == nil || stderr == nil {
		t.Fatal("drainServeOnCleanup: cancel and a stderr buffer are both required — " +
			"the cleanup calls the one and reports with the other")
	}
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

// drainLoopOnCleanup is drainServeOnCleanup for the in-process loops —
// the sweepers, the ingest loop, the regenerator — which carry no exit
// code and no captured streams, only a done channel their goroutine
// closes. Same reasoning, same reason it is a cleanup rather than a
// tail; see that helper's docblock for both.
//
// These are in some ways the sharper half. A stranded `serve` writes
// into a directory that is merely being removed, whereas these loops
// hold a *manifest.Store and an analyze.Pool that the FIXTURE tears
// down explicitly — `t.Cleanup(store.Close)`, `t.Cleanup(pool.Stop)` —
// so a mid-body t.Fatalf leaves a live loop calling into a closed
// SQLite handle. Two of them had no deferred cancel at all, which meant
// the loop was never even asked to stop.
//
// ⚠️ A `defer` BEATS EVERY t.Cleanup, so a fixture that tears down with
// `defer store.Close()` cannot be drained by anything registered here:
// the Close runs first, by construction, on the failing path. Convert
// such a teardown to a t.Cleanup registered BEFORE the drain — that is
// what makes LIFO put the drain first. The sweep test below checks that
// a drain is registered; it cannot see this, so it is on you.
func drainLoopOnCleanup(t *testing.T, cancel context.CancelFunc, done <-chan struct{}, what string) {
	t.Helper()
	// Refused up front for drainServeOnCleanup's reasons, and `what`
	// with them: it is the whole message on the timeout path.
	if cancel == nil || done == nil || what == "" {
		t.Fatal("drainLoopOnCleanup: cancel, a done channel and a description are all required")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not exit on ctx cancel", what)
		}
	})
}

// TestEveryBackgroundGoroutineDrainsOnCleanup is the sweep behind both
// helpers: what needs pinning is the POPULATION, not the call sites.
//
// It was three sites when written (PR #944, the `serve` boot tests) and
// is thirteen now (PR #945 added the in-process loops). That growth is
// the argument for it. The boot tests were written months apart and the
// surviving shape had reached only the newest; then the loop sweep's
// own first enumeration — five files, listed by hand from a
// `defer cancel()` grep — MISSED auto_optimize_test.go, which has the
// hazard and no `defer cancel()` to grep for. Matching the shape found
// it. Enumerating by hand is what this test exists to stop.
//
// Parsed with go/parser, not grepped, because a text scan is wrong in
// BOTH directions here. Looking for the compliance marker finds PROSE:
// several of these tests name a drain helper in a comment that points at
// its reasoning, so a test mentioning one without calling it would pass
// — the commentary-quotes-the-fixed-string trap. Looking for the old
// `defer cancel()` shape misses the sites that never had one (two
// duplicates-sweeper tests, and the auto-optimize one that uses
// t.Cleanup(cancel)) while flagging tests that hold a ctx and start
// nothing. Parsing is also what keeps this agnostic to line endings on
// the Windows leg, where nothing pins eol and a `\n` literal scan would
// match nothing at all.
//
// The file list comes from runtime.Caller rather than the working
// directory, because one of the tests in scope chdirs mid-run.
//
// It is a SHAPE check and no more, and the limits are worth stating
// plainly, since this docblock is all a later reader has to size their
// trust by. It sees that a drain helper is called — not that the
// goroutine it drains is the one that was launched, and not that the
// fixture's teardown is ORDERED behind it, which is the `defer`
// -beats-t.Cleanup trap drainLoopOnCleanup's docblock describes and no
// AST shape can catch. It matches the `go func(){ defer close(ch) … }()`
// form: a launch factored out into a fixture helper would not be seen,
// which the floor cannot reveal either, since the thirteen that exist
// would still satisfy it. Widen the match in the same change that adds
// such a helper.
func TestEveryBackgroundGoroutineDrainsOnCleanup(t *testing.T) {
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
		checked += auditBackgroundTestsIn(t, fset, path)
	}
	// Thirteen at the time of writing. Adding a drained test only raises
	// this, so the floor never needs bumping — it trips when the count
	// DROPS, which is the scan silently ceasing to match.
	if checked < 13 {
		t.Fatalf("matched %d test(s) starting a drainable goroutine, want at least 13 — "+
			"the scan has stopped matching what it is meant to match", checked)
	}
}

// auditBackgroundTestsIn reports how many tests in one file start a
// drainable goroutine, and errors for each that does so without
// registering a drain. Split out from the test body to keep the nesting
// shallow (SonarCloud go:S3776 on PR #944); the count it returns is what
// feeds the floor.
func auditBackgroundTestsIn(t *testing.T, fset *token.FileSet, path string) int {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	matched := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		if !launchesADrainableGoroutine(fn.Body) {
			continue
		}
		matched++
		if !callsFunc(fn.Body, "drainServeOnCleanup") && !callsFunc(fn.Body, "drainLoopOnCleanup") {
			t.Errorf("%s: %s starts a background goroutine without draining it on cleanup. "+
				"A t.Fatalf in its body returns while that goroutine is still running, and "+
				"the fixture's teardown — t.TempDir removal, store.Close, pool.Stop — then "+
				"runs underneath it. What gets reported is the teardown, not the assertion "+
				"that failed. Call drainServeOnCleanup or drainLoopOnCleanup.",
				fset.Position(fn.Pos()), fn.Name.Name)
		}
	}
	return matched
}

// launchesADrainableGoroutine reports whether body holds a `go`
// statement whose closure signals completion by closing a channel —
// `go func(){ defer close(done); … }()`. That is the shape every
// long-running goroutine in this package's tests uses, and it is what
// makes the goroutine drainable in the first place: the helper waits on
// exactly that channel.
//
// Keyed on the `defer close`, deliberately, not on the callee. The
// callees are eight different functions across the package and the list
// would need editing for a ninth — which is the enumeration this test
// exists to replace, moved one level down.
//
// The `close` is looked for ANYWHERE inside the deferred call, not just
// as `defer close(done)`: `defer func() { close(done); wg.Done() }()`
// is the same signal, and a matcher that missed it would let a test
// bypass this guard in silence, with the floor none the wiser — the
// count would simply stay where it is. (Gemini, PR #945.)
//
// Widening further, to any `close` anywhere in the `go` statement, was
// measured and REJECTED: it adds exactly one test,
// TestWaitForWorkersLetsHealthyWorkersFinish, whose goroutine is a stub
// worker (`time.Sleep` then `close`) with no context and no cancel func
// to hand a drain — the test already waits for it. That is a false
// positive, and it would demand a drain that cannot be written.
func launchesADrainableGoroutine(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		ast.Inspect(g, func(inner ast.Node) bool {
			if found {
				return false
			}
			d, ok := inner.(*ast.DeferStmt)
			if !ok {
				return true
			}
			if callsFunc(d, "close") {
				found = true
			}
			return !found
		})
		return !found
	})
	return found
}

// callsFunc reports whether n contains a call whose callee is the bare
// identifier name. Bare, so it answers for package-level funcs (the two
// drain helpers) and never for a method selector that happens to share
// the name.
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
