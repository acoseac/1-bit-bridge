package updater

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Backlog B692's round 2: an operator's rollback stopped after its record
// stays recognisable until its rename has landed, and a stop while the
// restamp or the hold waits for the install marker's lock is a stop.

// stopTheProcessHere ends the goroutine it runs on, as the process ending
// there would end the rollback: its deferred calls run (the lock's release),
// nothing after it does.
func stopTheProcessHere(string, string) error {
	runtime.Goexit()
	return nil
}

// renameThenStop puts the backup back, then ends the goroutine before the
// rollback clears its record: Argon's mirror window.
func renameThenStop(dst, ext string) error {
	if err := RollbackBinary(dst, ext); err != nil {
		return err
	}
	runtime.Goexit()
	return nil
}

// rollBackUntilStopped runs the operator's rollback with restore in place of
// its rename, on a goroutine of its own, and waits for it to end.
func rollBackUntilStopped(t *testing.T, opts InstallOptions, restore func(string, string) error) {
	t.Helper()
	opts.RestoreBackup = restore
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = New(Options{}).Rollback(context.Background(), opts)
	}()
	select {
	case <-done:
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never ended")
	}
}

// TestARollbackStoppedBeforeItsRenameStaysAnInstallTheTargetsStartResolves:
// a rollback stopped after its record and before its rename leaves the
// version it was rolling back from on disk, and its record says the rollback
// is under way. On that still-new binary each step keeps the store from a
// downgrade: a migrating open of the target refuses under the veto, the
// target's serve admission records that it opened the data dir and lifts the
// veto in the one write, the migration after it goes on, and a second manual
// rollback then refuses.
func TestARollbackStoppedBeforeItsRenameStaysAnInstallTheTargetsStartResolves(t *testing.T) {
	l := newRollbackLayout(t)
	rollBackUntilStopped(t, l.opts(), stopTheProcessHere)
	if f := l.files(); f[1] != "bridge-binary-0.2.3" || f[2] != "bridge-binary-0.2.1" {
		t.Fatalf("the binary %q and the backup %q after a rollback stopped before its rename, want them as the install left them", f[1], f[2])
	}
	st, err := LoadState(l.dataDir)
	if err != nil || !st.RollbackStarted || !InstallArmed(st) || st.MigrationVeto != "0.2.3" || st.RejectedVersion != "0.2.3" {
		t.Fatalf("the marker a rollback stopped before its rename leaves = %+v (%v), want its record of the rollback under way: "+
			"the install armed, the hold and the veto of 0.2.3", st, err)
	}
	if _, err := DisarmBeforeMigrating(l.dataDir, "0.2.3"); !errors.Is(err, ErrMigrationVetoed) {
		t.Errorf("a migrating open of the target before its start = %v, want it vetoed", err)
	}
	res, err := AdmitBoot(context.Background(), l.dataDir, "0.2.3", nil)
	if err != nil || !res.Recorded || !res.VetoLifted {
		t.Fatalf("the target's admission over the record = %+v, %v; want its record of the data dir made and the veto lifted", res, err)
	}
	if st, err := LoadState(l.dataDir); err != nil || st.DataDirOpenedBy != "0.2.3" || st.MigrationVeto != "" {
		t.Errorf("the marker after the target's admission = %+v (%v), want 0.2.3 recorded and no veto", st, err)
	}
	if _, err := DisarmBeforeMigrating(l.dataDir, "0.2.3"); err != nil {
		t.Errorf("a migrating open of the target after its admission = %v, want it to go on", err)
	}
	before := l.files()
	if err := New(Options{}).Rollback(context.Background(), l.opts()); !errors.Is(err, ErrRollbackRefused) {
		t.Fatalf("a second manual rollback after the target opened the data dir = %v, want it refused", err)
	}
	l.requireUnchanged(t, before)
}

