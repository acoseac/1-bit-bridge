package auth

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAWriteAsRootKeepsTheTokenStoreOwner pins that a tokens.json write
// gives its staged file to the owner of the file it replaces
// (fsutil.KeepOwner), so a `sudo bridge pair` beside a service install
// leaves a file the service can still read. Driven through
// fsutil.SimulateRootForTest, since the chown itself needs root; fsutil's
// TestKeepOwnerAsRoot pins the real one.
func TestAWriteAsRootKeepsTheTokenStoreOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	path := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if _, _, err := s.Mint("phone"); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: path, UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != want {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
}
