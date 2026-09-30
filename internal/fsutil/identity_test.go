package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// replaceDir moves the directory at dir aside and makes another at its path,
// the shape a clean unmount leaves at a mountpoint. It returns where the
// first one went.
func replaceDir(t *testing.T, dir string) (moved string) {
	t.Helper()
	moved = dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return moved
}

// newDir makes an empty directory of its own under a test directory.
func newDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "variants")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDirIdentitySeesAnotherDirectoryAtThePath — an identity taken with
// DirIdentity is the directory's, not the path's: another directory made
// at its path is not the same file, the first one, moved, still is, and a
// directory taken twice is the same file. On every platform. The identity
// taken first is compared with nothing before the replacement: on Windows a
// comparison reads an os.Stat's identity, and fixes it, at that moment, so
// an earlier one would let an os.Stat pass here.
func TestDirIdentitySeesAnotherDirectoryAtThePath(t *testing.T) {
	dir := newDir(t)
	before, err := DirIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	moved := replaceDir(t, dir)
	after, err := DirIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the directory made at the path compares as the same file as the one that was there")
	}
	elsewhere, err := DirIdentity(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, elsewhere) {
		t.Error("the directory that was at the path, moved, no longer compares as itself")
	}
	again, err := DirIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(after, again) {
		t.Error("DirIdentity of one directory, taken twice, is not the same file")
	}
}

// TestDirIdentityRefusesWhatIsNoDirectory — a path that names a file, or
// nothing, answers an error rather than an identity.
func TestDirIdentityRefusesWhatIsNoDirectory(t *testing.T) {
	dir := newDir(t)
	file := filepath.Join(dir, "a.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DirIdentity(file); err == nil {
		t.Error("DirIdentity of a file answered an identity")
	}
	if _, err := DirIdentity(filepath.Join(dir, "gone")); err == nil {
		t.Error("DirIdentity of a missing path answered an identity")
	}
}

// TestOSStatLeavesTheWindowsIdentityToTheComparison — the premise
// DirIdentity exists for: on Windows, os.Stat of a plain directory leaves
// its identity to be read when os.SameFile asks, from whatever the path
// names then, so a stat taken before the directory was replaced and one
// taken after compare as the same file. On POSIX they do not. The day Go
// reads the identity at the stat on Windows too, this fails there.
func TestOSStatLeavesTheWindowsIdentityToTheComparison(t *testing.T) {
	dir := newDir(t)
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	replaceDir(t, dir)
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := os.SameFile(before, after), runtime.GOOS == "windows"; got != want {
		t.Errorf("on %s os.SameFile(stat before, stat after a replacement) = %v, want %v", runtime.GOOS, got, want)
	}
}
