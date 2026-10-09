package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runningPathRename is the rename Windows gives a rollback that
// replaces a mapped image: the destination still exists, so the call
// is refused, and a destination that does not exist is a real rename.
// The first call of a correct rollback moves dst to a new name.
func runningPathRename(calls *[][2]string) func(string, string) error {
	return func(oldpath, newpath string) error {
		*calls = append(*calls, [2]string{oldpath, newpath})
		if _, err := os.Stat(newpath); err == nil {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		return os.Rename(oldpath, newpath)
	}
}

func useRename(t *testing.T, fn func(string, string) error) {
	t.Helper()
	prev := renameFunc
	t.Cleanup(func() { renameFunc = prev })
	renameFunc = fn
}

func TestRollbackReplaceVacatesARunningPathBeforeRenamingBak(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 {
		t.Fatalf("renames = %v, want the vacate and then bak onto dst", calls)
	}
	if calls[0][0] != dst {
		t.Fatalf("first rename moved %s, want the running path", calls[0][0])
	}
	aside := calls[0][1]
	if aside == bak || !strings.Contains(filepath.Base(aside), ".rollback-") {
		t.Fatalf("aside = %s, want a .rollback- name that is not the backup", aside)
	}
	if calls[1][0] != bak || calls[1][1] != dst {
		t.Fatalf("second rename = %s -> %s, want the backup onto dst", calls[1][0], calls[1][1])
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("dst = %q, want the backup bytes", got)
	}
	if _, err := os.Stat(bak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup stat = %v, want consumed", err)
	}
	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aside stat = %v, want the unmapped leftover removed", err)
	}
}

func TestRollbackReplaceRestoresDstWhenTheSecondRenameFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	n := 0
	useRename(t, func(oldpath, newpath string) error {
		n++
		calls = append(calls, [2]string{oldpath, newpath})
		if n == 2 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		if _, err := os.Stat(newpath); err == nil {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		return os.Rename(oldpath, newpath)
	})

	err := replaceRunningBinary(dst, bak)
	if err == nil {
		t.Fatal("expected the second rename to fail")
	}
	if len(calls) != 3 || calls[2][0] != calls[0][1] || calls[2][1] != dst {
		t.Fatalf("renames = %v, want the aside renamed back onto dst", calls)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "RUNNING" {
		t.Fatalf("dst = %q, want the running bytes restored", got)
	}
	got, err = os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("backup = %q, want it left in place", got)
	}
}

func TestRollbackReplaceOfAMissingDestinationIsOneRename(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][0] != bak || calls[0][1] != dst {
		t.Fatalf("renames = %v, want one rename of the backup onto dst", calls)
	}
}

func TestRollbackReplaceLeavesALeftoverItCannotRemove(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(dir, "bridge.exe.rollback-stuck")
	if err := os.Mkdir(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "held"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("stuck leftover: %v", err)
	}
	if calls[0][1] == stuck {
		t.Fatal("vacate replaced the leftover that could not be removed")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("dst = %q", got)
	}
}
