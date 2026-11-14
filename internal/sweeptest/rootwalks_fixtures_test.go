package sweeptest

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shapesOfWalk is a package holding one function for each way a function
// walks a tree, beside functions that make no walk: one that only hands a
// walk on, one that lists a directory once, one that spells a walk in a
// comment and a string.
const shapesOfWalk = `package walks

import (
	"io/fs"
	"os"
	"path/filepath"
)

func direct(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { return err })
}

func old(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error { return err })
}

func overFS(fsys fs.FS) error { return fs.WalkDir(fsys, ".", visit) }

func visit(path string, d fs.DirEntry, err error) error { return err }

func handed(walk func(root string, fn fs.WalkDirFunc) error, root string) error { return walk(root, visit) }

type walker func(string, fs.WalkDirFunc) error

func handedNamed(w walker, root string) error { return w(root, visit) }

type sweep struct{ walk func(string, fs.WalkDirFunc) error }

func (s sweep) held(root string) error { return s.walk(root, visit) }

func local(root string) error {
	w := filepath.WalkDir
	return w(root, visit)
}

func listAll(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		listAll(filepath.Join(dir, e.Name()))
	}
}

func closure(root string) {
	var list func(string)
	list = func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			list(filepath.Join(dir, e.Name()))
		}
	}
	list(root)
}

type tree struct{}

func (tree) WalkDir(root string, fn fs.WalkDirFunc) error { return filepath.WalkDir(root, fn) }

func viaMethod(root string) error {
	return tree{}.WalkDir(root, func(path string, d fs.DirEntry, err error) error { return err })
}

var seam = filepath.WalkDir

func handsOn(root string) error { return handed(filepath.WalkDir, root) }

func listsOnce(dir string) int {
	entries, _ := os.ReadDir(dir)
	return len(entries)
}

// spelled names filepath.WalkDir(root, fn) in this comment and in a string.
func spelled() string {
	f := func(a, b string) error { return nil }
	_ = f
	return "filepath.WalkDir(root, fn)"
}
`

// shapesOfWalkTest is a test file of that package whose test walks a tree.
const shapesOfWalkTest = `package walks

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func TestWalksAbove(t *testing.T) {
	_ = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error { return err })
}
`

// oneWalk is a file of one function, walk, that walks a tree.
const oneWalk = "package p\n\nimport (\n\t\"io/fs\"\n\t\"path/filepath\"\n)\n\n" +
	"func walk(root string) error {\n\treturn filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { return err })\n}\n"

