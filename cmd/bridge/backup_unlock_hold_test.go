package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// holdCaseEnv makes the re-exec'd test binary run a case of an unlockHold's
// cleanup, a test that fails, as a process of its own, whose output its
// parent test reads (runHoldCase). Its name does not start with BRIDGE_.
const holdCaseEnv = "CMDBRIDGE_TEST_HOLD_CASE"

// holdCaseMark begins the line a case prints from a cleanup it registers
// before its hold, so once the hold's own cleanups have run.
const holdCaseMark = "the case's cleanup after the hold's ran"

// runHoldCase runs the test whose name run matches as a process of its own,
// the case name set in holdCaseEnv, and answers what it printed and its exit
// code.
func runHoldCase(t *testing.T, name, run string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run="+run, "-test.count=1", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), holdCaseEnv+"="+name)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		t.Fatalf("the case's process: %v\n%s", err, out)
		return "", 0
	}
}

// requireTheCasesOwnFailure fails the test unless the case's process ended as
// a test that failed, exit 1 with no panic and no race, printing its own
// failure alone and each of want.
func requireTheCasesOwnFailure(t *testing.T, out string, code int, want ...string) {
	t.Helper()
	if code != 1 || strings.Contains(out, "panic:") || strings.Contains(out, "DATA RACE") {
		t.Fatalf("the case's process exited %d, want 1 with its test's own failure:\n%s", code, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("the case's process did not print %q:\n%s", w, out)
		}
	}
	if n := strings.Count(out, "_test.go:"); n != 1 {
		t.Errorf("the case's test printed %d failures, want its own alone:\n%s", n, out)
	}
}

// TestAnEndedInstallSnapshotPutsTheLockReleaseSeamBack: once
// endedInstallSnapshot has returned, the lock-release seam is back as it
// stood before the hold (backup.SetOnLockReleaseForTest), so a holder of the
// backups folder's lock after it, the prune a test runs next, never runs the
// hold's hook: that prune's worker can outlive its own drain, and read the
// seam beside the cleanup that puts it back (backlog B869).
func TestAnEndedInstallSnapshotPutsTheLockReleaseSeamBack(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	var releases atomic.Int32
	t.Cleanup(backup.SetOnLockReleaseForTest(func(dir string) {
		if dir == root {
			releases.Add(1)
		}
	}))
	endedInstallSnapshot(t, cfgPath, root)
	if n := releases.Load(); n != 0 {
		t.Fatalf("the seam that stood before the hold ran %d times during the snapshot, want none", n)
	}
	pr, err := backup.Prune(root, 7)
	if err != nil || pr.Skipped {
		t.Fatalf("the prune after the snapshot: %+v, %v; want it run", pr, err)
	}
	// A prune answers before its worker lets the lock go.
	backuptest.AwaitIdle(t)
	if n := releases.Load(); n != 1 {
		t.Errorf("the prune's release ran the seam that stood before the hold %d times, want once: the hold's hook was still in place", n)
	}
}

// TestAHoldArmedForAVerificationRefusedBeforeItsWorkerLeavesTheTestsFailure:
// a hold armed as the bundle is kept (OnKept) for a verification refused
// before its worker begins, here at this process's slot, which a check given
// up in another backups folder holds, waits for no end: the hold's cleanup
// finds no hold left and returns, ending nothing and saying nothing, so the
// test fails with the snapshot's own failure and the cleanups after the
// hold's run (backlog B869). The round before waited 30 s for an end no
// worker would bring and then ended the run, the test's failure unprinted.
// The case runs as a process of its own (refusedBeforeItsWorker), whose
// output this test reads.
func TestAHoldArmedForAVerificationRefusedBeforeItsWorkerLeavesTheTestsFailure(t *testing.T) {
	if os.Getenv(holdCaseEnv) == "refused" {
		refusedBeforeItsWorker(t)
		return
	}
	out, code := runHoldCase(t, "refused", "^TestAHoldArmedForAVerificationRefusedBeforeItsWorkerLeavesTheTestsFailure$")
	requireTheCasesOwnFailure(t, out, code, "the snapshot: kept true",
		holdCaseMark+": kept true, its worker ended false, holds 0")
}

