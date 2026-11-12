package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// keepOne is a prune's retention of one snapshot.
func keepOne() int { return 1 }

// requireReturnedWithin waits for done, which the call sends on once it
// returns, and fails the test unless it came within bound of from.
func requireReturnedWithin[T any](t *testing.T, done <-chan T, from time.Time, bound time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-done:
		if took := time.Since(from); took > bound {
			t.Errorf("%s returned %v after its stop; its bound is %v", what, took, bound)
		}
		return v
	case <-time.After(bound + 10*time.Second):
		t.Fatalf("%s did not return within %v of its stop", what, bound+10*time.Second)
	}
	var zero T
	return zero
}

// awaitVerificationEnd waits until the verification that owes the captured
// snapshot in dir its check has ended, failing the test past a deadline:
// every hold dropped (awaitClean), which a worker does last, once its lock,
// its slot and the capture's ownership are let go (hold.release lets the
// ownership go before the lock, backlog B849), and then the ownership taken
// and let go as its next taker would.
func awaitVerificationEnd(t *testing.T, dir string) {
	t.Helper()
	awaitClean(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		release, err := backup.TakeOwnerForTest(dir)
		if err == nil {
			release()
			return
		}
		if !errors.Is(err, backup.ErrClaimedElsewhere) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the verification of %s never ended", filepath.Base(dir))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAStopWhileTheLocksOpenHangsReturnsWithinItsBoundAndTakesNoLockLate is
// backlog B653's second round's case for the lock's open: a verification
// stopped while the open of the backups folder's lock file does not answer
// (a hung mount) answers its caller within StopWriteBound, and the open that
// answers after the stop is let go at once, so no lock is held late: once
// the verification has ended, a prune runs, and the record says the check
// did not end. Not red at the round before by any real object (a local
// filesystem's open of a lock file does not hang); its control is the open
// taken back out of the verification's worker.
func TestAStopWhileTheLocksOpenHangsReturnsWithinItsBoundAndTakesNoLockLate(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return fsutil.OpenLockFile(dir, name)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	callers := joinBackupCallers(t, func() { letGo(); cancel() })
	done := make(chan backup.Result, 1)
	callers.start(func() { done <- backup.FinishCapture(ctx, c, keepOne, backup.VerifyOptions{}) })
	<-entered
	stoppedAt := time.Now()
	cancel()
	res := requireReturnedWithin(t, done, stoppedAt, backup.StopWriteBound+time.Second, "a verification stopped in its lock's open")
	if res.VerifyErr == nil {
		t.Error("a verification stopped in its lock's open answered no error")
	}
	letGo()
	awaitVerificationEnd(t, c.Dir)
	pr, err := backup.Prune(filepath.Dir(c.Dir), 7)
	if err != nil || pr.Skipped {
		t.Errorf("a prune once the stopped verification ended: %+v, %v; want it run, no lock held late", pr, err)
	}
	if v := listed(t, c.Dir); v.DatabaseState != backup.StateIncomplete {
		t.Errorf("the stopped verification's snapshot reads %s %v, want INCOMPLETE", v.DatabaseState, v.DatabaseReasons)
	}
}

// TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting: the
// deadline frees a check its context cannot reach (a read of a hung mount,
// here a check whose thread does not return): the verification answers its
// caller, whose own context never ends, within its deadline and half of
// StopWriteBound, saying its check was given up (ErrCheckGivenUp, the
// deadline's), and records INCOMPLETE (deadline). The check's thread ends,
// without a check, once the test lets it go, and the test joins the
// verification's worker, which keeps this process's slot until then, before
// it puts either seam back (backlog B653's third round). The outer-expiry
// case blocks recovery and the answer independently, proves Unknown at expiry,
// then releases recovery and reads INCOMPLETE from disk (ninth round).
func TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting(t *testing.T) {
	t.Run("worker answer", func(t *testing.T) {
		src := storeWithTracks(t, 5)
		dir, err := backup.Snapshot(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		const deadline = 300 * time.Millisecond
		t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
		// The check's thread waits until it is let go, then ends without a
		// check, touching nothing of the bundle.
		entered, hung := make(chan struct{}), make(chan struct{})
		var parked atomic.Bool
		t.Cleanup(backup.SetOnLowPriorityForTest(func() {
			if parked.CompareAndSwap(false, true) {
				close(entered)
				<-hung
				runtime.Goexit()
			}
		}))
		// Registered last, so run first: the thread let go and the worker
		// joined before the seams are put back.
		letGo := sync.OnceFunc(func() { close(hung) })
		t.Cleanup(func() {
			letGo()
			awaitSlotFree(t)
		})
		callers := joinBackupCallers(t, letGo)
		done := make(chan error, 1)
		start := time.Now()
		callers.start(func() {
			_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
			done <- err
		})
		err = requireReturnedWithin(t, done, start, deadline+backup.StopWriteBound/2+time.Second, "a verification past its deadline")
		<-entered
		if !errors.Is(err, backup.ErrCheckGivenUp) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a verification past its deadline answered %v, want its check given up at the deadline", err)
		}
		v := listed(t, dir)
		if v.DatabaseState != backup.StateIncomplete || !slices.Contains(v.DatabaseReasons, backup.ReasonDeadline) {
			t.Errorf("the record reads %s %v, want INCOMPLETE (deadline)", v.DatabaseState, v.DatabaseReasons)
		}
		letGo()
		awaitSlotFree(t)
		if _, held := backup.StuckCheckOf(filepath.Dir(dir), backup.ProbeByDoctor); held {
			t.Error("the check reads as one still running once its thread has ended")
		}
	})
	t.Run("outer expiration before recovery", func(t *testing.T) {
		c := captureT(t, storeWithTracks(t, 5))
		const deadline = 300 * time.Millisecond
		t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
		check, recovery, answer := newParking(), newParking(), newParking()
		t.Cleanup(backup.SetOnLowPriorityForTest(check.park))
		// Block the worker before publishOf can reject its completed verdict and
		// fall back to stepIncomplete. Its answer has an independent barrier.
		t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
			if dir == c.Dir {
				recovery.park()
			}
		}))
		var expired atomic.Bool
		t.Cleanup(backup.SetVerifyHandoffForTest(func(dir string) {
			if dir == c.Dir {
				answer.park()
			}
		}, func(dir string) {
			if dir == c.Dir {
				expired.Store(true)
			}
		}))
		callers := joinBackupCallers(t, func() { check.release(); recovery.release(); answer.release() })
		done := make(chan backup.Result, 1)
		start := time.Now()
		callers.start(func() {
			done <- backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{LockWait: time.Millisecond})
		})
		select {
		case <-recovery.entered:
		case <-time.After(30 * time.Second):
			t.Fatal("worker never reached the recovery barrier")
		}
		res := requireReturnedWithin(t, done, start, deadline+3*backup.StopWriteBound+time.Second, "outer expiration")
		if !expired.Load() || res.Verify.Write != backup.WriteUnknown || !errors.Is(res.VerifyErr, context.DeadlineExceeded) {
			t.Fatalf("want forced outer expiry with unknown write and deadline, got expired=%v, %+v", expired.Load(), res)
		}
		if res.Verify.RecordRead {
			t.Error("outer expiry invented a record read-back")
		}
		if v := listed(t, c.Dir); v.DatabaseState != backup.StatePending {
			t.Fatalf("recovery wrote before release: %+v", v)
		}
		recovery.release()
		select {
		case <-answer.entered:
		case <-time.After(30 * time.Second):
			t.Fatal("worker never reached its independent answer barrier")
		}
		requireVerdict(t, "recovery after outer expiry", listed(t, c.Dir), backup.StateIncomplete, backup.ReasonDeadline, backup.StateIncomplete, backup.ReasonDeadline)
		check.release()
		answer.release()
		callers.wait(t)
		awaitClean(t)
	})
}

