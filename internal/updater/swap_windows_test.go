//go:build windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// swapBinary's rename-trick has to work against an unwritten file
// in the success path, and roll back cleanly when the second rename
// fails. The SCM-stop coordination is integration-tested separately
// (needs a real Service installed); these tests hit the file-system
// pieces without standing up SCM.

func TestSwapWindows_RenameTrick(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	new := filepath.Join(dir, "extracted-bridge.exe")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(new, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(dst, new, ".bak", nil); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}

	// dst now has NEW contents.
	got, _ := os.ReadFile(dst)
	if string(got) != "NEW" {
		t.Errorf("post-swap dst = %q, want NEW", string(got))
	}
	// .bak holds OLD.
	bak, _ := os.ReadFile(dst + ".bak")
	if string(bak) != "OLD" {
		t.Errorf("post-swap .bak = %q, want OLD", string(bak))
	}
	// extracted source no longer present.
	if _, err := os.Stat(new); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("extracted source should be consumed; stat = %v", err)
	}
}

func TestSwapWindows_RollbackOverwritesExistingBak(t *testing.T) {
	// A prior cycle left a stale .bak. swapBinary should overwrite
	// it on the dst → bak rename — never accumulate multiple
	// .bak files.
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	new := filepath.Join(dir, "extracted-bridge.exe")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("STALE-BAK"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(new, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(dst, new, ".bak", nil); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	bakBytes, _ := os.ReadFile(bak)
	if string(bakBytes) != "OLD" {
		t.Errorf("post-swap .bak = %q, want OLD (stale .bak should have been overwritten)",
			string(bakBytes))
	}
}

func TestRollbackBinary_Windows(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("BROKEN-NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RollbackBinary(dst, ".bak"); err != nil {
		t.Fatalf("RollbackBinary: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "KNOWN-GOOD" {
		t.Errorf("post-rollback dst = %q, want KNOWN-GOOD", string(got))
	}
	// .bak is consumed by the rollback.
	if _, err := os.Stat(bak); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".bak should be consumed; stat = %v", err)
	}
}

func TestRollbackBinary_MissingBakErrors(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	if err := os.WriteFile(dst, []byte("CURRENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RollbackBinary(dst, ".bak"); err == nil {
		t.Errorf("RollbackBinary with missing .bak: expected error, got nil")
	}
}

func TestRemoveBackup_Windows(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(bak, []byte("STALE"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveBackup(dst, ".bak"); err != nil {
		t.Fatalf("RemoveBackup: %v", err)
	}
	if _, err := os.Stat(bak); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("RemoveBackup should have removed .bak; stat = %v", err)
	}

	// Idempotent on missing .bak.
	if err := RemoveBackup(dst, ".bak"); err != nil {
		t.Errorf("RemoveBackup idempotent: %v", err)
	}
}
