package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B613's round 4: the veto of a rolled-back version's store
// migrations kept past the housekeeping of the version put back, the boot's
// own housekeeping under the install marker's lock, a wait for that lock
// that ends with serve's stop, and a confirmation only on the install's own
// record.

// requireTheLatestSchema checks whether the store in dataDir has what this
// build's last migration adds, present or not (latestSchemaPresent): a store
// genuinely older than this build lacks it.
func requireTheLatestSchema(t *testing.T, dataDir string, present bool) {
	t.Helper()
	if got := latestSchemaPresent(t, dataDir); got != present {
		t.Errorf("the store has v62's table: %v, want %v", got, present)
	}
}

// latestSchemaPresent reports whether the store in dataDir has what this
// build's last migration adds (v62: the waveform_regen_failures table,
// backlog B652; v61's tracks.correction_key, backlog B489, until then, and
// v60's sample-kind stat columns and index before it). A new migration
// names its own here.
func latestSchemaPresent(t *testing.T, dataDir string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var tables int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'waveform_regen_failures'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	return tables == 1
}

// openStoreIn opens dataDir's store on a goroutine of the test's, as a
// command of this version would, and drains it on cleanup: unhold, which
// lets go of whatever the test holds the open in, is the drain's cancel,
// since an open takes no context. It returns where its answer comes and the
// channel closed once it has returned.
func openStoreIn(t *testing.T, dataDir string, unhold func()) (opened <-chan error, done <-chan struct{}) {
	t.Helper()
	answer, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		s, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
		if err == nil {
			err = s.Close()
		}
		answer <- err
	}()
	drainLoopOnCleanup(t, unhold, finished, "the store's open")
	return answer, finished
}

// TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots
// is the later arrival (round 4): a command of this version (a `bridge
// doctor`, a scan) opens the store before the early rollback and is held
// before the store's migration lock; the rollback puts the previous version
// back; the previous version starts (its admission, a migration of its own,
// its housekeeping, which finishes the rollback's record); and only then
// does the command reach its migration, which must refuse, leaving the store
// as the previous version knows it. The previous version's own migration is
// not refused, and a start of this version that gets past its settings lifts
// the veto, after which the store migrates.
func TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	r.withAnOlderStore(t)
	before := storeVersion(t, r.dataDir)
	db := manifest.DefaultDBPath(r.dataDir)

	held, release := newOnceCloser(), newOnceCloser()
	t.Cleanup(manifest.SetBeforeMigrationLockForTest(func(path string) {
		if path != db {
			return
		}
		held.close()
		<-release.ch
	}))
	migrated, _ := openStoreIn(t, r.dataDir, release.close)
	select {
	case <-held.ch:
	case <-time.After(withinTheTest):
		t.Fatal("the command's store open never reached the migration lock")
	}

	if code, stderr := r.serve(t, serveOpts{}); code != supervision.RestartExitCode {
		t.Fatalf("the early rollback = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr)
	}
	// The previous version's start: its admission, a migration check of its
	// own store open, and its housekeeping.
	if _, err := updater.AdmitBoot(t.Context(), r.dataDir, previousVersion, nil); err != nil {
		t.Fatalf("the previous version's admission = %v", err)
	}
	if _, err := updater.DisarmBeforeMigrating(r.dataDir, previousVersion); err != nil {
		t.Errorf("the previous version's own migration = %v, want it to go on", err)
	}
	var housekeeping bytes.Buffer
	if err := bootHousekeeping(t.Context(), &housekeeping, r.dataDir, r.bin, previousVersion, nil); err != nil {
		t.Fatalf("the previous version's housekeeping = %v; stderr=%s", err, housekeeping.String())
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || updater.InstallArmed(st) || st.RejectedVersion != version.ServerVersion {
		t.Fatalf("the marker after the previous version's start = %+v (%v), want the rollback finished: no install armed, the hold kept", st, err)
	}

	release.close()
	if err := <-migrated; !errors.Is(err, updater.ErrMigrationVetoed) {
		t.Fatalf("the command's migration after the previous version's start = %v, want it vetoed", err)
	}
	if v := storeVersion(t, r.dataDir); v != before {
		t.Errorf("the store is at %d, want %d: the command migrated it past the version put back", v, before)
	}
	requireTheLatestSchema(t, r.dataDir, false)

	// This version's own start on settings it loads lifts the veto.
	if res, err := updater.AdmitBoot(t.Context(), r.dataDir, version.ServerVersion, nil); err != nil || !res.VetoLifted {
		t.Fatalf("this version's admission = %+v, %v; want the veto lifted", res, err)
	}
	reopened, _ := openStoreIn(t, r.dataDir, func() {})
	if err := <-reopened; err != nil {
		t.Fatalf("the store open after this version's admission = %v", err)
	}
	requireTheLatestSchema(t, r.dataDir, true)
}

// TestAMigrationThatHoldsTheMarkersLockFirstStopsTheBootRollback: a command
// of the version an install put in place (this build) migrates the store,
// holding the install marker's lock while it records that it opened the data
// dir, and the housekeeping of a start that did not come up as that version
// (BootInstallFailed) waits for that lock, reads the record, and puts no
// backup back: the store migrated, the binaries as the install left them.
func TestAMigrationThatHoldsTheMarkersLockFirstStopsTheBootRollback(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	latest := latestStoreVersion(t)
	files := watch(t, r.bin, r.bak)

	held, release := newOnceCloser(), newOnceCloser()
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(string) {
		held.close()
		<-release.ch
	}))
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))

	ctx, cancel := context.WithCancel(context.Background())
	migrated, _ := openStoreIn(t, r.dataDir, release.close)
	select {
	case <-held.ch:
	case <-time.After(withinTheTest):
		t.Fatal("the migration never reached its record")
	}

	var stderr safeBuffer
	housekept, housekeepingDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(housekeepingDone)
		housekept <- bootHousekeeping(ctx, &stderr, r.dataDir, r.bin, previousVersion, nil)
	}()
	drainLoopOnCleanup(t, cancel, housekeepingDone, "the boot's housekeeping")
	t.Cleanup(release.close)
	select {
	case <-waits:
	case <-housekeepingDone:
		t.Fatalf("the housekeeping did not wait for the migration that held the marker's lock (%v); stderr=%s", <-housekept, stderr.String())
	case <-time.After(withinTheTest):
		t.Fatalf("the housekeeping never waited for the marker's lock; stderr=%s", stderr.String())
	}
	release.close()
	if err := <-migrated; err != nil {
		t.Fatalf("the migration = %v", err)
	}
	if err := <-housekept; err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
	}
	files.requireUnchanged(t)
	if v := storeVersion(t, r.dataDir); v != latest {
		t.Errorf("the store is at %d, want %d", v, latest)
	}
	if !strings.Contains(updaterLines(stderr.String()), "opened the data dir") {
		t.Errorf("the housekeeping did not say why it left the backup; stderr=%s", stderr.String())
	}
}

// TestABootRollbackThatHoldsTheMarkersLockFirstStopsTheMigration: the boot's
// housekeeping holds the install marker's lock from its read through the
// backup's rename (BootInstallFailed), and a command of the version the
// rename replaces, running from its own image, waits for that lock and then
// refuses to migrate the store under the binary put back. The binary is
// swapped back and the store not migrated: never both.
func TestABootRollbackThatHoldsTheMarkersLockFirstStopsTheMigration(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	before := storeVersion(t, r.dataDir)

	atRestore, release := newOnceCloser(), newOnceCloser()
	restore := func(binaryPath, suffix string) error {
		atRestore.close()
		<-release.ch
		return updater.RollbackBinary(binaryPath, suffix)
	}
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))

	ctx, cancel := context.WithCancel(context.Background())
	var stderr safeBuffer
	housekept, housekeepingDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(housekeepingDone)
		housekept <- bootHousekeeping(ctx, &stderr, r.dataDir, r.bin, previousVersion, restore)
	}()
	drainLoopOnCleanup(t, cancel, housekeepingDone, "the boot's housekeeping")
	select {
	case <-atRestore.ch:
	case <-housekeepingDone:
		t.Fatalf("the housekeeping ended before its rename (%v); stderr=%s", <-housekept, stderr.String())
	case <-time.After(withinTheTest):
		t.Fatal("the housekeeping never reached its rename")
	}

	migrated, migrationDone := openStoreIn(t, r.dataDir, release.close)
	t.Cleanup(release.close)
	select {
	case <-waits:
	case <-migrationDone:
		t.Fatalf("the migration did not wait for the housekeeping that held the marker's lock: %v", <-migrated)
	case <-time.After(withinTheTest):
		t.Fatal("the migration never waited for the marker's lock")
	}
	release.close()
	if err := <-housekept; err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
	}
	if err := <-migrated; !errors.Is(err, updater.ErrMigrationVetoed) {
		t.Fatalf("the migration after the boot rollback = %v, want it vetoed", err)
	}
	if v := storeVersion(t, r.dataDir); v != before {
		t.Errorf("the store is at %d, want %d: the migration ran under the binary put back", v, before)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s: the backup was not put back", got, previousVersion)
	}
}

