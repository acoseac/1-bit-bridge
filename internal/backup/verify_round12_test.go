package backup_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

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
// thread's pace. What no grace holds is the caller's own expiry,
// StopWriteBound from the stop (verifyAndThen): a check's answer and the
// worker's read-back after it slower than that still end the verification as
// "the verification was left to end on its own 2s after its stop, holding
// the snapshots' lock until it does: context canceled", which is that bound,
// not this one.
func TestAnIncompleteNoBarrierRecheckLeavesItsRecordTrusted(t *testing.T) {
	dir := verifiedSnapshot(t, storeWithTracks(t, 5))
	before := readBytes(t, filepath.Join(dir, backup.ManifestFile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	begun, pass := checkGraceOf(t, dir)
	// The open's hook stops the verification, then holds the check's thread
	// until its grace has begun, or until the test lets it go.
	ended := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(ended) })
	t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
		cancel()
		select {
		case <-begun:
		case <-ended:
		}
	}))
	t.Cleanup(func() { awaitClean(t) })
	t.Cleanup(letGo)
	t.Cleanup(pass)
	res, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
	pass()
	letGo()
	awaitClean(t)
	select {
	case <-begun:
	default:
		t.Fatalf("the check's grace never began, and the recheck answered %v: the test did not reach the case", err)
	}
	if !errors.Is(err, backup.ErrRecheckNotRecorded) || res.Found.DatabaseState != backup.StateIncomplete {
		t.Fatalf("expected a recheck interrupted at SQLite's open: %+v, %v", res, err)
	}
	if vetoed(t, dir) || !bytes.Equal(before, readBytes(t, filepath.Join(dir, backup.ManifestFile))) {
		t.Error("an incomplete recheck changed the record or made a veto")
	}
	requireVerdict(t, "interrupted recheck", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
}
