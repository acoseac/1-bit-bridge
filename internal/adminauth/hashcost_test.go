package adminauth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// TestMain lowers the work factor for this package's suite. Everything here
// that touches a password pays bcrypt otherwise, which is the reason CI's race
// job runs for twenty minutes. It also drops the settle every rotation and
// sign-out waits before confirming its write (commitAndConfirm): a second
// each, and the tests that need the in-flight write land it through
// beforeConfirmHook rather than wait for it.
func TestMain(m *testing.M) {
	SetTestHashCost(bcrypt.MinCost)
	confirmSettle = 0
	os.Exit(m.Run())
}

// The shipped cost is what protects real credentials, so it must not drift —
// and the indirection introduced for tests must not become the way it drifts.
func TestShippedHashCostIsUnchanged(t *testing.T) {
	if adminBcryptCost != 12 {
		t.Fatalf("adminBcryptCost = %d, want 12 — lowering it weakens every stored password", adminBcryptCost)
	}
	// With no override, the store must use the shipped value.
	saved := getTestHashCost()
	SetTestHashCost(0)
	defer SetTestHashCost(saved)
	if got := hashCost(); got != adminBcryptCost {
		t.Errorf("hashCost() = %d with no override, want the shipped %d", got, adminBcryptCost)
	}
}

// Nothing stops production code calling the setter at compile time. This walk is
// what stops it — the same shape as the manifest package's hand-rolled-SQL
// guard, and for the same reason: a convention no test walks is a convention
// that decays.
//
// It parses each file and looks for CALL EXPRESSIONS rather than scanning text.
// A text scan would report this repo's own commentary: the setter's docblock
// names it, this file's comments name it, and CLAUDE.md records the same trap
// twice — a guard that finds the sentence explaining a defect and reports the
// defect as present. Parsing drops comments for free, and it distinguishes the
// declaration (an *ast.FuncDecl) from a call without a special case.
func TestNoProductionCodeLowersTheHashCost(t *testing.T) {
	root := filepath.Join("..", "..")
	offenders, visited, err := hashCostSetterCallers(root)
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	// Files actually visited, so a walk that silently matches nothing — a wrong
	// root, a skip rule that swallowed the tree — fails instead of passing
	// vacuously. Not just none: the walk skips whole directories that are
	// other checkouts, so the floor has to catch a rule that swallowed the
	// largest subtree, internal/ (355 of the 410 files the tree held when it
	// was set), and not only one that swallowed all of it.
	if visited < 100 {
		t.Fatalf("walked %s and read %d non-test Go files, want >=100 — the walk is not seeing the tree", root, visited)
	}
	if len(offenders) > 0 {
		t.Errorf("non-test files call %s, which would weaken password hashing in production: %v", hashCostSetter, offenders)
	}
}

// hashCostSetter is the setter TestNoProductionCodeLowersTheHashCost keeps
// out of production code.
const hashCostSetter = "SetTestHashCost"

// hashCostSetterCallers returns the non-test Go files under root that call
// hashCostSetter, or that it cannot parse, with how many files it read.
//
// It walks with filepath.WalkDir (hashCostWalkDir), which asks
// hashCostDirRule about a directory before listing it. filepath.Walk lists
// it first, so a directory the rule skips still failed the run when it could
// not be listed.
func hashCostSetterCallers(root string) (offenders []string, visited int, err error) {
	err = hashCostWalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return hashCostDirRule(root, path, d.Name())
		}
		if !hashCostReads(path, d.Name()) {
			return nil
		}
		visited++
		offenders = append(offenders, hashCostSetterCallsIn(path)...)
		return nil
	})
	return offenders, visited, err
}

// hashCostWalkDir is the walk hashCostSetterCallers makes: filepath.WalkDir,
// which a test replaces with a sweeptest.Removal to stand in for another
// process removing a directory while the sweep is under way.
var hashCostWalkDir = filepath.WalkDir