// refusedBeforeItsWorker is that case: the snapshot's verification is refused
// once OnKept has armed the hold, so snapshotInstall fails the test.
func refusedBeforeItsWorker(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	var u *unlockHold
	var kept atomic.Bool
	t.Cleanup(func() {
		if u == nil {
			return
		}
		ended := false
		select {
		case <-u.ended:
			ended = true
		default:
		}
		fmt.Fprintf(os.Stderr, "%s: kept %v, its worker ended %v, holds %d\n", holdCaseMark, kept.Load(), ended, backup.ActiveHoldsForTest())
	})
	u = holdUnlockOf(t, root)
	// A check given up in another backups folder holds this process's slot
	// until it is let go, by parkChecks' cleanup, which runs before the hold's.
	_, otherData, otherRoot := backupInstall(t)
	other, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: otherData, ManifestDB: manifest.DefaultDBPath(otherData)})
	if err != nil {
		t.Fatal(err)
	}
	parkChecks(t, otherRoot)
	given := make(chan error, 1)
	go func() {
		_, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: otherRoot})
		given <- err
	}()
	select {
	case err := <-given:
		if !errors.Is(err, backup.ErrCheckGivenUp) {
			t.Fatalf("the other folder's check answered %v, want it given up", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the other folder's check was never given up")
	}
	snapshotInstall(t, cfgPath, func(dir string) {
		kept.Store(true)
		u.arm(t, dir)
	})
	t.Error("the snapshot's verification ran, want it refused before its worker began")
}

// TestAHoldsCleanupWaitsForAWorkerParkedBeforeItsUnlock: a test that fails
// while the worker of the verification its hold was armed for is parked
// before its unlock, in the let-go of its capture's ownership, has the hold's
// cleanup wait for that worker, though it never came to the hold's hook: no
// cleanup after the hold's, the seam's restore and the fixture's removal
// among them, runs beside it (backlog B869). The case runs as a process of
// its own (parkedBeforeItsUnlock), its worker let go a second after its
// test's body ends.
func TestAHoldsCleanupWaitsForAWorkerParkedBeforeItsUnlock(t *testing.T) {
	if os.Getenv(holdCaseEnv) == "parked" {
		parkedBeforeItsUnlock(t)
		return
	}
	out, code := runHoldCase(t, "parked", "^TestAHoldsCleanupWaitsForAWorkerParkedBeforeItsUnlock$")
	requireTheCasesOwnFailure(t, out, code, "its verification's worker parked before its unlock",
		holdCaseMark+": holds 0")
}

// parkedBeforeItsUnlock is that case. The ownership seam is put back after the
// hold's cleanup, which drains the worker that read it.
func parkedBeforeItsUnlock(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	t.Cleanup(func() {
		fmt.Fprintf(os.Stderr, "%s: holds %d\n", holdCaseMark, backup.ActiveHoldsForTest())
	})
	var kept, parked atomic.Bool
	entered, out := make(chan struct{}), make(chan struct{})
	t.Cleanup(backup.SetOnOwnerReleaseForTest(func(string) {
		if kept.Load() && parked.CompareAndSwap(false, true) {
			close(entered)
			<-out
		}
	}))
	letParkedGo(t, &parked, out)
	u := holdUnlockOf(t, root)
	snapshotInstall(t, cfgPath, func(dir string) {
		kept.Store(true)
		u.arm(t, dir)
	})
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never came to its ownership's let-go")
	}
	t.Fatal("the test fails here, its verification's worker parked before its unlock")
}

// letParkedGo closes out a second after the test's body ends where parked is
// set by then, and at once where it is not: a case that failed before
// anything parked leaves no goroutine asleep behind it.
func letParkedGo(t *testing.T, parked *atomic.Bool, out chan struct{}) {
	t.Helper()
	context.AfterFunc(t.Context(), func() {
		if parked.Load() {
			time.Sleep(time.Second)
		}
		close(out)
	})
}

// requireTheRunEnded fails the test unless the case's process ended the run,
// exit 2 with a panic saying what, no race, and no line from the cleanup it
// registered before its hold, which runs only once the hold's own have.
func requireTheRunEnded(t *testing.T, out string, code int, what string) {
	t.Helper()
	if code != 2 || !strings.Contains(out, "panic: ") || !strings.Contains(out, what) || strings.Contains(out, "DATA RACE") {
		t.Fatalf("the case's process exited %d, want 2, the run ended saying %q:\n%s", code, what, out)
	}
	if strings.Contains(out, holdCaseMark) {
		t.Errorf("a cleanup after the hold's ran, though the run was to end there:\n%s", out)
	}
}

