package main

import (
	"fmt"
	"go/ast"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// sweepWalks are the two walks that skip what skipsForCitations names, each
// with what it answers for a tree walked by walk, one sorted line a fact, and
// the fact that sweepSeenFile adds where the walk reads it, %s its slash path
// below the root.
var sweepWalks = []struct {
	name  string
	facts func(t *testing.T, walk func(root string, fn fs.WalkDirFunc) error, root string) []string
	seen  string
}{
	{"the citation guard's walk", citationFacts, "cites TestOnlyAFileTheWalkReadsCites in %s"},
	{"productionCallsIn", productionCallFacts, "calls seen in %s"},
}

// sweepSeenFile is a file each walk notices wherever it reads it: a
// citation of a test nothing defines, and a call.
var sweepSeenFile = map[string]string{
	"main.go": "package main\n\n// Guarded by TestOnlyAFileTheWalkReadsCites.\nfunc main() { seen() }\n",
}

// citationFacts is what scanTestCitationsIn answers for the tree at root,
// walked by walk: each test it found defined, each citation with the file
// that makes it, and how many names the tracked docs cite.
func citationFacts(t *testing.T, walk func(root string, fn fs.WalkDirFunc) error, root string) []string {
	t.Helper()
	cited, defined, mdCited := scanTestCitationsIn(t, walk, root, map[string]bool{"notes.md": true})
	facts := []string{fmt.Sprintf("the docs cite %d", mdCited)}
	for name := range defined {
		facts = append(facts, "defines "+name)
	}
	for name, files := range cited {
		for _, file := range files {
			facts = append(facts, "cites "+name+" in "+filepath.ToSlash(file))
		}
	}
	sort.Strings(facts)
	return facts
}

// productionCallFacts is what productionCallsIn finds under root, walked by
// walk: each call of a function by its bare name, with the file that makes
// it.
func productionCallFacts(t *testing.T, walk func(root string, fn fs.WalkDirFunc) error, root string) []string {
	t.Helper()
	var facts []string
	productionCallsIn(t, walk, root, func(call *ast.CallExpr, rel string) {
		if fn, ok := call.Fun.(*ast.Ident); ok {
			facts = append(facts, "calls "+fn.Name+" in "+rel)
		}
	})
	sort.Strings(facts)
	return facts
}

// writeSweepBaseTree writes the tree both walks are given: one real test,
// cited from a tracked doc and from production code, which also cites a test
// nothing defines and makes one call.
func writeSweepBaseTree(t *testing.T, root string) {
	t.Helper()
	writeTree(t, root, map[string]string{
		"prod.go":   "package x\n\n// Guarded by TestReal and by TestParkedNeverRuns.\nfunc f() { g() }\n\nfunc g() {}\n",
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc TestReal(t *testing.T) { _ = t }\n",
		"notes.md":  "Pinned by TestReal.\n",
	})
}

// TestTheCitationGuardsWalksSkipMakeTestsTMPDIRAtTheRoot plants, one per row,
// what make test leaves in the directory it gives every package's tests as
// TMPDIR when the caller sets none (sweeptest.MakeTestTmpDir, at the root),
// and the same file where the walks must still read it.
//
// The suite runs every package's tests at once, so while these walks read
// the checkout, another package's temporary Go files sat in that directory:
// a test there satisfied a citation this tree does not back, a comment there
// was collected as this tree's citation, a call there was taken for one of
// this module's, and a file in mid-edit failed the walk (B567). The rows in
// that directory leave each walk's answer as it is without them. The rows in
// a directory of the same name below the root, in one whose name begins with
// it, and beside the Go program .github holds are read as before: the walks
// skip that directory at the root alone, as the build-tag census does (B740).
func TestTheCitationGuardsWalksSkipMakeTestsTMPDIRAtTheRoot(t *testing.T) {
	tmp := path.Join(sweeptest.MakeTestTmpDir, "TestSomething123", "001")
	rows := []struct {
		name  string
		dir   string // where the files go, a slash path below the root
		files map[string]string
		read  bool
	}{
		// What another package's test leaves there while the suite runs: a
		// test of the name prod.go cites and nothing here defines, a
		// citation of a test nothing defines, and a call.
		{"files in make test's TMPDIR", tmp, map[string]string{
			"x_test.go": "package x\n\nimport \"testing\"\n\n// Mirrors TestOnlyATemporaryTreeCites.\nfunc TestParkedNeverRuns(t *testing.T) { _ = t }\n",
			"main.go":   "package main\n\n// Guarded by TestOnlyATemporaryTreeCites.\nfunc main() { leftOver() }\n",
		}, false},
		// And what it is still writing: a test file and a source file in
		// mid-edit, which the walks fail to parse.
		{"files in mid-edit in make test's TMPDIR", tmp, map[string]string{
			"half_test.go": "package x\n\nimport \"testing\"\n\nfunc TestHalf(t *testing.T) {\n",
			"half.go":      "package main\n\nfunc half(\n",
		}, false},
		{"a directory of that name below the root", path.Join("internal", "x", sweeptest.MakeTestTmpDir), sweepSeenFile, true},
		{"a directory whose name begins with it", sweeptest.MakeTestTmpDir + "foo", sweepSeenFile, true},
		{"beside the Go program .github holds", path.Join(".github", "scripts", "tool"), sweepSeenFile, true},
	}
	for _, sweep := range sweepWalks {
		t.Run(sweep.name, func(t *testing.T) {
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					root := t.TempDir()
					writeSweepBaseTree(t, root)
					want := sweep.facts(t, filepath.WalkDir, root)
					writeTree(t, filepath.Join(root, filepath.FromSlash(row.dir)), row.files)
					if row.read {
						want = append(want, fmt.Sprintf(sweep.seen, path.Join(row.dir, "main.go")))
						sort.Strings(want)
					}
					if got := sweep.facts(t, filepath.WalkDir, root); !slices.Equal(got, want) {
						t.Errorf("the walk found %q, want %q", got, want)
					}
				})
			}
		})
	}
}