// hashCostDirRule is hashCostSetterCallers' answer for a directory: SkipDir
// for one that holds none of this checkout's production code, nil to
// descend.
func hashCostDirRule(root, path, name string) error {
	// The rules are for the directories below the root. The whole-tree
	// test's root is "../..", which no rule names, but a caller's root may
	// be called anything, and one called dist skipped the whole tree.
	// (Gemini on #1007.)
	if path == root {
		return nil
	}
	switch name {
	case ".git", "dist", "bin", "node_modules", "testdata":
		return filepath.SkipDir
	}
	// Nor a directory whose name begins with "_", which the go tool ignores
	// at any depth (`go help packages`), so no build compiles what is in it.
	// CLAUDE.md tells an operator to put a throwaway helper in one, and a
	// helper in mid-edit there failed this guard as "could not parse". The
	// go tool's "." rule is not borrowed with it: this walk reads the Go
	// program tracked under .github/, and that rule would drop it.
	if strings.HasPrefix(name, "_") {
		return filepath.SkipDir
	}
	// Nor make test's TMPDIR, the root's own .tmp (sweeptest.IsMakeTestTmpDir),
	// where every package's tests keep their temporary files while the suite
	// runs, this package's fixtures among them: a call there was reported as
	// production code, a file there in mid-edit as one that does not parse,
	// and a directory there removed between its listing and its own failed
	// the walk (B567). Only that one: a .tmp further down is the tree's.
	if sweeptest.IsMakeTestTmpDir(root, path) {
		return filepath.SkipDir
	}
	// Nor another checkout inside this one (sweeptest.IsOtherCheckout), such
	// as Claude Code's worktrees of other branches: none of it is this
	// checkout's production code, and a half-written file there failed this
	// checkout's run.
	if sweeptest.IsOtherCheckout(root, path) {
		return filepath.SkipDir
	}
	return nil
}

// hashCostReads reports whether hashCostSetterCallers reads the file at
// path: non-test Go source.
func hashCostReads(path, name string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	// Nor a file the go tool ignores (a name beginning with "." or "_"): it
	// is never compiled, so it cannot weaken production. Parsing one is not
	// fail-closed, it is wrong: emacs's `.#store.go` lock is a dangling
	// symlink or a file of lock data, and it was reported here as a
	// production caller of the setter.
	return !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// hashCostSetterCallsIn returns path once for each call of hashCostSetter
// in the file at path, or once, with the error, when the file does not
// parse.
func hashCostSetterCallsIn(path string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		// A file this package cannot parse is not evidence of compliance.
		return []string{path + ": could not parse: " + err.Error()}
	}
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calledName(call) == hashCostSetter {
			calls = append(calls, path)
		}
		return true
	})
	return calls
}

// calledName returns the name a call is made through: the identifier for
// SetTestHashCost(…) from inside this package, the selector's for
// adminauth.SetTestHashCost(…) from outside, and "" for anything else.
func calledName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// TestHashCostSweepSkipsOtherCheckouts runs the sweep over a tree shaped like
// the main checkout: a root that is a checkout itself, with two callers below
// it that must be found, and other checkouts inside it whose work in progress
// must not be. One is Claude Code's worktree of another branch, with a call
// and a half-written file; the other sits at a plain path and has no go.mod
// of its own. Before the sweep skipped them, each failed this checkout's run.
//
// The root's own name is one the walk skips below it, dist. The walk tests
// the names of the directories under the root, never the root's.
func TestHashCostSweepSkipsOtherCheckouts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dist")
	writeSweepTree(t, root, map[string]string{
		".git/HEAD":                  "ref: refs/heads/main\n",
		"cmd/tool/main.go":           "package main\n\nimport \"example/internal/adminauth\"\n\nfunc main() { adminauth.SetTestHashCost(4) }\n",
		"internal/adminauth/cost.go": "package adminauth\n\nfunc init() { SetTestHashCost(4) }\n",

		".claude/worktrees/old/.git":                      "gitdir: /elsewhere/.git/worktrees/old\n",
		".claude/worktrees/old/go.mod":                    "module example\n",
		".claude/worktrees/old/internal/adminauth/wip.go": "package adminauth\n\nfunc init() { SetTestHashCost(4) }\n",
		".claude/worktrees/old/internal/manifest/half.go": "package manifest\n\nfunc half(\n",

		"worktrees/plain/.git": "gitdir: /elsewhere/.git/worktrees/plain\n",
		"worktrees/plain/x.go": "package x\n\nfunc init() { SetTestHashCost(4) }\n",
	})
	offenders, visited, err := hashCostSetterCallers(root)
	if err != nil {
		t.Fatal(err)
	}
	// A call from outside the package and one from inside it, in the order
	// the walk reaches them.
	want := []string{filepath.Join(root, "cmd", "tool", "main.go"), filepath.Join(root, "internal", "adminauth", "cost.go")}
	if !slices.Equal(offenders, want) || visited != 2 {
		t.Errorf("offenders = %q after reading %d files, want %q after reading 2 — "+
			"the sweep read another checkout, or stopped reading this one", offenders, visited, want)
	}
}