// TestTheBootLeavesTheInstallWhereItCannotHoldTheMarkersLock: the boot's
// housekeeping decides, records and swaps only holding the install marker's
// lock, so one that cannot hold it (another holder past the wait, no lock to
// give, a filesystem that keeps none, a failure) leaves the marker and both
// binaries as they are, says why, and returns its refusal.
func TestTheBootLeavesTheInstallWhereItCannotHoldTheMarkersLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		lay  func(t *testing.T, dataDir string)
	}{
		{"held by another open", func(t *testing.T, dataDir string) {
			l, err := updater.LockState(t.Context(), dataDir)
			if err != nil || !l.Held() {
				t.Fatalf("hold the marker's lock: %+v, %v", l, err)
			}
			t.Cleanup(l.Release)
		}},
		{"no lock to give", func(t *testing.T, _ string) {
			t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.ENOLCK }))
		}},
		{"a filesystem that keeps none", func(t *testing.T, _ string) {
			t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.EOPNOTSUPP }))
		}},
		{"an I/O error", func(t *testing.T, _ string) {
			t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.EIO }))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(updater.SetStateLockWaitForTest(100 * time.Millisecond))
			r := newRefusedInstall(t, "")
			tc.lay(t, r.dataDir)
			files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
			var stderr bytes.Buffer
			if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, nil); err == nil {
				t.Errorf("the housekeeping without the marker's lock = nil, want its refusal; stderr=%s", stderr.String())
			}
			files.requireUnchanged(t)
			if !strings.Contains(updaterLines(stderr.String()), "left as it is") {
				t.Errorf("the housekeeping did not say it left the install; stderr=%s", stderr.String())
			}
		})
	}
}

// heldMarkersLock holds dataDir's install marker's lock for the rest of the
// test, with LockState's wait at d, and returns where a waiter's first wait
// is signalled.
func heldMarkersLock(t *testing.T, dataDir string, d time.Duration) <-chan struct{} {
	t.Helper()
	l, err := updater.LockState(t.Context(), dataDir)
	if err != nil || !l.Held() {
		t.Fatalf("hold the marker's lock: %+v, %v", l, err)
	}
	t.Cleanup(l.Release)
	t.Cleanup(updater.SetStateLockWaitForTest(d))
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))
	return waits
}

