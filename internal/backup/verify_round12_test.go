package backup_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestAnIncompleteNoBarrierRecheckLeavesItsRecordTrusted: where no directory
// can be synced, a recheck of a verified snapshot that its caller stops at
// SQLite's open answers ErrRecheckNotRecorded, its check INCOMPLETE, and
// leaves the record as it was, with no veto, so it still reads VERIFIED.
//
// The check's grace past the stop is the test's (backlog B901). runCheck gives
// a check whose context has ended half of StopWriteBound more, then gives it
// up, and on macOS's CI the check's thread, at the lowest priority, did not
// answer the stop within it: "a step of the verification did not end and was
// given up, …: its check did not end within its bound (context canceled)".
// The grace is now held until the verification has answered (checkGraceOf,
// given dir, the path runCheck hands the seam: resolveSnapshotIn resolves the
// snapshot, and verifiedSnapshot answers it resolved), and the stop, made on
// the check's own thread at the open, holds that thread until the grace has
// begun, so in every run the check answers within the grace, whatever its
// thread's pace. The read-back after it is held at its gate, so the stop's
// bound on that step gives nothing up either (SetStepGateForTest); and the
// recheck runs on a caller of the test's, its answer awaited under a watchdog
// that says what had come, and joined at the test's end once everything the
// test holds is let go (joinBackupCallers). What no seam holds is the
// caller's own expiry, StopWriteBound from the stop (verifyAndThen): a
// check's answer and the worker's read-back after it slower than that still
// end the verification as "the verification was left to end on its own 2s
// after its stop, holding the snapshots' lock until it does: context
// canceled", which is that bound, not this one.
func TestAnIncompleteNoBarrierRecheckLeavesItsRecordTrusted(t *testing.T) {
	dir := verifiedSnapshot(t, storeWithTracks(t, 5))
	root := filepath.Dir(dir)
	before := readBytes(t, filepath.Join(dir, backup.ManifestFile))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	begun, pass := checkGraceOf(t, dir)
	gate, restoreGate := backup.SetStepGateForTest(root, filepath.Base(dir), "its read of the record after it")
	t.Cleanup(restoreGate)
	// The open's hook stops the verification, then holds the check's thread
	// until its grace has begun, or until the test's end lets it go.
	ended := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(ended) })
	t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
		cancel()
		select {
		case <-begun:
		case <-ended:
		}
	}))
	// Registered after every seam, so it runs first: the hook, the grace and
	// the gate let go, the recheck joined and its holds drained, and only then
	// the seams put back.
	callers := joinBackupCallers(t, func() {
		letGo()
		pass()
		gate.FireAll()
		cancel()
	})
	done := make(chan verifyAnswer, 1)
	callers.start(func() {
		res, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
		done <- verifyAnswer{res, err}
	})
	a := awaitRecheckAnswer(t, begun, done)
	pass()
	awaitClean(t)
	select {
	case <-begun:
	default:
		t.Fatalf("the check's grace never began, and the recheck answered %v: the test did not reach the case", a.err)
	}
	if errors.Is(a.err, backup.ErrCheckGivenUp) {
		t.Fatalf("the recheck was given up (%v): the test did not reach the case", a.err)
	}
	if !errors.Is(a.err, backup.ErrRecheckNotRecorded) || a.res.Found.DatabaseState != backup.StateIncomplete {
		t.Fatalf("expected a recheck interrupted at SQLite's open: %+v, %v", a.res, a.err)
	}
	if vetoed(t, dir) || !bytes.Equal(before, readBytes(t, filepath.Join(dir, backup.ManifestFile))) {
		t.Error("an incomplete recheck changed the record or made a veto")
	}
	requireVerdict(t, "interrupted recheck", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
}

// awaitRecheckAnswer is the answer done gives, the beginning of the check's
// grace (begun) heard beside it in either order, under a watchdog of two
// minutes at most, capped by the test binary's deadline (waitWithin), past
// which the test fails saying whether the grace had begun.
func awaitRecheckAnswer(t *testing.T, begun <-chan struct{}, done <-chan verifyAnswer) verifyAnswer {
	t.Helper()
	bound := waitWithin(t, 2*time.Minute)
	watchdog := time.NewTimer(bound)
	defer watchdog.Stop()
	for {
		select {
		case <-begun:
			begun = nil
		case a := <-done:
			return a
		case <-watchdog.C:
			t.Fatalf("the recheck did not answer within %v (its check's grace begun: %v)", bound, begun == nil)
			return verifyAnswer{}
		}
	}
}
