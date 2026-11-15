//go:build !windows

package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// Backlog B898 through Install and the operator's Rollback: the install
// fixture is a tar.gz holding `bridge`, so not on Windows, as every install
// test here. The swap is Windows' filesystem part on every host (swapByVacate),
// its moves a heldRig's.

// installSwapper is an install's swap through swapByVacate, counting in stops
// the SCM stop a foreground install on Windows makes first.
func installSwapper(stops *int) func(dst, newBinary, backupExt string, mark func() error) error {
	return func(dst, newBinary, backupExt string, mark func() error) error {
		if stops != nil {
			*stops++
		}
		return swapByVacate(dst, newBinary, backupExt, mark)
	}
}

// heldInstall is an install of fix's release over live in dir, its swap
// through swapByVacate.
func heldInstall(t *testing.T, fix *installFixture, dir, live string, stops *int) error {
	t.Helper()
	_, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(), InstallOptions{DataDir: dir, BinaryPath: live,
		Force: true, Verifier: noopVerifier, swap: installSwapper(stops)})
	return err
}

// countMarkerWrites counts each write of the install marker, by its data dir,
// for the rest of the test.
func countMarkerWrites(t *testing.T) func(dataDir string) int {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	t.Cleanup(SetMarkerWriteObserverForTest(func(dir string) {
		mu.Lock()
		counts[dir]++
		mu.Unlock()
	}))
	return func(dir string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[dir]
	}
}

// TestAMissingBinaryRefusesEveryInstallAndRollback: an install's swap that
// left no binary latches the disk, not the process: another Updater over the
// same data dir, as another process would make it, refuses an install of the
// same release and of a newer one, before their download, and a rollback,
// writing nothing, with the marker as the swap left it and with one past its
// window; the Updates panel's predicate says the latch's reason before the
// marker's. The rename the swap's error names puts the binary back, and the
// latch opens.
func TestAMissingBinaryRefusesEveryInstallAndRollback(t *testing.T) {
	fix, newer := newInstallFixture(t, "0.2.3"), newInstallFixture(t, "0.2.4")
	dir, live := installLayout(t)
	r := heldRigAt(t, live, map[string]*refusals{"staged>dst": held(1), "bak>dst": held(-1)})
	err := heldInstall(t, fix, dir, live, nil)
	var swapErr *SwapError
	if !errors.Is(err, ErrBinaryMissing) || !errors.As(err, &swapErr) {
		t.Fatalf("the install = %v, want its swap to leave no binary", err)
	}
	r.script = map[string]*refusals{}
	refusesAll := func(when string) {
		t.Helper()
		before := readFile(t, StatePath(dir))
		for _, f := range []*installFixture{fix, newer} {
			fetched := f.archiveFetches.Load()
			_, err := installingUpdater(t, f, "0.2.1").Install(context.Background(),
				InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier})
			if !errors.Is(err, ErrBinaryMissing) || f.archiveFetches.Load() != fetched {
				t.Errorf("%s, an install of %s = %v (downloads %d to %d), want the latch's refusal before any download",
					when, f.latestVersion, err, fetched, f.archiveFetches.Load())
			}
		}
		// The latch's own error, never a refusal of the rollback's, which the
		// console would answer as 409 rollback-refused.
		err := New(Options{}).Rollback(context.Background(), InstallOptions{DataDir: dir, BinaryPath: live, Force: true})
		if !errors.Is(err, ErrBinaryMissing) || errors.Is(err, ErrRollbackRefused) {
			t.Errorf("%s, a rollback = %v, want the latch's refusal", when, err)
		}
		if after := readFile(t, StatePath(dir)); after != before {
			t.Errorf("%s, the marker was written: %q, was %q", when, after, before)
		}
	}
	refusesAll("as the swap left it")
	old := State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
		AttemptedAt: time.Now().UTC().Add(-2 * recencyWindow), SwapStarted: true}
	if err := SaveState(dir, old); err != nil {
		t.Fatal(err)
	}
	refusesAll("with the marker past its window")
	opened := old
	opened.DataDirOpenedBy = "0.2.3"
	if got := RollbackRefusalAt(opened, nil, live); got != latchMissing {
		t.Errorf("the panel's reason = %q, want the latch's first", got)
	}
	if got := RollbackRefusalAt(State{}, errDenied, live); got != latchMissing {
		t.Errorf("the panel's reason over a marker that cannot be read = %q, want the latch's first", got)
	}
	if want := "rename " + live + ".bak to " + live; swapErr.Recover != want {
		t.Fatalf("Recover = %q, want %q", swapErr.Recover, want)
	}
	if err := os.Rename(live+".bak", live); err != nil {
		t.Fatal(err)
	}
	if got := RollbackRefusalAt(opened, nil, live); got != RollbackRefusalOn(opened, nil) {
		t.Errorf("the panel's reason with the binary back = %q, want the marker's", got)
	}
	if _, err := installingUpdater(t, newer, "0.2.1").Install(context.Background(),
		InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
		t.Fatalf("an install with the binary back = %v", err)
	}
	if got := readFile(t, live); got != "bridge-binary-0.2.4" {
		t.Errorf("the binary = %q, want the newer release", got)
	}
}

