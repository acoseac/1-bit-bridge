//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestEntryIdentitysHandleRefusesNoRenameOrRemoval — the handle EntryIdentity
// reads through (openEntry) asks no access, so it is counted in no sharing: a
// rename and a removal of its file land while it is open, where a handle
// os.Open makes, which shares no deletion, refuses the rename (the premise,
// asked first). The case holds the handle open across the rename and the
// removal, and the identity read through it after the rename is the moved
// file's.
func TestEntryIdentitysHandleRefusesNoRenameOrRemoval(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.exe.bak")
	writeEntry(t, p, "the backup")
	moved := p + ".moved"
	reader, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	premise := os.Rename(p, moved)
	_ = reader.Close()
	if !errors.Is(premise, windows.ERROR_SHARING_VIOLATION) {
		if premise == nil {
			_ = os.Rename(moved, p)
		}
		t.Fatalf("the premise: a rename of a file an os.Open handle holds answered %v, want a sharing violation", premise)
	}

	h, err := openEntry(p)
	if err != nil {
		t.Fatal(err)
	}
	renamed := os.Rename(p, moved)
	fi, statErr := h.Stat()
	closed := h.Close()
	if renamed != nil {
		t.Fatalf("a rename while EntryIdentity's handle is open answered %v, want it to land", renamed)
	}
	if statErr != nil || closed != nil {
		t.Fatalf("the read through the handle answered %v, its close %v", statErr, closed)
	}
	m, err := EntryIdentity(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, m) {
		t.Error("the identity read through the handle is not the moved file's")
	}

	h, err = openEntry(moved)
	if err != nil {
		t.Fatal(err)
	}
	removed := os.Remove(moved)
	_ = h.Close()
	if removed != nil {
		t.Errorf("a removal while EntryIdentity's handle is open answered %v, want it to land", removed)
	}
	if _, err := os.Lstat(moved); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file is there after its removal and the handle's close: %v", err)
	}
}

// TestEntryIdentityReadsBesideAHandleThatSharesNothing — a file another handle
// holds asking to read and write and sharing nothing is read all the same,
// its identity the file's: an open that asks no access is checked against no
// handle's sharing. The premise, asked first: a read of the file's bytes
// beside that handle is refused (B898FU2's rule for a held file's read).
func TestEntryIdentityReadsBesideAHandleThatSharesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bridge.exe.bak")
	writeEntry(t, p, "the backup")
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	letGo := func() {
		if held {
			held = false
			_ = windows.CloseHandle(holder)
		}
	}
	defer letGo()
	if _, err := os.ReadFile(p); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("the premise: a read beside a handle that shares nothing answered %v, want a sharing violation", err)
	}
	fi, err := EntryIdentity(p)
	letGo()
	if err != nil {
		t.Fatalf("EntryIdentity beside a handle that shares nothing answered %v, want the file's identity", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, opened) {
		t.Errorf("the identity read beside the holder (%v) is not the file's", fi.Mode())
	}
}

// TestEntryIdentityReadsAJunctionAsNoRegularFile — a directory junction at the
// path, a reparse point that is no symbolic link, is read as the entry it is:
// no regular file and no directory, and not the directory it leads to.
func TestEntryIdentityReadsAJunctionAsNoRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "junction")
	makeJunction(t, target, link)
	fi, err := EntryIdentity(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().IsRegular() || fi.IsDir() {
		t.Errorf("the junction reads as %v, want neither a regular file nor a directory", fi.Mode())
	}
	ti, err := EntryIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(fi, ti) {
		t.Error("the junction compares as its target: it was followed")
	}
}
