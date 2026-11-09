package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The auto-installer's wait for the running install's probation (backlog
// B660): it holds a release back only where the backup beside the binary is
// one the crash rollback would put back. These run on every platform: they
// need no release archive, and an install they let through fails at once at
// its download, on a port that refuses.

// probationHoldsUpdater is an auto-installer of 0.0.1 that has seen 9.9.9
// published, over the marker st in a data dir of its own, the binary it runs
// as in a directory of its own and, with bak, the backup beside it, up for
// uptime; it returns the options of its automatic install too.
func probationHoldsUpdater(t *testing.T, st State, bak bool, uptime time.Duration) (*Updater, InstallOptions) {
	t.Helper()
	dataDir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bridge")
	if err := os.WriteFile(bin, []byte("bridge-binary-0.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if bak {
		if err := os.WriteFile(bin+".bak", []byte("bridge-binary-0.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, dataDir, st)
	opts := &InstallOptions{DataDir: dataDir, BinaryPath: bin, Sessions: NewTracker(), Force: true}
	upd := New(Options{RepoOverride: "fake/repo", Client: NewClient("fake/repo", time.Second).WithBaseURL("http://127.0.0.1:1"),
		AutoInstall: true, AutoInstallOpts: opts, AutoInstallRestart: func() {}, Uptime: func() time.Duration { return uptime }})
	upd.mu.Lock()
	upd.status.LatestVersion, upd.status.UpdateAvailable, upd.status.CurrentVersion = "9.9.9", true, "0.0.1"
	upd.mu.Unlock()
	auto := *opts
	auto.Automatic = true
	return upd, auto
}

// onProbation is the running 0.0.1's own install over 0.0.0, swapped,
// awaiting its confirmation with one start counted: the crash rollback's
// marker checks take it at their limit.
func onProbation() State {
	return State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", SwapStarted: true,
		AttemptedAt: time.Now().UTC().Truncate(time.Second), DataDirOpenedBy: "0.0.1", FailedStarts: 1}
}

// requireWaits requires the auto-installer to wait for the running install's
// confirmation, at its gate and under the install marker's lock.
func requireWaits(t *testing.T, upd *Updater, auto InstallOptions) {
	t.Helper()
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; !strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer's deferred reason = %q, want it waiting for 0.0.1's confirmation", why)
	}
	if err := upd.decideUnderLock(admitted(t, upd), auto, "9.9.9"); !errors.Is(err, ErrInstallAwaitsConfirmation) {
		t.Errorf("the decision under the lock = %v, want it waiting", err)
	}
}

// requireNoWait requires the auto-installer not to wait for the running
// install, at its gate or under the install marker's lock.
func requireNoWait(t *testing.T, upd *Updater, auto InstallOptions) {
	t.Helper()
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer waits: %q", why)
	}
	if err := upd.decideUnderLock(admitted(t, upd), auto, "9.9.9"); errors.Is(err, ErrInstallAwaitsConfirmation) {
		t.Errorf("the decision under the lock = %v, want no wait", err)
	}
}

// TestTheAutoInstallerWaitsForTheRunningInstallsProbation: while the running
// version's own install awaits its confirmation, the backup beside it one the
// crash rollback would put back, and the process has been up for less than
// twice the window, the auto-installer defers, at its gate and again under
// the install marker's lock, and says why; a manual install is not held.
func TestTheAutoInstallerWaitsForTheRunningInstallsProbation(t *testing.T) {
	upd, auto := probationHoldsUpdater(t, onProbation(), true, time.Minute)
	requireWaits(t, upd, auto)
	manual := auto
	manual.Automatic = false
	if err := upd.decideUnderLock(admitted(t, upd), manual, "9.9.9"); err != nil {
		t.Errorf("the decision under the lock for a manual install = %v, want none", err)
	}
}

// TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack: a migration
// of the store begun under the running install, which no crash rollback
// passes, holds nothing back.
func TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack(t *testing.T) {
	st := onProbation()
	st.StoreMigratedBy = "0.0.1"
	upd, auto := probationHoldsUpdater(t, st, true, time.Minute)
	requireNoWait(t, upd, auto)
}

// TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered: past twice
// the window, an install that never answered does not wait on itself, since
// a newer release may be its fix; and an install of another version, or a
// confirmed one, holds nothing back.
func TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered(t *testing.T) {
	notReady := onProbation()
	notReady.NotReady = true
	for _, tc := range []struct {
		name   string
		uptime time.Duration
		st     State
	}{
		{"past twice the window", 2 * ProbationWindow, notReady},
		{"an install of another version", time.Minute, State{Status: "installing", TargetVersion: "0.0.2", PreviousVersion: "0.0.1", SwapStarted: true}},
		{"a confirmed install", time.Minute, State{Status: "installed", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", InstalledAt: time.Now()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upd, auto := probationHoldsUpdater(t, tc.st, true, tc.uptime)
			requireNoWait(t, upd, auto)
		})
	}
	upd, auto := probationHoldsUpdater(t, onProbation(), true, 2*ProbationWindow-time.Second)
	requireWaits(t, upd, auto)
}

// TestTheAutoInstallerDoesNotWaitBesideARefusedRollback: a crash rollback
// that was refused (State.RollbackRefused) puts nothing back, so there is no
// backup to keep for it and nothing is held (backlog B660's second round).
func TestTheAutoInstallerDoesNotWaitBesideARefusedRollback(t *testing.T) {
	st := onProbation()
	st.RollbackRefused = "the backup, 0.0.0, refuses this bridge's settings, so it would not start on them: line 3"
	upd, auto := probationHoldsUpdater(t, st, true, time.Minute)
	requireNoWait(t, upd, auto)
}

// TestTheAutoInstallerDoesNotWaitWithoutABackup: with no backup beside the
// binary serve runs as, the crash rollback has nothing to put back, and
// nothing is held (backlog B660's second round).
func TestTheAutoInstallerDoesNotWaitWithoutABackup(t *testing.T) {
	upd, auto := probationHoldsUpdater(t, onProbation(), false, time.Minute)
	requireNoWait(t, upd, auto)
}

// TestTheAutoInstallerDoesNotWaitWhereTheCrashRollbackWouldRefuse: a marker
// the crash rollback's own checks refuse at its limit (an install that never
// reached its swap, one that names no version to go back to, a data dir
// another version opened since) puts nothing back, and nothing is held
// (backlog B660's second round).
func TestTheAutoInstallerDoesNotWaitWhereTheCrashRollbackWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		lay  func(*State)
	}{
		{"the install never reached its swap", func(st *State) { st.SwapStarted = false }},
		{"no version to go back to", func(st *State) { st.PreviousVersion = "" }},
		{"another version opened the data dir", func(st *State) { st.DataDirOpenedBy = "0.0.2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := onProbation()
			tc.lay(&st)
			upd, auto := probationHoldsUpdater(t, st, true, time.Minute)
			requireNoWait(t, upd, auto)
		})
	}
}
