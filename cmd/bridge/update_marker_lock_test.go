package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B692: the console's rollback and install, `bridge update` and the
// auto-installer hold the install marker's lock (update-state.lock) as the
// boot's housekeeping does, so neither acts on a marker the other is
// changing.

// rollBackFromTheConsole runs the operator's rollback of r's install, as the
// console's does, on a goroutine, held where hold says (before its record
// once it has read the marker and decided; nil holds it nowhere).
func rollBackFromTheConsole(t *testing.T, r *refusedInstall, hold func(), unhold func()) (<-chan error, <-chan struct{}) {
	t.Helper()
	answer, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		answer <- updater.New(updater.Options{}).Rollback(context.Background(),
			updater.InstallOptions{DataDir: r.dataDir, BinaryPath: r.bin, Force: true, BeforeRollbackRecord: hold})
	}()
	drainLoopOnCleanup(t, unhold, done, "the rollback")
	return answer, done
}

// TestAManualRollbackThatHoldsTheMarkersLockFirstLeavesTheBootNothingToDo:
// the operator's rollback holds the install marker's lock from its read of
// the marker through its record and the backup's rename. The housekeeping of
// a start of the version installed, arriving after that read and before the
// record, has nothing to do and asks for no lock (the install is on
// probation: backlog B660), and the confirmation of that install (until B660
// the housekeeping confirmed it here, after the same wait) waits for the
// lock, then reads the rollback's record and does nothing: no install
// confirmed over the backup put back, the hold and the veto as the rollback
// recorded them.
func TestAManualRollbackThatHoldsTheMarkersLockFirstLeavesTheBootNothingToDo(t *testing.T) {
	r := newRefusedInstall(t, "")
	atRecord, release := newOnceCloser(), newOnceCloser()
	hold := func() {
		atRecord.close()
		<-release.ch
	}
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))
	rolledBack, _ := rollBackFromTheConsole(t, r, hold, release.close)
	select {
	case <-atRecord.ch:
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never reached its record")
	}

	var housekeeping bytes.Buffer
	if err := bootHousekeeping(t.Context(), &housekeeping, r.dataDir, r.bin, version.ServerVersion, nil); err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, housekeeping.String())
	}
	select {
	case <-waits:
		t.Fatal("the housekeeping of an install on probation waited for the marker's lock")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	confirmed, confirmationDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(confirmationDone)
		ok, err := updater.ConfirmInstall(ctx, r.dataDir, version.ServerVersion, time.Now().UTC())
		if ok {
			err = errors.New("the install was confirmed")
		}
		confirmed <- err
	}()
	drainLoopOnCleanup(t, cancel, confirmationDone, "the install's confirmation")
	select {
	case <-waits:
	case <-confirmationDone:
		t.Fatalf("the confirmation did not wait for the rollback that held the marker's lock (%v)", <-confirmed)
	case <-time.After(withinTheTest):
		t.Fatal("the confirmation never waited for the marker's lock")
	}
	release.close()
	if err := <-rolledBack; err != nil {
		t.Fatalf("the rollback = %v", err)
	}
	if err := <-confirmed; err != nil {
		t.Fatalf("the confirmation over the rollback's record = %v, want nothing done", err)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s put back", got, previousVersion)
	}
	st, err := updater.LoadState(r.dataDir)
	if err != nil || st.Status != "" || st.RejectedVersion != version.ServerVersion || st.MigrationVeto != version.ServerVersion {
		t.Errorf("the marker = %+v (%v), want the rollback's record: no install, the hold and the veto of %s", st, err, version.ServerVersion)
	}
	if lines := updaterLines(housekeeping.String()); lines != "" {
		t.Errorf("the housekeeping acted on the install: %s", lines)
	}
}