// TestHashCostSweepSkipsUnderscoreDirectories runs the sweep over a tree
// holding the directories CLAUDE.md tells an operator to put a throwaway
// helper in: a directory whose name begins with "_", which the go tool
// ignores at any depth, so no build compiles what is in it. One sits at the
// top of the tree, with a helper in mid-edit and a call of the setter, and
// one below a directory the sweep reads, with a call. Before the sweep
// skipped them, the helper in mid-edit failed this checkout's run as "could
// not parse", and each call was reported as production code.
//
// The root's own name begins with "_" too. The rule is for the directories
// below it, so a checkout cloned into such a directory is still read.
func TestHashCostSweepSkipsUnderscoreDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "_checkout")
	const call = "package main\n\nimport \"example/internal/adminauth\"\n\nfunc main() { adminauth.SetTestHashCost(4) }\n"
	writeSweepTree(t, root, map[string]string{
		"cmd/tool/main.go": call,

		"_scratch/main.go":                     "package main\n\nfunc main() { db := open(\n",
		"_scratch/seed.go":                     call,
		"internal/manifest/_reextract/main.go": call,
	})
	offenders, visited, err := hashCostSetterCallers(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "cmd", "tool", "main.go")}
	if !slices.Equal(offenders, want) || visited != 1 {
		t.Errorf("offenders = %q after reading %d files, want %q after reading 1 — "+
			"the sweep read a directory the go tool ignores, or stopped reading this tree", offenders, visited, want)
	}

	// Skipped by its name before it is listed, so the sweep never asks what
	// is in one it cannot list. Before, this failed the run with "permission
	// denied": filepath.Walk lists a directory before its callback can skip
	// it, and filepath.WalkDir does not.
	t.Run("one the sweep cannot list", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permission bits do not stop this process listing a directory here")
		}
		sealed := filepath.Join(root, "_sealed")
		writeSweepTree(t, sealed, map[string]string{"main.go": call})
		if err := os.Chmod(sealed, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
		if _, _, err := hashCostSetterCallers(root); err != nil {
			t.Errorf("the sweep failed on a directory the go tool ignores, which it should not have listed: %v", err)
		}
	})
}

