package backup_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// awaitMarkedWithin waits until a hold given up in this process holds the
// backups folder at root, asked by that spelling, and answers what it says,
// failing the test where none does within bound.
func awaitMarkedWithin(t *testing.T, root string, bound time.Duration) backup.StuckCheck {
	t.Helper()
	for deadline := time.Now().Add(bound); ; time.Sleep(10 * time.Millisecond) {
		if s, held := backup.StuckCheckHere(root); held {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("no hold given up held the backups folder within %v", bound)
		}
	}
}

// lockHeldHere reports whether a holder holds the backups folder's lock at
// root now: a try on the lock file from an open of its own, which the
// kernel's lock on another open refuses (fsutil.TryLock), let go at once.
func lockHeldHere(t *testing.T, root string) bool {
	t.Helper()
	f, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	switch err := fsutil.TryLock(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
		return false
	case errors.Is(err, fsutil.ErrLocked):
		return true
	default:
		t.Fatal(err)
	}
	return false
}

// vetoed reports whether a veto stands beside the snapshot in dir, in the
// bundle or beside it.
func vetoed(t *testing.T, dir string) bool {
	t.Helper()
	return pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir))
}

// TestAWriteGivenUpLandsNothingOnceItResumes is backlog B653's fifth round's
// red for a write its hold gave up (Codex's P1 and the Claude API's Medium on
// round 4, both reviews of the round's brief): a verdict's write that stalls
// inside its staging, or just before its rename, and the write of INCOMPLETE
// for a verification stopped before it began (abandonPending), are each given
// up within their bound, and once the stall ends the write renames nothing:
// the record is the bytes it held before, the veto the write put in place
// before its staging stands, and no reader reads the snapshot verified. A
// write whose rename landed before its sync stalled (a rename in the kernel
// is not taken back) removes no veto once it resumes. Red on the round
// before, whose write resumed and landed its VERIFIED, or its INCOMPLETE,
// after its caller was told it was given up, and removed the veto.
func TestAWriteGivenUpLandsNothingOnceItResumes(t *testing.T) {
	for _, tc := range []string{"a stall inside its staging", "a stall just before its rename", "a stall in its sync once its rename landed", "abandonPending's write"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			c := captureT(t, src)
			root := filepath.Dir(c.Dir)
			mpath := filepath.Join(c.Dir, backup.ManifestFile)
			before := readBytes(t, mpath)
			// The real check keeps its bound; only the parked write expires.
			t.Cleanup(backup.SetStepBoundForTest(c.Dir, 300*time.Millisecond, "its verdict's write", "its write of INCOMPLETE"))
			p := newParking()
			var stage, rename func(string) error
			switch tc {
			case "a stall just before its rename":
				rename = func(path string) error {
					if path == mpath {
						p.park()
					}
					return nil
				}
			case "a stall in its sync once its rename landed":
				t.Cleanup(backup.SetSyncVerdictForTest(func(path string) error {
					if path == mpath {
						p.park()
					}
					return fsutil.SyncParentDir(path)
				}))
			default:
				stage = func(dir string) error {
					if dir == c.Dir {
						p.park()
					}
					return nil
				}
			}
			t.Cleanup(backup.SetVerdictWriteFailuresForTest(stage, rename, nil))
			// Registered last, so run first: the stall let go and the hold's
			// end joined before any seam is put back.
			callerDone := make(chan struct{})
			t.Cleanup(func() {
				p.release()
				waitCaller(t, callerDone)
				awaitClean(t)
			})
			done := make(chan backup.Result, 1)
			started := time.Now()
			if tc == "abandonPending's write" {
				stopped, cancel := context.WithCancel(context.Background())
				cancel()
				go func() {
					defer close(callerDone)
					res, err := backup.VerifySnapshot(stopped, c.Dir, backup.VerifyOptions{BackupsRoot: root})
					done <- backup.Result{Verify: res, VerifyErr: err}
				}()
			} else {
				go func() {
					defer close(callerDone)
					done <- backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
				}()
			}
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the write never began")
			}
			res := requireReturnedWithin(t, done, started, 5*time.Second+2*backup.StopWriteBound+3*time.Second, "the verification whose write stalls")
			step := "its verdict's write"
			if tc == "abandonPending's write" {
				step = "its write of INCOMPLETE"
				if !errors.Is(res.VerifyErr, context.Canceled) {
					t.Errorf("the stopped verification answered %v, want its stop", res.VerifyErr)
				}
			} else {
				if res.Verify.Found.DatabaseState != backup.StateVerified || res.Verify.Found.FilesState != backup.StateVerified {
					t.Fatalf("the check found %s/%s, want VERIFIED: the test did not reach the case",
						res.Verify.Found.DatabaseState, res.Verify.Found.FilesState)
				}
				if !errors.Is(res.VerifyErr, backup.ErrCheckGivenUp) || !strings.Contains(res.VerifyErr.Error(), step) {
					t.Errorf("the verification answered %v, want it given up at %q", res.VerifyErr, step)
				}
			}
			// The write's own give-up marks the folder, naming it.
			if s := awaitMarkedWithin(t, root, 2*backup.StopWriteBound+3*time.Second); s.Step != step {
				t.Errorf("the folder is marked at %q, want %q", s.Step, step)
			}
			p.release()
			awaitClean(t)
			after := readBytes(t, mpath)
			switch tc {
			case "a stall in its sync once its rename landed":
				if bytes.Equal(after, before) {
					t.Fatal("the record is as it was: the rename did not land before the sync, and the test did not reach the case")
				}
				if !vetoed(t, c.Dir) {
					t.Error("the write given up removed its veto once its sync answered")
				}
			case "abandonPending's write":
				if !bytes.Equal(after, before) {
					t.Errorf("the write of INCOMPLETE given up landed once it resumed:\n%s", after)
				}
			default:
				if !bytes.Equal(after, before) {
					t.Errorf("the write given up landed once it resumed:\n%s", after)
				}
				if !vetoed(t, c.Dir) {
					t.Error("the veto the write put in place before its staging was removed")
				}
			}
			if got := readersVerified(t, c.Dir); len(got) > 0 {
				t.Errorf("after the write given up resumed, %s", strings.Join(got, "; "))
			}
		})
	}
}