// TestTheCitationGuardsWalksFinishWhileMakeTestsTMPDIRChanges: a directory in
// make test's TMPDIR that another process removes while either walk is under
// way, as go test removes a test's temporary directory and the go command a
// work directory, is one the walk never lists, so it finishes with the answer
// the tree gives without it. Reading that directory, each walk failed on the
// directory it had just listed (B567). A sweeptest.Removal stands in for the
// other process at that moment of the walk, handed to each walk as its walk.
func TestTheCitationGuardsWalksFinishWhileMakeTestsTMPDIRChanges(t *testing.T) {
	for _, sweep := range sweepWalks {
		t.Run(sweep.name, func(t *testing.T) {
			for _, gone := range []struct{ name, dir string }{
				{"a test's temporary directory", "TestSomething123"},
				{"the go command's work directory", "go-build123/b001"},
			} {
				t.Run(gone.name, func(t *testing.T) {
					root := t.TempDir()
					writeSweepBaseTree(t, root)
					want := sweep.facts(t, filepath.WalkDir, root)
					dir := filepath.Join(root, sweeptest.MakeTestTmpDir, filepath.FromSlash(gone.dir))
					writeTree(t, dir, map[string]string{"x_test.go": "package x\n"})
					removal := &sweeptest.Removal{Dir: dir}
					got := sweep.facts(t, removal.WalkDir, root)
					if !removal.Walked {
						t.Fatal("the walk did not go through the stand-in, so this case proves nothing")
					}
					if removal.Reached {
						t.Errorf("the walk reached %s, in make test's TMPDIR", dir)
					}
					if !slices.Equal(got, want) {
						t.Errorf("the walk found %q, want %q", got, want)
					}
				})
			}
		})
	}
}
