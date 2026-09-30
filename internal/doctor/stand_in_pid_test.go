package doctor

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStandInTestsHoldWhenTheStandInIsThisProcess runs every test in this
// package that reaches standInPID again, in a child test binary that
// records its own pid as the stand-in: the collision backlog B106 met by
// chance, made on every run and on every platform.
//
// On the macOS leg of one CI run the test binary WAS pid 4242, the pid two
// port tests recorded as a bridge of their own choosing. Both bound their
// port in this process and left the owner probe to the host, so lsof named
// the test binary, the recorded pid, as the port's holder, and both answered
// "bound by our own bridge (pid 4242)" where they want a FAIL. The rule was
// already written down (CLAUDE.md: a test that records a pid of its own
// choosing forces what the probe says about it), and three tests had not
// followed it. So this runs them with the collision in place: a test whose
// verdict still comes from the host fails here, whatever the host's pids.
//
// The tests are found from the source, not listed: every top-level function
// or variable that names standInPID reaches it, and so does every one that
// names something that reaches it. A test that records a stand-in with a pid
// literal would escape that, so the scan also refuses one.
func TestStandInTestsHoldWhenTheStandInIsThisProcess(t *testing.T) {
	if os.Getenv(standInIsSelfEnv) != "" {
		t.Skip("this is the child run")
	}
	names, literals := standInTests(t)
	for _, l := range literals {
		t.Errorf("%s records a pid literal: record standInPID, which this test runs with the "+
			"collision in place, and force what the probe says about it", l)
	}
	// Six on Windows, where the tests of the unix probe are not built;
	// eleven elsewhere.
	if len(names) < 6 || !slices.Contains(names, "TestPortCheck_DeadPIDStillFails") {
		t.Fatalf("found %d tests that reach standInPID (%v); the scan no longer reaches the port "+
			"tests, so it proves nothing", len(names), names)
	}
	args := []string{"-test.run=^(" + strings.Join(names, "|") + ")$", "-test.count=1", "-test.v"}
	if deadline, ok := t.Deadline(); ok {
		// The child reports its own hang, with its stacks, before this
		// binary's deadline ends both.
		if left := time.Until(deadline) - 10*time.Second; left > 0 {
			args = append(args, fmt.Sprintf("-test.timeout=%s", left))
		}
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), standInIsSelfEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("with the stand-in set to the test binary's own pid, the tests that record it "+
			"fail (%v): a verdict there comes from what the host's probe says about this "+
			"process, not from the test.\n%s", err, out.String())
	}
	for _, name := range names {
		if !strings.Contains(out.String(), "=== RUN   "+name+"\n") {
			t.Errorf("the child run did not run %s, so this proves nothing about it:\n%s", name, out.String())
		}
	}
	t.Logf("ran %d tests with the stand-in set to the child's own pid: %s", len(names), strings.Join(names, ", "))
}

// standInTests returns the top-level tests in this package's test files, as
// this build compiles them, that reach standInPID, and every call that hands
// writePIDFile a pid literal. A name is matched by its spelling, so a local
// spelled like a declaration that reaches the stand-in selects a test too
// many, which the child then runs for nothing, and never one too few.
func standInTests(t *testing.T) (names, literals []string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	refs := map[string]map[string]bool{} // top-level name -> identifiers it names
	isTest := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// Decided from the name before anything is opened (MatchFile also
		// refuses a `.` or `_` name first, as the go tool does), then by
		// this build's constraints, so the child has every test it is asked
		// to run.
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := build.Default.MatchFile(".", name); err != nil || !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			for _, top := range topLevelNames(decl) {
				if refs[top] == nil {
					refs[top] = map[string]bool{}
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name != top {
						refs[top][id.Name] = true
					}
					return true
				})
			}
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
				isTest[fd.Name.Name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "writePIDFile" {
				if _, lit := call.Args[1].(*ast.BasicLit); lit {
					literals = append(literals, fset.Position(call.Pos()).String())
				}
			}
			return true
		})
	}
	reached := map[string]bool{"standInPID": true}
	for grew := true; grew; {
		grew = false
		for top, named := range refs {
			if reached[top] {
				continue
			}
			for id := range named {
				if reached[id] {
					reached[top], grew = true, true
					break
				}
			}
		}
	}
	for top := range reached {
		if isTest[top] && top != "TestStandInTestsHoldWhenTheStandInIsThisProcess" {
			names = append(names, top)
		}
	}
	slices.Sort(names)
	return names, literals
}

// topLevelNames is the names a top-level declaration declares: a function's
// (not a method's), or each variable's and constant's.
func topLevelNames(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return []string{d.Name.Name}
		}
	case *ast.GenDecl:
		var out []string
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range vs.Names {
					out = append(out, n.Name)
				}
			}
		}
		return out
	}
	return nil
}
