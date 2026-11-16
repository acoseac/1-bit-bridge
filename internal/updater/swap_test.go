//go:build !windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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

	if err := swapBinary(live, newBin, ".bak", nil); err != nil {
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

func TestSwapBinaryFallsBackToRenameWhenHardlinkUnsupported(t *testing.T) {
	// B35: swapBinary keeps dst present by hardlinking dst→bak first, but
	// filesystems without hardlink support (some FUSE/FAT/network mounts)
	// or a cross-device .bak make os.Link fail. There it must fall back to
	// the two-rename swap and still land NEW at live + OLD at .bak.
	orig := linkFunc
	linkFunc = func(oldname, newname string) error {
		return errors.New("simulated link-unsupported filesystem")
	}
	t.Cleanup(func() { linkFunc = orig })

	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "OLD")
	newBin := fakeBinary(t, dir, "bridge.new", "NEW")

	if err := swapBinary(live, newBin, ".bak", nil); err != nil {
		t.Fatalf("swapBinary (fallback path): %v", err)
	}
	got, _ := os.ReadFile(live)
	if string(got) != "NEW" {
		t.Errorf("live binary = %q, want NEW (fallback path)", string(got))
	}
	bak, _ := os.ReadFile(live + ".bak")
	if string(bak) != "OLD" {
		t.Errorf(".bak = %q, want OLD (fallback path)", string(bak))
	}
}

func TestSwapBinaryFallsBackToCopyOnCrossDeviceRename(t *testing.T) {
	// EXDEV: the scratch dir and the install path live on different
	// filesystems (e.g. /var vs /usr), so os.Rename(newBinary, dst) fails.
	// placeNewBinary must copy newBinary into dst's own directory and
	// atomically rename there — landing NEW at dst while bak still holds
	// OLD (the rollback contract survives a cross-device swap). The new
	// binary is in a directory of its own, as the scratch dir is, so the
	// swap stages it (backlog B898), and the copy is shown to have run: the
	// rename was refused, and the bytes at dst are another file than the
	// one staged from.
	orig := renameFunc
	refused := 0
	renameFunc = func(oldname, newname string) error {
		refused++
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFunc = orig })

	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "OLD")
	newBin := fakeBinary(t, t.TempDir(), "bridge", "NEW")
	source, err := os.Stat(newBin)
	if err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(live, newBin, ".bak", nil); err != nil {
		t.Fatalf("swapBinary (EXDEV copy fallback): %v", err)
	}
	if installed, err := os.Stat(live); err != nil || refused == 0 || os.SameFile(installed, source) {
		t.Errorf("the staging's renames refused %d, the binary %v (%v): want the cross-device copy to have made it", refused, installed, err)
	}
	if got, _ := os.ReadFile(live); string(got) != "NEW" {
		t.Errorf("live binary = %q, want NEW (copy fallback)", string(got))
	}
	if bak, _ := os.ReadFile(live + ".bak"); string(bak) != "OLD" {
		t.Errorf(".bak = %q, want OLD (rollback contract under EXDEV)", string(bak))
	}
	// Source consumed after the cross-device copy.
	if _, err := os.Stat(newBin); !os.IsNotExist(err) {
		t.Errorf("newBinary should be removed after copy; stat err = %v", err)
	}
	// Installed binary must carry the executable bit (copyAndRename chmods it).
	fi, err := os.Stat(live)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("live mode = %v, want executable (owner-exec bit set)", fi.Mode().Perm())
	}
}

// TestASwapOfTheCheckedCandidateGivesItTheBinarysMode: the settings check's
// copy beside the binary is the swap's staged file as it is (backlog B898),
// and still takes the mode the binary has, whatever mode it was left with,
// under the runbook's umask as at 0.
func TestASwapOfTheCheckedCandidateGivesItTheBinarysMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o750} {
		t.Run(mode.String(), func(t *testing.T) {
			old := syscall.Umask(0o027)
			t.Cleanup(func() { syscall.Umask(old) })
			dir := t.TempDir()
			live := fakeBinary(t, dir, "bridge", "OLD")
			if err := os.Chmod(live, mode); err != nil {
				t.Fatal(err)
			}
			checked := fakeBinary(t, dir, checkStagePrefix+"1", "NEW")
			if err := os.Chmod(checked, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := swapBinary(live, checked, ".bak", nil); err != nil {
				t.Fatalf("swapBinary: %v", err)
			}
			fi, err := os.Stat(live)
			if err != nil || fi.Mode().Perm() != mode || readFile(t, live) != "NEW" {
				t.Errorf("the binary = %v (%v), want NEW at %v", fi, err, mode)
			}
			if _, err := os.Stat(checked); !os.IsNotExist(err) {
				t.Errorf("the checked candidate is still beside the binary: %v", err)
			}
		})
	}
}

func TestSwapBinaryOverwritesStaleBak(t *testing.T) {
	// A previous install cycle left a .bak. The new install must
	// replace it (we keep at most one .bak, never an accumulation).
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "v2")
	_ = fakeBinary(t, dir, "bridge.bak", "very-old")
	newBin := fakeBinary(t, dir, "bridge.new", "v3")

	if err := swapBinary(live, newBin, ".bak", nil); err != nil {
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