// TestARollbackThatLeavesNoBinaryKeepsItsRecord: an operator's rollback whose
// put-back left no binary keeps the record it wrote before its restore, byte
// for byte, never the marker as it read it: the backup's version, renamed
// back by hand, ends it as a rollback done and keeps its hold and its veto,
// and the version rolled back from, the aside renamed back, overtakes it.
func TestARollbackThatLeavesNoBinaryKeepsItsRecord(t *testing.T) {
	l := newRollbackLayout(t)
	heldRigAt(t, l.live, map[string]*refusals{"bak>dst": held(1), "aside>dst": held(-1)})
	opts := l.opts()
	opts.RestoreBackup = func(dst, ext string) error { return replaceRunningBinary(dst, dst+ext) }
	if err := New(Options{}).Rollback(context.Background(), opts); !errors.Is(err, ErrBinaryMissing) {
		t.Fatalf("the rollback = %v, want its put-back to leave no binary", err)
	}
	want := t.TempDir()
	if err := SaveState(want, OperatorRollbackRecord(l.marker, "0.2.3")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, StatePath(l.dataDir)); got != readFile(t, StatePath(want)) {
		t.Fatalf("the marker = %s\nwant the rollback's record %s", got, readFile(t, StatePath(want)))
	}
	st, _ := LoadState(l.dataDir)
	now := time.Now().UTC()
	if got := DecideBootAction(st, "0.2.1", now); got != BootRollbackFinished {
		t.Errorf("a start of the backup's version = %v, want the rollback done", got)
	}
	if err := ClearInstall(want, st); err != nil {
		t.Fatal(err)
	}
	if kept, _ := LoadState(want); kept.RejectedVersion != "0.2.3" || kept.MigrationVeto != "0.2.3" {
		t.Errorf("what that start keeps = %+v, want the hold and the veto", kept)
	}
	if got := DecideBootAction(st, "0.2.3", now); got != BootRollbackOvertaken {
		t.Errorf("a start of the version rolled back from = %v, want it to overtake the rollback", got)
	}
}

// TestAnInstallWhoseMarkerCannotBePutBackSaysSo: a swap that failed whose
// marker cannot be written back is the install's answer, and the operator's
// rollback's, a *MarkerNotRestoredError over the failure: never a log line
// alone.
func TestAnInstallWhoseMarkerCannotBePutBackSaysSo(t *testing.T) {
	block := func(t *testing.T, dir string) {
		if err := os.Remove(StatePath(dir)); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(StatePath(dir), "in the way"), 0o700); err != nil {
			t.Error(err)
		}
	}
	check := func(t *testing.T, err error) {
		var mnr *MarkerNotRestoredError
		if !errors.As(err, &mnr) || !errors.Is(err, ErrMarkerNotRestored) || !errors.Is(err, errDenied) || errors.Is(err, ErrBinaryMissing) {
			t.Errorf("err = %v (%T), want the marker not restored over the swap's failure", err, err)
		}
	}
	t.Run("install", func(t *testing.T) {
		fix := newInstallFixture(t, "0.2.3")
		dir, live := installLayout(t)
		_, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(), InstallOptions{DataDir: dir, BinaryPath: live,
			Force: true, Verifier: noopVerifier, swap: func(_, _, _ string, mark func() error) error {
				if err := mark(); err != nil {
					return err
				}
				block(t, dir)
				return errDenied
			}})
		check(t, err)
	})
	t.Run("rollback", func(t *testing.T) {
		l := newRollbackLayout(t)
		opts := l.opts()
		opts.RestoreBackup = func(string, string) error { block(t, l.dataDir); return errDenied }
		check(t, New(Options{}).Rollback(context.Background(), opts))
	})
}

