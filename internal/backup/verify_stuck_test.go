package backup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// awaitSlotFree waits until no verification holds this process's slot,
// failing the test past a deadline.
func awaitSlotFree(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for backup.SlotHeldForTest() {
		if time.Now().After(deadline) {
			t.Fatal("the verification slot was never let go")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitMarker waits until the backups folder at root holds the marker of a
// check that did not end, which its worker writes on a goroutine of its own.
func awaitMarker(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !pathExists(t, filepath.Join(root, backup.StuckMarkerName)) {
		if time.Now().After(deadline) {
			t.Fatal("no marker of the check that did not end was written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parkCheck parks the first check that reaches SQLite's open of its
// snapshot's database (SetOnDatabaseOpenForTest), the database's file open,
// until release is called; entered closes once one is parked. Its cleanup
// lets the check go and joins the verification that owes owed its check (a
// capture's; "" for none) and this process's slot before it puts the seam
// back.
func parkCheck(t *testing.T, owed string) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
		if parked.CompareAndSwap(false, true) {
			close(in)
			<-out
		}
	}))
	release = sync.OnceFunc(func() { close(out) })
	t.Cleanup(func() {
		release()
		if owed != "" {
			awaitVerificationEnd(t, owed)
		}
		awaitSlotFree(t)
	})
	return in, release
}

// unlockHold holds the next unlock of a backups folder's lock once armed
// (SetOnLockReleaseForTest) until the test lets it go, and hears the end of
// the worker of the verification it is armed for (SetOnVerifyEndForTest). A
// verification answers its caller before its worker lets go of that lock
// (verifyAndThen), so the window between the answer and the unlock is held
// open, on every platform, and a step the test takes once the verification
// has answered is shown to wait for the worker's end, never the answer
// (backlog B849).
type unlockHold struct {
	root  string
	armed atomic.Bool
	p     *parking
	of    atomic.Pointer[string]
	heard sync.Once
	ended chan struct{}
	// held and heldErr are what the end found of the folder's lock, written
	// before ended closes.
	held    bool
	heldErr error
}

// holdUnlockOf installs an unlockHold on the backups folder at root, not
// armed. It is installed before the verifications whose unlock it may hold
// start, since SetOnLockReleaseForTest's seam is read without a lock, and its
// seams are put back once every hold has ended: joinBackupCallers' cleanup,
// registered after it, runs first.
func holdUnlockOf(t *testing.T, root string) *unlockHold {
	t.Helper()
	u := &unlockHold{root: root, p: newParking(), ended: make(chan struct{})}
	t.Cleanup(backup.SetOnLockReleaseForTest(func(dir string) {
		if dir == u.root && u.armed.CompareAndSwap(true, false) {
			u.p.park()
		}
	}))
	t.Cleanup(backup.SetOnVerifyEndForTest(func(dir string) {
		if of := u.of.Load(); of != nil && dir == *of {
			u.heard.Do(func() {
				// Asked where the end happens, on the worker's goroutine.
				u.held, u.heldErr = backupsLockHeld(u.root)
				close(u.ended)
			})
		}
	}))
	return u
}

// arm holds the next unlock of the folder's lock, and hears the end of the
// verification of snapshot, the dir its caller passes.
func (u *unlockHold) arm(snapshot string) {
	u.of.Store(&snapshot)
	u.armed.Store(true)
}

// requireHeld waits until the unlock is held: the verification has answered,
// and its worker holds the lock still.
func (u *unlockHold) requireHeld(t *testing.T) {
	t.Helper()
	select {
	case <-u.p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no holder of the backups folder's lock came to let it go")
	}
}

// letGoAndAwaitEnd lets the unlock go and waits for the verification's worker
// to have let go of everything it held, the folder's lock free as its end is
// heard.
func (u *unlockHold) letGoAndAwaitEnd(t *testing.T) {
	t.Helper()
	u.p.release()
	select {
	case <-u.ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the verification's worker never let go of what it held")
	}
	if u.heldErr != nil {
		t.Fatalf("asking the backups folder's lock as the verification's worker ended: %v", u.heldErr)
	}
	if u.held {
		t.Error("the verification's worker ended with the backups folder's lock still held")
	}
}

// release lets the unlock go: a cleanup's, for a test that ends early.
func (u *unlockHold) release() { u.p.release() }

// backupsLockHeld reports whether the lock of the backups folder at root is
// held, asked as another process asks it: an exclusive try through an open of
// its own, let go at once.
func backupsLockHeld(root string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(root, backup.LockFileName), os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	switch err := fsutil.TryLock(f); {
	case err == nil:
		return false, fsutil.Unlock(f)
	case errors.Is(err, fsutil.ErrLocked):
		return true, nil
	default:
		return false, err
	}
}

// TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds is backlog B653's
// third round's red for a check given up past its deadline (Codex's first
// finding on round 2): the verification answers its caller within its bound,
// its check given up and recorded INCOMPLETE (deadline), and its worker keeps
// what it holds until the check's goroutine has ended, the snapshot's
// database still open, each hold asked apart: this process's slot (a
// verification in another backups folder, whose lock nobody holds, answers
// ErrCheckStillRunning at once), the backups folder's kernel lock (a
// verification of another snapshot there, made as another process makes it,
// is refused), the capture's ownership (its owner lock is held, asked as
// another process asks it), and the folder's prunes: a prune is skipped for
// the check, and the verification's own prune does not run, so the oldest
// verified snapshot is still there. The doctor names the check
// (StuckCheckOf). Once the check ends each hold is let go and taken again,
// the check's own result dropped (its record is as it was given up, never
// the VERIFIED it found), and the next prune removes the oldest snapshot,
// once the verification that took the folder's lock again as another
// process has ended: it answers before its worker lets go of that lock, whose
// unlock is held from the answer until the test waits for the worker's end
// (unlockHold, backlog B849). Red on the round before, which let all of it go
// once its caller had its answer, and pruned under it.
func TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds(t *testing.T) {
	src := storeWithTracks(t, 5)
	oldest := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	older := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	c := captureT(t, src)
	root := filepath.Dir(c.Dir)
	other, err := backup.Snapshot(t.Context(), storeWithTracks(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Dir(other)
	const deadline = 300 * time.Millisecond
	restoreDeadline := backup.SetVerifyDeadlineForTest(deadline)
	t.Cleanup(restoreDeadline)
	unlock := holdUnlockOf(t, root)
	entered, release := parkCheck(t, c.Dir)

	callers := joinBackupCallers(t, func() { release(); unlock.release() })
	done := make(chan backup.Result, 1)
	start := time.Now()
	callers.start(func() { done <- backup.FinishCapture(context.Background(), c, keepOne, backup.VerifyOptions{}) })
	<-entered
	res := requireReturnedWithin(t, done, start, deadline+backup.StopWriteBound/2+2*time.Second, "a verification whose check was given up")
	if !errors.Is(res.VerifyErr, backup.ErrCheckGivenUp) || !errors.Is(res.VerifyErr, context.DeadlineExceeded) {
		t.Errorf("the verification answered %v, want its check given up at the deadline", res.VerifyErr)
	}
	if !res.PruneSkipped {
		t.Errorf("the verification's own prune ran beside the check it gave up: %+v", res.Prune)
	}
	requireVerdict(t, "the record of the check given up", listed(t, c.Dir), backup.StateIncomplete, backup.ReasonDeadline,
		backup.StateIncomplete, backup.ReasonDeadline)
	recorded := readBytes(t, filepath.Join(c.Dir, backup.ManifestFile))

	// The slot: a verification in another backups folder, which would wait
	// its turn for ten seconds at most.
	asked := time.Now()
	slotCtx, cancelSlot := context.WithTimeout(t.Context(), 10*time.Second)
	_, err = backup.VerifySnapshot(slotCtx, other, backup.VerifyOptions{BackupsRoot: otherRoot})
	cancelSlot()
	if !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("a verification in another backups folder beside the check that runs on answered %v, want ErrCheckStillRunning", err)
	}
	if took := time.Since(asked); took > 2*time.Second {
		t.Errorf("the verification in another backups folder waited %v to be refused", took)
	}
	// The kernel's lock: a verification as another process makes it, once
	// the check's marker is there for it to read.
	awaitMarker(t, root)
	if _, err := backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), older, backup.VerifyOptions{LockWait: time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("a verification as another process beside the check that runs on answered %v, want ErrCheckStillRunning", err)
	}
	// The capture's ownership.
	if !backup.OwnerHeldAsAnotherProcessForTest(c.Dir) {
		t.Error("the capture's owner lock was let go while its check runs on")
	}
	// The folder's prune.
	pr, err := backup.Prune(root, 1)
	if err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
		t.Errorf("a prune beside the check that runs on: %+v, %v; want it skipped for the check", pr, err)
	}
	if !pathExists(t, oldest) {
		t.Error("a prune ran beside the check that runs on: the oldest verified snapshot is gone")
	}
	if s, held := backup.StuckCheckOf(root); !held || s.Snapshot != filepath.Base(c.Dir) {
		t.Errorf("StuckCheckOf = %+v, %v; want the check of %s", s, held, filepath.Base(c.Dir))
	}

	release()
	callers.wait(t)
	awaitVerificationEnd(t, c.Dir)
	awaitSlotFree(t)
	// Only the parked check was supposed to expire.
	restoreDeadline()

	if got := readBytes(t, filepath.Join(c.Dir, backup.ManifestFile)); !bytes.Equal(got, recorded) {
		t.Errorf("the check's own result was recorded once it ended:\n%s", got)
	}
	requireVerdict(t, "the record once the check ended", listed(t, c.Dir), backup.StateIncomplete, backup.ReasonDeadline,
		backup.StateIncomplete, backup.ReasonDeadline)
	if _, held := backup.StuckCheckOf(root); held {
		t.Error("the check reads as one still running once it ended")
	}
	if pathExists(t, filepath.Join(root, backup.StuckMarkerName)) {
		t.Error("the check's marker outlived it")
	}
	if _, err := backup.VerifySnapshot(t.Context(), other, backup.VerifyOptions{BackupsRoot: otherRoot}); err != nil {
		t.Errorf("a verification in another backups folder once the check ended: %v", err)
	}
	// older's record holds a VERIFIED: where no directory can be synced, its
	// check runs and records nothing, which is that answer. It answers before
	// its worker lets go of the snapshots' lock (verifyAndThen), whose unlock
	// is held here from the answer on, so the prune below waits for that
	// worker's end, never the answer (backlog B849: on Windows' CI the prune
	// that followed the answer found the lock still held).
	unlock.arm(older)
	if _, err := backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), older, backup.VerifyOptions{}); err != nil &&
		(fsutil.SyncsParentDir || !errors.Is(err, backup.ErrRecheckNotRecorded)) {
		t.Errorf("a verification as another process once the check ended: %v", err)
	}
	unlock.requireHeld(t)
	unlock.letGoAndAwaitEnd(t)
	if backup.OwnerHeldAsAnotherProcessForTest(c.Dir) {
		t.Error("the capture's owner lock is still held once its check ended")
	}
	pr, err = backup.Prune(root, 1)
	if err != nil || pr.Skipped || pathExists(t, oldest) {
		t.Errorf("the prune once the check ended: %+v, %v; want it run, the oldest verified snapshot gone", pr, err)
	}
}

