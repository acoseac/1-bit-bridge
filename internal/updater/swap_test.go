//go:build !windows

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBinary writes a tiny "binary" with distinguishable contents so
// the test can verify which version landed at the live path.
func fakeBinary(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return p
}

func TestSwapBinaryAtomicallyReplacesAndKeepsBak(t *testing.T) {
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "OLD")
	newBin := fakeBinary(t, dir, "bridge.new", "NEW")

	if err := swapBinary(live, newBin, ".bak"); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}

	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(got) != "NEW" {
		t.Errorf("live binary contents = %q, want NEW", string(got))
	}
	bak, err := os.ReadFile(live + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != "OLD" {
		t.Errorf(".bak contents = %q, want OLD", string(bak))
	}
}

func TestSwapBinaryOverwritesStaleBak(t *testing.T) {
	// A previous install cycle left a .bak. The new install must
	// replace it (we keep at most one .bak, never an accumulation).
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "v2")
	_ = fakeBinary(t, dir, "bridge.bak", "very-old")
	newBin := fakeBinary(t, dir, "bridge.new", "v3")

	if err := swapBinary(live, newBin, ".bak"); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	bak, _ := os.ReadFile(live + ".bak")
	if string(bak) != "v2" {
		t.Errorf(".bak = %q, want v2 (the previous live, not the stale one)", string(bak))
	}
}

func TestRollbackBinaryRestoresFromBak(t *testing.T) {
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "BAD-NEW")
	_ = fakeBinary(t, dir, "bridge.bak", "GOOD-OLD")

	if err := RollbackBinary(live, ".bak"); err != nil {
		t.Fatalf("RollbackBinary: %v", err)
	}
	got, _ := os.ReadFile(live)
	if string(got) != "GOOD-OLD" {
		t.Errorf("post-rollback live = %q, want GOOD-OLD", string(got))
	}
	if _, err := os.Stat(live + ".bak"); !os.IsNotExist(err) {
		t.Errorf(".bak should be gone after rollback (rename consumed it): err=%v", err)
	}
}

func TestRollbackBinaryFailsWhenBakMissing(t *testing.T) {
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "current")
	if err := RollbackBinary(live, ".bak"); err == nil {
		t.Error("RollbackBinary without .bak should error")
	}
}

func TestRemoveBackupDeletesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "bridge")
	bak := fakeBinary(t, dir, "bridge.bak", "spare")

	if err := RemoveBackup(live, ".bak"); err != nil {
		t.Fatalf("RemoveBackup: %v", err)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf(".bak still present: err=%v", err)
	}
	// Second call is a no-op (idempotent) — boot housekeeping might
	// hit it twice if state.json is in a weird mid-transition state.
	if err := RemoveBackup(live, ".bak"); err != nil {
		t.Errorf("second RemoveBackup: %v (should be nil)", err)
	}
}
