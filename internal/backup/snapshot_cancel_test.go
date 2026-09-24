package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
)

// TestSnapshotStoppedMidVacuumLeavesNothing cancels a snapshot while its
// VACUUM INTO is copying. TestSnapshotFailureReapsPartialDir's pre-cancelled
// context never gets that far: the copy fails before it starts. Here the
// snapshot directory and the VACUUM's destination file both exist when the
// cancel lands, and the test pins what the serve-side ticker's silence on
// shutdown relies on: Snapshot returns the cancellation itself, and neither
// the directory nor the file survives.
//
// The uncancelled case is the control. The same park, let go without a
// cancel, has to produce a complete snapshot, so the failure in the other
// case is the cancel's and not the park's.
func TestSnapshotStoppedMidVacuumLeavesNothing(t *testing.T) {
	t.Run("cancelled mid-copy", func(t *testing.T) {
		dst, err := snapshotParkedMidVacuum(t, true)
		assertCancelledSnapshotLeftNothing(t, dst, err)
	})
	t.Run("let go without a cancel", func(t *testing.T) {
		dst, err := snapshotParkedMidVacuum(t, false)
		if err != nil {
			t.Fatalf("Snapshot let go without a cancel: %v", err)
		}
		if !backup.LooksLikeSnapshotDir(dst) {
			t.Errorf("Snapshot let go without a cancel wrote %s, which is not a complete snapshot", dst)
		}
	})
}

// snapshotParkedMidVacuum runs a Snapshot, parks its VACUUM INTO mid-copy,
// checks that the partial snapshot is on disk at that moment, then cancels
// the snapshot's context if cancelIt is set and lets the copy go. After a
// cancel it also checks that nothing is left under the backups root. It
// returns what Snapshot returned.
func snapshotParkedMidVacuum(t *testing.T, cancelIt bool) (string, error) {
	t.Helper()
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	backuptest.WriteSource(t, src.ManifestDB)

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	// Registered before the park, so it runs after the park's own cleanup
	// has let any held comparison go: on a failing path the snapshot must
	// still finish before t.TempDir removes the directory it is writing
	// into.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Errorf("the snapshot did not return within 10s of its cancel")
		}
	})
	park := backuptest.ParkVacuum(t)

	var dst string
	var err error
	go func() {
		defer close(finished)
		dst, err = backup.Snapshot(ctx, src)
	}()

	park.Wait(t)
	partial := onlySnapshotDir(t, filepath.Join(dataDir, backup.BackupsDirName))
	if !pathExists(t, filepath.Join(partial, backup.ManifestDBFileName)) {
		t.Fatalf("the VACUUM is copying, but its destination under %s does not exist yet", partial)
	}
	if cancelIt {
		cancel()
	}
	park.ReleaseUntil(t, finished)
	if cancelIt {
		// Nothing may be left under the root the partial snapshot was in.
		assertEmptyDir(t, filepath.Dir(partial))
	}
	return dst, err
}

// assertCancelledSnapshotLeftNothing checks what a Snapshot cancelled
// mid-copy returned: the cancellation itself, and no path.
func assertCancelledSnapshotLeftNothing(t *testing.T, dst string, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot cancelled mid-copy: err = %v, want context.Canceled", err)
	}
	if dst != "" {
		t.Errorf("Snapshot cancelled mid-copy returned the path %s", dst)
	}
}

// assertEmptyDir fails the test for every entry under dir.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		t.Errorf("a snapshot cancelled mid-copy left %s under %s", e.Name(), dir)
	}
}

// onlySnapshotDir returns the one directory under backupsRoot, failing the
// test unless there is exactly one.
func onlySnapshotDir(t *testing.T, backupsRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(backupsRoot)
	if err != nil {
		t.Fatalf("read backups root: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(backupsRoot, e.Name()))
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("backups root holds %d snapshot directories (%v), want exactly 1", len(dirs), dirs)
	}
	return dirs[0]
}