// TestARecheckThatDidNotEndLeavesTheVetoItMade is backlog B653's fifth
// round's red for a veto only a check that ended earns (Codex's P2 and the
// Claude API's Medium on round 4, both reviews of the round's brief): with no
// veto beside the VERIFIED before it began, a recheck that ends INCOMPLETE
// within its bound, past its deadline, stopped by its caller, or on a panic of
// its check, leaves the veto it put in place before its check over the
// VERIFIED its record carries, so no reader reads the snapshot verified and
// the prune keeps the older verified snapshot. A check that ends lets it go
// (TestARecheckGivenUpLeavesTheVetoItMade). Red on the round before, which
// let the veto go for any verdict over a VERIFIED where none stood before, an
// INCOMPLETE included.
func TestARecheckThatDidNotEndLeavesTheVetoItMade(t *testing.T) {
	for _, tc := range []string{"past its deadline", "stopped by its caller", "on a panic of its check"} {
		t.Run(tc, func(t *testing.T) {
			requireDirectorySync(t)
			src := storeWithTracks(t, 20)
			anchor := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedSnapshot(t, src)
			if vetoed(t, dir) {
				t.Fatal("a veto stands before the recheck: the test did not reach the case")
			}
			ctx := context.Background()
			switch tc {
			case "past its deadline":
				t.Cleanup(backup.SetVerifyDeadlineForTest(150 * time.Millisecond))
				t.Cleanup(backup.SetOnDatabaseOpenForTest(func() { time.Sleep(400 * time.Millisecond) }))
			case "stopped by its caller":
				// One byte a second: the check's first read waits, and the stop
				// finds it there (TestACancelledVerificationWritesIncomplete).
				stopped, cancel := context.WithCancel(ctx)
				t.Cleanup(cancel)
				ctx = stopped
				t.Cleanup(backup.SetVerifyReadRateForTest(1))
				var once sync.Once
				t.Cleanup(backup.SetOnLowPriorityForTest(func() {
					once.Do(func() { time.AfterFunc(50*time.Millisecond, cancel) })
				}))
			case "on a panic of its check":
				var panicked atomic.Bool
				t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
					if panicked.CompareAndSwap(false, true) {
						panic("injected: a defect of the bridge's own")
					}
				}))
			}
			t.Cleanup(func() { awaitClean(t) })
			res, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
			if errors.Is(err, backup.ErrCheckGivenUp) {
				t.Fatalf("the recheck was given up (%v): the test did not reach the case", err)
			}
			if res.Found.DatabaseState != backup.StateIncomplete {
				t.Fatalf("the recheck found %s, want INCOMPLETE: the test did not reach the case", res.Found.DatabaseState)
			}
			awaitClean(t)
			if !vetoed(t, dir) {
				t.Error("the veto the recheck made before its check was removed, though its check did not end")
			}
			if got := readersVerified(t, dir); len(got) > 0 {
				t.Errorf("after the recheck that did not end, %s", strings.Join(got, "; "))
			}
			if pr, err := backup.Prune(filepath.Dir(dir), 1); err != nil || pr.Skipped {
				t.Fatalf("the prune answered %+v, %v; want it run: a skipped prune keeps the anchor whatever it reads", pr, err)
			}
			if !pathExists(t, anchor) {
				t.Error("the prune let the older verified snapshot go for one whose recheck did not end")
			}
		})
	}
}

