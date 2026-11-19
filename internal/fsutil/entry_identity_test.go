package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeEntry makes a regular file at p holding body.
func writeEntry(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEntryIdentityIsTheFilesAtItsRead — an identity EntryIdentity reads is
// the file's at the read, not the path's: a file made at the path after it is
// another file, the first one, moved, is still itself, and one file read twice
// is one file, on every platform. Nothing is compared before the replacement,
// so an identity left to the comparison (an os.Lstat's on Windows,
// TestOSStatLeavesTheWindowsIdentityToTheComparison's premise) could not pass.
func TestEntryIdentityIsTheFilesAtItsRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.bak")
	writeEntry(t, p, "the backup")
	before, err := EntryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Mode().IsRegular() {
		t.Errorf("a regular file reads as %v", before.Mode())
	}
	moved := p + ".moved"
	if err := os.Rename(p, moved); err != nil {
		t.Fatal(err)
	}
	writeEntry(t, p, "another file at its path")
	after, err := EntryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the file made at the path compares as the one that was there")
	}
	elsewhere, err := EntryIdentity(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, elsewhere) {
		t.Error("the file that was at the path, moved, no longer compares as itself")
	}
	again, err := EntryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(after, again) {
		t.Error("one file read twice is not the same file")
	}
}

// TestEntryIdentityReadsALinkAsALink — a symbolic link at the path is read as
// the link, never followed: no regular file, and not its target. Where the
// host makes no symbolic link without a privilege (Windows), it skips.
func TestEntryIdentityReadsALinkAsALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeEntry(t, target, "the target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("this host makes no symbolic link without a privilege: %v", err)
		}
		t.Fatal(err)
	}
	fi, err := EntryIdentity(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 || fi.Mode().IsRegular() {
		t.Errorf("the link reads as %v, want a symbolic link and no regular file", fi.Mode())
	}
	ti, err := EntryIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(fi, ti) {
		t.Error("the link compares as its target: it was followed")
	}
}

// TestEntryIdentityOfNothingIsNotExist — a path where nothing is answers an
// error matching fs.ErrNotExist.
func TestEntryIdentityOfNothingIsNotExist(t *testing.T) {
	fi, err := EntryIdentity(filepath.Join(t.TempDir(), "nothing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("EntryIdentity of nothing = %v, %v; want an error matching fs.ErrNotExist", fi, err)
	}
}
