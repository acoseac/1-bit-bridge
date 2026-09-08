package adminauth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
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
	const setter = "SetTestHashCost"
	var offenders []string
	// Files actually visited, so a walk that silently matches nothing — a wrong
	// root, a skip rule that swallowed the tree — fails instead of passing
	// vacuously.
	visited := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "dist", "bin", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		visited++
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file this package cannot parse is not evidence of compliance.
			offenders = append(offenders, path+": could not parse: "+perr.Error())
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident: // SetTestHashCost(…), from inside this package
				if fn.Name == setter {
					offenders = append(offenders, path)
				}
			case *ast.SelectorExpr: // adminauth.SetTestHashCost(…), from outside
				if fn.Sel.Name == setter {
					offenders = append(offenders, path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if visited == 0 {
		t.Fatalf("walked %s and found no non-test Go files — the guard proved nothing", root)
	}
	if len(offenders) > 0 {
		t.Errorf("non-test files call %s, which would weaken password hashing in production: %v", setter, offenders)
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
