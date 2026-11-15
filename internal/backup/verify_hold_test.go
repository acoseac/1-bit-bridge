package backup_test

import (
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

// parking parks every caller of park until release, entered closing at the
// first.
type parking struct {
	entered  chan struct{}
	out      chan struct{}
	entering sync.Once
	leaving  sync.Once
}

func newParking() *parking {
	return &parking{entered: make(chan struct{}), out: make(chan struct{})}
}

func (p *parking) park() {
	p.entering.Do(func() { close(p.entered) })
	<-p.out
}

func (p *parking) release() { p.leaving.Do(func() { close(p.out) }) }

// awaitClean waits until no verification holds this process's slot and no
// hold given up is marked, failing the test past a deadline: a hold a test
// gave up has ended, all it held let go, before the next test begins.
func awaitClean(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for backup.ActiveHoldsForTest() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a hold given up was never let go")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// metadataStall stands a mount whose metadata stops answering in for what a
// verification, a prune and a reader ask of a path (the resolution of its
// links, the prune's Lstat, the listing, the lock's probe). Its seams are
// installed before anything that calls them starts, passing every call to
// the platform's own; once armed, every call parks until release, and
// answers an error then; once released, every call is the platform's again.
// The seams are put back at the test's end, once every hold has ended
// (awaitClean, which a cleanup registered after the stall runs first).
type metadataStall struct {
	armed   atomic.Bool
	p       *parking
	restore func()
}

// stallMetadata installs a stall, not armed.
func stallMetadata(t *testing.T) *metadataStall {
	t.Helper()
	s := &metadataStall{p: newParking()}
	stalled := errors.New("stalled")
	restoreMeta := backup.SetMetadataForTest(func(p string) (string, error) {
		if s.stalls() {
			return "", stalled
		}
		return fsutil.ResolveLinks(p)
	}, func(p string) (os.FileInfo, error) {
		if s.stalls() {
			return nil, stalled
		}
		return os.Lstat(p)
	})
	restoreDir := backup.SetReadDirForTest(func(p string) ([]os.DirEntry, error) {
		if s.stalls() {
			return nil, stalled
		}
		return os.ReadDir(p)
	})
	restoreProbe := backup.SetOpenProbeForTest(func(p string) (*os.File, error) {
		if s.stalls() {
			return nil, stalled
		}
		return os.OpenFile(p, os.O_RDWR, 0)
	})
	s.restore = func() { restoreProbe(); restoreDir(); restoreMeta() }
	t.Cleanup(func() { s.restore() })
	return s
}

// stalls parks a call while the stall is armed, until release, and reports
// whether it parked.
func (s *metadataStall) stalls() bool {
	if !s.armed.Load() {
		return false
	}
	s.p.park()
	return true
}

// arm makes every call from now on park, until release.
func (s *metadataStall) arm() { s.armed.Store(true) }

// release lets every parked call answer, and passes every later one on.
func (s *metadataStall) release() {
	s.armed.Store(false)
	s.p.release()
}

// requireDirectorySync skips a test of a recheck's veto where no directory
// can be synced (Windows): a recheck of a VERIFIED records nothing there
// (TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing).
func requireDirectorySync(t *testing.T) {
	t.Helper()
	if !fsutil.SyncsParentDir {
		t.Skip("no directory can be synced here, so a recheck of a VERIFIED records nothing")
	}
}

// answersWithin runs ask, failing the test where it has not answered within
// bound.
func answersWithin(t *testing.T, bound time.Duration, what string, release, ask func()) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { release(); waitCaller(t, done); awaitClean(t) })
	go func() {
		defer close(done)
		ask()
	}()
	select {
	case <-done:
	case <-time.After(bound):
		t.Errorf("%s did not answer within %v: it asked the stalled filesystem", what, bound)
	}
}