// TestASwapInterruptedBeforeItsArmingLeavesTheStaleBackupAlone: a swap held
// in its staging's wait, or in a probe's, has armed nothing, so a kill there
// leaves the install's marker without SwapStarted beside the stale backup of
// an earlier cycle, and the next start's decision clears it and leaves that
// backup where it is.
func TestASwapInterruptedBeforeItsArmingLeavesTheStaleBackupAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script map[string]*refusals
	}{
		{"held in its staging's wait", map[string]*refusals{"src>staged": held(1)}},
		{"held in a probe's wait", map[string]*refusals{"?dst": held(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := newInstallFixture(t, "0.2.3")
			dir, live := installLayout(t)
			if err := os.WriteFile(live+".bak", []byte("P"), 0o755); err != nil {
				t.Fatal(err)
			}
			r := heldRigAt(t, live, tc.script)
			var at *State
			var bak string
			r.onWait = func() {
				if at == nil {
					st, _ := LoadState(dir)
					at, bak = &st, readFile(t, live+".bak")
				}
			}
			if err := heldInstall(t, fix, dir, live, nil); err != nil {
				t.Fatalf("the install = %v", err)
			}
			if at == nil {
				t.Fatal("the swap never waited")
			}
			if at.Status != "installing" || at.SwapStarted || bak != "P" {
				t.Errorf("at the wait the marker = %+v and the backup %q, want the install armed, not swapped, beside P", *at, bak)
			}
			if got := DecideBootAction(*at, at.PreviousVersion, time.Now().UTC()); got != BootClearNotSwapped {
				t.Errorf("the next start's decision = %v, want the install cleared and the backup left", got)
			}
		})
	}
}

// TestAVacateRefusedOnceArmedDisarmsTheInstall: once the swap is armed its
// vacate has one attempt; refused, it moved nothing, and the install puts the
// marker back as it read it, with no SwapStarted.
func TestAVacateRefusedOnceArmedDisarmsTheInstall(t *testing.T) {
	fix := newInstallFixture(t, "0.2.3")
	dir, live := installLayout(t)
	r := heldRigAt(t, live, map[string]*refusals{"dst>bak": held(-1)})
	err := heldInstall(t, fix, dir, live, nil)
	if !errors.Is(err, ErrBinaryInUse) || errors.Is(err, ErrBinaryMissing) {
		t.Fatalf("the install = %v, want the hold's refusal with nothing moved", err)
	}
	if st, _ := LoadState(dir); st.SwapStarted || st.Status != "" {
		t.Errorf("the marker = %+v, want it as the install read it (none)", st)
	}
	vacates := 0
	for _, c := range r.calls {
		if c == "dst>bak" {
			vacates++
		}
	}
	if vacates != 1 || len(r.waits) != 0 {
		t.Errorf("vacates %d, waits %v; want one vacate and no wait", vacates, r.waits)
	}
	if got := readFile(t, live); got != "bridge-binary-0.2.1" {
		t.Errorf("the binary = %q, want it where it was", got)
	}
}

// TestADeferredRestartIsNotRequestedWhileTheBinaryIsMissing: the auto-
// installer's deferred restart passed its gates and waits for the install
// marker's lock behind an operator's rollback, or behind another process's
// install, whose put-back leaves no binary: it requests no restart, which
// would end the one bridge running with nothing to start again.
func TestADeferredRestartIsNotRequestedWhileTheBinaryIsMissing(t *testing.T) {
	for _, behind := range []string{"a rollback", "an install"} {
		t.Run("behind "+behind, func(t *testing.T) {
			var dir, live string
			atHold, release := newGate(), newGate()
			var run func() error
			if behind == "a rollback" {
				l := newRollbackLayout(t)
				dir, live = l.dataDir, l.live
				heldRigAt(t, live, map[string]*refusals{"bak>dst": held(1), "aside>dst": held(-1)})
				opts := l.opts()
				opts.RestoreBackup = func(dst, ext string) error {
					atHold.open()
					<-release.ch
					return replaceRunningBinary(dst, dst+ext)
				}
				run = func() error { return New(Options{}).Rollback(context.Background(), opts) }
			} else {
				fix := newInstallFixture(t, "0.2.3")
				dir, live = installLayout(t)
				heldRigAt(t, live, map[string]*refusals{"staged>dst": held(1), "bak>dst": held(-1)})
				run = func() error {
					_, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(), InstallOptions{DataDir: dir,
						BinaryPath: live, Force: true, Verifier: noopVerifier, swap: func(dst, nb, ext string, mark func() error) error {
							atHold.open()
							<-release.ch
							return swapByVacate(dst, nb, ext, mark)
						}})
					return err
				}
			}
			answer, done := make(chan error, 1), make(chan struct{})
			go func() { defer close(done); answer <- run() }()
			drainOnCleanup(t, release.open, done, behind)
			awaitGate(t, atHold, behind+"'s hold of the marker's lock")
			restarts := &atomic.Int32{}
			u := New(Options{AutoInstallOpts: &InstallOptions{DataDir: dir, BinaryPath: live}, AutoInstallRestart: func() { restarts.Add(1) }})
			waits := signalWaits(t)
			restarted := make(chan struct{})
			go func() { defer close(restarted); u.restartWhenDrained(context.Background()) }()
			drainOnCleanup(t, release.open, restarted, "the restart")
			select {
			case <-waits:
			case <-time.After(withinTheTest):
				t.Fatal("the restart never waited for the marker's lock")
			}
			release.open()
			if err := <-answer; !errors.Is(err, ErrBinaryMissing) {
				t.Fatalf("%s = %v, want it to leave no binary", behind, err)
			}
			select {
			case <-restarted:
			case <-time.After(withinTheTest):
				t.Fatal("the restart did not return")
			}
			if n := restarts.Load(); n != 0 {
				t.Errorf("the restart was requested %d time(s) with no binary on disk", n)
			}
		})
	}
}

