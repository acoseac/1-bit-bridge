package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAVerdictsWriteIsAdmittedOnlyBeforeAGiveUp is backlog B653's sixth
// round's red for the admission of a verdict's write (Codex's P1 on round 5,
// and both reviews of the round's brief): a verification parked once its check
// has ended, before its write, is stopped by its caller, which gives its
// worker up StopWriteBound later and answers; the worker then resumes, and its
// write is not admitted, the hold having been given up before it, so only an
// INCOMPLETE lands: over a PENDING, the record reads INCOMPLETE (cancelled);
// over a VERIFIED, whose merge keeps it, the record is byte for byte as it
// was and the veto put in place before the check stands, so no reader reads
// the snapshot verified. Red on the round before, whose write took a fresh
// signal whatever the hold's give-up said and published the completed check:
// the VERIFIED over the PENDING, and over the VERIFIED a new one, its veto
// removed.
func TestAVerdictsWriteIsAdmittedOnlyBeforeAGiveUp(t *testing.T) {
	for _, tc := range []string{"over a PENDING", "over a VERIFIED"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			var dir string
			if tc == "over a VERIFIED" {
				requireDirectorySync(t)
				dir = verifiedSnapshot(t, src)
			} else {
				d, err := backup.Snapshot(t.Context(), src)
				if err != nil {
					t.Fatal(err)
				}
				dir = d
			}
			root := filepath.Dir(dir)
			mpath := filepath.Join(dir, backup.ManifestFile)
			before := readBytes(t, mpath)
			p := newParking()
			t.Cleanup(backup.SetAfterCheckForTest(func(snap string) {
				if filepath.Base(snap) == filepath.Base(dir) {
					p.park()
				}
			}))
			// Registered last, so run first: the park let go and the hold's end
			// joined before the seam is put back.
			callerDone := make(chan struct{})
			t.Cleanup(func() {
				p.release()
				waitCaller(t, callerDone)
				awaitClean(t)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer close(callerDone)
				_, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{BackupsRoot: root})
				done <- err
			}()
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the check never ended")
			}
			if tc == "over a VERIFIED" && !vetoed(t, dir) {
				t.Fatal("no veto stands over the VERIFIED once the recheck has checked: the test did not reach the case")
			}
			cancel()
			// Its caller gives the worker up StopWriteBound after the stop, and
			// answers; only then does the worker go on to its write.
			err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+3*time.Second, "the verification stopped after its check")
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "left to end on its own") {
				t.Fatalf("the verification answered %v, want its worker given up after its stop", err)
			}
			p.release()
			awaitClean(t)
			switch tc {
			case "over a PENDING":
				requireVerdict(t, "the record once the worker given up wrote", listed(t, dir),
					backup.StateIncomplete, backup.ReasonCancelled, backup.StateIncomplete, backup.ReasonCancelled)
			case "over a VERIFIED":
				if got := readBytes(t, mpath); string(got) != string(before) {
					t.Errorf("a write after its hold's give-up changed the record over its VERIFIED:\n%s", got)
				}
				if !vetoed(t, dir) {
					t.Error("a write after its hold's give-up removed the veto its recheck made")
				}
			}
			if got := readersVerified(t, dir); len(got) > 0 {
				t.Errorf("after a write its hold gave up before it, %s", strings.Join(got, "; "))
			}
		})
	}
}

