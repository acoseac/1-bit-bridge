package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Backlog B660's fourth round, in the updater: a probe of a start's lock
// cannot pass for the start, and a take-back past its deadline still tries
// the install marker's lock once.

// aCrashedStart lays out in dir what a start killed before its migration
// leaves: one start counted, its token in the marker, and its lock file,
// which nobody holds.
func aCrashedStart(t *testing.T, dir string) State {
	t.Helper()
	st := withToken(probationMarker(1), aStartsToken)
	mustSave(t, dir, st)
	if err := os.WriteFile(filepath.Join(dir, "serve-start-"+aStartsToken+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestAProbeHeldBeforeItsUnlockIsNoRunningStart: a start killed before its
// migration leaves one start counted, its token and its lock file, which
// nobody holds. A probe of that lock (a `bridge doctor`'s) descheduled after
// its try took the lock and before it let go is no running start to a
// migrating open beside it, whose own probe meets it: the open is held, and
// the probe reads no start running either. Until the fourth round every probe
// took the lock exclusively, so the open's probe met the doctor's, read the
// crashed start as running, took the sole failure off the count and
// migrated, refusing the crash rollback for good.
func TestAProbeHeldBeforeItsUnlockIsNoRunningStart(t *testing.T) {
	dir := t.TempDir()
	st := aCrashedStart(t, dir)
	parked, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	// The first probe alone is held; every later one goes on at once (a
	// sync.Once would hold a concurrent one inside Do too).
	t.Cleanup(SetStartProbeHeldForTest(func(string) {
		if first.CompareAndSwap(false, true) {
			close(parked)
			<-release
		}
	}))
	running, probed := false, make(chan struct{})
	go func() {
		defer close(probed)
		running = RunningServeCounted(dir, st, "0.2.4")
	}()
	t.Cleanup(func() {
		letGo()
		<-probed
	})
	select {
	case <-parked:
	case <-time.After(20 * time.Second):
		t.Fatal("the diagnostic probe never took the crashed start's lock")
	}
	_, err := DisarmBeforeMigrating(dir, "0.2.4")
	letGo()
	<-probed
	if !errors.Is(err, ErrMigrationHeld) {
		t.Errorf("a migrating open beside a probe holding the crashed start's lock = %v, want it held: "+
			"the one start counted did not end cleanly", err)
	}
	if got := mustLoad(t, dir); got.StoreMigratedBy != "" {
		t.Errorf("the marker after the open = %+v, want no migration recorded", got)
	}
	if running {
		t.Error("the diagnostic probe read the crashed start as running")
	}
}

// TestATakeBackPastItsDeadlineStillTriesTheLockOnce: a take-back whose
// context has ended still tries the install marker's lock once, as LockState
// tries before it waits: it lands where the lock is free, and gives up at once
// where another holds it.
func TestATakeBackPastItsDeadlineStillTriesTheLockOnce(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	free := t.TempDir()
	mustSave(t, free, probationMarker(1))
	if took, err := TakeBackFailedStart(ended, free, "0.2.4", 1); err != nil || !took {
		t.Errorf("a take-back past its deadline, the lock free = %v, %v; want it landed", took, err)
	}
	held := t.TempDir()
	mustSave(t, held, probationMarker(1))
	l, err := LockState(t.Context(), held)
	if err != nil || !l.Held() {
		t.Fatalf("hold the install marker's lock: %+v, %v", l, err)
	}
	t.Cleanup(l.Release)
	asked := time.Now()
	if took, err := TakeBackFailedStart(ended, held, "0.2.4", 1); err == nil || took {
		t.Errorf("a take-back past its deadline, the lock held = %v, %v; want it given up", took, err)
	}
	if waited := time.Since(asked); waited > time.Second {
		t.Errorf("the take-back past its deadline waited %s for the held lock", waited)
	}
	if st := mustLoad(t, held); st.FailedStarts != 1 {
		t.Errorf("the marker beside the held lock = %+v, want the count left", st)
	}
}
