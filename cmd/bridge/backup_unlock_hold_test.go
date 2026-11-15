package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	context.AfterFunc(t.Context(), func() {
		time.Sleep(time.Second)
		close(out)
	})
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
