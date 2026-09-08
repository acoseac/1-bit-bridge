package adminauth

import (
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
	saved := testHashCost
	SetTestHashCost(0)
	defer SetTestHashCost(saved)
	if got := hashCost(); got != adminBcryptCost {
		t.Errorf("hashCost() = %d with no override, want the shipped %d", got, adminBcryptCost)
	}
}

// The override is a plain variable, so nothing stops production code setting it.
// This sweep is what stops it: the same shape as the manifest package's
// hand-rolled-SQL guard, and for the same reason — a convention no test walks
// is a convention that decays.
func TestNoProductionCodeLowersTheHashCost(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
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
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Normalise line endings first: no .gitattributes pins eol, so a
		// Windows checkout would otherwise make this scan find nothing and
		// pass vacuously.
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		// Look for CALLS, not the declaration itself: the setter necessarily
		// lives in a non-test file, and a scan that flags its own definition
		// reports a violation that can never be fixed.
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "SetTestHashCost(") {
				continue
			}
			if strings.Contains(line, "func SetTestHashCost(") {
				continue
			}
			// A mention inside a comment is documentation, not a call.
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			offenders = append(offenders, path+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("non-test files call SetTestHashCost, which would weaken password hashing in production: %v", offenders)
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
	saved := testHashCost
	SetTestHashCost(0) // pretend the process now ships at cost 12
	defer SetTestHashCost(saved)
	if err := s.Verify("admin", "correct horse battery staple"); err != nil {
		t.Errorf("a hash written at the test cost failed to verify at the shipped cost: %v", err)
	}
}
