package atomicwrite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestWriteBytesAsRootKeepsTheInstallOwner pins that WriteBytes gives the
// directory it creates and the file it writes the install's owner
// (fsutil.MkdirAll, fsutil.KeepOwner), so a `sudo bridge scan` over a
// service install leaves an artwork cache the service can read and add to.
// Driven through fsutil.SimulateRootForTest, since the chown itself needs
// root; fsutil's root tests pin the real one, and cmd/bridge's
// TestJobCLIsRunAsRootKeepTheInstallOwner the whole scan.
func TestWriteBytesAsRootKeepsTheInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dir := filepath.Join(t.TempDir(), "artwork")
	path := filepath.Join(dir, "local-x-500.jpg")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := WriteBytes(path, []byte{0xFF, 0xD8, 0xFF}, ".scan-*.jpg.tmp"); err != nil {
		t.Fatal(err)
	}
	want := []fsutil.OwnerChange{
		{Dst: dir, UID: 4242, GID: 4243},  // the cache directory it made
		{Dst: path, UID: 4242, GID: 4243}, // the cover it wrote
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
	if b, err := os.ReadFile(path); err != nil || len(b) != 3 {
		t.Fatalf("the cover was not written: %v %v", b, err)
	}
}
