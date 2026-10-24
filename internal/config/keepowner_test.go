package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestSaveAsRootKeepsTheConfigOwner pins that Save gives its staged file
// to the owner of the bridge.yaml it replaces (fsutil.KeepOwner), so a
// `sudo bridge library add` beside a service install leaves a config the
// service can still read at its next start; and that the lock beside it,
// which the first writer creates (LockFile), takes the directory's owner
// too (fsutil.OpenLockFile's Precreate), or the service could not open it
// for its next save. Driven through fsutil.SimulateRootForTest, since the
// chown itself needs root; fsutil's TestKeepOwnerAsRoot pins the real one.
func TestSaveAsRootKeepsTheConfigOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	var c Config
	if err := saveAt(&c, path); err != nil {
		t.Fatal(err)
	}
	want := []fsutil.OwnerChange{
		{Dst: filepath.Join(filepath.Dir(path), LockFileName(path)), UID: 4242, GID: 4243},
		{Dst: path, UID: 4242, GID: 4243},
	}
	if got := changes(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
}