// TestAVerificationBegunBeforeItsEndHookNeverRunsIt is backlog B849's second
// round (Codex's P2 on its first): a worker drops its hold, which is all a
// test's drain waits for, before it runs the end hook (SetOnVerifyEndForTest),
// so a worker that stood between the two while a test set that hook for a
// later verification of the same snapshot ran it once it went on, and the
// test's wait could pass on the earlier verification's end. Here the worker
// is parked there (SetAroundVerifyEndForTest), the hook is set, and the
// worker goes on: it runs no hook, since none stood when its verification
// began; a verification begun once the hook is set runs it. Red on the round
// before, which read the hook as the worker ended.
func TestAVerificationBegunBeforeItsEndHookNeverRunsIt(t *testing.T) {
	c := captureT(t, storeWithTracks(t, 5))
	c.Release()
	dir := c.Dir
	p := newParking()
	passed := make(chan struct{})
	var parkedOne atomic.Bool
	t.Cleanup(backup.SetAroundVerifyEndForTest(func(d string, end func()) {
		if d != dir || !parkedOne.CompareAndSwap(false, true) {
			end()
			return
		}
		p.park()
		end()
		close(passed)
	}))
	t.Cleanup(func() {
		p.release()
		select {
		case <-p.entered:
		default:
			return
		}
		select {
		case <-passed:
		case <-time.After(30 * time.Second):
			t.Error("the parked worker never passed its last step")
		}
	})
	if _, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the verification's worker never came to its last step")
	}
	if n := backup.ActiveHoldsForTest(); n != 0 {
		t.Fatalf("%d holds active with the worker at its last step; want its hold dropped, as a test's drain finds it", n)
	}
	ran := make(chan struct{}, 2)
	t.Cleanup(backup.SetOnVerifyEndForTest(func(d string) {
		if d == dir {
			select {
			case ran <- struct{}{}:
			default:
			}
		}
	}))
	p.release()
	select {
	case <-passed:
	case <-time.After(30 * time.Second):
		t.Fatal("the parked worker never passed its last step")
	}
	select {
	case <-ran:
		t.Error("a verification begun before the end hook was set ran it as its worker ended")
	default:
	}
	// Where no directory can be synced, a recheck of a VERIFIED records
	// nothing, which is that answer.
	if _, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{}); err != nil &&
		(fsutil.SyncsParentDir || !errors.Is(err, backup.ErrRecheckNotRecorded)) {
		t.Fatal(err)
	}
	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("a verification begun once the end hook was set never ran it")
	}
}