// TestARecheckThatRecordedNothingIsFollowedByNoPrune is backlog B653's fifth
// round's red for the prune after a recheck (the Claude API's High on round
// 4): a recheck of a verified snapshot that records nothing, where no
// directory can be synced (its check finding the database damaged since,
// ErrRecheckNotRecorded) or where no veto could be made durable before it
// (ErrRecheckRefused), is followed by no prune, so the older verified
// snapshot, the one good one now, outlives a keep of one, and the result says
// the prune was skipped and why (Result.PruneSkipped, VerifyErr). Red on the
// round before, whose prune after it trusted the VERIFIED the recheck could
// not record against, and removed the older snapshot.
func TestARecheckThatRecordedNothingIsFollowedByNoPrune(t *testing.T) {
	for _, tc := range []string{"where no directory can be synced", "refused for want of a durable veto"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 20)
			older := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedSnapshot(t, src)
			flipLastByte(t, dir, backup.ManifestDBFileName)
			var want error
			switch tc {
			case "where no directory can be synced":
				t.Cleanup(backup.SetSyncsParentDirForTest(false))
				want = backup.ErrRecheckNotRecorded
			case "refused for want of a durable veto":
				requireDirectorySync(t)
				t.Cleanup(backup.SetSyncVetoForTest(func(string) error { return errors.New("injected: the veto's sync failed") }))
				want = backup.ErrRecheckRefused
			}
			res := backup.Finish(context.Background(), dir, keepOne, backup.VerifyOptions{BackupsRoot: filepath.Dir(dir)})
			if !errors.Is(res.VerifyErr, want) {
				t.Fatalf("the recheck answered %v, want %v", res.VerifyErr, want)
			}
			if tc == "where no directory can be synced" && res.Verify.Found.DatabaseState != backup.StateFailed {
				t.Fatalf("the recheck found %s, want the damage: the test did not reach the case", res.Verify.Found.DatabaseState)
			}
			if !res.PruneSkipped || res.Prune.Deleted != 0 {
				t.Errorf("after a recheck that recorded nothing: prune skipped %v, deleted %d; want it skipped", res.PruneSkipped, res.Prune.Deleted)
			}
			if !pathExists(t, older) {
				t.Error("the prune removed the older verified snapshot, trusting the VERIFIED the recheck recorded nothing against")
			}
		})
	}
}

