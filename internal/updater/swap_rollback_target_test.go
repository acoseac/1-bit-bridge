//go:build !windows

package updater

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// R5 (cross-PR review of the #511-#540 audit batch): swapBinary opened with
// an unconditional os.Remove(bak) so os.Link wouldn't hit EEXIST. The shape
// it replaced began with os.Rename(dst, bak) — an atomic overwrite that
// left the previous .bak intact on failure — so the remove-first version
// gave up the rollback target before anything was committed. If the link
// failed (link-less FS, fs.protected_hardlinks) AND the rename fallback's
// first rename also failed, the install aborted with no .bak at all, and
// RollbackBinary hard-fails on a missing backup.
func TestSwapBinaryPreservesExistingBackupWhenSwapFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge")
	bak := dst + ".bak"

	if err := os.WriteFile(dst, []byte("CURRENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A good rollback target from a previous install cycle.
	if err := os.WriteFile(bak, []byte("PREVIOUS-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Force the hardlink path to fail the way a link-less filesystem does,
	// pushing us into swapBinaryViaRename...
	origLink := linkFunc
	linkFunc = func(string, string) error { return errors.New("operation not supported") }
	defer func() { linkFunc = origLink }()

	// ...and make that fallback's FIRST rename (dst -> bak, the vacate
	// step) fail too, so the install aborts before anything is committed.
	// This double failure is exactly what the fix protects: pre-fix bak
	// had already been unconditionally removed at the top of swapBinary,
	// so it was gone; post-fix it is still untouched here.
	origRename := renameFunc
	renameFunc = func(oldname, newname string) error {
		if filepath.Ext(newname) == ".bak" {
			return errors.New("rename refused")
		}
		return os.Rename(oldname, newname)
	}
	defer func() { renameFunc = origRename }()

	newBin := filepath.Join(dir, "staged")
	if err := os.WriteFile(newBin, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The swap is expected to fail; what matters is what it left behind.
	_ = swapBinary(dst, newBin, ".bak")

	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("rollback target destroyed by a failed swap: %v", err)
	}
	if string(got) != "PREVIOUS-GOOD" {
		t.Fatalf("backup = %q, want the untouched PREVIOUS-GOOD rollback target", got)
	}
	// And the operator must still have a bootable binary.
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("dst missing after a failed swap: %v", err)
	}
}

// TestSwapBinaryClearsStaleBackupOnEEXIST is the companion: the stale-bak
// clear must STILL happen when EEXIST is genuinely what blocks the link,
// otherwise every second install would fall through to the rename path.
func TestSwapBinaryClearsStaleBackupOnEEXIST(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge")
	bak := dst + ".bak"

	if err := os.WriteFile(dst, []byte("CURRENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("STALE"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "staged")
	if err := os.WriteFile(newBin, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	// First link attempt reports EEXIST (what a real os.Link does against
	// an existing bak); the retry after the clear must succeed.
	calls := 0
	origLink := linkFunc
	linkFunc = func(oldname, newname string) error {
		calls++
		if calls == 1 {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: fs.ErrExist}
		}
		return os.Link(oldname, newname)
	}
	defer func() { linkFunc = origLink }()

	if err := swapBinary(dst, newBin, ".bak"); err != nil {
		t.Fatalf("swap with a stale backup present: %v", err)
	}
	if calls < 2 {
		t.Fatalf("linkFunc called %d times; expected a retry after clearing the stale backup", calls)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Fatalf("dst = %q, want the new binary", got)
	}
	// bak must now hold the binary we replaced.
	gotBak, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("no rollback target after a successful swap: %v", err)
	}
	if string(gotBak) != "CURRENT" {
		t.Fatalf("bak = %q, want the replaced binary", gotBak)
	}
}
