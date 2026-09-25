package adminauth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// TestMain lowers the work factor for this package's suite. Everything here
// that touches a password pays bcrypt otherwise, which is the reason CI's race
// job runs for twenty minutes.
func TestMain(m *testing.M) {
	SetTestHashCost(bcrypt.MinCost)
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
func hashCostSetterCallers(root string) (offenders []string, visited int, err error) {
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return hashCostDirRule(root, path, info.Name())
		}
		if !hashCostReads(path, info.Name()) {
			return nil
		}
		visited++
		offenders = append(offenders, hashCostSetterCallsIn(path)...)
		return nil
	})
	return offenders, visited, err
}

// hashCostDirRule is hashCostSetterCallers' answer for a directory: SkipDir
// for one that holds none of this checkout's production code, nil to
// descend.
func hashCostDirRule(root, path, name string) error {
	switch name {
	case ".git", "dist", "bin", "node_modules", "testdata":
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
func TestHashCostSweepSkipsOtherCheckouts(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		".git/HEAD":                  "ref: refs/heads/main\n",
		"cmd/tool/main.go":           "package main\n\nimport \"example/internal/adminauth\"\n\nfunc main() { adminauth.SetTestHashCost(4) }\n",
		"internal/adminauth/cost.go": "package adminauth\n\nfunc init() { SetTestHashCost(4) }\n",

		".claude/worktrees/old/.git":                      "gitdir: /elsewhere/.git/worktrees/old\n",
		".claude/worktrees/old/go.mod":                    "module example\n",
		".claude/worktrees/old/internal/adminauth/wip.go": "package adminauth\n\nfunc init() { SetTestHashCost(4) }\n",
		".claude/worktrees/old/internal/manifest/half.go": "package manifest\n\nfunc half(\n",

		"worktrees/plain/.git": "gitdir: /elsewhere/.git/worktrees/plain\n",
		"worktrees/plain/x.go": "package x\n\nfunc init() { SetTestHashCost(4) }\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