// TestAClaimStoppedWhileItsLockOpenHangsReturnsWithinItsBound: the startup
// pass's claim of a snapshot that owes its check (Claim) runs its steps
// within the same bound as a verification: stopped while the open of the
// backups folder's lock does not answer, it answers its caller within
// StopWriteBound, and the claim that lands once the open answers lets its
// ownership go at once, so the snapshot is no one's.
func TestAClaimStoppedWhileItsLockOpenHangsReturnsWithinItsBound(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	c.Release()
	entered, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	t.Cleanup(func() {
		letGo()
		awaitVerificationEnd(t, c.Dir)
	})
	t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return fsutil.OpenLockFile(dir, name)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type answer struct {
		c   *backup.Capture
		err error
	}
	callers := joinBackupCallers(t, func() { letGo(); cancel() })
	done := make(chan answer, 1)
	callers.start(func() {
		claimed, err := backup.Claim(ctx, c.Dir, filepath.Dir(c.Dir))
		done <- answer{claimed, err}
	})
	<-entered
	stoppedAt := time.Now()
	cancel()
	a := requireReturnedWithin(t, done, stoppedAt, backup.StopWriteBound+time.Second, "a claim stopped in its lock's open")
	if a.err == nil || a.c != nil {
		t.Errorf("a claim stopped in its lock's open answered %+v, %v; want no claim and the stop", a.c, a.err)
	}
	letGo()
	awaitVerificationEnd(t, c.Dir)
}
