package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B692's round 2: an operator's rollback that the process's end stops
// after its record keeps the store from a downgrade across the restart.

// rollBackFromTheConsoleUntilStopped runs the console's rollback of r's
// install with restore in place of its rename, on a goroutine, and waits for
// it to end: restore ends it with runtime.Goexit where the process would end.
func rollBackFromTheConsoleUntilStopped(t *testing.T, r *refusedInstall, restore func(dst, ext string) error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = updater.New(updater.Options{}).Rollback(context.Background(),
			updater.InstallOptions{DataDir: r.dataDir, BinaryPath: r.bin, Force: true, RestoreBackup: restore})
	}()
	// Nothing holds it: it ends by itself, at restore's Goexit or its return.
	drainLoopOnCleanup(t, func() {}, done, "the rollback")
	select {
	case <-done:
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never ended")
	}
}

// TestAManualRollbackStoppedBeforeItsRenameKeepsTheStoreFromASecondRollback
// is the restart test of Codex's P1: the console's rollback ends after its
// record and before its rename, so the version installed (this build) is
// still the binary in place. A command of it then refuses to migrate the
// store; its serve's start records that it opened the data dir and lifts the
// veto in one write, its store's open migrates and its housekeeping confirms
// the install; and a second manual rollback refuses, the binaries as they
// are, where before round 2 it put the backup back over the migrated store.
func TestAManualRollbackStoppedBeforeItsRenameKeepsTheStoreFromASecondRollback(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	before, latest := storeVersion(t, r.dataDir), latestStoreVersion(t)
	rollBackFromTheConsoleUntilStopped(t, r, func(string, string) error {
		runtime.Goexit()
		return nil
	})
	if got, err := os.ReadFile(r.bin); err != nil || string(got) != "the new bridge binary" {
		t.Fatalf("the binary after a rollback stopped before its rename = %q (%v), want the install's", got, err)
	}

	// A command of the version installed, before any restart.
	migrated, _ := openStoreIn(t, r.dataDir, func() {})
	if err := <-migrated; !errors.Is(err, updater.ErrMigrationVetoed) {
		t.Fatalf("a command's migrating open after the stopped rollback = %v, want it vetoed", err)
	}
	if v := storeVersion(t, r.dataDir); v != before {
		t.Fatalf("the store is at %d, want %d: a command migrated it under the rollback's record", v, before)
	}

	// The restart: this build's serve, on settings it loads.
	stderr := r.bootAndStop(t)
	if v := storeVersion(t, r.dataDir); v != latest {
		t.Errorf("the store after the restart is at %d, want %d: serve's own open migrates", v, latest)
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil || st.DataDirOpenedBy != version.ServerVersion || st.RollbackStarted || st.Status != "installed" || st.MigrationVeto != "" {
		t.Fatalf("the marker after the restart = %+v (%v), want %s recorded as opening the data dir and the install confirmed; stderr=%s",
			st, err, version.ServerVersion, updaterLines(stderr))
	}

	// The second manual rollback.
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	err = updater.New(updater.Options{}).Rollback(t.Context(),
		updater.InstallOptions{DataDir: r.dataDir, BinaryPath: r.bin, Force: true})
	if !errors.Is(err, updater.ErrRollbackRefused) {
		t.Fatalf("a second manual rollback after the restart = %v, want it refused", err)
	}
	files.requireUnchanged(t)
}

// TestAManualRollbackStoppedAfterItsRenameIsFinishedByThePreviousVersionsBoot
// is Argon's mirror window: the backup is back and the rollback ended before
// it cleared its record. The start of the version put back finishes the
// record as a rollback done (the install cleared, the hold and the veto
// kept), and a command of the version rolled back from still refuses to
// migrate.
func TestAManualRollbackStoppedAfterItsRenameIsFinishedByThePreviousVersionsBoot(t *testing.T) {
	r := newRefusedInstall(t, "")
	rollBackFromTheConsoleUntilStopped(t, r, func(dst, ext string) error {
		if err := updater.RollbackBinary(dst, ext); err != nil {
			return err
		}
		runtime.Goexit()
		return nil
	})
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Fatalf("the binary reports %q, want %s put back", got, previousVersion)
	}
	var stderr bytes.Buffer
	if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, nil); err != nil {
		t.Fatalf("the previous version's housekeeping = %v; stderr=%s", err, stderr.String())
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil || st.RollbackStarted || updater.InstallArmed(st) || st.TargetVersion != "" ||
		st.RejectedVersion != version.ServerVersion || st.MigrationVeto != version.ServerVersion {
		t.Errorf("the marker after the previous version's boot = %+v (%v), want the rollback finished: the install cleared, "+
			"the hold and the veto of %s kept", st, err, version.ServerVersion)
	}
	if _, err := updater.DisarmBeforeMigrating(r.dataDir, version.ServerVersion); !errors.Is(err, updater.ErrMigrationVetoed) {
		t.Errorf("a command of the version rolled back from = %v, want its migration vetoed", err)
	}
}