// TestAStoppedVerificationRecordsNothingOverAVERIFIEDWhereNoDirectoryCanBeSynced
// is backlog B653's fifth round's red for the write of INCOMPLETE a
// verification stopped before it began leaves (abandonPending; Codex's P2 on
// round 4): where no directory can be synced (Windows, stood in for through
// its seam), a record that holds a VERIFIED, here its database's beside files
// INCOMPLETE, which another INCOMPLETE would change, is left byte for byte,
// with no veto made beside it, as a verification's own recheck leaves it
// there. Red on the round before, which merged the INCOMPLETE into the files'
// verdict and made a veto for the VERIFIED it kept.
func TestAStoppedVerificationRecordsNothingOverAVERIFIEDWhereNoDirectoryCanBeSynced(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir := verifiedSnapshot(t, src)
	m := readManifestT(t, dir)
	m.Verification.FilesState, m.Verification.FilesReasons = backup.StateIncomplete, []backup.Reason{backup.ReasonCancelled}
	writeManifestT(t, dir, m)
	mpath := filepath.Join(dir, backup.ManifestFile)
	before := readBytes(t, mpath)
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	t.Cleanup(func() { awaitClean(t) })
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backup.VerifySnapshot(stopped, dir, backup.VerifyOptions{BackupsRoot: filepath.Dir(dir)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped verification answered %v, want its stop", err)
	}
	awaitClean(t)
	if got := readBytes(t, mpath); !bytes.Equal(got, before) {
		t.Errorf("the write of INCOMPLETE changed the record over its VERIFIED:\n%s", got)
	}
	if vetoed(t, dir) {
		t.Error("the write of INCOMPLETE made a veto where no directory can be synced")
	}
}

// TestAPruneThatDoesNotEndIsGivenUpAndMarked is backlog B653's fifth round's
// red for the prune that holds the backups folder on its own (PruneContext;
// Codex's P2 and the Claude API's Medium on round 4): a prune whose removals
// hang is given up within its bound, counted from the lock, and answers
// ErrPruneGivenUp; the folder is marked at once, naming the prune and its
// step, so a verification and a prune behind it are refused at once; and once
// the removals end, the lock and the mark go. Red on the round before, whose
// prune held the lock unbounded, with no mark.
func TestAPruneThatDoesNotEndIsGivenUpAndMarked(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir := verifiedSnapshot(t, src)
	root := filepath.Dir(dir)
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	p := newParking()
	t.Cleanup(backup.SetOnPruneForTest(p.park))
	callerDone := make(chan struct{})
	t.Cleanup(func() {
		p.release()
		waitCaller(t, callerDone)
		awaitClean(t)
	})
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		defer close(callerDone)
		_, err := backup.Prune(root, 1)
		done <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the prune never began")
	}
	if err := requireReturnedWithin(t, done, started, 300*time.Millisecond+3*time.Second, "a prune whose removals hang"); !errors.Is(err, backup.ErrPruneGivenUp) {
		t.Errorf("the prune answered %v, want ErrPruneGivenUp", err)
	}
	if s, held := backup.StuckCheckHere(root); !held || !s.Pruning() || s.Step != "its removals" {
		t.Errorf("the folder reads %+v, %v; want it marked by the prune, at its removals", s, held)
	}
	answersWithin(t, 2*time.Second, "a verification behind the prune", p.release, func() {
		_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
		if !errors.Is(err, backup.ErrCheckStillRunning) || !strings.Contains(err.Error(), "a prune") {
			t.Errorf("a verification behind the prune answered %v, want ErrCheckStillRunning naming the prune", err)
		}
	})
	answersWithin(t, 2*time.Second, "a prune behind the prune", p.release, func() {
		if pr, err := backup.Prune(root, 1); err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
			t.Errorf("a prune behind the prune answered %+v, %v; want it skipped for the hold given up", pr, err)
		}
	})
	p.release()
	awaitClean(t)
	if lockHeldHere(t, root) {
		t.Error("the prune's lock was never let go")
	}
	if pr, err := backup.Prune(root, 1); err != nil || pr.Skipped {
		t.Errorf("a prune once the one given up had ended answered %+v, %v; want it run", pr, err)
	}
}