// TestEveryProtectedOperationFitsInsideTheMarkersLockWait: what the operator's
// rollback and an install hold the install marker's lock for, built from the
// constants (a marker write's retries, the forward and the recovery
// schedules, the SCM's stop), fits inside the wait every other taker gives it;
// and each row is the terms its path makes, run at its worst (the forward
// deadline spent whole, the place refused, the put-back landing at its last
// attempt), the writes and the waits recorded. The rollbacks at a start are
// cmd/bridge's (TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait).
func TestEveryProtectedOperationFitsInsideTheMarkersLockWait(t *testing.T) {
	writes := countMarkerWrites(t)
	l := newRollbackLayout(t)
	r := heldRigAt(t, l.live, map[string]*refusals{"?dst": held(7), "bak>dst": held(1), "aside>dst": held(6)})
	opts := l.opts()
	opts.RestoreBackup = func(dst, ext string) error { return replaceRunningBinary(dst, dst+ext) }
	before := writes(l.dataDir)
	if err := New(Options{}).Rollback(context.Background(), opts); !errors.As(err, new(*SwapError)) {
		t.Fatalf("the rollback = %v, want it put back at its worst", err)
	}
	rollbackWaits, rollbackWrites := waitsTotal(r.waits), writes(l.dataDir)-before

	fix := newInstallFixture(t, "0.2.3")
	dir, live := installLayout(t)
	ri := heldRigAt(t, live, map[string]*refusals{"?staged": held(7), "staged>dst": held(1), "bak>dst": held(6)})
	stops := 0
	before = writes(dir)
	if err := heldInstall(t, fix, dir, live, &stops); !errors.As(err, new(*SwapError)) {
		t.Fatalf("the install = %v, want it put back at its worst", err)
	}
	installWaits, installWrites := waitsTotal(ri.waits), writes(dir)-before

	write, waits := atomicwrite.RenameRetryBudget(), ForwardWait()+RecoveryWait()
	for _, row := range []struct {
		name            string
		bound           time.Duration
		writes, wantW   int
		waited          time.Duration
		stops, wantStop int
	}{
		{"the operator's rollback", 2*write + waits, rollbackWrites, 2, rollbackWaits, 0, 0},
		{"an install as the service", 3*write + waits, installWrites, 3, installWaits, 0, 0},
		{"an install in the foreground", 3*write + waits + scmStopWait, installWrites, 3, installWaits, stops, 1},
	} {
		if row.bound > defaultStateLockWait {
			t.Errorf("%s holds the marker's lock up to %s, past the %s every other taker waits", row.name, row.bound, defaultStateLockWait)
		}
		if row.writes != row.wantW || row.waited != waits || row.stops != row.wantStop {
			t.Errorf("%s made %d writes, waited %s and stopped %d services; its row counts %d, %s and %d",
				row.name, row.writes, row.waited, row.stops, row.wantW, waits, row.wantStop)
		}
	}
}

// TestASlowCopyAcrossVolumesLeavesTheSwapsProbesUntried: the swap's own
// staging, its rename refused across volumes after the forward deadline's
// time and the copy that stands in for it made, leaves the probes after it
// untried: nothing moved, nothing armed, the candidate's copy removed and
// ErrDeadlinePassed the answer, never a hold.
func TestASlowCopyAcrossVolumesLeavesTheSwapsProbesUntried(t *testing.T) {
	r := newHeldRig(t, "elsewhere", map[string]*refusals{"src>staged": {n: 1, err: &os.LinkError{Op: "rename", Err: syscall.EXDEV}}})
	r.took = map[string]time.Duration{"src>staged": secs(5)}
	err := r.swap()
	if got := strings.Join(r.calls, " "); got != "src>staged" {
		t.Errorf("calls = %q, want the staging alone", got)
	}
	if !errors.Is(err, ErrDeadlinePassed) || errors.Is(err, ErrBinaryInUse) || r.armedAt >= 0 {
		t.Errorf("the swap = %v, armed %v; want the probes untried for the deadline and nothing armed", err, r.armedAt >= 0)
	}
	if got := r.files(); got != "dst=N bak=P aside=- cand=-" {
		t.Errorf("files = %s, want the binary and its backup where they were", got)
	}
}