// requireRefusedNaming asks a second verification, a prune and the doctor's
// reading of the backups folder at root, its metadata stalled by the caller,
// while a hold of the snapshot named holds it given up at the step named:
// each answers at once, refusing for that hold and naming its snapshot.
func requireRefusedNaming(t *testing.T, root, second, name, step string, release func()) {
	t.Helper()
	answersWithin(t, 2*time.Second, "a second verification", release, func() {
		_, err := backup.VerifySnapshot(context.Background(), second, backup.VerifyOptions{BackupsRoot: root})
		if !errors.Is(err, backup.ErrCheckStillRunning) || err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a second verification answered %v, want ErrCheckStillRunning naming %s", err, name)
		}
	})
	answersWithin(t, 2*time.Second, "a prune", release, func() {
		pr, err := backup.Prune(root, 1)
		if err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
			t.Errorf("a prune answered %+v, %v; want it skipped for the hold given up", pr, err)
		}
	})
	answersWithin(t, 2*time.Second, "the doctor's reading", release, func() {
		s, held := backup.StuckCheckOf(root)
		if !held || s.Snapshot != name || s.Step != step {
			t.Errorf("StuckCheckOf = %+v, %v; want %s given up at %q", s, held, name, step)
		}
	})
}

// TestEveryStepUnderTheLockIsBoundedAndMarked is backlog B653's fourth
// round's red for the held lifetime past the check (the Claude API's Medium
// and Codex's P2 on round 3, both reviews of the round's brief): a step that
// hangs under the backups folder's lock, putting the veto in place (its
// directory's sync), the verdict's write, the prune after it, and the write
// of INCOMPLETE for a verification its caller stopped before it began
// (abandonPending), is given up within its bound, counted from the lock, and
// the folder marked at once, with no filesystem asked: the metadata path
// stalls too as soon as the step hangs, before the give-up, and the
// verification still answers in its bound, a second verification is refused
// at once naming the snapshot, a prune is skipped for it, and the doctor's
// reading names it and its step. Once the hang ends, all it held is let go,
// no prune ran, and where the veto's sync hung, no check ran either. Red on
// the round before, where only the check was ever given up or marked.
func TestEveryStepUnderTheLockIsBoundedAndMarked(t *testing.T) {
	for _, tc := range []struct {
		name string
		step string
	}{
		{name: "the veto's sync", step: "putting its veto in place"},
		{name: "the verdict's write", step: "its verdict's write"},
		{name: "the prune", step: "the prune after it"},
		{name: "abandonPending's write", step: "its write of INCOMPLETE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "the veto's sync" {
				requireDirectorySync(t)
			}
			src := storeWithTracks(t, 5)
			oldest := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			older := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			root := filepath.Dir(older)
			p := newParking()
			var target string
			var c *backup.Capture
			if tc.name == "the veto's sync" {
				target = verifiedSnapshot(t, src)
				t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
					p.park()
					return fsutil.SyncParentDir(path)
				}))
			} else {
				c = captureT(t, src)
				target = c.Dir
				switch tc.name {
				case "the verdict's write", "abandonPending's write":
					t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(dir string) error {
						if dir == target {
							p.park()
						}
						return nil
					}, nil, nil))
				case "the prune":
					t.Cleanup(backup.SetOnPruneForTest(p.park))
				}
			}
			t.Cleanup(backup.SetStepBoundForTest(target, 300*time.Millisecond, tc.step, "its read of the record after it"))
			checks := checksRun(t)
			before := checks.Load()
			stall := stallMetadata(t)
			// Registered last, so run first: the stall and the hang let go and
			// the hold's end joined before any seam is put back.
			callerDone := make(chan struct{})
			t.Cleanup(func() {
				stall.release()
				p.release()
				waitCaller(t, callerDone)
				awaitClean(t)
			})
			name := filepath.Base(target)
			done := make(chan backup.Result, 1)
			started := time.Now()
			if tc.name == "abandonPending's write" {
				stopped, cancel := context.WithCancel(context.Background())
				cancel()
				go func() {
					defer close(callerDone)
					res, err := backup.VerifySnapshot(stopped, target, backup.VerifyOptions{BackupsRoot: root})
					done <- backup.Result{Dir: target, Verify: res, VerifyErr: err}
				}()
			} else {
				go func() {
					defer close(callerDone)
					if c != nil {
						done <- backup.FinishCapture(context.Background(), c, keepOne, backup.VerifyOptions{})
					} else {
						done <- backup.Finish(context.Background(), target, keepOne, backup.VerifyOptions{BackupsRoot: root})
					}
				}()
			}
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the step never reached its parking hook")
			}
			// The mount stops answering its metadata too, before the give-up.
			stall.arm()
			res := requireReturnedWithin(t, done, started, 5*time.Second+backup.StopWriteBound+3*time.Second, "the verification whose step hangs")
			if tc.name == "abandonPending's write" {
				if !errors.Is(res.VerifyErr, context.Canceled) {
					t.Errorf("the stopped verification answered %v, want its stop", res.VerifyErr)
				}
			} else if !errors.Is(res.VerifyErr, backup.ErrCheckGivenUp) || !strings.Contains(res.VerifyErr.Error(), tc.step) {
				t.Errorf("the verification answered %v, want it given up at %q", res.VerifyErr, tc.step)
			}
			// The mark, which abandonPending raises past its own bound.
			for wait := time.Now().Add(2 * backup.StopWriteBound); ; time.Sleep(10 * time.Millisecond) {
				if _, held := backup.StuckCheckHere(root); held {
					break
				}
				if time.Now().After(wait) {
					t.Fatal("the hold that hangs was never marked")
				}
			}
			requireRefusedNaming(t, root, older, name, tc.step, func() { stall.release(); p.release() })
			stall.release()
			p.release()
			awaitClean(t)
			if !pathExists(t, oldest) {
				t.Error("a prune ran beside the hold given up, or after it")
			}
			if tc.name == "the veto's sync" {
				if n := checks.Load() - before; n != 0 {
					t.Errorf("%d checks ran once the veto's sync given up was released, want none", n)
				}
				if got := readersVerified(t, target); len(got) > 0 {
					t.Errorf("a reader reads the snapshot whose recheck was given up verified: %s", strings.Join(got, "; "))
				}
			}
		})
	}
}