// TestALockThatDoesNotLetGoIsMarkedUntilItDoes is backlog B653's fifth round's
// red for the let-go of the backups folder's lock (Codex's P2 on round 4, the
// review of the round's brief): a verification that kept within every bound,
// and recorded its verdict, whose unlock of the lock (or its close) does not
// answer, is given up past releaseBound: the folder is marked in this
// process, naming that step, with no marker written beside the lock once its
// let-go has begun, so a verification behind it is refused at once; and once
// the unlock answers, the mark goes and the lock is free. Red on the round
// before, whose let-go of the lock had no bound and left the folder unmarked.
func TestALockThatDoesNotLetGoIsMarkedUntilItDoes(t *testing.T) {
	src := storeWithTracks(t, 5)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	other, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(dir)
	t.Cleanup(backup.SetReleaseBoundForTest(200 * time.Millisecond))
	p := newParking()
	var first atomic.Bool
	t.Cleanup(backup.SetOnLockReleaseForTest(func(string) {
		if first.CompareAndSwap(false, true) {
			p.park()
		}
	}))
	t.Cleanup(func() {
		p.release()
		awaitClean(t)
	})
	res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
	if err != nil || !res.Recorded {
		t.Fatalf("the verification answered %v, recorded %v; want its verdict recorded", err, res.Recorded)
	}
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the lock's let-go never began")
	}
	if s := awaitMarkedWithin(t, root, 3*time.Second); s.Step != "letting go of the snapshots' lock" || s.Snapshot != filepath.Base(dir) {
		t.Errorf("the folder is marked %+v; want the verification of %s, at the let-go of the lock", s, filepath.Base(dir))
	}
	if pathExists(t, filepath.Join(root, backup.StuckMarkerName)) {
		t.Error("a marker was written beside the lock once its let-go had begun")
	}
	answersWithin(t, 2*time.Second, "a verification behind the let-go", p.release, func() {
		if _, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: filepath.Dir(other)}); !errors.Is(err, backup.ErrCheckStillRunning) {
			t.Errorf("a verification behind the let-go answered %v, want ErrCheckStillRunning", err)
		}
	})
	p.release()
	awaitClean(t)
	if lockHeldHere(t, root) {
		t.Error("the lock was never let go")
	}
	if pathExists(t, filepath.Join(root, backup.StuckMarkerName)) {
		t.Error("a marker was left beside the lock")
	}
}

