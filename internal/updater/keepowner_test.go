package updater

import (
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestSaveStateAsRootKeepsTheMarkerOwner pins that SaveState gives its
// staged file to the owner of the update-state.json it replaces
// (fsutil.KeepOwner). A `sudo bridge update` beside a service install
// otherwise left a root-owned 0600 marker in the data dir, which the
// service cannot read at the boot that is meant to act on it. Driven
// through fsutil.SimulateRootForTest, since the chown itself needs root;
// fsutil's TestKeepOwnerAsRoot pins the real one.
func TestSaveStateAsRootKeepsTheMarkerOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	dir := t.TempDir()
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := SaveState(dir, State{RejectedVersion: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: StatePath(dir), UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != want {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
}