// TestARollbackStoppedAfterItsRenameIsFinishedByTheVersionThatRuns: the
// mirror window (Argon). The backup is back, and the rollback ended before
// it cleared its record, so the record still says the rollback is under way:
// the boot reads which version runs and finishes the record from it, a
// rollback done on the previous version, an interrupted one on the target,
// and the target's migrations stay vetoed meanwhile.
func TestARollbackStoppedAfterItsRenameIsFinishedByTheVersionThatRuns(t *testing.T) {
	l := newRollbackLayout(t)
	rollBackUntilStopped(t, l.opts(), renameThenStop)
	if f := l.files(); f[1] != "bridge-binary-0.2.1" || f[2] != "" {
		t.Fatalf("the binary %q and the backup %q after the rename, want the backup back", f[1], f[2])
	}
	st, err := LoadState(l.dataDir)
	if err != nil || !st.RollbackStarted {
		t.Fatalf("the marker after a rollback stopped past its rename = %+v (%v), want its record still under way", st, err)
	}
	if a := DecideBootAction(st, "0.2.1", time.Now().UTC()); a != BootRollbackFinished {
		t.Errorf("the previous version's boot over the record = %v, want BootRollbackFinished", a)
	}
	if a := DecideBootAction(st, "0.2.3", time.Now().UTC()); a != BootRollbackOvertaken {
		t.Errorf("the target's boot over the record = %v, want BootRollbackOvertaken", a)
	}
	if _, err := DisarmBeforeMigrating(l.dataDir, "0.2.3"); !errors.Is(err, ErrMigrationVetoed) {
		t.Errorf("a migrating open of the target meanwhile = %v, want it vetoed", err)
	}
	if res, err := AdmitBoot(context.Background(), l.dataDir, "0.2.1", nil); err != nil || res.Recorded || res.VetoLifted {
		t.Errorf("the previous version's admission over the record = %+v, %v; want nothing written", res, err)
	}
}

// TestARollbackClearsItsRecordOnceItsRenameHasLanded: a rollback that ends
// whole leaves no rollback under way, so no boot acts on its marker.
func TestARollbackClearsItsRecordOnceItsRenameHasLanded(t *testing.T) {
	l := newRollbackLayout(t)
	if err := New(Options{}).Rollback(context.Background(), l.opts()); err != nil {
		t.Fatalf("Rollback = %v", err)
	}
	st, err := LoadState(l.dataDir)
	if err != nil || st.RollbackStarted || InstallArmed(st) {
		t.Fatalf("the marker after a rollback = %+v (%v), want no rollback under way and no install armed", st, err)
	}
	for _, v := range []string{"0.2.1", "0.2.3"} {
		if a := DecideBootAction(st, v, time.Now().UTC()); a != BootNoop {
			t.Errorf("a boot of %s after the rollback = %v, want BootNoop", v, a)
		}
	}
}

// TestAStopWhileTheRestampWaitsRequestsNoRestart: the auto-installer's
// restamp before its restart waits for the install marker's lock, and a stop
// in that wait (the operator's, the service manager's) is a stop: no restart
// is requested, which would turn it into exit 75 and a supervisor's restart,
// and the marker is left as it is; so with a stop before an uncontended take.
// The lock free and no stop, the restart is requested once.
func TestAStopWhileTheRestampWaitsRequestsNoRestart(t *testing.T) {
	setUp := func(t *testing.T) (*Updater, string, *atomic.Int32) {
		dir := t.TempDir()
		if err := SaveState(dir, State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
			AttemptedAt: time.Now().UTC().Add(-2 * time.Hour), SwapStarted: true}); err != nil {
			t.Fatal(err)
		}
		restarts := &atomic.Int32{}
		return New(Options{AutoInstallOpts: &InstallOptions{DataDir: dir}, AutoInstallRestart: func() { restarts.Add(1) }}), dir, restarts
	}
	t.Run("stopped while it waits", func(t *testing.T) {
		u, dir, restarts := setUp(t)
		holdStateLock(t, dir, withinTheTest)
		waits := signalWaits(t)
		before, _ := os.ReadFile(StatePath(dir))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			u.restartWhenDrained(ctx)
		}()
		drainOnCleanup(t, cancel, done, "the restart")
		select {
		case <-waits:
		case <-done:
			t.Fatal("the restamp did not wait for the marker's lock another process held")
		case <-time.After(withinTheTest):
			t.Fatal("the restamp never waited for the marker's lock")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(withinTheTest):
			t.Fatal("the restart did not return after its stop")
		}
		if n := restarts.Load(); n != 0 {
			t.Errorf("a stop while the restamp waited requested %d restart(s), want none", n)
		}
		if after, _ := os.ReadFile(StatePath(dir)); string(after) != string(before) {
			t.Errorf("the marker was written after the stop: %s", after)
		}
	})
	t.Run("stopped before an uncontended take", func(t *testing.T) {
		u, _, restarts := setUp(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		u.restartWhenDrained(ctx)
		if n := restarts.Load(); n != 0 {
			t.Errorf("a restart whose context had ended requested %d restart(s), want none", n)
		}
	})
	t.Run("the lock free", func(t *testing.T) {
		u, _, restarts := setUp(t)
		u.restartWhenDrained(context.Background())
		if n := restarts.Load(); n != 1 {
			t.Errorf("a restart with the lock free requested %d restart(s), want one", n)
		}
	})
}