// TestAnEndHooksAskUnderWayHoldsTheHoldsCleanup: an end hook that has begun to
// ask the backups folder's lock (its probe) when the test's body ends holds
// the hold's cleanup until the ask ends, so no cleanup after the hold's, the
// fixture's removal among them, runs beside it: the worker drops its hold
// before it runs its end hook, so the count of holds says nothing of it
// (backlog B869). The ask is parked until a second after the body ends.
func TestAnEndHooksAskUnderWayHoldsTheHoldsCleanup(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	var askEnded atomic.Bool
	t.Cleanup(func() {
		if !askEnded.Load() {
			t.Error("the end hook's ask of the folder's lock was still running when the cleanup before the fixture's removal ran")
		}
	})
	u := holdUnlockOf(t, root)
	asking, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	u.probe = func(root string) (bool, error) {
		parked.Store(true)
		close(asking)
		<-out
		held, err := backupsLockHeld(root)
		askEnded.Store(true)
		return held, err
	}
	letParkedGo(t, &parked, out)
	snapshotInstall(t, cfgPath, func(dir string) { u.arm(t, dir) })
	u.requireHeld(t)
	u.release()
	select {
	case <-asking:
	case <-time.After(30 * time.Second):
		t.Fatal("the end hook never began its ask")
	}
}

// TestAnEndHookThatComesAfterTheTestsEndAsksNothing: an end hook that comes
// once the test's end is marked (letGo) asks nothing of the backups folder's
// lock, so it opens nothing in the folders the cleanups remove, and still
// closes the end it owes (backlog B869). The worker is held at its last step,
// its end hook's call, until the end is marked.
func TestAnEndHookThatComesAfterTheTestsEndAsksNothing(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	u := holdUnlockOf(t, root)
	var asks atomic.Int32
	u.probe = func(root string) (bool, error) {
		asks.Add(1)
		return backupsLockHeld(root)
	}
	var kept atomic.Pointer[string]
	atEnd, goOn := make(chan struct{}), make(chan struct{})
	var arrived, gone sync.Once
	t.Cleanup(backup.SetAroundVerifyEndForTest(func(dir string, end func()) {
		if k := kept.Load(); k != nil && dir == *k {
			arrived.Do(func() { close(atEnd) })
			<-goOn
		}
		end()
	}))
	letOn := func() { gone.Do(func() { close(goOn) }) }
	t.Cleanup(letOn)
	snapshotInstall(t, cfgPath, func(dir string) {
		kept.Store(&dir)
		u.arm(t, dir)
	})
	u.requireHeld(t)
	u.release()
	select {
	case <-atEnd:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never came to its last step")
	}
	// The test's end, as its context's end marks it.
	u.letGo()
	letOn()
	select {
	case <-u.ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the end hook never closed the end it owes")
	}
	if n := asks.Load(); n != 0 {
		t.Errorf("an end hook that came once the test's end was marked asked the folder's lock %d times, want none", n)
	}
}

// TestAnEndHooksAskThatNeverEndsEndsTheRun: an end hook's ask of the backups
// folder's lock that never ends, begun before the test's body ended, ends the
// run at the hold's cleanup's bound, saying so, and no cleanup after the
// hold's runs: a t.Fatal there would let them all run (backlog B869). The
// case runs as a process of its own (askThatNeverEnds).
func TestAnEndHooksAskThatNeverEndsEndsTheRun(t *testing.T) {
	if os.Getenv(holdCaseEnv) == "ask" {
		askThatNeverEnds(t)
		return
	}
	out, code := runHoldCase(t, "ask", "^TestAnEndHooksAskThatNeverEndsEndsTheRun$")
	requireTheRunEnded(t, out, code, "the end hook's ask of the backups folder's lock was still running")
}

// askThatNeverEnds is that case, its drain bound a second.
func askThatNeverEnds(t *testing.T) {
	holdDrainBound = time.Second
	cfgPath, _, root := backupInstall(t)
	t.Cleanup(func() { fmt.Fprintf(os.Stderr, "%s\n", holdCaseMark) })
	u := holdUnlockOf(t, root)
	asking := make(chan struct{})
	u.probe = func(string) (bool, error) {
		close(asking)
		select {}
	}
	snapshotInstall(t, cfgPath, func(dir string) { u.arm(t, dir) })
	u.requireHeld(t)
	u.release()
	select {
	case <-asking:
	case <-time.After(30 * time.Second):
		t.Fatal("the end hook never began its ask")
	}
}

