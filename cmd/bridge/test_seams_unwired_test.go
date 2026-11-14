package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoProductionCodeCallsATestOnlySeam: the seams backlog B872 part 2 adds
// in non-test files are a test's alone: the observer the rollback's put-back
// tells of each rename that fails (updater.SetRollbackObserverForTest), the
// stand-in for the read of a snapshot's manifest bridge restore makes before
// its prompt (backup.SetRestorePreflightReadForTest), the doctor's lsof probe
// limit (doctor.SetProbeLimitForTest) and its lsof spawn
// (doctor.SetLsofCommandForTest, doctor.LsofForTest). Each is nil, or
// production's own, until a test sets it. The walk reads every Go file of
// the module the go tool builds, but tests (productionCalls), and lists each
// call of a seam it finds, which must be none. It must see the seams declared
// where they are, so a renamed seam fails it rather than pass it, and the
// rollback's put-back telling the observer of each of its four renames, so
// it reads the code the observer is wired into.
func TestNoProductionCodeCallsATestOnlySeam(t *testing.T) {
	seams := []struct{ name, file string }{
		{"SetRollbackObserverForTest", "internal/updater/rollback_observer.go"},
		{"SetRestorePreflightReadForTest", "internal/backup/seams.go"},
		{"SetProbeLimitForTest", "internal/doctor/doctor.go"},
		{"SetLsofCommandForTest", "internal/doctor/doctor_notwindows.go"},
		{"LsofForTest", "internal/doctor/doctor_notwindows.go"},
	}
	root := repoRootForCitations(t)
	names := map[string]bool{}
	for _, s := range seams {
		names[s.name] = true
		if !declaresFunc(t, filepath.Join(root, filepath.FromSlash(s.file)), s.name) {
			t.Errorf("%s does not declare %s: the walk would look for a seam that is not there", s.file, s.name)
		}
	}
	var calls []string
	observed := 0
	productionCalls(t, func(call *ast.CallExpr, rel string) {
		var fn string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			fn = fun.Sel.Name
		case *ast.Ident:
			fn = fun.Name
		}
		if names[fn] {
			calls = append(calls, rel+": "+fn)
		}
		if fn == "observeRollbackRename" && rel == "internal/updater/rollback_replace.go" {
			observed++
		}
	})
	if observed != 4 {
		t.Fatalf("the walk saw the rollback's put-back tell its observer of %d rename(s), want its four: it reads "+
			"nothing the observer is wired into", observed)
	}
	if len(calls) != 0 {
		t.Errorf("production code calls a test-only seam:\n%s", strings.Join(calls, "\n"))
	}
}

// declaresFunc reports whether the Go file at path declares a function
// (no method) named name.
func declaresFunc(t *testing.T, path, name string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return true
		}
	}
	return false
}