// TestAStopWhileServeWaitsForTheMarkersLockEndsTheStartThere: the waits for
// the install marker's lock that serve's own context bounds (round 4): its
// admission, whose stop ends the start with exit 0 and nothing written, even
// where no install is armed and an admission that could not take the lock
// would have gone on; the early rollback, which leaves the install as it is
// and says it was stopped; and the boot's housekeeping, which leaves the
// marker and the binaries as they are. A store open's own wait keeps its
// bound alone: OpenStore takes no context.
func TestAStopWhileServeWaitsForTheMarkersLockEndsTheStartThere(t *testing.T) {
	t.Run("the admission", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		if err := updater.ClearState(r.dataDir); err != nil {
			t.Fatal(err)
		}
		waits := heldMarkersLock(t, r.dataDir, 3*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		var stdout, stderr safeBuffer
		codes, serveDone := make(chan int, 1), make(chan struct{})
		go func() {
			defer close(serveDone)
			codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin}, &stdout, &stderr)
		}()
		drainLoopOnCleanup(t, cancel, serveDone, "serve")
		select {
		case <-waits:
		case <-serveDone:
			t.Fatalf("serve = %d before it waited for the marker's lock; stderr=%s", <-codes, stderr.String())
		case <-time.After(withinTheTest):
			t.Fatal("serve never waited for the marker's lock")
		}
		cancel()
		if code := <-codes; code != 0 {
			t.Errorf("serve stopped in its admission = %d, want 0; stderr=%s", code, stderr.String())
		}
		requireNoStoreOpened(t, r.dataDir)
	})
	t.Run("the early rollback", func(t *testing.T) {
		r := newRefusedInstall(t, "scanIntervalSec: 010\n")
		files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
		waits := heldMarkersLock(t, r.dataDir, 3*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		var stdout, stderr safeBuffer
		codes, serveDone := make(chan int, 1), make(chan struct{})
		go func() {
			defer close(serveDone)
			codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin}, &stdout, &stderr)
		}()
		drainLoopOnCleanup(t, cancel, serveDone, "serve")
		select {
		case <-waits:
		case <-serveDone:
			t.Fatalf("serve = %d before it waited for the marker's lock; stderr=%s", <-codes, stderr.String())
		case <-time.After(withinTheTest):
			t.Fatal("serve never waited for the marker's lock")
		}
		cancel()
		if code := <-codes; code != 2 {
			t.Errorf("serve stopped in its early rollback = %d, want 2, the refusal as before; stderr=%s", code, stderr.String())
		}
		files.requireUnchanged(t)
		if !strings.Contains(updaterLines(stderr.String()), "stopped") {
			t.Errorf("serve did not say its rollback was stopped; stderr=%s", stderr.String())
		}
	})
	t.Run("the housekeeping", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
		waits := heldMarkersLock(t, r.dataDir, 3*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		var stderr safeBuffer
		housekept, done := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(done)
			housekept <- bootHousekeeping(ctx, &stderr, r.dataDir, r.bin, previousVersion, nil)
		}()
		drainLoopOnCleanup(t, cancel, done, "the boot's housekeeping")
		select {
		case <-waits:
		case <-done:
			t.Fatalf("the housekeeping ended before it waited for the marker's lock (%v); stderr=%s", <-housekept, stderr.String())
		case <-time.After(withinTheTest):
			t.Fatal("the housekeeping never waited for the marker's lock")
		}
		cancel()
		if err := <-housekept; !errors.Is(err, context.Canceled) {
			t.Errorf("the housekeeping stopped in its wait = %v, want the stop", err)
		}
		files.requireUnchanged(t)
	})
}

// TestTheBootConfirmsAnInterruptedRollbackOnlyOnTheInstallsOwnRecord: the
// housekeeping of this version over the early rollback's record puts the
// install back on probation (backlog B660: confirmed it until B660) only where
// the record names this version as having opened the data dir, which serve's
// admission writes before the store opens; without it the marker is left as
// it is, and the line says why (round 4).
func TestTheBootConfirmsAnInterruptedRollbackOnlyOnTheInstallsOwnRecord(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker = rollbackStoppedBeforeItsRename(r.marker)
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	var stderr bytes.Buffer
	if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, version.ServerVersion, nil); err == nil {
		t.Errorf("the housekeeping over a record no admission of this version wrote = nil, want its refusal; stderr=%s", stderr.String())
	}
	files.requireUnchanged(t)
	if !strings.Contains(updaterLines(stderr.String()), "not putting the install") {
		t.Errorf("the housekeeping did not say why it left the record; stderr=%s", stderr.String())
	}

	r.marker.DataDirOpenedBy = version.ServerVersion
	r.marker.MigrationVeto = ""
	r.saveMarker(t, r.dataDir)
	stderr.Reset()
	if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, version.ServerVersion, nil); err != nil {
		t.Fatalf("the housekeeping over this version's own record = %v; stderr=%s", err, stderr.String())
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || st.Status != "installing" || st.RollbackStarted {
		t.Errorf("the marker after the housekeeping over this version's own record = %+v (%v), want the install back on probation", st, err)
	}
}