// TestNoVetoRemovalBeginsAfterItsWriteIsGivenUp is backlog B653's sixth
// round's red for the removals a verdict's write makes once it is durable
// (Codex's P1 on round 5, the Claude API's Low, both reviews of the round's
// brief): a write whose removal of the veto in the bundle stalls past the
// write's bound is given up, its caller told so; once the stall ends, that
// removal, already under way when the give-up came, finishes, and the next,
// of the veto beside the bundle, never begins, so no reader reads the
// snapshot verified. That one exception is pinned beside it: a lone veto
// whose removal was under way is gone once it finishes, and the record then
// reads what the check that ended found and the write made durable, VERIFIED.
// Red on the round before, whose write removed both vetoes, and the snapshot
// read verified after its caller was told the write was given up.
func TestNoVetoRemovalBeginsAfterItsWriteIsGivenUp(t *testing.T) {
	for _, tc := range []string{"the second of two removals", "a lone veto's removal under way"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(dir)
			inBundle, beside := filepath.Join(dir, backup.VetoFileName), backup.RootVetoPath(dir)
			if tc == "the second of two removals" {
				// Two vetoes a write that failed left, in the bundle and beside
				// it: a check that replaces both verdicts earns their removal.
				for _, v := range []string{inBundle, beside} {
					if err := os.WriteFile(v, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			// The real check keeps its bound; only the parked write expires.
			t.Cleanup(backup.SetStepBoundForTest(dir, 300*time.Millisecond, "its verdict's write"))
			p := newParking()
			t.Cleanup(backup.SetRemoveVetoForTest(func(path string) error {
				if path == inBundle {
					p.park()
				}
				return os.Remove(path)
			}))
			callerDone := make(chan struct{})
			t.Cleanup(func() {
				p.release()
				waitCaller(t, callerDone)
				awaitClean(t)
			})
			type answer struct {
				res backup.VerifyResult
				err error
			}
			done := make(chan answer, 1)
			started := time.Now()
			go func() {
				defer close(callerDone)
				res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
				done <- answer{res, err}
			}()
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the write never began to remove a veto")
			}
			a := requireReturnedWithin(t, done, started, 5*time.Second+2*backup.StopWriteBound+3*time.Second, "the verification whose veto's removal stalls")
			if a.res.Found.DatabaseState != backup.StateVerified || a.res.Found.FilesState != backup.StateVerified {
				t.Fatalf("the check found %s/%s, want VERIFIED: the test did not reach the case", a.res.Found.DatabaseState, a.res.Found.FilesState)
			}
			if !errors.Is(a.err, backup.ErrCheckGivenUp) || !strings.Contains(a.err.Error(), "its verdict's write") || a.res.Write != backup.WriteUnfinished {
				t.Fatalf("the verification answered %v, its write %v; want its write given up", a.err, a.res.Write)
			}
			p.release()
			awaitClean(t)
			if pathExists(t, inBundle) {
				t.Error("the removal under way when the write was given up never finished")
			}
			switch tc {
			case "the second of two removals":
				if !pathExists(t, beside) {
					t.Error("the removal of the veto beside the bundle began after the write was given up")
				}
				if got := readersVerified(t, dir); len(got) > 0 {
					t.Errorf("after the write given up, %s", strings.Join(got, "; "))
				}
			case "a lone veto's removal under way":
				requireVerdict(t, "the record once the removal under way finished", listed(t, dir),
					backup.StateVerified, "", backup.StateVerified, "")
			}
		})
	}
}

// TestAVetoSetAsideIsKeptOnceItsStepIsGivenUp is backlog B653's sixth round's
// red for the veto a recheck puts in place before its check (the Claude API's
// Low on round 5, and Codex's review of the round's brief): a recheck makes
// its veto in the bundle, whose sync stalls past the step's bound and then
// fails, and the veto beside the bundle cannot be made either; the step is
// given up, its caller told so, and once the sync answers, the veto it made
// stays: no further veto is tried and none made is removed once the step was
// given up, so the record reads after its caller's answer as it read then,
// INCOMPLETE, and no reader reads the snapshot verified. Red on the round
// before, whose setAside went on to the veto beside the bundle and, refused
// there, removed the veto it had made, the VERIFIED readable again after its
// caller was told the recheck was given up.
func TestAVetoSetAsideIsKeptOnceItsStepIsGivenUp(t *testing.T) {
	requireDirectorySync(t)
	src := storeWithTracks(t, 5)
	dir := verifiedSnapshot(t, src)
	root := filepath.Dir(dir)
	inBundle, beside := filepath.Join(dir, backup.VetoFileName), backup.RootVetoPath(dir)
	if vetoed(t, dir) {
		t.Fatal("a veto stands before the recheck: the test did not reach the case")
	}
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	var triedBeside atomic.Bool
	t.Cleanup(backup.SetVerdictWriteFailuresForTest(nil, nil, func(path string) error {
		if path == beside {
			triedBeside.Store(true)
			return errors.New("injected: no veto can be made beside the bundle")
		}
		return nil
	}))
	p := newParking()
	t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
		if path == inBundle {
			p.park()
			return errors.New("injected: the veto's sync failed")
		}
		return fsutil.SyncParentDir(path)
	}))
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
		_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
		done <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the veto's sync never began")
	}
	if !pathExists(t, inBundle) {
		t.Fatal("no veto was made in the bundle before its sync: the test did not reach the case")
	}
	err := requireReturnedWithin(t, done, started, 300*time.Millisecond+backup.StopWriteBound+3*time.Second, "the recheck whose veto's sync stalls")
	if !errors.Is(err, backup.ErrCheckGivenUp) || !strings.Contains(err.Error(), "putting its veto in place") {
		t.Fatalf("the recheck answered %v, want it given up putting its veto in place", err)
	}
	p.release()
	awaitClean(t)
	if !pathExists(t, inBundle) {
		t.Error("the veto the step made was removed once its sync answered, after the step was given up")
	}
	if triedBeside.Load() {
		t.Error("a veto beside the bundle was tried after the step was given up")
	}
	if got := readersVerified(t, dir); len(got) > 0 {
		t.Errorf("after the recheck given up, %s", strings.Join(got, "; "))
	}
}

