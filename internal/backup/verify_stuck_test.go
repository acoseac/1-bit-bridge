package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// awaitMarker reads the marker of a hold given up beside the lock of the
// backups folder at root, which the hold writes on a goroutine of its own,
// until it decodes as the hold want takes (what names it), and answers it: a
// read every 10 ms until its wait's deadline, 30 s capped by the test
// binary's (awaitMarkerWithin), touching no production hook, so a write that
// ended before the wait began is found as one under way is. The writer
// creates the marker before it writes it (writeMarker), so a read between the
// two meets an empty file, which a wait for the file alone handed to a decode
// (backlog B899, on main's macOS CI; the helper is B871's round 5's). A
// marker not there yet, one that does not decode, and another hold's, left
// by an earlier one, are read again; past the deadline the test fails,
// naming what the last read met.
func awaitMarker(t *testing.T, root, what string, want func(backup.StuckCheck) bool) backup.StuckCheck {
	t.Helper()
	return awaitMarkerWithin(t, root, what, want, 30*time.Second, t.Fatalf)
}

// awaitMarkerWithin is awaitMarker's wait, limit at most, capped by the test
// binary's deadline (waitWithin), failing through fatalf (awaitMarker's
// t.Fatalf, a case's own record in its tests). Its deadline is absolute,
// asked after every read, and each pause before the next read is cut to what
// is left of it, so neither a slow read nor a host that runs it late takes
// the wait past it, as a count of reads fixed when the wait began (B871's
// round 5's) did. Each read's answer goes to onMarkerRead, nil but in its
// tests.
func awaitMarkerWithin(t *testing.T, root, what string, want func(backup.StuckCheck) bool, limit time.Duration, fatalf func(format string, args ...any)) backup.StuckCheck {
	t.Helper()
	const every = 10 * time.Millisecond
	deadline := time.Now().Add(waitWithin(t, limit))
	marker := filepath.Join(root, backup.StuckMarkerName)
	reads, last := 0, ""
	for {
		data, err := os.ReadFile(marker)
		reads++
		if onMarkerRead != nil {
			onMarkerRead(data, err)
		}
		var s backup.StuckCheck
		switch {
		case err != nil:
			last = err.Error()
		case json.Unmarshal(data, &s) != nil:
			last = fmt.Sprintf("%d bytes that do not decode", len(data))
		case !want(s):
			last = fmt.Sprintf("another hold's marker, %+v", s)
		default:
			return s
		}
		left := time.Until(deadline)
		if left <= 0 {
			fatalf("no marker of %s was read beside the lock of %s in %d reads; the last met %s", what, root, reads, last)
			return backup.StuckCheck{}
		}
		time.Sleep(min(every, left))
	}
}

// onMarkerRead, nil but in awaitMarker's own tests (setOnMarkerRead), hears
// each read awaitMarkerWithin makes, on its goroutine, as the read returns and
// before it is decoded, given what the read answered: a case fills or
// replaces the marker there, between two reads, so nothing touches the file
// while one is under way.
var onMarkerRead func(data []byte, err error)

// setOnMarkerRead sets onMarkerRead to fn and registers at once the cleanup
// that puts the previous one back. A case that sets it is never parallel and
// starts no goroutine that reads it, so that cleanup runs once nothing can.
func setOnMarkerRead(t *testing.T, fn func(data []byte, err error)) {
	t.Helper()
	prev := onMarkerRead
	onMarkerRead = fn
	t.Cleanup(func() { onMarkerRead = prev })
}