// writeModule writes each file in files, named by its slash path below root,
// creating the directories it needs.
func writeModule(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// walkKeys is the key of every walk readModule finds under root with walk,
// in order.
func walkKeys(t *testing.T, walk func(root string, fn fs.WalkDirFunc) error, root string) []string {
	t.Helper()
	mod, err := readModule(walk, root)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	return slices.Sorted(maps.Keys(mod.walks()))
}

// TestTheWalkSweepFindsEveryShapeOfWalk runs readModule and walks over a
// module of its own. Every way a function walks a tree is found, by its key
// alone: a call of each walk the standard library and fsutil make, a
// callback written in the function or declared beside it and handed on, a
// walk handed in as a parameter (the shape the sweeps from the root take
// since the B567 follow-up, so a new sweep made that way is found where it
// walks), a walk of a named type, one a struct holds, one in a variable, a
// directory listed by a function that calls itself or by a closure that
// does, a WalkDir method, and a walk held at package level. A function that
// only hands a walk on, lists one directory, or names a walk in a comment or
// a string is no walk.
//
// The same walk is planted in each kind of directory the sweep reads or
// skips: read in .github, in a .tmp below the root and in one whose name
// begins with .tmp; skipped in the root's .tmp, where a file in mid-edit
// would fail the parse, and in .git, a directory beginning with "_",
// testdata, vendor, node_modules, another module and another checkout. A
// file the go tool ignores, an editor's lock among them, is never parsed.
func TestTheWalkSweepFindsEveryShapeOfWalk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	writeModule(t, root, map[string]string{
		"go.mod":              "module example\n",
		".git/HEAD":           "ref: refs/heads/main\n",
		"walks/walks.go":      shapesOfWalk,
		"walks/walks_test.go": shapesOfWalkTest,
		"walks/.#walks.go":    "someone@host.1:1",
		"walks/_parked.go":    oneWalk,

		".github/tool/x.go": oneWalk,
		"sub/.tmp/x.go":     oneWalk,
		".tmpfoo/x.go":      oneWalk,

		MakeTestTmpDir + "/TestSomething123/001/x.go":    oneWalk,
		MakeTestTmpDir + "/TestSomething123/001/half.go": "package p\n\nfunc half(\n",
		".git/hooks/x.go":     oneWalk,
		"_scratch/x.go":       oneWalk,
		"walks/testdata/x.go": oneWalk,
		"vendor/v/x.go":       oneWalk,
		"node_modules/n/x.go": oneWalk,
		"nested/go.mod":       "module nested\n",
		"nested/x.go":         oneWalk,
		"worktrees/old/.git":  "gitdir: /elsewhere/.git/worktrees/old\n",
		"worktrees/old/x.go":  oneWalk,
	})
	mod, err := readModule(filepath.WalkDir, root)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if mod.files != 4 || mod.tests != 1 {
		t.Errorf("read %d non-test and %d test files, want 4 and 1: walks.go, the three planted where the sweep reads, and the test file",
			mod.files, mod.tests)
	}
	want := []string{
		".github/tool/x.go walk",
		".tmpfoo/x.go walk",
		"sub/.tmp/x.go walk",
		"walks/walks.go (package level)",
		"walks/walks.go (sweep).held",
		"walks/walks.go (tree).WalkDir",
		"walks/walks.go closure",
		"walks/walks.go direct",
		"walks/walks.go handed",
		"walks/walks.go handedNamed",
		"walks/walks.go listAll",
		"walks/walks.go local",
		"walks/walks.go old",
		"walks/walks.go overFS",
		"walks/walks.go viaMethod",
		"walks/walks_test.go TestWalksAbove",
	}
	slices.Sort(want)
	if got := slices.Sorted(maps.Keys(mod.walks())); !slices.Equal(got, want) {
		t.Errorf("found the walks\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheWalkSweepFinishesWhileMakeTestsTMPDIRChanges: readModule, which
// walks the checkout's root itself, skips make test's TMPDIR as the sweeps
// it lists do. A directory in it that another process removes mid-walk, as
// go test removes a test's temporary directory and the go command a work
// directory, is one it never lists, and the TMPDIR itself, removed as the
// walk reaches it in the root's listing, it skips by its name alone, before
// it lists it or asks anything of it: either way it finishes with the walks
// the tree holds without them. A Removal stands in for the other process,
// handed to the sweep as its walk.
func TestTheWalkSweepFinishesWhileMakeTestsTMPDIRChanges(t *testing.T) {
	for _, gone := range []struct {
		name    string
		dir     string // a slash path below make test's TMPDIR, "" for the TMPDIR itself
		reached bool   // whether the walk reaches it: the root's listing names the TMPDIR alone
	}{
		{"a test's temporary directory", "TestSomething123", false},
		{"the go command's work directory", "go-build123/b001", false},
		{"make test's TMPDIR itself", "", true},
	} {
		t.Run(gone.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeModule(t, root, map[string]string{"go.mod": "module example\n", ".git/HEAD": "ref: refs/heads/main\n", "p/x.go": oneWalk})
			want := walkKeys(t, filepath.WalkDir, root)
			dir := filepath.Join(root, MakeTestTmpDir, filepath.FromSlash(gone.dir))
			writeModule(t, dir, map[string]string{"x.go": oneWalk})
			removal := &Removal{Dir: dir}
			got := walkKeys(t, removal.WalkDir, root)
			if !removal.Walked {
				t.Fatal("the sweep did not walk through the stand-in, so this case proves nothing")
			}
			if removal.Reached != gone.reached {
				if gone.reached {
					t.Fatalf("the sweep never reached %s, which the root's listing names, so this case proves nothing", dir)
				}
				t.Errorf("the sweep reached %s, in make test's TMPDIR", dir)
			}
			if !slices.Equal(got, want) {
				t.Errorf("the sweep found %q, want %q", got, want)
			}
		})
	}
}

// TestTheInventoryReadsEachItemAsWritten: rootWalkInventory reads the one
// list a doc holds, an item a walk, its lines joined and its tests split on
// the commas and the "and" of a list, and names every item it cannot read
// rather than skipping it, and a doc with no list or with two.
func TestTheInventoryReadsEachItemAsWritten(t *testing.T) {
	doc := "The walks:\n\n" +
		"  - a/b_test.go walkTree, from TestOne: by its name, in skips.\n" +
		"  - c/d.go walkOther, from TestTwo, TestThree and\n" +
		"    TestFour: as a dot-directory, in walkOther.\n" +
		"\nAnd prose after it.\n"
	listed, problems := rootWalkInventory(doc)
	want := []listedWalk{
		{key: "a/b_test.go walkTree", tests: []string{"TestOne"}, rule: ruleByName, in: "skips"},
		{key: "c/d.go walkOther", tests: []string{"TestTwo", "TestThree", "TestFour"}, rule: ruleDotDirectory, in: "walkOther"},
	}
	if len(problems) != 0 || !slices.EqualFunc(listed, want, func(a, b listedWalk) bool {
		return a.key == b.key && slices.Equal(a.tests, b.tests) && a.rule == b.rule && a.in == b.in
	}) {
		t.Errorf("read %+v with problems %q, want %+v and none", listed, problems, want)
	}

	for _, c := range []struct{ name, doc, problem string }{
		{"an item in another shape", "The walks:\n\n  - a/b.go walkTree: by its name.\n", `lists "a/b.go walkTree: by its name."`},
		{"a rule it does not know", "The walks:\n\n  - a/b.go walkTree, from TestOne: by its path, in skips.\n", `lists "a/b.go walkTree, from TestOne: by its path, in skips."`},
		{"no list", "Prose alone.\n", "holds 0 lists"},
		{"two lists", "  - a/b.go walkTree, from TestOne: by its name, in skips.\n\nProse.\n\n  - c/d.go walkOther, from TestTwo: by its name, in skips.\n", "holds 2 lists"},
	} {
		t.Run(c.name, func(t *testing.T) {
			listed, problems := rootWalkInventory(c.doc)
			if len(listed) != 0 || len(problems) != 1 || !strings.Contains(problems[0], c.problem) {
				t.Errorf("read %+v with problems %q, want nothing read and one problem holding %q", listed, problems, c.problem)
			}
		})
	}
}

// checkedModule is a module of three walks: one from the root, through a
// parameter, that skips make test's TMPDIR by its name in a function of its
// own; one that skips it as a dot-directory in its own branch for a
// directory, beside a walk that skips dot-files and no dot-directory; and a
// fixture's. Each has a test that makes it.
var checkedModule = map[string]string{
	"go.mod": "module example\n",
	"sweep/sweep.go": `package sweep

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

func walkTree(walk func(string, fs.WalkDirFunc) error, root string) error {
	return walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skips(root, path) {
				return filepath.SkipDir
			}
		}
		return nil
	})
}

func skips(root, path string) bool { return sweeptest.IsMakeTestTmpDir(root, path) }

func unusedRule(name string) bool { return strings.HasPrefix(name, ".") }
`,
	"sweep/sweep_test.go": `package sweep

import (
	"path/filepath"
	"testing"
)

func TestSweepTheRoot(t *testing.T) { _ = walkTree(filepath.WalkDir, "..") }

func TestUnrelated(t *testing.T) {}
`,
	"dots/dots.go": `package dots

import (
	"io/fs"
	"path/filepath"
	"strings"
)

func walkDots(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return err
	})
}

func walkFiles(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		return err
	})
}
`,
	"dots/dots_test.go": `package dots

import "testing"

func TestDots(t *testing.T) { _ = walkDots("..") }

func TestFiles(t *testing.T) { _ = walkFiles("..") }
`,
	"fix/fix_test.go": `package fix

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func TestFixture(t *testing.T) {
	_ = filepath.WalkDir(t.TempDir(), func(path string, d fs.DirEntry, err error) error { return err })
}
`,
}

// newSweep is a sweep from the root added through a parameter, as the
// sweeps B567 lists take their walk, with the test that makes it, and listed
// nowhere.
const newSweep = `package fresh

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func sweepFresh(walk func(string, fs.WalkDirFunc) error, root string) error {
	return walk(root, func(path string, d fs.DirEntry, err error) error { return err })
}

func TestFreshSweep(t *testing.T) { _ = sweepFresh(filepath.WalkDir, "../..") }
`

// TestCheckWalksNamesEveryWayTheListsCanBeWrong runs checkWalks over
// checkedModule, whose lists are right as they stand, and then once for each
// way the module or a list can be wrong, each of which must be the one
// problem it reports: a walk listed nowhere (a sweep from the root added
// through a parameter); a walk a test file holds at package level, listed or
// not; one the bridge's code holds there, unlisted; a walk in both lists;
// an item or an entry naming no walk; a listed test that does not reach its
// walk, is another directory's, or is not there; a rule its walk's branch for
// a directory does not reach, held by a function that branch does not call,
// or not the rule the function holds; and an item listed twice.
func TestCheckWalksNamesEveryWayTheListsCanBeWrong(t *testing.T) {
	sweepItem := listedWalk{key: "sweep/sweep.go walkTree", tests: []string{"TestSweepTheRoot"}, rule: ruleByName, in: "skips"}
	dotsItem := listedWalk{key: "dots/dots.go walkDots", tests: []string{"TestDots"}, rule: ruleDotDirectory, in: "walkDots"}
	baseElsewhere := map[string]string{
		"fix/fix_test.go TestFixture": "a test's tree",
		"dots/dots.go walkFiles":      "a tree its test names",
	}
	for _, row := range []struct {
		name  string
		files map[string]string
		edit  func(listed []listedWalk, elsewhere map[string]string) []listedWalk
		want  string // what the one problem holds, "" for none
	}{
		{name: "the lists as they stand"},
		{name: "a sweep from the root added through a parameter and listed nowhere",
			files: map[string]string{"fresh/fresh_test.go": newSweep},
			want:  "fresh/fresh_test.go sweepFresh walks a directory tree (calls walk, a walk it is handed; a walk callback) and is named neither"},
		{name: "a walk a test file holds at package level",
			files: map[string]string{"fix/seam_test.go": "package fix\n\nimport \"path/filepath\"\n\nvar seamWalk = filepath.WalkDir\n"},
			want:  "fix/seam_test.go (package level) holds a walk at package level in a test file"},
		{name: "a walk a test file holds at package level, though walksElsewhere names it",
			files: map[string]string{"fix/seam_test.go": "package fix\n\nimport \"path/filepath\"\n\nvar seamWalk = filepath.WalkDir\n"},
			edit: func(listed []listedWalk, elsewhere map[string]string) []listedWalk {
				elsewhere["fix/seam_test.go (package level)"] = "a seam"
				return listed
			},
			want: "fix/seam_test.go (package level) holds a walk at package level in a test file"},
		{name: "a walk the bridge's code holds at package level, unlisted",
			files: map[string]string{"fix/seam.go": "package fix\n\nimport \"path/filepath\"\n\nvar seamWalk = filepath.WalkDir\n"},
			want:  "fix/seam.go (package level) holds a walk at package level (holds filepath.WalkDir)"},
		{name: "a walk in both lists",
			edit: func(listed []listedWalk, elsewhere map[string]string) []listedWalk {
				elsewhere["sweep/sweep.go walkTree"] = "a tree"
				return listed
			},
			want: "sweep/sweep.go walkTree is a walk from the root in MakeTestTmpDir's doc and a walk elsewhere"},
		{name: "an item that names no walk",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				return append(listed, listedWalk{key: "gone/gone.go walkGone", tests: []string{"TestGone"}, rule: ruleByName, in: "walkGone"})
			},
			want: "MakeTestTmpDir's doc lists gone/gone.go walkGone, which walks no tree"},
		{name: "an entry of walksElsewhere that names no walk",
			edit: func(listed []listedWalk, elsewhere map[string]string) []listedWalk {
				elsewhere["gone/gone.go walkGone"] = "a tree"
				return listed
			},
			want: "walksElsewhere names gone/gone.go walkGone, which walks no tree"},
		{name: "a listed test that does not reach its walk",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				listed[0].tests = []string{"TestUnrelated"}
				return listed
			},
			want: "says TestUnrelated makes sweep/sweep.go walkTree from the root, and no test of that name in its directory reaches it"},
		{name: "a listed test of another directory",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				listed[1].tests = []string{"TestSweepTheRoot"}
				return listed
			},
			want: "says TestSweepTheRoot makes dots/dots.go walkDots from the root, and no test of that name in its directory reaches it"},
		{name: "a listed test that is not there",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				listed[0].tests = []string{"TestNotThere"}
				return listed
			},
			want: "says TestNotThere makes sweep/sweep.go walkTree from the root"},
		{name: "a rule its walk's branch for a directory does not reach",
			edit: func(listed []listedWalk, elsewhere map[string]string) []listedWalk {
				delete(elsewhere, "dots/dots.go walkFiles")
				return append(listed, listedWalk{key: "dots/dots.go walkFiles", tests: []string{"TestFiles"}, rule: ruleDotDirectory, in: "walkFiles"})
			},
			want: "says dots/dots.go walkFiles skips the TMPDIR as a dot-directory, in walkFiles, and no branch its walk takes for a directory"},
		{name: "a rule held by a function the branch does not call",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				listed[0].rule, listed[0].in = ruleDotDirectory, "unusedRule"
				return listed
			},
			want: "says sweep/sweep.go walkTree skips the TMPDIR as a dot-directory, in unusedRule"},
		{name: "a rule its function does not hold",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				listed[0].rule = ruleDotDirectory
				return listed
			},
			want: "says sweep/sweep.go walkTree skips the TMPDIR as a dot-directory, in skips"},
		{name: "an item listed twice",
			edit: func(listed []listedWalk, _ map[string]string) []listedWalk {
				return append(listed, sweepItem)
			},
			want: "MakeTestTmpDir's doc lists sweep/sweep.go walkTree twice"},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeModule(t, root, checkedModule)
			writeModule(t, root, row.files)
			mod, err := readModule(filepath.WalkDir, root)
			if err != nil {
				t.Fatalf("the sweep failed: %v", err)
			}
			listed := []listedWalk{sweepItem, dotsItem}
			listed[0].tests = slices.Clone(sweepItem.tests)
			listed[1].tests = slices.Clone(dotsItem.tests)
			elsewhere := maps.Clone(baseElsewhere)
			if row.edit != nil {
				listed = row.edit(listed, elsewhere)
			}
			problems := checkWalks(mod, listed, elsewhere)
			switch {
			case row.want == "" && len(problems) != 0:
				t.Errorf("checkWalks found %q, want nothing", problems)
			case row.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], row.want)):
				t.Errorf("checkWalks found %q, want one problem holding %q", problems, row.want)
			}
		})
	}
}
