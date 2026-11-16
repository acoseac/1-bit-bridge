package backup_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// TestACaptureNeverOpensADatabaseItDoesNotHold: a capture whose database's
// directory is not there as it starts takes the store's lifetime lock all
// the same. Another process makes that directory and the database in it,
// holding the lock exclusively as a restore does, after the capture began
// and before its hold: the capture is refused (manifest.ErrStoreRestoring)
// before its VACUUM INTO and before any snapshot is written, no backups
// folder made. A hold of this process's own would prove nothing: an opener
// here counts on this process's exclusive hold. Until B653 PR 2a's second
// round a capture that found no directory went on with no hold, and copied
// the database the restore was putting in place.
func TestACaptureNeverOpensADatabaseItDoesNotHold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db := filepath.Join(dir, backup.ManifestDBFileName)
	restoring := false
	t.Cleanup(backup.SetBeforeLiveStoreHoldForTest(func(p string) {
		if p != db || restoring {
			return
		}
		restoring = true
		restoreInAnotherProcess(t, db, backupLockHelperMakeEnv+"=1")
	}))
	c, err := backup.CaptureSnapshot(t.Context(), backup.Sources{DataDir: dir, ManifestDB: db})
	if c != nil {
		c.Release()
	}
	if !restoring {
		t.Fatal("the capture never reached its hold of the store")
	}
	if !errors.Is(err, manifest.ErrStoreRestoring) {
		t.Fatalf("a capture beside a restore that made its database = %v, %v; want it refused with ErrStoreRestoring", c, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, backup.BackupsDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused capture made the backups folder (%v)", err)
	}
}

// TestACaptureWithNoDataDirMakesNothing: a capture with no data dir is
// refused before it makes anything, the database's directory and the lock
// file the store's hold makes in it included: the data dir's check comes
// before the capture's hold, which makes the directory it holds.
func TestACaptureWithNoDataDirMakesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	c, err := backup.CaptureSnapshot(t.Context(), backup.Sources{ManifestDB: filepath.Join(dir, backup.ManifestDBFileName)})
	if c != nil {
		c.Release()
	}
	if err == nil || !strings.Contains(err.Error(), "DataDir is required") {
		t.Fatalf("a capture with no data dir = %v, want it refused for its data dir", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused capture made the database's directory (%v)", err)
	}
}

// TestTheFullRestoreFindsAnUnfinishedRestoreBeforeItsSnapshot: through the
// library's entry point (bridge restore asks the journal itself, earlier,
// which hides this), an unfinished restore of the database refuses the full
// restore (manifest.ErrRestoreIncomplete) before it looks at its snapshot: a
// snapshot that is not there is refused for the journal, never for its
// absence; one that is there is never read, its source's worker never run
// and its backups folder byte for byte as it was. Until B653 PR 2a's second
// round the journal was asked after the snapshot's admission.
func TestTheFullRestoreFindsAnUnfinishedRestoreBeforeItsSnapshot(t *testing.T) {
	snap := ladderTopSnapshot(t)
	backups := filepath.Dir(snap)
	gone := filepath.Join(backups, "not-there")
	var reads atomic.Int32
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(d string) {
		if d == snap || d == gone {
			reads.Add(1)
		}
	}))
	dst := t.TempDir()
	db := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, db)
	if err := os.WriteFile(manifest.RestoreJournalPath(db), []byte(`{"format":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, backups)
	for _, c := range []struct{ name, snap string }{
		{"a snapshot that is there", snap},
		{"a snapshot that is not there", gone},
	} {
		err := backup.RestoreWithOptions(c.snap, backup.Targets{ManifestDB: db}, backup.RestoreOptions{})
		if !errors.Is(err, manifest.ErrRestoreIncomplete) || !strings.Contains(err.Error(), "nothing was restored") {
			t.Errorf("%s, over an unfinished restore = %v; want ErrRestoreIncomplete, nothing restored", c.name, err)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("the source's worker ran %d time(s) before the journal refused the restore", n)
	}
	requireSameTree(t, backups, before)
}

// TestARestoreByTheVersionAnInstallReplacesChangesNothing: over an armed
// install whose previous version is this one, a full restore of the
// database is refused by the install marker's admission
// (updater.ErrRestoreByPreviousVersion) before it writes anything: the
// destination's player seal, left incomplete, is still owed, and the data
// directory, its marker included, is byte for byte as it was. Until B653 PR
// 2a's second round such a restore went on and recorded nothing, a migration
// of this version's needing no record.
func TestARestoreByTheVersionAnInstallReplacesChangesNothing(t *testing.T) {
	reservationStorage(t)
	snap := ladderTopSnapshot(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	liveStore(t, db)
	marker := updater.State{Status: "installing", TargetVersion: "99.0.0-next", PreviousVersion: version.ServerVersion,
		SwapStarted: true, AttemptedAt: time.Now().UTC()}
	if err := updater.SaveState(dir, marker); err != nil {
		t.Fatal(err)
	}
	// The marker's lock file, made as every disarm makes it, before the
	// state the refusal must leave.
	lock, err := updater.LockState(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	before := treeState(t, dir)
	err = backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: db, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)},
		backup.RestoreOptions{})
	if !errors.Is(err, updater.ErrRestoreByPreviousVersion) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("a restore by the version an armed install replaces = %v, want ErrRestoreByPreviousVersion, nothing restored", err)
	}
	requireSameTree(t, dir, before)
	requireSealStillOwed(t, dir)
}