// TestASlowStepWithinItsBoundLeavesNoMark: a step under the lock that is
// slow and keeps within its bound (the veto's sync parked until a second
// verification is waiting, then for at least 1.2 s of its explicit 3 s bound)
// raises no mark, and a verification waiting its turn
// meanwhile gets it and records its verdict (backlog B653's fourth round, the
// review of its brief: a mark only for an overrun).
func TestASlowStepWithinItsBoundLeavesNoMark(t *testing.T) {
	requireDirectorySync(t)
	src := storeWithTracks(t, 5)
	target := verifiedSnapshot(t, src)
	root := filepath.Dir(target)
	other, err := backup.Snapshot(t.Context(), storeWithTracks(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetStepBoundForTest(target, 3*time.Second, "putting its veto in place"))
	p := newParking()
	var once sync.Once
	t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
		once.Do(p.park)
		return fsutil.SyncParentDir(path)
	}))
	waiting := make(chan struct{})
	var waited sync.Once
	t.Cleanup(backup.SetOnSlotWaitForTest(func() { waited.Do(func() { close(waiting) }) }))
	callers := joinBackupCallers(t, p.release)
	first := make(chan error, 1)
	callers.start(func() {
		_, err := backup.VerifySnapshot(context.Background(), target, backup.VerifyOptions{BackupsRoot: root})
		first <- err
	})
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow sync never began")
	}
	second := make(chan verifyAnswer, 1)
	callers.start(func() {
		res, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: filepath.Dir(other)})
		second <- verifyAnswer{res, err}
	})
	select {
	case <-waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the second verification never waited")
	}
	for until := time.Now().Add(1200 * time.Millisecond); ; time.Sleep(10 * time.Millisecond) {
		if s, held := backup.StuckCheckHere(root); held {
			t.Fatalf("a step within its bound was marked: %+v", s)
		}
		if !time.Now().Before(until) {
			break
		}
	}
	p.release()
	if err := receiveWithin(t, first, time.Minute, "the slow verification, its sync let go"); err != nil {
		t.Errorf("the slow verification answered %v", err)
	}
	if a := receiveWithin(t, second, time.Minute, "the verification that waited its turn"); a.err != nil || !a.res.Recorded {
		t.Errorf("the verification that waited its turn answered %v, recorded %v; want its turn and its verdict", a.err, a.res.Recorded)
	}
}

// receiveWithin is what ch gives, failing the test past bound: a receive with
// no bound would wait as long as the verification's own watchdog, its lock
// wait and its deadline, an hour (backlog B872's second round).
func receiveWithin[T any](t *testing.T, ch <-chan T, bound time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(bound):
		t.Fatalf("%s did not answer within %v", what, bound)
	}
	var zero T
	return zero
}

// TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries is
// backlog B653's fourth round's red for a veto an earlier write left (Codex's
// P1 on round 3; the Claude API's High on the round's brief): beside a
// VERIFIED whose write once failed, a verification that does not find both
// verdicts itself, a retry its caller stopped before it began
// (abandonPending), one that hits its deadline, one given up, one that
// panics, and a quick VERIFIED over a full one, leaves the veto standing over
// the VERIFIED the record carries over, so no reader reads it verified and
// the prune keeps the older verified snapshot. Red on the round before,
// which removed every veto after any write. The retry given up parks before
// it reads the snapshot and is awaited with its answer in either order,
// bounded, and required given up (backlog B872's second round: parked at
// SQLite's open behind its deadline, it could end as a retry past its
// deadline and pass all the same).
//
// The retry past its deadline takes B886's arrangement (backlog B901): its
// check alone is parked on its thread before it reads anything (parkCheckOf)
// until its 150 ms deadline has passed and its grace has begun; then it is
// let go, answers INCOMPLETE (deadline), and only once the verification has
// answered is its grace ended (checkGraceOf), every other step the deadline
// bounds held at its gate (SetStepGateForTest); and its answer is read,
// required the deadline's and never a check given up. It slept 400 ms at
// SQLite's open, so its check answered within the half of StopWriteBound
// runCheck gives it past the deadline only on a host that ran its thread in
// time, and its error was dropped, so a retry given up passed as one past its
// deadline.
func TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries(t *testing.T) {
	for _, tc := range []string{"a retry stopped before it began", "a retry past its deadline", "a retry given up",
		"a retry that panics", "a quick VERIFIED over a full one"} {
		t.Run(tc, func(t *testing.T) {
			requireDirectorySync(t)
			src := storeWithTracks(t, 20)
			anchor := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedSnapshot(t, src)
			if tc == "a quick VERIFIED over a full one" {
				verifyT(t, dir, backup.CheckFull)
				if err := os.WriteFile(filepath.Join(dir, backup.VetoFileName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				// An earlier write failed: its veto stays beside the VERIFIED.
				restore := backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected") }, nil, nil)
				_, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
				awaitClean(t)
				restore()
				if err == nil {
					t.Fatal("the earlier write did not fail")
				}
			}
			if !pathExists(t, filepath.Join(dir, backup.VetoFileName)) {
				t.Fatal("no veto stands beside the VERIFIED: the test did not reach the case")
			}
			ctx := context.Background()
			// The check a given-up retry leaves parked, let go once it has
			// answered, so its hold can end before the record is read.
			release := func() {}
			var parked <-chan struct{}
			const givenUpDeadline = 300 * time.Millisecond
			// The retry past its deadline's park and grace, and what the
			// test's end lets go before it joins the retry.
			var pastParked, graceBegun <-chan struct{}
			letCheckGo, endGrace := func() {}, func() {}
			var letGo []func()
			switch tc {
			case "a retry stopped before it began":
				stopped, cancel := context.WithCancel(ctx)
				cancel()
				ctx = stopped
			case "a retry past its deadline":
				entered, letCheck := parkCheckOf(t, dir)
				t.Cleanup(backup.SetVerifyDeadlineForTest(150 * time.Millisecond))
				begun, pass := checkGraceOf(t, dir)
				gate, restoreGate := backup.SetStepGateForTest(filepath.Dir(dir), filepath.Base(dir), "its read of the snapshot's record",
					"putting its veto in place", "its verdict's write", "its write of INCOMPLETE", "its read of the record after it")
				t.Cleanup(restoreGate)
				pastParked, graceBegun, letCheckGo, endGrace = entered, begun, letCheck, pass
				letGo = append(letGo, letCheck, pass, gate.FireAll)
			case "a retry given up":
				t.Cleanup(backup.SetVerifyDeadlineForTest(givenUpDeadline))
				p := newParking()
				// Before it reads the snapshot, where every check comes
				// whatever its deadline.
				t.Cleanup(backup.SetOnLowPriorityForTest(p.park))
				t.Cleanup(func() {
					p.release()
					awaitClean(t)
				})
				release, parked = p.release, p.entered
			case "a retry that panics":
				var panicked atomic.Bool
				t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
					if panicked.CompareAndSwap(false, true) {
						panic("injected: a defect of the bridge's own")
					}
				}))
			}
			level := backup.CheckQuick
			var res backup.VerifyResult
			switch {
			case parked != nil:
				callers := joinBackupCallers(t, release)
				done := make(chan verifyAnswer, 1)
				start := time.Now()
				callers.start(func() {
					r, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{Level: level})
					done <- verifyAnswer{r, err}
				})
				a := awaitParkedAndGivenUp(t, parked, done, verifyAnswer.read, start, givenUpDeadline+backup.StopWriteBound/2+2*time.Second)
				if !errors.Is(a.err, backup.ErrCheckGivenUp) {
					t.Fatalf("the retry answered %v, want its check given up: the test did not reach the case", a.err)
				}
				res = a.res
			case pastParked != nil:
				callers := joinBackupCallers(t, func() {
					for _, f := range letGo {
						f()
					}
				})
				done := make(chan verifyAnswer, 1)
				callers.start(func() {
					r, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{Level: level})
					done <- verifyAnswer{r, err}
				})
				a, ok := awaitParkedPastItsDeadline(t, pastParked, graceBegun, done)
				if !ok {
					// The check let go, its answer awaited, and only then its
					// grace ended: a grace ended while the check is still parked
					// gives it up (runCheck).
					letCheckGo()
					a = awaitVerifyAnswer(t, done, "the retry")
				}
				endGrace()
				if errors.Is(a.err, backup.ErrCheckGivenUp) || !errors.Is(a.err, context.DeadlineExceeded) {
					t.Fatalf("the retry answered %v, want its check's answer past its deadline, never its check given up: the test did not reach the case", a.err)
				}
				requireVerdict(t, "what the retry past its deadline found", a.res.Found, backup.StateIncomplete, backup.ReasonDeadline,
					backup.StateIncomplete, backup.ReasonDeadline)
				res = a.res
			default:
				res, _ = backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{Level: level})
			}
			if tc == "a quick VERIFIED over a full one" && res.Found.DatabaseState != backup.StateVerified {
				t.Fatalf("the quick check found %s, want VERIFIED: the test did not reach the case", res.Found.DatabaseState)
			}
			if tc != "a quick VERIFIED over a full one" && tc != "a retry stopped before it began" && res.Found.DatabaseState != backup.StateIncomplete {
				t.Fatalf("the retry found %s, want INCOMPLETE: the test did not reach the case", res.Found.DatabaseState)
			}
			release()
			awaitClean(t)
			if !pathExists(t, filepath.Join(dir, backup.VetoFileName)) {
				t.Error("the veto an earlier write left was removed over the VERIFIED the record carries")
			}
			if got := readersVerified(t, dir); len(got) > 0 {
				t.Errorf("after the retry, %s", strings.Join(got, "; "))
			}
			if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
				t.Fatal(err)
			}
			if !pathExists(t, anchor) {
				t.Error("the prune let the older verified snapshot go for one whose VERIFIED is in doubt")
			}
		})
	}
}

