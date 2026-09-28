package manifest

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestOpenStoreAsRootKeepsTheInstallOwner pins that a store OpenStore
// creates takes the install's owner: its directory (fsutil.MkdirAll) and
// its database file, created empty before SQLite opens it
// (fsutil.Precreate), since SQLite gives the main file no owner. SQLite
// gives the -wal and -shm the database file's owner by itself when it runs
// as root, which the root test measures. An existing store is left alone.
// Driven through fsutil.SimulateRootForTest, since the chown itself needs
// root; cmd/bridge's TestJobCLIsRunAsRootKeepTheInstallOwner runs it as
// root.
func TestOpenStoreAsRootKeepsTheInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := DefaultDBPath(dir)
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// The precreated empty file is a database SQLite migrated: the store
	// works.
	if err := s.UpsertTrack(context.Background(), &Track{Path: "a.flac"}); err != nil {
		t.Fatalf("a store opened over a precreated file cannot write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := []fsutil.OwnerChange{
		{Dst: dir, UID: 4242, GID: 4243},  // the data directory it made
		{Dst: path, UID: 4242, GID: 4243}, // the database it created
	}
	got := changes()
	if len(got) != len(want) {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
		}
	}

	// Opening it again creates nothing, so nothing is given away.
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := changes(); len(got) != len(want) {
		t.Fatalf("reopening an existing store gave away %+v", got[len(want):])
	}
}