// TestABootThatHoldsTheMarkersLockFirstLeavesTheManualRollbackNothingToPutBack:
// the boot's housekeeping holds the install marker's lock from its read
// through the backup's rename (an install that did not come up as its
// version), and the operator's rollback, arriving meanwhile, waits for it,
// then reads the marker the boot left and finds no backup to put back:
// it refuses, writing and renaming nothing, and the boot's veto stands.
func TestABootThatHoldsTheMarkersLockFirstLeavesTheManualRollbackNothingToPutBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	atRestore, release := newOnceCloser(), newOnceCloser()
	restore := func(dst, ext string) error {
		atRestore.close()
		<-release.ch
		return updater.RollbackBinary(dst, ext)
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
	t.Cleanup(release.close)
	select {
	case <-atRestore.ch:
	case <-housekeepingDone:
		t.Fatalf("the housekeeping ended before its rename (%v); stderr=%s", <-housekept, stderr.String())
	case <-time.After(withinTheTest):
		t.Fatal("the housekeeping never reached its rename")
	}

	rolledBack, rollbackDone := rollBackFromTheConsole(t, r, nil, release.close)
	select {
	case <-waits:
	case <-rollbackDone:
		t.Fatalf("the rollback did not wait for the housekeeping that held the marker's lock: %v", <-rolledBack)
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never waited for the marker's lock")
	}
	release.close()
	if err := <-housekept; err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
	}
	after := watch(t, r.bin, updater.StatePath(r.dataDir))
	if err := <-rolledBack; err == nil {
		t.Fatal("the rollback after the boot put the backup back = nil, want its refusal: there is no backup left")
	}
	after.requireUnchanged(t)
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s, which the boot put back", got, previousVersion)
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || st.MigrationVeto != version.ServerVersion || updater.InstallArmed(st) {
		t.Errorf("the marker = %+v (%v), want the boot's: the install cleared, its veto kept", st, err)
	}
	if strings.Contains(stderr.String(), "rollback failed") {
		t.Errorf("the boot's rename met a backup the rollback had taken; stderr=%s", stderr.String())
	}
}

// TestServeStopsWhileAnotherProcessHoldsTheMarkersLock: with no install
// counting its start, nothing in serve's stop takes the install marker's
// lock, so a `bridge update` that holds it while it stops the service (on
// Windows, through the SCM, for up to scmStopWait) is never waiting on the
// serve it stops; a start an install counts waits for it to take its count
// back, on a goroutine while it tears down and for at most takeBackWait after
// (backlog B660).
func TestServeStopsWhileAnotherProcessHoldsTheMarkersLock(t *testing.T) {
	r := newRefusedInstall(t, "")
	if err := updater.ClearState(r.dataDir); err != nil {
		t.Fatal(err)
	}
	sb := r.boot(t)
	t.Cleanup(updater.SetStateLockWaitForTest(10 * time.Minute))
	l, err := updater.LockState(t.Context(), r.dataDir)
	if err != nil || !l.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", l, err)
	}
	t.Cleanup(l.Release)
	waited := newOnceCloser()
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { waited.close() }))
	sb.stop()
	select {
	case code := <-sb.done:
		if code != 0 {
			t.Errorf("serve = %d after its stop, want 0; stderr=%s", code, sb.stderr.String())
		}
	case <-waited.ch:
		l.Release()
		t.Fatalf("serve's stop waited for the install marker's lock another process held; stderr=%s", sb.stderr.String())
	}
}

// TestTheConsolesRollbackIsRefusedOverAStoreTheInstalledVersionOpened: the
// console's rollback (the adapter over the updater) refuses where the version
// installed opened the data dir, answering the console's own sentinel with
// words that name the version and no path, and leaves the binaries and the
// marker as they are.
func TestTheConsolesRollbackIsRefusedOverAStoreTheInstalledVersionOpened(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	a := updateInfoAdapter{u: updater.New(updater.Options{}), dataDir: r.dataDir, binaryPath: r.bin}
	err := a.Rollback(t.Context(), true)
	if !errors.Is(err, admin.ErrUpdateRollbackRefused) {
		t.Fatalf("the console's rollback over a store %s opened = %v, want the console's refusal", version.ServerVersion, err)
	}
	if msg := err.Error(); strings.Contains(msg, r.dir) || !strings.Contains(msg, version.ServerVersion) {
		t.Errorf("the refusal %q names a path, or not the version that opened the data dir", msg)
	}
	files.requireUnchanged(t)
}
