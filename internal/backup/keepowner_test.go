package backup_test

import (
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestSnapshotAndRestoreAsRootKeepTheInstallOwner pins that a snapshot
// gives every directory and file it makes the install's owner (the backups
// root and the snapshot directory through fsutil.MkdirAll / fsutil.Mkdir,
// the copies and manifest.json through fsutil.KeepOwner, and the database
// VACUUM INTO writes through fsutil.Precreate), and that a restore gives
// each file it replaces the owner of the file it replaces. Before, `sudo
// bridge backup` left a root 0700 snapshot the service's prune could not
// remove, and `sudo bridge restore` left the config, the store, the token
// file and the TLS pair root's 0600, so the service could not start.
// Driven through fsutil.SimulateRootForTest, since the chown itself needs
// root; cmd/bridge's TestJobCLIsRunAsRootKeepTheInstallOwner runs both
// commands as root.
func TestSnapshotAndRestoreAsRootKeepTheInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()

	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	wantSnapshot := []string{
		filepath.Join(dataDir, backup.BackupsDirName),
		snap,
		filepath.Join(snap, backup.ManifestDBFileName),
		filepath.Join(snap, "tokens.json"),
		filepath.Join(snap, "server.crt"),
		filepath.Join(snap, "server.key"),
		filepath.Join(snap, "bridge.yaml"),
		filepath.Join(snap, backup.ManifestFile),
	}
	assertGivenAway(t, changes(), wantSnapshot)

	before := len(changes())
	if err := backup.Restore(snap, backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertGivenAway(t, changes()[before:], []string{
		src.ManifestDB, src.TokensJSON, src.ServerCert, src.ServerKey, src.BridgeYAML,
	})
}

// assertGivenAway requires exactly one change to 4242:4243 per path, in any
// order.
func assertGivenAway(t *testing.T, got []fsutil.OwnerChange, paths []string) {
	t.Helper()
	var dsts []string
	for _, c := range got {
		if c.UID != 4242 || c.GID != 4243 {
			t.Errorf("%s given to %d:%d, want 4242:4243", c.Dst, c.UID, c.GID)
		}
		dsts = append(dsts, c.Dst)
	}
	want := append([]string(nil), paths...)
	sort.Strings(dsts)
	sort.Strings(want)
	if len(dsts) != len(want) {
		t.Fatalf("given away %v, want exactly %v", dsts, want)
	}
	for i := range want {
		if dsts[i] != want[i] {
			t.Fatalf("given away %v, want exactly %v", dsts, want)
		}
	}
}