// TestHashCostSweepSkipsMakeTestsTMPDIRAtTheRoot plants, one per row, what
// make test leaves in the directory it gives every package's tests as TMPDIR
// when the caller sets none (sweeptest.MakeTestTmpDir, at the root), and the
// same call where the sweep must still find it.
//
// The suite runs every package's tests at once, so while the whole-tree
// guard walked the checkout, another package's temporary Go files sat in
// that directory, this package's own fixtures among them: a call there was
// reported as production code, and a file in mid-edit as one that does not
// parse (B567). The rows in that directory leave the sweep's answer as it is
// without them. The rows in a directory of the same name below the root, in
// one whose name begins with it, and beside the Go program .github holds are
// found as before: the sweep skips that directory at the root alone, as the
// build-tag census does (B740).
func TestHashCostSweepSkipsMakeTestsTMPDIRAtTheRoot(t *testing.T) {
	const call = "package main\n\nimport \"example/internal/adminauth\"\n\nfunc main() { adminauth.SetTestHashCost(4) }\n"
	tmp := path.Join(sweeptest.MakeTestTmpDir, "TestSomething123", "001")
	for _, row := range []struct {
		name  string
		file  string // a slash path below the root
		body  string
		found bool // whether the sweep must report the file
	}{
		{"a call in make test's TMPDIR", path.Join(tmp, "cmd", "tool", "main.go"), call, false},
		{"a file in mid-edit in make test's TMPDIR", path.Join(tmp, "half.go"), "package main\n\nfunc main() { db := open(\n", false},
		{"a call in a directory of that name below the root", path.Join("internal", "x", sweeptest.MakeTestTmpDir, "main.go"), call, true},
		{"a call in a directory whose name begins with it", path.Join(sweeptest.MakeTestTmpDir+"foo", "main.go"), call, true},
		{"a call beside the Go program .github holds", path.Join(".github", "scripts", "tool", "main.go"), call, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeSweepTree(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "cmd/tool/main.go": call})
			want, wantVisited, err := hashCostSetterCallers(root)
			if err != nil {
				t.Fatal(err)
			}
			writeSweepTree(t, root, map[string]string{row.file: row.body})
			if row.found {
				want = append(want, filepath.Join(root, filepath.FromSlash(row.file)))
				wantVisited++
			}
			offenders, visited, err := hashCostSetterCallers(root)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(want)
			slices.Sort(offenders)
			if !slices.Equal(offenders, want) || visited != wantVisited {
				t.Errorf("offenders = %q after reading %d files, want %q after reading %d", offenders, visited, want, wantVisited)
			}
		})
	}
}

// TestHashCostSweepFinishesWhileMakeTestsTMPDIRChanges: a directory in make
// test's TMPDIR that another process removes while the sweep is under way,
// as go test removes a test's temporary directory and the go command a work
// directory, is one the sweep never lists, so it finishes with the answer
// the tree gives without it. Reading that directory, the sweep failed on the
// directory it had just listed (B567). A sweeptest.Removal stands in for the
// other process at that moment of the walk.
func TestHashCostSweepFinishesWhileMakeTestsTMPDIRChanges(t *testing.T) {
	const call = "package main\n\nimport \"example/internal/adminauth\"\n\nfunc main() { adminauth.SetTestHashCost(4) }\n"
	for _, gone := range []struct{ name, dir string }{
		{"a test's temporary directory", "TestSomething123"},
		{"the go command's work directory", "go-build123/b001"},
	} {
		t.Run(gone.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeSweepTree(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "cmd/tool/main.go": call})
			want, wantVisited, err := hashCostSetterCallers(root)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, sweeptest.MakeTestTmpDir, filepath.FromSlash(gone.dir))
			writeSweepTree(t, dir, map[string]string{"main.go": call})
			removal := &sweeptest.Removal{Dir: dir}
			hashCostWalkDir = removal.WalkDir
			t.Cleanup(func() { hashCostWalkDir = filepath.WalkDir })
			offenders, visited, err := hashCostSetterCallers(root)
			if !removal.Walked {
				t.Fatal("the sweep did not walk through the stand-in, so this case proves nothing")
			}
			if err != nil {
				t.Fatalf("the sweep failed: %v", err)
			}
			if removal.Reached {
				t.Errorf("the sweep reached %s, in make test's TMPDIR", dir)
			}
			if !slices.Equal(offenders, want) || visited != wantVisited {
				t.Errorf("offenders = %q after reading %d files, want %q after reading %d", offenders, visited, want, wantVisited)
			}
		})
	}
}

// writeSweepTree writes each file in files, named by its slash path below
// root, creating the directories it needs.
func writeSweepTree(t *testing.T, root string, files map[string]string) {
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

// A hash written at one cost must still verify when the process uses another,
// or lowering the cost in tests would quietly change what is being tested.
func TestHashesVerifyAcrossCosts(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "adminauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInitialPassword("admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	saved := getTestHashCost()
	SetTestHashCost(0) // pretend the process now ships at cost 12
	defer SetTestHashCost(saved)
	if err := s.Verify("admin", "correct horse battery staple"); err != nil {
		t.Errorf("a hash written at the test cost failed to verify at the shipped cost: %v", err)
	}
}
