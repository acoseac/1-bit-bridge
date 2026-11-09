package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
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
// snapshot in dir its check has ended, its capture's ownership let go (which
// comes after its lock is), failing the test past a deadline.
func awaitVerificationEnd(t *testing.T, dir string) {
	t.Helper()
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
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return fsutil.OpenLockFile(dir, name)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan backup.Result, 1)
	go func() { done <- backup.FinishCapture(ctx, c, keepOne, backup.VerifyOptions{}) }()
	<-entered
	stoppedAt := time.Now()
	cancel()
	res := requireReturnedWithin(t, done, stoppedAt, backup.StopWriteBound+time.Second, "a verification stopped in its lock's open")
	if res.VerifyErr == nil {
		t.Error("a verification stopped in its lock's open answered no error")
	}
	close(release)
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
// StopWriteBound, and records INCOMPLETE (deadline).
func TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	const deadline = 300 * time.Millisecond
	t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
	// The check's thread waits until the test ends, then ends without a
	// check, touching nothing of the bundle the test's cleanup removes. It
	// read the seam before it hung, so the seam's restore waits until it has
	// entered (the repo's rule for a package-level seam).
	entered, hung := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hung) })
	t.Cleanup(backup.SetOnLowPriorityForTest(func() {
		close(entered)
		<-hung
		runtime.Goexit()
	}))
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
		done <- err
	}()
	err = requireReturnedWithin(t, done, start, deadline+backup.StopWriteBound/2+time.Second, "a verification past its deadline")
	<-entered
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a verification past its deadline answered %v, want the deadline", err)
	}
	v := listed(t, dir)
	if v.DatabaseState != backup.StateIncomplete || !slices.Contains(v.DatabaseReasons, backup.ReasonDeadline) {
		t.Errorf("the record reads %s %v, want INCOMPLETE (deadline)", v.DatabaseState, v.DatabaseReasons)
	}
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
	defer cancel()
	type answer struct {
		c   *backup.Capture
		err error
	}
	done := make(chan answer, 1)
	go func() {
		claimed, err := backup.Claim(ctx, c.Dir, filepath.Dir(c.Dir))
		done <- answer{claimed, err}
	}()
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