// TestARecheckGivenUpLeavesTheVetoItMade: a recheck of a verified snapshot
// whose check is given up (backlog B653's fourth round: once a step is given
// up, nothing is written but its INCOMPLETE) leaves the veto it put in place
// before its check, though none stood before it began and the record, whose
// VERIFIED the merge keeps, is as it was: no check that finished has cleared
// it, so no reader reads the snapshot verified and the prune keeps the older
// verified one, until a check of it finishes, which lets the veto go. The
// check parks before it reads the snapshot (SetOnLowPriorityForTest), so its
// deadline gives it up whenever it passes (backlog B872: parked at SQLite's
// open, after the veto's sync and the paced hash, a deadline that passed
// first answered INCOMPLETE, no check given up), and the park and the answer
// are awaited in either order, bounded (awaitParkedAndGivenUp, B872's second
// round: the call was made on the test's goroutine, waited for no park, and
// had no bound of the test's).
func TestARecheckGivenUpLeavesTheVetoItMade(t *testing.T) {
	requireDirectorySync(t)
	src := storeWithTracks(t, 20)
	anchor := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	dir := verifiedSnapshot(t, src)
	vetoes := func() bool {
		return pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir))
	}
	if vetoes() {
		t.Fatal("a veto stands before the recheck: the test did not reach the case")
	}
	const deadline = 300 * time.Millisecond
	restoreDeadline := backup.SetVerifyDeadlineForTest(deadline)
	t.Cleanup(restoreDeadline)
	p := newParking()
	t.Cleanup(backup.SetOnLowPriorityForTest(p.park))
	t.Cleanup(func() {
		p.release()
		awaitClean(t)
	})
	callers := joinBackupCallers(t, p.release)
	done := make(chan verifyAnswer, 1)
	start := time.Now()
	callers.start(func() {
		res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
		done <- verifyAnswer{res, err}
	})
	a := awaitParkedAndGivenUp(t, p.entered, done, verifyAnswer.read, start, deadline+backup.StopWriteBound/2+2*time.Second)
	res, err := a.res, a.err
	if !errors.Is(err, backup.ErrCheckGivenUp) {
		t.Fatalf("the recheck answered %v; want it given up", err)
	}
	if res.Recorded {
		t.Error("the recheck given up recorded what it was given up with, over the VERIFIED its record keeps")
	}
	p.release()
	awaitClean(t)
	if !vetoes() {
		t.Error("the veto the recheck given up made was removed")
	}
	if got := readersVerified(t, dir); len(got) > 0 {
		t.Errorf("after the recheck given up, %s", strings.Join(got, "; "))
	}
	if _, err := backup.Prune(filepath.Dir(dir), 1); err != nil {
		t.Fatal(err)
	}
	if !pathExists(t, anchor) {
		t.Error("the prune let the older verified snapshot go for one whose recheck did not finish")
	}
	// A check that finishes lets the veto go.
	restoreDeadline()
	requireVerdict(t, "the check that finished", verifyT(t, dir, backup.CheckQuick).Record, backup.StateVerified, "", backup.StateVerified, "")
	if vetoes() {
		t.Error("the check that finished left the veto")
	}
}

