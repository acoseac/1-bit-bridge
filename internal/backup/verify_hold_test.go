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
	for backup.SlotHeldForTest() || backup.StuckAnywhereForTest() {
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
func answersWithin(t *testing.T, bound time.Duration, what string, ask func()) {
	t.Helper()
	done := make(chan struct{})
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
func requireRefusedNaming(t *testing.T, root, second, name, step string) {
	t.Helper()
	answersWithin(t, 2*time.Second, "a second verification", func() {
		_, err := backup.VerifySnapshot(context.Background(), second, backup.VerifyOptions{BackupsRoot: root})
		if !errors.Is(err, backup.ErrCheckStillRunning) || err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a second verification answered %v, want ErrCheckStillRunning naming %s", err, name)
		}
	})
	answersWithin(t, 2*time.Second, "a prune", func() {
		pr, err := backup.Prune(root, 1)
		if err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
			t.Errorf("a prune answered %+v, %v; want it skipped for the hold given up", pr, err)
		}
	})
	answersWithin(t, 2*time.Second, "the doctor's reading", func() {
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
			t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
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
			checks := checksRun(t)
			before := checks.Load()
			stall := stallMetadata(t)
			// Registered last, so run first: the stall and the hang let go and
			// the hold's end joined before any seam is put back.
			t.Cleanup(func() {
				stall.release()
				p.release()
				awaitClean(t)
			})
			name := filepath.Base(target)
			done := make(chan backup.Result, 1)
			started := time.Now()
			if tc.name == "abandonPending's write" {
				stopped, cancel := context.WithCancel(context.Background())
				cancel()
				go func() {
					res, err := backup.VerifySnapshot(stopped, target, backup.VerifyOptions{BackupsRoot: root})
					done <- backup.Result{Dir: target, Verify: res, VerifyErr: err}
				}()
			} else {
				go func() {
					if c != nil {
						done <- backup.FinishCapture(context.Background(), c, keepOne, backup.VerifyOptions{})
					} else {
						done <- backup.Finish(context.Background(), target, keepOne, backup.VerifyOptions{BackupsRoot: root})
					}
				}()
			}
			<-p.entered
			// The mount stops answering its metadata too, before the give-up.
			stall.arm()
			res := requireReturnedWithin(t, done, started, 300*time.Millisecond+backup.StopWriteBound+3*time.Second, "the verification whose step hangs")
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
			requireRefusedNaming(t, root, older, name, tc.step)
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
// slow and keeps within its bound (the veto's sync taking a second of a
// four-second deadline) raises no mark, and a verification waiting its turn
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
	t.Cleanup(backup.SetVerifyDeadlineForTest(4 * time.Second))
	slow := make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
		once.Do(func() { close(slow) })
		time.Sleep(1200 * time.Millisecond)
		return fsutil.SyncParentDir(path)
	}))
	waiting := make(chan struct{})
	var waited sync.Once
	t.Cleanup(backup.SetOnSlotWaitForTest(func() { waited.Do(func() { close(waiting) }) }))
	t.Cleanup(func() { awaitClean(t) })
	first := make(chan error, 1)
	go func() {
		_, err := backup.VerifySnapshot(context.Background(), target, backup.VerifyOptions{BackupsRoot: root})
		first <- err
	}()
	<-slow
	second := make(chan backup.VerifyResult, 1)
	secondErr := make(chan error, 1)
	go func() {
		res, err := backup.VerifySnapshot(context.Background(), other, backup.VerifyOptions{BackupsRoot: filepath.Dir(other)})
		second <- res
		secondErr <- err
	}()
	<-waiting
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if s, held := backup.StuckCheckHere(root); held {
			t.Fatalf("a step within its bound was marked: %+v", s)
		}
	}
	if err := <-first; err != nil {
		t.Errorf("the slow verification answered %v", err)
	}
	if res, err := <-second, <-secondErr; err != nil || !res.Recorded {
		t.Errorf("the verification that waited its turn answered %v, recorded %v; want its turn and its verdict", err, res.Recorded)
	}
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
// which removed every veto after any write.
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
			switch tc {
			case "a retry stopped before it began":
				stopped, cancel := context.WithCancel(ctx)
				cancel()
				ctx = stopped
			case "a retry past its deadline":
				t.Cleanup(backup.SetVerifyDeadlineForTest(150 * time.Millisecond))
				t.Cleanup(backup.SetOnDatabaseOpenForTest(func() { time.Sleep(400 * time.Millisecond) }))
			case "a retry given up":
				t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
				p := newParking()
				t.Cleanup(backup.SetOnDatabaseOpenForTest(p.park))
				t.Cleanup(func() {
					p.release()
					awaitClean(t)
				})
				release = p.release
			case "a retry that panics":
				var panicked atomic.Bool
				t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
					if panicked.CompareAndSwap(false, true) {
						panic("injected: a defect of the bridge's own")
					}
				}))
			}
			level := backup.CheckQuick
			res, _ := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{Level: level})
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
// verified one, until a check of it finishes, which lets the veto go.
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
	restoreDeadline := backup.SetVerifyDeadlineForTest(300 * time.Millisecond)
	t.Cleanup(restoreDeadline)
	p := newParking()
	t.Cleanup(backup.SetOnDatabaseOpenForTest(p.park))
	t.Cleanup(func() {
		p.release()
		awaitClean(t)
	})
	res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
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