// checkGivenUpIn gives up a check of a snapshot of the install at dataDir,
// whose backups folder is root, parked on its thread past a deadline of
// 300 ms: a hold that marks that folder and holds this process's slot until
// letGo lets the check go. The seams the check reads are put back only once
// it is drained: a cleanup registered after them, so run before them, lets
// u go (disarmed, so the check's own unlock of a folder u holds is not
// held), lets the check go, joins its caller, which reads the verify
// deadline once it has started the worker, and waits until no hold is left,
// each bounded by holdDrainBound and ending the run past it (endTheRun). It
// drains the check alone: a refused verification's let-go is left to u's own
// cleanup (holdUnlockOf). A cleanup registered before the seams, so run right
// after they are put back, fails the test where they were put back beside the
// check or its caller.
func checkGivenUpIn(t *testing.T, u *unlockHold, dataDir, root string) (letGo func()) {
	t.Helper()
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)})
	if err != nil {
		t.Fatal(err)
	}
	callerDone := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-callerDone:
		default:
			t.Error("the blocking check's caller was still running when the seams it reads were put back")
		}
		if n := backup.ActiveHoldsForTest(); n != 0 {
			t.Errorf("%d holds were left when the blocking check's seams were put back", n)
		}
	})
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	out := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(out) }) }
	t.Cleanup(backup.SetOnLowPriorityForTest(func() { <-out }))
	t.Cleanup(func() {
		if u != nil {
			u.letGo()
		}
		letGo()
		select {
		case <-callerDone:
		case <-time.After(holdDrainBound):
			endTheRun(t, fmt.Sprintf("the blocking check's caller did not return within %v of the check's let-go", holdDrainBound))
		}
		for deadline := time.Now().Add(holdDrainBound); backup.ActiveHoldsForTest() != 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				endTheRun(t, fmt.Sprintf("the blocking check's hold was still held %v after its let-go", holdDrainBound))
			}
		}
	})
	given := make(chan error, 1)
	go func() {
		defer close(callerDone)
		_, err := backup.VerifySnapshot(context.Background(), snap, backup.VerifyOptions{BackupsRoot: root})
		given <- err
	}()
	select {
	case err := <-given:
		if !errors.Is(err, backup.ErrCheckGivenUp) {
			t.Fatalf("the blocking check answered %v, want it given up", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the blocking check was never given up")
	}
	return letGo
}

// refusedLetGo is a snapshot of the install at dataDir, its backups folder
// root, whose verification is refused before its worker begins, at the
// folder's mark or at the slot, a check given up there or in another folder
// blocking it, with its capture's ownership let go on a goroutine of its own
// (letGoWithin) and that let-go parked, until out closes, once the ownership
// is let go, after its unlock and close (SetAfterOwnerReleaseForTest). Once
// the refusal is answered the blocker is let go and every hold is dropped, so
// only that let-go is left (backup.ActiveReleasesForTest). It answers the
// snapshot. A cleanup registered after the ownership seam and before the
// hold, so run once the hold's cleanups have run and before that seam is put
// back, requires that the let-go went on past its park, a signal of its own
// set there (the ownership is free from before the park), and otherwise
// waits for it, bounded, so the cleanups after it run beside nothing.
func refusedLetGo(t *testing.T, refusal, dataDir, root string, out chan struct{}, parked *atomic.Bool) (u *unlockHold, kept string) {
	t.Helper()
	var dir atomic.Pointer[string]
	var wentOn atomic.Bool
	entered := make(chan struct{})
	t.Cleanup(backup.SetAfterOwnerReleaseForTest(func(key string) {
		if d := dir.Load(); d != nil && filepath.Base(key) == filepath.Base(*d) && parked.CompareAndSwap(false, true) {
			close(entered)
			<-out
			wentOn.Store(true)
		}
	}))
	t.Cleanup(func() {
		if !parked.Load() || wentOn.Load() {
			return
		}
		t.Error("the refused verification's let-go was still parked past its ownership's release when the cleanup after the hold's ran")
		for deadline := time.Now().Add(holdDrainBound); !wentOn.Load(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				endTheRun(t, "the refused verification's let-go never went on past its park")
			}
		}
	})
	u = holdUnlockOf(t, root)
	blockerData, blockerRoot := dataDir, root
	if refusal == "at the slot" {
		_, blockerData, blockerRoot = backupInstall(t)
	}
	letBlockerGo := checkGivenUpIn(t, u, blockerData, blockerRoot)
	res := backup.SnapshotAndVerify(context.Background(), backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)},
		backup.SnapshotOptions{OnKept: func(d string) {
			dir.Store(&d)
			u.arm(t, d)
		}})
	if !res.Kept() || !errors.Is(res.VerifyErr, backup.ErrCheckStillRunning) {
		t.Fatalf("the snapshot: kept %v, %v; want it kept and its verification refused", res.Kept(), res.VerifyErr)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the refused verification never let its ownership go")
	}
	// Refused, the verification owes the hold no unlock: disarmed, so the
	// blocker's own, in this folder at its mark, is not held.
	u.armed.Store(false)
	letBlockerGo()
	for deadline := time.Now().Add(30 * time.Second); backup.ActiveHoldsForTest() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d holds were left once the blocker was let go", backup.ActiveHoldsForTest())
		}
	}
	if n := backup.ActiveReleasesForTest(); n != 1 {
		t.Fatalf("with the refused verification's let-go parked, %d let-goes are counted, want 1", n)
	}
	return u, res.Dir
}