// stopWithItsWriteParked runs a verification of a capture stopped before it
// took the lock, whose write of INCOMPLETE (abandonPending) waits for the lock
// another holder keeps 1.5 s past the stop, so the write takes it late and its
// own bound ends late, then hangs inside its staging until the test's end; the
// worker's open of the lock file answers half a second late, so its caller
// gives it up while it waits for that write. It answers the backups folder,
// the parking the write hangs in, and the verification's answer; the test's
// end lets everything go and waits for every hold to end.
func stopWithItsWriteParked(t *testing.T) (root string, p *parking, err error) {
	t.Helper()
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	root = filepath.Dir(c.Dir)
	// Another holder of the folder's lock, which lets it go 1.5 s after the
	// stop, so the write takes the lock late and its own bound ends late.
	other, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(other); err != nil {
		t.Fatal(err)
	}
	var letGo sync.Once
	release := func() {
		letGo.Do(func() {
			_ = fsutil.Unlock(other)
			_ = other.Close()
		})
	}
	t.Cleanup(release)
	// The worker's open of the lock file answers half a second late, so its
	// caller gives it up while it waits for the write, before it ends.
	var opens atomic.Int32
	t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
		if opens.Add(1) == 1 {
			time.Sleep(500 * time.Millisecond)
		}
		return fsutil.OpenLockFile(dir, name)
	}))
	p = newParking()
	t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(dir string) error {
		if dir == c.Dir {
			p.park()
		}
		return nil
	}, nil, nil))
	t.Cleanup(func() {
		release()
		p.release()
		awaitClean(t)
	})
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	time.AfterFunc(1500*time.Millisecond, release)
	_, err = backup.VerifySnapshot(stopped, c.Dir, backup.VerifyOptions{BackupsRoot: root})
	return root, p, err
}

// TestAStoppedVerificationMarksNothingUntilTheWriteItStartedOverruns: a
// verification stopped before it took the lock starts the write of its
// INCOMPLETE (abandonPending), which waits for the lock and then hangs
// (stopWithItsWriteParked). Its caller gives the worker up while it waits for
// that write, and nothing is marked, then or once the worker has ended and
// let go of this process's slot, while the write's own hold is within its
// bound: a wait that overran nothing marks nothing (backlog B653's fifth
// round, the Claude API's Low on round 4; the fourth round marked the folder
// there, naming the write). Once the write's own step overruns its bound, its
// hold marks the folder, naming the write. Red on the round before, whose
// worker's give-up marked the folder at once.
func TestAStoppedVerificationMarksNothingUntilTheWriteItStartedOverruns(t *testing.T) {
	root, p, err := stopWithItsWriteParked(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped verification answered %v, want its stop", err)
	}
	if s, held := backup.StuckCheckHere(root); held {
		t.Errorf("once its caller gave the worker up, the folder reads %+v: a wait that overran nothing marked it", s)
	}
	select {
	case <-p.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the write of INCOMPLETE never began")
	}
	for deadline := time.Now().Add(30 * time.Second); backup.SlotHeldForTest(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the worker never let go of the slot")
		}
	}
	if s, held := backup.StuckCheckHere(root); held {
		t.Errorf("once the worker had ended, within the write's own bound, the folder reads %+v; want it unmarked", s)
	}
	if s := awaitMarkedWithin(t, root, 2*backup.StopWriteBound+3*time.Second); s.Step != "its write of INCOMPLETE" {
		t.Errorf("once the write overran its bound, the folder is marked at %q, want the write of INCOMPLETE", s.Step)
	}
}

