package tls

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestGenerateAsRootKeepsThePairOwner pins that both halves of a minted
// pair give their staged file to the owner of the file they replace
// (fsutil.KeepOwner). A `sudo bridge cert rotate` beside a service install
// otherwise left a root-owned 0600 key, and the service could not load its
// pair at the next start. Driven through fsutil.SimulateRootForTest, since
// the chown itself needs root; fsutil's TestKeepOwnerAsRoot pins the real
// one.
func TestGenerateAsRootKeepsThePairOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := GenerateWithOptions(certPath, keyPath, GenerateOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []fsutil.OwnerChange{
		{Dst: certPath, UID: 4242, GID: 4243},
		{Dst: keyPath, UID: 4242, GID: 4243},
	}
	got := changes()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("owner changes = %+v, want %+v", got, want)
	}
}