// TestAVerificationWaitingItsTurnHearsOfACheckGivenUp: a verification that
// began to wait for this process's slot behind another, whose check is then
// given up, answers ErrCheckStillRunning as soon as it is given up, not once
// its own context ends or the check does (backlog B653's third round).
func TestAVerificationWaitingItsTurnHearsOfACheckGivenUp(t *testing.T) {
	first, err := backup.Snapshot(t.Context(), storeWithTracks(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	other, err := backup.Snapshot(t.Context(), storeWithTracks(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	const deadline = 300 * time.Millisecond
	t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
	waiting := make(chan struct{})
	var waited sync.Once
	t.Cleanup(backup.SetOnSlotWaitForTest(func() { waited.Do(func() { close(waiting) }) }))
	entered, release := parkCheck(t, "")
	callers := joinBackupCallers(t, release)
	firstDone := make(chan error, 1)
	callers.start(func() {
		_, err := backup.VerifySnapshot(context.Background(), first, backup.VerifyOptions{})
		firstDone <- err
	})
	<-entered
	secondDone := make(chan error, 1)
	callers.start(func() {
		_, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: filepath.Dir(other)})
		secondDone <- err
	})
	<-waiting
	err = requireReturnedWithin(t, firstDone, time.Now(), deadline+backup.StopWriteBound/2+2*time.Second, "the verification whose check is given up")
	if !errors.Is(err, backup.ErrCheckGivenUp) {
		t.Fatalf("the first verification answered %v, want its check given up", err)
	}
	if err := requireReturnedWithin(t, secondDone, time.Now(), 2*time.Second, "the verification waiting its turn"); !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("the verification waiting its turn answered %v, want ErrCheckStillRunning", err)
	}
}

// TestAPanicInAVerificationLetsGoOfWhatItHolds: a check that panics (a
// defect of the bridge's own, in SQLite's code or the hash's) answers
// INCOMPLETE (aborted), and a panic in the verification's own steps answers
// an error saying so; either way the slot, the backups folder's lock and the
// capture's ownership are let go, and the next verification and prune run
// (backlog B653's third round, the review of its brief).
func TestAPanicInAVerificationLetsGoOfWhatItHolds(t *testing.T) {
	for _, where := range []string{"the check", "the verification's own steps"} {
		t.Run(where, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			c := captureT(t, src)
			var panicked atomic.Bool
			hook := func() {
				if panicked.CompareAndSwap(false, true) {
					panic("injected: a defect of the bridge's own")
				}
			}
			if where == "the check" {
				t.Cleanup(backup.SetOnDatabaseOpenForTest(hook))
			} else {
				t.Cleanup(backup.SetAfterCheckForTest(func(string) { hook() }))
			}
			res := backup.FinishCapture(t.Context(), c, keepOne, backup.VerifyOptions{})
			if where == "the check" {
				if res.VerifyErr != nil {
					t.Errorf("a check that panicked answered %v, want its INCOMPLETE recorded", res.VerifyErr)
				}
				requireVerdict(t, "the record of a check that panicked", listed(t, c.Dir), backup.StateIncomplete, backup.ReasonAborted,
					backup.StateIncomplete, backup.ReasonAborted)
			} else if res.VerifyErr == nil || !strings.Contains(res.VerifyErr.Error(), "an error of the bridge's own") {
				t.Errorf("a verification that panicked answered %v, want the panic said", res.VerifyErr)
			}
			awaitVerificationEnd(t, c.Dir)
			awaitSlotFree(t)
			if pr, err := backup.Prune(filepath.Dir(c.Dir), 7); err != nil || pr.Skipped {
				t.Errorf("a prune after the panic: %+v, %v; want it run, the lock let go", pr, err)
			}
			res2, err := backup.VerifySnapshot(t.Context(), c.Dir, backup.VerifyOptions{})
			if err != nil || res2.Record.DatabaseState != backup.StateVerified {
				t.Errorf("the next verification after the panic: %s, %v; want it run and VERIFIED", res2.Record.DatabaseState, err)
			}
		})
	}
}