// TestAStoppedVerificationsMarkStandsUntilTheWriteItStartedEnds: the write of
// INCOMPLETE a stopped verification started (abandonPending) that overruns
// its own bound marks the folder, naming that write, and the mark stands
// while the write hangs, though the verification's worker has ended and let
// go of this process's slot long before, and goes once the write ends
// (backlog B653's fourth round, whose worker kept a mark of its own until
// then; since the fifth round the write's own hold keeps it, and the worker
// marks nothing: TestAStoppedVerificationMarksNothingUntilTheWriteItStartedOverruns).
func TestAStoppedVerificationsMarkStandsUntilTheWriteItStartedEnds(t *testing.T) {
	root, p, err := stopWithItsWriteParked(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped verification answered %v, want its stop", err)
	}
	if s := awaitMarkedWithin(t, root, 2*backup.StopWriteBound+3*time.Second); s.Step != "its write of INCOMPLETE" {
		t.Fatalf("once the write overran its bound, the folder is marked at %q, want the write of INCOMPLETE", s.Step)
	}
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, held := backup.StuckCheckHere(root); !held {
			t.Fatal("the mark went while the write it names still hung")
		}
	}
	if backup.SlotHeldForTest() {
		t.Error("the stopped verification's worker still holds this process's slot")
	}
	p.release()
	awaitClean(t)
}

// TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing keeps the
// manifest unchanged but disqualifies a completed finding that disproves it.
func TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing(t *testing.T) {
	src := storeWithTracks(t, 20)
	dir := verifiedSnapshot(t, src)
	root := filepath.Dir(dir)
	flipLastByte(t, dir, backup.ManifestDBFileName)
	mpath := filepath.Join(dir, backup.ManifestFile)
	before := readBytes(t, mpath)
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(root, past, past); err != nil {
		t.Fatal(err)
	}
	restore := backup.SetSyncsParentDirForTest(false)
	t.Cleanup(restore)
	t.Cleanup(func() { awaitClean(t) })
	res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	if !errors.Is(err, backup.ErrRecheckNotRecorded) {
		t.Fatalf("a recheck with no directory barrier answered %v, want ErrRecheckNotRecorded", err)
	}
	if res.Found.DatabaseState != backup.StateFailed || res.Recorded {
		t.Errorf("the recheck found %s, recorded %v; want the damage found and nothing recorded", res.Found.DatabaseState, res.Recorded)
	}
	if got := readBytes(t, mpath); string(got) != string(before) {
		t.Error("the recheck wrote the record")
	}
	if !pathExists(t, filepath.Join(dir, backup.VetoFileName)) && !pathExists(t, backup.RootVetoPath(dir)) {
		t.Error("the disproved record has no veto")
	}
	requireVerdict(t, "the record the recheck left", listed(t, dir), backup.StateIncomplete, backup.ReasonUnsynced, backup.StateIncomplete, backup.ReasonUnsynced)
	fresh, err := backup.Snapshot(t.Context(), storeWithTracks(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	freshRes, err := backup.VerifySnapshot(t.Context(), fresh, backup.VerifyOptions{})
	if err != nil || !freshRes.Recorded || freshRes.Record.DatabaseState != backup.StateVerified {
		t.Errorf("a fresh snapshot's check with no directory barrier: %v, recorded %v, %s; want its VERIFIED recorded",
			err, freshRes.Recorded, freshRes.Record.DatabaseState)
	}
}

// TestAGivenUpVerificationReadsItsRecordBack is backlog B653's fourth round's
// red for what a verification given up says it recorded (the Claude API's
// Low on round 3, both reviews of the round's brief): Recorded says only
// that what it wrote landed, and Record is the record read back. Given up
// over a PENDING, its INCOMPLETE is recorded and read back; over a FAILED, the
// merge keeps the FAILED, nothing is written and Recorded is false; and where
// its INCOMPLETE's write fails, Recorded is false and the record reads as it
// was. Red on the round before, which set Recorded for a write that changed
// nothing. The park and the answer are awaited in either order, bounded
// (awaitParkedAndGivenUp; backlog B872's second round: the call was made on
// the test's goroutine, waited for no park, and had no bound of the test's).
func TestAGivenUpVerificationReadsItsRecordBack(t *testing.T) {
	for _, tc := range []string{"over a PENDING", "over a FAILED", "whose INCOMPLETE is not written"} {
		t.Run(tc, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			var dir string
			var c *backup.Capture
			if tc == "over a FAILED" {
				d, err := backup.Snapshot(t.Context(), src)
				if err != nil {
					t.Fatal(err)
				}
				dir = d
				flipLastByte(t, dir, backup.ManifestDBFileName)
				flipLastByte(t, dir, "tokens.json")
				verifyT(t, dir, backup.CheckQuick)
				requireVerdict(t, "the record before", listed(t, dir), backup.StateFailed, backup.ReasonHash, backup.StateFailed, backup.ReasonHash)
			} else {
				c = captureT(t, src)
				dir = c.Dir
			}
			if tc == "whose INCOMPLETE is not written" {
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected: the staging failed") }, nil, nil))
			}
			const deadline = 300 * time.Millisecond
			t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
			p := newParking()
			t.Cleanup(backup.SetOnLowPriorityForTest(p.park))
			t.Cleanup(func() {
				p.release()
				awaitClean(t)
			})
			callers := joinBackupCallers(t, p.release)
			done := make(chan verifyAnswer, 1)
			start := time.Now()
			callers.start(func() {
				if c != nil {
					r := backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
					done <- verifyAnswer{r.Verify, r.VerifyErr}
					return
				}
				res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
				done <- verifyAnswer{res, err}
			})
			a := awaitParkedAndGivenUp(t, p.entered, done, verifyAnswer.read, start, deadline+backup.StopWriteBound/2+2*time.Second)
			res, err := a.res, a.err
			if !errors.Is(err, backup.ErrCheckGivenUp) {
				t.Fatalf("the verification answered %v, want it given up", err)
			}
			switch tc {
			case "over a PENDING":
				if !res.Recorded || res.Record.DatabaseState != backup.StateIncomplete {
					t.Errorf("given up over a PENDING: recorded %v, the record reads %s; want its INCOMPLETE recorded", res.Recorded, res.Record.DatabaseState)
				}
				if strings.Contains(err.Error(), "cache stamp was skipped") {
					t.Errorf("the live recovery step skipped its cache stamp: %v", err)
				}
			case "over a FAILED":
				if res.Recorded || res.Record.DatabaseState != backup.StateFailed {
					t.Errorf("given up over a FAILED: recorded %v, the record reads %s; want nothing recorded and FAILED read", res.Recorded, res.Record.DatabaseState)
				}
			case "whose INCOMPLETE is not written":
				if res.Recorded || res.Record.DatabaseState != backup.StatePending || !strings.Contains(err.Error(), "injected") {
					t.Errorf("an INCOMPLETE not written: recorded %v, the record reads %s, %v; want nothing recorded, the PENDING read and the write's failure said",
						res.Recorded, res.Record.DatabaseState, err)
				}
			}
		})
	}
}

// waitCaller joins the outer caller as well as its worker: a free slot alone
// does not synchronize with the caller's watchdog setup or result handling.
func waitCaller(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the verification caller did not end")
	}
}

// backupCallers joins all outer callers even when an assertion exits early.
// The release unblocks every seam before callers and retained work are joined.
type backupCallers struct{ wg sync.WaitGroup }

func joinBackupCallers(t *testing.T, release func()) *backupCallers {
	t.Helper()
	c := &backupCallers{}
	t.Cleanup(func() {
		release()
		c.wait(t)
		awaitClean(t)
	})
	return c
}

func (c *backupCallers) start(fn func()) {
	c.wg.Add(1)
	go func() { defer c.wg.Done(); fn() }()
}

func (c *backupCallers) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	waitCaller(t, done)
}