// TestAStoppedVerificationsMarkStandsUntilTheWriteItStartedEnds: a
// verification stopped before it took the lock starts the write of its
// INCOMPLETE (abandonPending), which waits for the lock (another holder has
// it for a while) and then hangs. Its caller gives the worker up while it
// waits for that write, naming that step, and once the worker has ended and
// let go of this process's slot, the folder stays marked, though the write's
// own hold is within its bound still, until the write ends (backlog B653's
// fourth round: the step test's abandonPending case, run twenty times under
// the race detector, once found no mark there, and once the worker's mark
// naming its wait for the lock).
func TestAStoppedVerificationsMarkStandsUntilTheWriteItStartedEnds(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	root := filepath.Dir(c.Dir)
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
	p := newParking()
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
	if _, err := backup.VerifySnapshot(stopped, c.Dir, backup.VerifyOptions{BackupsRoot: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped verification answered %v, want its stop", err)
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
	if s, held := backup.StuckCheckHere(root); !held || s.Step != "its write of INCOMPLETE" {
		t.Errorf("once the worker had ended, the folder reads %+v, %v; want it marked, naming the write of INCOMPLETE", s, held)
	}
}

// TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing is backlog
// B653's fourth round's red for a platform with no directory barrier (Codex's
// P1 and the Claude API's Medium on round 3: Windows, where
// fsutil.SyncParentDir does nothing and fsutil.SyncsParentDir is false): a
// recheck of a VERIFIED snapshot makes no veto, leaves no veto file and no
// stamp on the backups folder, runs its check and says what it found (its
// database FAILED here), and records nothing over the VERIFIED
// (ErrRecheckNotRecorded); and a fresh snapshot's check there records its
// verdict as before. Red on the round before, which took the no-op barrier
// for a durable veto.
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
	if pathExists(t, filepath.Join(dir, backup.VetoFileName)) || pathExists(t, backup.RootVetoPath(dir)) {
		t.Error("the recheck left a veto")
	}
	if info, err := os.Stat(root); err != nil || !info.ModTime().Equal(past) {
		t.Errorf("the backups folder's time moved: %v, %v", info.ModTime(), err)
	}
	requireVerdict(t, "the record the recheck left", listed(t, dir), backup.StateVerified, "", backup.StateVerified, "")
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
// nothing.
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
			t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
			p := newParking()
			t.Cleanup(backup.SetOnLowPriorityForTest(p.park))
			t.Cleanup(func() {
				p.release()
				awaitClean(t)
			})
			var res backup.VerifyResult
			var err error
			if c != nil {
				r := backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
				res, err = r.Verify, r.VerifyErr
			} else {
				res, err = backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{})
			}
			if !errors.Is(err, backup.ErrCheckGivenUp) {
				t.Fatalf("the verification answered %v, want it given up", err)
			}
			switch tc {
			case "over a PENDING":
				if !res.Recorded || res.Record.DatabaseState != backup.StateIncomplete {
					t.Errorf("given up over a PENDING: recorded %v, the record reads %s; want its INCOMPLETE recorded", res.Recorded, res.Record.DatabaseState)
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
