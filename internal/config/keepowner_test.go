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
// service can still read at its next start. Driven through
// fsutil.SimulateRootForTest, since the chown itself needs root; fsutil's
// TestKeepOwnerAsRoot pins the real one.
func TestSaveAsRootKeepsTheConfigOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	var c Config
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: path, UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != want {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
}