// TestARefusedVerificationsLetGoHoldsTheHoldsCleanup: a verification refused
// before its worker began, at its folder's mark or at the slot a check given
// up holds, leaves its capture's ownership to a let-go its caller answers
// without once StopWriteBound has passed (letGoWithin). The hold's cleanup
// waits for that let-go too (backup.ActiveReleasesForTest), so the cleanup
// after it finds the let-go gone on past its park, which comes once the
// ownership is let go (refusedLetGo; backlog B869). The blocker is let go and
// every hold dropped before the test's body ends, so only that let-go is left
// to wait for; it is parked until a second after the body ends.
func TestARefusedVerificationsLetGoHoldsTheHoldsCleanup(t *testing.T) {
	for _, refusal := range []string{"at its folder's mark", "at the slot"} {
		t.Run(refusal, func(t *testing.T) {
			_, dataDir, root := backupInstall(t)
			out := make(chan struct{})
			var parked atomic.Bool
			letParkedGo(t, &parked, out)
			refusedLetGo(t, refusal, dataDir, root, out, &parked)
		})
	}
}

// TestABlockingChecksSeamsGoBackOnlyOnceItIsDrained: a test that fails with
// its blocking check given up and parked (checkGivenUpIn), before it drains
// that check itself, has the check let go, its caller joined and its hold
// dropped before the seams the check reads are put back: the cleanup right
// after those restores finds no hold left and the caller ended, and the test
// fails with its own failure alone, with no race and no panic (backlog B869).
// The case runs as a process of its own (failsWithItsBlockerParked).
func TestABlockingChecksSeamsGoBackOnlyOnceItIsDrained(t *testing.T) {
	if os.Getenv(holdCaseEnv) == "blocker" {
		failsWithItsBlockerParked(t)
		return
	}
	out, code := runHoldCase(t, "blocker", "^TestABlockingChecksSeamsGoBackOnlyOnceItIsDrained$")
	requireTheCasesOwnFailure(t, out, code, "the test fails here, its blocking check parked", holdCaseMark+": holds 0")
}

// failsWithItsBlockerParked is that case, its blocking check in the folder
// its hold is on.
func failsWithItsBlockerParked(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	t.Cleanup(func() {
		fmt.Fprintf(os.Stderr, "%s: holds %d\n", holdCaseMark, backup.ActiveHoldsForTest())
	})
	u := holdUnlockOf(t, root)
	checkGivenUpIn(t, u, dataDir, root)
	t.Fatal("the test fails here, its blocking check parked")
}

// TestARefusedVerificationsLetGoThatNeverEndsEndsTheRun: the let-go of a
// refused verification's capture's ownership that never ends ends the run at
// the hold's cleanup's bound, saying so, and no cleanup after the hold's
// runs (backlog B869). The case runs as a process of its own
// (letGoThatNeverEnds).
func TestARefusedVerificationsLetGoThatNeverEndsEndsTheRun(t *testing.T) {
	if os.Getenv(holdCaseEnv) == "letgo" {
		letGoThatNeverEnds(t)
		return
	}
	out, code := runHoldCase(t, "letgo", "^TestARefusedVerificationsLetGoThatNeverEndsEndsTheRun$")
	requireTheRunEnded(t, out, code, "0 holds and 1 refused verifications' let-goes still running")
}

// letGoThatNeverEnds is that case, refused at the slot, its drain bound a
// second.
func letGoThatNeverEnds(t *testing.T) {
	holdDrainBound = time.Second
	_, dataDir, root := backupInstall(t)
	t.Cleanup(func() { fmt.Fprintf(os.Stderr, "%s\n", holdCaseMark) })
	var parked atomic.Bool
	refusedLetGo(t, "at the slot", dataDir, root, make(chan struct{}), &parked)
}