// TestAnOwnershipIsLetGoBeforeTheLock is backlog B653's fifth round's red for
// a snapshot's ownership, every way a holder lets it go (Codex's P2 and the
// Claude API's Medium on round 4, both reviews of the round's brief): an
// ownership a claim took once its caller had stopped waiting, one its step
// took once that step was given up, and a capture's own once its
// verification has ended, is let go by the hold that holds the backups
// folder's lock, before that lock, as a step bounded releaseBound: while its
// unlock does not answer the lock stays held, and past the bound the folder
// is marked; and a verification refused at once, before it held anything,
// answers within StopWriteBound whatever the ownership's let-go does. Red on
// the round before, where each of these ownerships was let go after the lock,
// or on a goroutine nobody waited for, with no bound and no mark.
func TestAnOwnershipIsLetGoBeforeTheLock(t *testing.T) {
	for _, tc := range []string{"taken once its claim's caller stopped waiting", "taken once its claim's step was given up",
		"a capture's, once its verification has ended", "a capture's, refused at once"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			var dir string
			var c *backup.Capture
			if tc == "a capture's, once its verification has ended" || tc == "a capture's, refused at once" {
				c = captureT(t, src)
				dir = c.Dir
			} else {
				d, err := backup.Snapshot(t.Context(), src)
				if err != nil {
					t.Fatal(err)
				}
				dir = d
			}
			root := filepath.Dir(dir)
			var blocker string
			if tc == "a capture's, refused at once" {
				b, err := backup.Snapshot(t.Context(), src)
				if err != nil {
					t.Fatal(err)
				}
				blocker = b
			}
			snap, err := fsutil.ResolveLinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			// The let-go of this snapshot's ownership alone hangs, until the
			// test lets it go.
			t.Cleanup(backup.SetReleaseBoundForTest(200 * time.Millisecond))
			ownerOut := newParking()
			t.Cleanup(backup.SetOnOwnerReleaseForTest(func(key string) {
				if key == snap {
					ownerOut.park()
				}
			}))
			switch tc {
			case "taken once its claim's caller stopped waiting":
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				answered := newParking()
				t.Cleanup(backup.SetOnClaimAnswerForTest(func() {
					cancel()
					answered.park()
				}))
				// Registered after every seam, so run first: every stand-in let
				// go and every hold's end joined before a seam is put back.
				t.Cleanup(func() {
					answered.release()
					ownerOut.release()
					awaitClean(t)
				})
				got, err := backup.Claim(ctx, dir, root)
				if got != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("the claim answered %v, %v; want it left to end on its own after its stop", got, err)
				}
				answered.release()
			case "taken once its claim's step was given up":
				snap, err := fsutil.ResolveLinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				// The claim's step resolves the snapshot's path the second time
				// it is resolved (the first is the claim's own resolution of
				// it), and stalls there past its bound.
				stall := newParking()
				var resolved atomic.Int32
				t.Cleanup(backup.SetMetadataForTest(func(p string) (string, error) {
					real, err := fsutil.ResolveLinks(p)
					if err == nil && real == snap && resolved.Add(1) == 2 {
						stall.park()
					}
					return real, err
				}, nil))
				t.Cleanup(func() {
					stall.release()
					ownerOut.release()
					awaitClean(t)
				})
				got, err := backup.Claim(context.Background(), dir, root)
				if got != nil || !errors.Is(err, backup.ErrCheckGivenUp) {
					t.Fatalf("the claim answered %v, %v; want its step given up", got, err)
				}
				stall.release()
			case "a capture's, once its verification has ended":
				t.Cleanup(func() {
					ownerOut.release()
					awaitClean(t)
				})
				res := backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
				if res.VerifyErr != nil || !res.Verify.Recorded {
					t.Fatalf("the verification answered %v, recorded %v; want its verdict recorded", res.VerifyErr, res.Verify.Recorded)
				}
			case "a capture's, refused at once":
				// A verification of another snapshot there given up holds the
				// folder, its check parked until the test's end.
				t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
				checkOut := newParking()
				t.Cleanup(backup.SetOnLowPriorityForTest(checkOut.park))
				callers := joinBackupCallers(t, func() { checkOut.release(); ownerOut.release() })
				if _, err := backup.VerifySnapshot(context.Background(), blocker, backup.VerifyOptions{BackupsRoot: root}); !errors.Is(err, backup.ErrCheckGivenUp) {
					t.Fatalf("the blocking verification answered %v, want it given up", err)
				}
				awaitMarkedWithin(t, root, 3*time.Second)
				done := make(chan backup.Result, 1)
				started := time.Now()
				callers.start(func() {
					done <- backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
				})
				res := requireReturnedWithin(t, done, started, backup.StopWriteBound+time.Second, "a verification refused at once whose ownership's let-go hangs")
				if !errors.Is(res.VerifyErr, backup.ErrCheckStillRunning) {
					t.Errorf("the verification answered %v, want ErrCheckStillRunning", res.VerifyErr)
				}
				select {
				case <-ownerOut.entered:
				case <-time.After(30 * time.Second):
					t.Fatal("the ownership was never let go")
				}
				return
			}
			select {
			case <-ownerOut.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the ownership was never let go")
			}
			// Its let-go hangs: the lock is still held, and the folder marked
			// once releaseBound has passed.
			time.Sleep(100 * time.Millisecond)
			if !lockHeldHere(t, root) {
				t.Error("the backups folder's lock was let go while the snapshot's ownership was still being let go")
			}
			s := awaitMarkedWithin(t, root, 3*time.Second)
			if tc == "a capture's, once its verification has ended" && s.Step != "letting go of its snapshot's ownership" {
				t.Errorf("the folder is marked at %q, want the ownership's let-go", s.Step)
			}
			ownerOut.release()
			awaitClean(t)
			if lockHeldHere(t, root) {
				t.Error("the lock was never let go")
			}
			release, err := backup.TakeOwnerForTest(dir)
			if err != nil {
				t.Fatalf("the snapshot's ownership was never let go: %v", err)
			}
			release()
		})
	}
}