// markerBytes is the marker a hold given up as s writes (writeMarker's
// json.Marshal).
func markerBytes(t *testing.T, s backup.StuckCheck) []byte {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sameHold reports whether a and b name one hold given up, their Since
// compared as instants.
func sameHold(a, b backup.StuckCheck) bool {
	return a.Snapshot == b.Snapshot && a.Step == b.Step && a.Since.Equal(b.Since) && a.Restore == b.Restore
}

// parkCheck parks the first check whose thread begins
// (SetOnLowPriorityForTest: on the check's own thread, its priority lowered,
// before it reads anything of the snapshot) until release is called; entered
// closes once one is parked. Every check comes to it, whatever its deadline:
// nothing before it asks the check's context, so a check given up before its
// thread ran (a thread the host runs late) parks there all the same, and
// what its worker holds stays held until it is let go. A park at SQLite's
// open came after the paced hash of the database, which a deadline that
// passed first cuts short, the check answering INCOMPLETE with no park, and
// a test waiting for that park hung (backlog B872). Its cleanup lets the
// check go and joins the verification that owes owed its check (a capture's;
// "" for none) and this process's slot before it puts the seam back.
func parkCheck(t *testing.T, owed string) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	t.Cleanup(backup.SetOnLowPriorityForTest(func() {
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

// parkCheckOf parks the check of the snapshot in dir, resolved, where every
// check comes (SetOnCheckEntryForTest: on the check's own thread, its
// priority lowered, before it reads anything of the snapshot), until release
// is called; entered closes once it is parked. Only the first check of that
// snapshot parks: the check of any other snapshot goes by, so no other
// verification's check, a stray one's included, takes the park (B886's
// second round: parkCheck parks the first check it sees, whatever its
// snapshot). Its cleanup lets the check go and waits until no verification
// holds this process's slot before it puts the seam back.
func parkCheckOf(t *testing.T, dir string) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	t.Cleanup(backup.SetOnCheckEntryForTest(func(d string) {
		if d == dir && parked.CompareAndSwap(false, true) {
			close(in)
			<-out
		}
	}))
	release = sync.OnceFunc(func() { close(out) })
	t.Cleanup(func() {
		release()
		awaitSlotFree(t)
	})
	return in, release
}

// checkGraceOf holds the grace a check of the snapshot in dir is given once
// its context has ended (SetCheckGraceForTest) until pass is called; begun
// closes as that grace begins, the check's deadline (or its caller's stop)
// passed. A check that answers within the grace is taken, so a test that
// passes it only once the verification has answered gives no check up past
// its deadline, however late the host runs the check's thread (backlog
// B886). The seam is put back at the test's end, once every verification has
// returned: a cleanup registered after this one passes the grace first.
func checkGraceOf(t *testing.T, dir string) (begun <-chan struct{}, pass func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetCheckGraceForTest(func(d string) <-chan struct{} {
		if d != dir {
			return nil
		}
		once.Do(func() { close(in) })
		return out
	}))
	return in, sync.OnceFunc(func() { close(out) })
}

// awaitParkedAndGivenUp waits for a check parked to be given up (entered,
// parkCheck's or a parking's) and for its verification's answer (done), which
// read reads, and which comes once the check's deadline has given it up, in
// whichever order the two come: a check whose thread the host runs late is
// given up before it parks, and parks all the same. The answer is required
// within bound of start. An answer that comes while no check is parked and is
// not a check given up (a verification refused before its check began, a
// check that ran to its end) fails the test at once, saying what it was, and
// so does either wait past bound and 30 s (backlog B872).
func awaitParkedAndGivenUp[T any](t *testing.T, entered <-chan struct{}, done <-chan T, read func(T) (backup.Verification, error), start time.Time, bound time.Duration) T {
	t.Helper()
	var answer T
	timeout := time.NewTimer(bound + 30*time.Second)
	defer timeout.Stop()
	for entered != nil || done != nil {
		select {
		case <-entered:
			entered = nil
		case answer = <-done:
			done = nil
			if took := time.Since(start); took > bound {
				t.Errorf("the verification whose check was given up answered %v after it began; its bound is %v", took, bound)
			}
			if record, err := read(answer); entered != nil && !errors.Is(err, backup.ErrCheckGivenUp) {
				t.Fatalf("the verification answered %v before its check reached its park, its record %s %v: no check was left to give up",
					err, record.DatabaseState, record.DatabaseReasons)
			}
		case <-timeout.C:
			if done == nil {
				t.Fatal("the check given up never reached its park")
			}
			t.Fatalf("the verification did not answer within %v (its check parked: %v)", bound+30*time.Second, entered == nil)
		}
	}
	return answer
}

// finishAnswer reads a FinishCapture's answer for awaitParkedAndGivenUp.
func finishAnswer(r backup.Result) (backup.Verification, error) { return r.Verify.Record, r.VerifyErr }

// verifyAnswer is a VerifySnapshot's answer, handed on by its caller.
type verifyAnswer struct {
	res backup.VerifyResult
	err error
}

// read reads it for awaitParkedAndGivenUp.
func (a verifyAnswer) read() (backup.Verification, error) { return a.res.Record, a.err }

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
	heard sync.Once
	ended chan struct{}
	// held and heldErr are what the end found of the folder's lock, written
	// before ended closes.
	held    bool
	heldErr error
}

// holdUnlockOf installs an unlockHold on the backups folder at root, not
// armed. It is installed before the verifications whose unlock it may hold
// start, since SetOnLockReleaseForTest's seam is read without a lock, and
// that seam is put back once every hold has ended: joinBackupCallers'
// cleanup, registered after it, runs first.
func holdUnlockOf(t *testing.T, root string) *unlockHold {
	t.Helper()
	u := &unlockHold{root: root, p: newParking(), ended: make(chan struct{})}
	t.Cleanup(backup.SetOnLockReleaseForTest(func(dir string) {
		if dir == u.root && u.armed.CompareAndSwap(true, false) {
			u.p.park()
		}
	}))
	return u
}

// arm holds the next unlock of the folder's lock, and sets the end hook for
// the verification of snapshot, the dir its caller passes. A worker runs the
// hook that stood as its verification began, so only a verification begun
// from here on can run it, never an earlier one of the same snapshot whose
// worker has yet to end (backlog B849's second round). The hook's restore,
// registered here, may run before such a worker calls the hook it took,
// which touches this hold's own state alone.
func (u *unlockHold) arm(t *testing.T, snapshot string) {
	t.Helper()
	t.Cleanup(backup.SetOnVerifyEndForTest(func(dir string) {
		if dir == snapshot {
			u.heard.Do(func() {
				// Asked where the end happens, on the worker's goroutine.
				u.held, u.heldErr = backupsLockHeld(u.root)
				close(u.ended)
			})
		}
	}))
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
// what it holds until the check's goroutine has ended, a check whose thread
// does not return (parked before it reads the snapshot, parkCheck, as one in
// a read of a hung mount is), each hold asked apart: this process's slot (a
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
// (unlockHold, backlog B849). The park and the answer are awaited in either
// order, bounded (awaitParkedAndGivenUp; backlog B872: parked at SQLite's
// open, the check a deadline cut short in its hash never parked, and the
// wait for it ran to the package's timeout on macOS's CI). Red on the round
// before, which let all of it go once its caller had its answer, and pruned
// under it.
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
	res := awaitParkedAndGivenUp(t, entered, done, finishAnswer, start, deadline+backup.StopWriteBound/2+2*time.Second)
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
	// the check's marker reads whole. No hold before the check's was given up
	// in root (the earlier verifications there ended), so the marker read is
	// the check's, which its words say. In this process the check's mark
	// refuses it first (verifyAndThen asks this process's marks before any
	// filesystem); another process's read of the marker is
	// TestACheckThatDidNotEndInAnotherProcessHoldsTheFolderUntilItEnds's.
	awaitMarker(t, root, "the check given up", func(s backup.StuckCheck) bool {
		return !s.Restoring() && s.Snapshot == filepath.Base(c.Dir)
	})
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
	unlock.arm(t, older)
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
// its own context ends or the check does (backlog B653's third round). The
// first verification's deadline counts from its lock, which the test holds as
// another process would until the second waits its turn, the first holding
// the slot meanwhile. The second is held where its wait is signalled
// (SetOnSlotWaitForTest) until the first has answered its check given up, its
// hold marked, so the give-up comes while the second waits, whatever the
// host's pace, and is to reach it as the notice of that give-up, which it must
// have heard (SetOnSlotNoticeForTest) when it answers. The first's check,
// parked before it reads anything (parkCheck), is given up and does not end.
// An answer from the second before its wait is a refusal as it arrived, which
// fails the case at once. The hold is bounded, and let go at the test's end
// before the callers are joined, whatever the first did. Backlog B872: the
// second began once the first's check had parked at SQLite's open, a park the
// first's deadline could cut short, which left a wait with no bound that
// never ended; and takeSlot signalled the wait before it asked whether the
// slot's holder was given up, so a second refused as it looked, never told,
// passed the case. With that order put back, the hold makes the second find
// the first given up with no notice heard, and the case fails saying so.
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
	unlock := holdLockAsAnotherProcess(t, filepath.Dir(first))
	lockWaiting, waiting := make(chan struct{}), make(chan struct{})
	// The second's wait is held until the first has answered its check given
	// up (firstGivenUp) or the test ends (abort), at most holdBound, past which
	// it goes on and says so to the test's goroutine (heldTooLong): a hook on
	// the verification's goroutine never fails the case itself.
	const holdBound = time.Minute
	firstGivenUp, abort := make(chan struct{}), make(chan struct{})
	heldTooLong, heard := make(chan struct{}, 1), make(chan struct{})
	var lockWaited, waited, noticed sync.Once
	t.Cleanup(backup.SetOnLockWaitForTest(func() { lockWaited.Do(func() { close(lockWaiting) }) }))
	t.Cleanup(backup.SetOnSlotWaitForTest(func() {
		waited.Do(func() {
			close(waiting)
			bound := time.NewTimer(holdBound)
			defer bound.Stop()
			select {
			case <-firstGivenUp:
			case <-abort:
			case <-bound.C:
				heldTooLong <- struct{}{}
			}
		})
	}))
	t.Cleanup(backup.SetOnSlotNoticeForTest(func() { noticed.Do(func() { close(heard) }) }))
	entered, release := parkCheck(t, "")
	letGo := sync.OnceFunc(func() { close(abort) })
	callers := joinBackupCallers(t, func() { letGo(); unlock(); release() })
	firstDone := make(chan error, 1)
	callers.start(func() {
		_, err := backup.VerifySnapshot(context.Background(), first, backup.VerifyOptions{})
		firstDone <- err
	})
	awaitEntered(t, lockWaiting, "the first verification's wait for its backups folder's lock, this process's slot held")
	secondDone := make(chan error, 1)
	callers.start(func() {
		_, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: filepath.Dir(other)})
		secondDone <- err
	})
	// The case's premise: the second waits its turn, the first not given up;
	// an answer before that is a refusal as it arrived, never a notice heard.
	select {
	case <-waiting:
	case err := <-secondDone:
		t.Fatalf("the second verification answered %v before it waited its turn: refused as it arrived, never told", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the second verification never waited its turn")
	}
	// The first takes its lock at its next try, its deadline counted from it,
	// and is given up while the second is held at its wait.
	unlocked := time.Now()
	unlock()
	err = requireReturnedWithin(t, firstDone, unlocked, deadline+backup.StopWriteBound/2+2*time.Second, "the verification whose check is given up")
	if !errors.Is(err, backup.ErrCheckGivenUp) {
		t.Fatalf("the first verification answered %v, want its check given up", err)
	}
	if _, marked := backup.StuckCheckHere(filepath.Dir(first)); !marked {
		t.Fatal("the first verification answered its check given up with no mark of it standing")
	}
	close(firstGivenUp)
	err = requireReturnedWithin(t, secondDone, time.Now(), 2*time.Second, "the verification waiting its turn")
	select {
	case <-heldTooLong:
		t.Fatalf("the second verification's wait was let go after %v, the first not given up", holdBound)
	default:
	}
	select {
	case <-heard:
	default:
		t.Fatalf("the second verification answered %v without hearing the notice of the first's give-up: it found the first given up as it looked, refused, never told", err)
	}
	if !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("the verification waiting its turn answered %v, want ErrCheckStillRunning", err)
	}
	awaitEntered(t, entered, "the park of the check given up")
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

