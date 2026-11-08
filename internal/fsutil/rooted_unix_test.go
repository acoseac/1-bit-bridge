//go:build !windows

package fsutil

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// rootThenReplace opens a root on a new directory, moves that directory
// aside and makes another at its path: what the root reaches from then on is
// the directory moved aside, whatever the path names.
func rootThenReplace(t *testing.T) (r *os.Root, path, aside string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "waveforms")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	aside = path + ".aside"
	if err := os.Rename(path, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return r, path, aside
}

func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("%s holds %v (%v), want nothing made there", dir, left, err)
	}
}

// TestMkdirInMakesTheDirectoryThroughItsRoot: MkdirIn makes the directory in
// the directory its root opened, never at the path, and run as root gives it
// the owner of the directory it is made in, read through the root, as Mkdir
// does (backlog B652, round 2).
func TestMkdirInMakesTheDirectoryThroughItsRoot(t *testing.T) {
	r, path, aside := rootThenReplace(t)
	changes, restore := SimulateRootOwnersForTest(func(p string) (int, int) {
		switch filepath.Clean(p) {
		case path:
			return 4001, 4001
		case filepath.Join(path, "a"):
			return 4002, 4002
		}
		return 1, 1
	})
	defer restore()
	if err := MkdirIn(r, "a", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MkdirIn(r, filepath.Join("a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(aside, "a", "b")); err != nil || !info.IsDir() {
		t.Errorf("a/b in the directory the root opened: %v, want it made there", err)
	}
	requireEmptyDir(t, path)
	want := []OwnerChange{{Dst: filepath.Join(path, "a"), UID: 4001, GID: 4001},
		{Dst: filepath.Join(path, "a", "b"), UID: 4002, GID: 4002}}
	if got := changes(); !reflect.DeepEqual(got, want) {
		t.Errorf("owners given = %+v, want each directory its parent's: %+v", got, want)
	}
	if err := MkdirIn(r, "a", 0o700); !os.IsExist(err) {
		t.Errorf("MkdirIn over a directory there: %v, want os.ErrExist told apart", err)
	}
}

// TestKeepOwnerInLooksTheReplacedEntryUpThroughTheRoot: run as root, a file
// staged through a root keeps the owner of the entry it replaces there, or of
// its directory there for a new file, both looked up through the root.
func TestKeepOwnerInLooksTheReplacedEntryUpThroughTheRoot(t *testing.T) {
	r, path, aside := rootThenReplace(t)
	if err := os.WriteFile(filepath.Join(aside, "old.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	changes, restore := SimulateRootOwnersForTest(func(p string) (int, int) {
		switch filepath.Clean(p) {
		case path:
			return 4001, 4001
		case filepath.Join(path, "old.bin"):
			return 4999, 4999
		}
		return 1, 1
	})
	defer restore()
	for _, dst := range []string{"old.bin", "new.bin"} {
		f, err := r.OpenFile(dst+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := KeepOwnerIn(r, f, dst); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	want := []OwnerChange{{Dst: filepath.Join(path, "old.bin"), UID: 4999, GID: 4999},
		{Dst: filepath.Join(path, "new.bin"), UID: 4001, GID: 4001}}
	if got := changes(); !reflect.DeepEqual(got, want) {
		t.Errorf("owners given = %+v, want the replaced entry's, then the directory's: %+v", got, want)
	}
	requireEmptyDir(t, path)
}

// TestFsyncInRootSyncsTheFileItsRootHolds: the barrier reaches the file the
// root holds, with the path naming another directory that holds none, and a
// file the root does not hold is an error, never a sync of something else.
func TestFsyncInRootSyncsTheFileItsRootHolds(t *testing.T) {
	r, path, aside := rootThenReplace(t)
	if err := os.WriteFile(filepath.Join(aside, "curve.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := FsyncInRoot(r, "curve.bin"); err != nil {
		t.Errorf("FsyncInRoot of the file the root holds: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "other.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := FsyncInRoot(r, "other.bin"); err == nil {
		t.Error("FsyncInRoot reached a file only the path's directory holds")
	}
}