// TestARecordNotReadBackIsSaidApartFromItsWrite is backlog B653's sixth
// round's red for the read-back (Codex's two P2s on round 5, the Claude API's
// Low, and Codex's review of the round's brief): a verification whose read of
// its record after its write reads nothing says so (ErrRecordNotReadBack,
// VerifyResult.ReadErr, why), apart from what its write did (Write, which
// keeps its own): a durable write whose read-back failed is recorded, which a
// reader reads, and its error says nothing of the write; a write that failed
// keeps its own error beside the read-back's; and a read-back given up says
// so, beside the give-up. Each error keeps its identity. Red on the round
// before, whose read-back read nothing and said nothing: a durable write
// answered no error, and a caller took what the check found for the record.
func TestARecordNotReadBackIsSaidApartFromItsWrite(t *testing.T) {
	for _, tc := range []string{"a durable write", "a write that failed", "a read-back given up"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(dir)
			readFailed := errors.New("injected: the read-back failed")
			stageFailed := errors.New("injected: the staging failed")
			switch tc {
			case "a read-back given up":
				t.Cleanup(backup.SetStepBoundForTest(dir, 300*time.Millisecond, "its read of the record after it"))
				p := newParking()
				t.Cleanup(backup.SetReadBackFailureForTest(func(string) error {
					p.park()
					return nil
				}))
				t.Cleanup(func() {
					p.release()
					awaitClean(t)
				})
			default:
				t.Cleanup(backup.SetReadBackFailureForTest(func(string) error { return readFailed }))
			}
			if tc == "a write that failed" {
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(string) error { return stageFailed }, nil, nil))
			}
			if tc != "a read-back given up" {
				// Registered after every seam, so run before they go back: the
				// verification answers before its worker lets go (backlog B871's
				// round 3). A read-back given up is let go and waited for by its
				// own cleanup, which a wait here would run before.
				t.Cleanup(func() { awaitClean(t) })
			}
			res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
			if res.RecordRead || !errors.Is(err, backup.ErrRecordNotReadBack) || res.ReadErr == nil {
				t.Fatalf("the verification answered %v, read back %v (%v); want its read-back said to have read nothing, and why", err, res.RecordRead, res.ReadErr)
			}
			switch tc {
			case "a durable write":
				if res.Write != backup.WriteRecorded || !res.Recorded {
					t.Errorf("its write %v, recorded %v; want it recorded", res.Write, res.Recorded)
				}
				if !errors.Is(err, readFailed) || !errors.Is(res.ReadErr, readFailed) {
					t.Errorf("the error %v, ReadErr %v; want the read-back's failure in each", err, res.ReadErr)
				}
				if errors.Is(err, backup.ErrVerdictNotDurable) || errors.Is(err, backup.ErrCheckGivenUp) {
					t.Errorf("the error %v says something of a durable write", err)
				}
				requireVerdict(t, "a reader, once the write landed", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
			case "a write that failed":
				if res.Write != backup.WriteFailed || res.Recorded {
					t.Errorf("its write %v, recorded %v; want it failed", res.Write, res.Recorded)
				}
				if !errors.Is(err, stageFailed) || !errors.Is(err, readFailed) {
					t.Errorf("the error %v lost an identity: want the write's and the read-back's", err)
				}
			case "a read-back given up":
				if res.Write != backup.WriteRecorded {
					t.Errorf("its write %v, want it recorded", res.Write)
				}
				if !errors.Is(res.ReadErr, backup.ErrReadBackGivenUp) || !errors.Is(err, backup.ErrReadBackGivenUp) || !errors.Is(err, backup.ErrCheckGivenUp) {
					t.Errorf("the error %v, ReadErr %v; want the read-back said given up beside the give-up", err, res.ReadErr)
				}
			}
		})
	}
}