// TestTheMarkerWaitReadsAnEmptyMarkerUntilItDecodes is backlog B899's red: a
// marker its writer has created (O_EXCL) and not yet written is an empty file
// (writeMarker), which main's wait, for the file to be there, handed to a
// decode, and macOS's CI failed
// TestARestoreRefusedForAnotherRestoreNamesItselfAndThatRestore with "the
// marker does not read: unexpected end of JSON input". The marker is planted
// empty, as that create leaves it, and written whole once awaitMarker has
// read it empty (onMarkerRead, between two of its reads, never a timer):
// awaitMarker answers the hold the written marker names, having read the
// empty file and read again. Red on main's wait, whose readStuckMarker
// failed at once at its decode of the empty file.
func TestTheMarkerWaitReadsAnEmptyMarkerUntilItDecodes(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, backup.StuckMarkerName)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	held := backup.StuckCheck{Snapshot: "S", Step: "its check", Since: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
	whole := markerBytes(t, held)
	var reads, empty int
	setOnMarkerRead(t, func(data []byte, err error) {
		reads++
		if err != nil || len(data) > 0 {
			return
		}
		empty++
		if empty == 1 {
			// The write that follows the create, between two reads, on the
			// test's own goroutine.
			if err := os.WriteFile(marker, whole, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	got := awaitMarker(t, root, "S's check", func(s backup.StuckCheck) bool { return s.Snapshot == "S" && !s.Restoring() })
	if !sameHold(got, held) {
		t.Errorf("awaitMarker answered %+v, want the written marker's %+v", got, held)
	}
	if empty < 1 || reads < 2 {
		t.Errorf("awaitMarker read the marker %d times, %d of them empty; want it read empty, then read again", reads, empty)
	}
}

// TestTheMarkerWaitReadsAnotherHoldsMarkerUntilItsOwnReplacesIt: a marker
// an earlier hold left beside the lock, which want refuses (snapshot A's
// verification, for a wait for A's restore), is read again until the awaited
// hold's replaces it, once awaitMarker has read it (onMarkerRead, between two
// of its reads), and awaitMarker answers the awaited hold, never the earlier
// one. Red on main's wait, whose readStuckMarker took the marker that stood
// there.
func TestTheMarkerWaitReadsAnotherHoldsMarkerUntilItsOwnReplacesIt(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, backup.StuckMarkerName)
	since := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	earlier := backup.StuckCheck{Snapshot: "A", Step: "its check", Since: since}
	awaited := backup.StuckCheck{Snapshot: "A", Step: "its read of the snapshot it restores", Since: since.Add(time.Minute), Restore: true}
	if err := os.WriteFile(marker, markerBytes(t, earlier), 0o644); err != nil {
		t.Fatal(err)
	}
	whole := markerBytes(t, awaited)
	var reads, earlierReads int
	setOnMarkerRead(t, func(data []byte, err error) {
		reads++
		var s backup.StuckCheck
		if err != nil || json.Unmarshal(data, &s) != nil || !sameHold(s, earlier) {
			return
		}
		earlierReads++
		if earlierReads == 1 {
			// The awaited hold's marker, between two reads, on the test's own
			// goroutine.
			if err := os.WriteFile(marker, whole, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	got := awaitMarker(t, root, "A's restore", func(s backup.StuckCheck) bool { return s.Restoring() && s.Snapshot == "A" })
	if !sameHold(got, awaited) {
		t.Errorf("awaitMarker answered %+v, want the awaited hold's %+v", got, awaited)
	}
	if earlierReads < 1 || reads < 2 {
		t.Errorf("awaitMarker read the marker %d times, %d of them the earlier hold's; want it read as the earlier hold's, then read again", reads, earlierReads)
	}
}

// TestTheMarkerWaitFailsAtItsDeadlineNamingWhatItsLastReadMet: a wait whose
// marker never reads as the hold it wants fails once its limit has passed,
// never before, once, through the fatalf it is given (a record here, so the
// failure it is asked for fails nothing), naming what it waited for, the
// folder, its reads and what the last one met: no marker, one that does not
// decode (a marker cut short), or another hold's. Its deadline is asked after
// every read, so a read that answers once the whole limit has passed is its
// last: red on a count of reads fixed as the wait began (B871's round 5's),
// which read on.
func TestTheMarkerWaitFailsAtItsDeadlineNamingWhatItsLastReadMet(t *testing.T) {
	const limit = 200 * time.Millisecond
	other := backup.StuckCheck{Snapshot: "B", Step: "its check", Since: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
	otherBytes := markerBytes(t, other)
	cut := otherBytes[:len(otherBytes)/2]
	for _, tc := range []struct {
		name    string
		planted []byte // nil: no marker
		slow    bool   // its first read answers once the whole limit has passed
		met     string // "": the read's own error
	}{
		{name: "no marker"},
		{name: "a marker cut short", planted: cut, met: fmt.Sprintf("%d bytes that do not decode", len(cut))},
		{name: "another hold's marker", planted: otherBytes, met: fmt.Sprintf("another hold's marker, %+v", other)},
		{name: "a first read that answers past the limit", planted: cut, slow: true, met: fmt.Sprintf("%d bytes that do not decode", len(cut))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, backup.StuckMarkerName)
			met := tc.met
			if tc.planted == nil {
				_, err := os.ReadFile(marker)
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("premise: the folder holds a marker (%v)", err)
				}
				met = err.Error()
			} else if err := os.WriteFile(marker, tc.planted, 0o644); err != nil {
				t.Fatal(err)
			}
			reads := 0
			setOnMarkerRead(t, func([]byte, error) {
				reads++
				if tc.slow && reads == 1 {
					// A read that answers once the whole limit has passed.
					time.Sleep(limit)
				}
			})
			var failed []string
			fatalf := func(format string, args ...any) { failed = append(failed, fmt.Sprintf(format, args...)) }
			start := time.Now()
			got := awaitMarkerWithin(t, root, "A's restore", func(s backup.StuckCheck) bool { return s.Restoring() && s.Snapshot == "A" }, limit, fatalf)
			took := time.Since(start)
			want := fmt.Sprintf("no marker of A's restore was read beside the lock of %s in %d reads; the last met %s", root, reads, met)
			if len(failed) != 1 || failed[0] != want {
				t.Errorf("the wait failed %q; want it failed once, %q", failed, want)
			}
			if got != (backup.StuckCheck{}) {
				t.Errorf("the wait that failed answered %+v", got)
			}
			if took < limit {
				t.Errorf("the wait failed %v in, before its limit of %v", took, limit)
			}
			if tc.slow && reads != 1 {
				t.Errorf("the wait read the marker %d times; its first read answered past the limit, so it was its last", reads)
			}
		})
	}
}
