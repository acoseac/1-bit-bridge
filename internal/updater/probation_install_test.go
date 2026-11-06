//go:build !windows

package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// TestAnAutomaticInstallOverAStagedVersionLeavesTheStagedOneAsTheBackup: a
// manual install staged and not started (0.2.0 over the running bridge),
// then the auto-installer of the same running process over it (0.3.0),
// which keeps today's behaviour (backlog B660): the probation does not hold
// it back, since the marker is an install of 0.2.0, not of the running
// version; .bak is the staged 0.2.0 while the marker names the running
// version as the one the install replaced. The crash rollback's first check
// passes on that marker at the limit, so it is the backup's own version, at
// its second check, that refuses to put 0.2.0 back for the running version
// (cmd/bridge's TestAnAutomaticInstallOverAStagedVersionCannotRollBackPastIt
// boots serve over this layout).
func TestAnAutomaticInstallOverAStagedVersionLeavesTheStagedOneAsTheBackup(t *testing.T) {
	dataDir, live := installLayout(t)
	staged := newInstallFixture(t, "0.2.0")
	if _, err := installingUpdater(t, staged, "0.1.0").Install(context.Background(), InstallOptions{
		DataDir: dataDir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
		t.Fatalf("the manual install of 0.2.0 = %v", err)
	}
	next := newInstallFixture(t, "0.3.0")
	upd := installingUpdater(t, next, "0.1.0")
	if why := upd.probationHolds(dataDir, "0.1.0"); why != "" {
		t.Fatalf("the auto-installer waits (%q) on an install of another version than the one running", why)
	}
	if _, err := upd.Install(context.Background(), InstallOptions{
		DataDir: dataDir, BinaryPath: live, Force: true, Verifier: noopVerifier, Automatic: true}); err != nil {
		t.Fatalf("the automatic install of 0.3.0 over the staged 0.2.0 = %v", err)
	}
	if got := readFile(t, live+".bak"); got != "bridge-binary-0.2.0" {
		t.Errorf(".bak = %q, want the staged 0.2.0 it replaced", got)
	}
	st := mustLoad(t, dataDir)
	if st.TargetVersion != "0.3.0" || st.PreviousVersion != version.ServerVersion {
		t.Fatalf("the marker = %+v, want 0.3.0 over the running %s", st, version.ServerVersion)
	}
	st.FailedStarts, st.DataDirOpenedBy = ProbationFailedStarts, "0.3.0"
	if ok, why := CrashRollback(st, "0.3.0"); !ok {
		t.Errorf("the crash rollback's first check = %q: the refusal must come from the backup's version, its second", why)
	}
}

// TestTheAutoInstallerWaitsForTheRunningInstallsProbation: while the running
// version's own install awaits its confirmation, with no migration begun, and
// the process has been up for less than twice the window, the auto-installer
// defers, at its gate and again under the install marker's lock, and says
// why; a manual install is not held.
func TestTheAutoInstallerWaitsForTheRunningInstallsProbation(t *testing.T) {
	upd, restarts := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return time.Minute }})
	dir := upd.autoInstallOpts.DataDir
	mustSave(t, dir, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", SwapStarted: true, FailedStarts: 1})
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; !strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer's deferred reason = %q, want it waiting for 0.0.1's confirmation", why)
	}
	if restarts.Load() != 0 {
		t.Error("the auto-installer restarted")
	}
	opts := *upd.autoInstallOpts
	opts.Automatic = true
	if err := upd.decideUnderLock(opts, "9.9.9"); !errors.Is(err, ErrInstallAwaitsConfirmation) {
		t.Errorf("the decision under the lock for the auto-installer = %v, want it waiting", err)
	}
	opts.Automatic = false
	if err := upd.decideUnderLock(opts, "9.9.9"); err != nil {
		t.Errorf("the decision under the lock for a manual install = %v, want none", err)
	}
}

// TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack: a migration
// of the store begun under the running install, which no crash rollback
// passes, holds nothing back.
func TestTheAutoInstallerDoesNotWaitWhereTheBackupCannotBePutBack(t *testing.T) {
	upd, _ := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return time.Minute }})
	mustSave(t, upd.autoInstallOpts.DataDir, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0",
		SwapStarted: true, FailedStarts: 1, StoreMigratedBy: "0.0.1"})
	upd.maybeAutoInstall(context.Background())
	if why := upd.Status().DeferredReason; strings.Contains(why, "awaits its confirmation") {
		t.Errorf("the auto-installer waits (%q) over a store a migration began on", why)
	}
	opts := *upd.autoInstallOpts
	opts.Automatic = true
	if err := upd.decideUnderLock(opts, "9.9.9"); err != nil {
		t.Errorf("the decision under the lock = %v, want none", err)
	}
}

// TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered: past twice
// the window, an install that never answered does not wait on itself, since
// a newer release may be its fix; and an install of another version, or a
// confirmed one, holds nothing back.
func TestTheAutoInstallerDoesNotWaitForAnInstallThatNeverAnswered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uptime time.Duration
		st     State
	}{
		{"past twice the window", 2 * ProbationWindow, State{Status: "installing", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", NotReady: true, FailedStarts: 1}},
		{"an install of another version", time.Minute, State{Status: "installing", TargetVersion: "0.0.2", PreviousVersion: "0.0.1"}},
		{"a confirmed install", time.Minute, State{Status: "installed", TargetVersion: "0.0.1", PreviousVersion: "0.0.0", InstalledAt: time.Now()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upd, _ := makeAutoInstallUpdater(t, Options{AutoInstall: true, Uptime: func() time.Duration { return tc.uptime }})
			mustSave(t, upd.autoInstallOpts.DataDir, tc.st)
			upd.maybeAutoInstall(context.Background())
			if why := upd.Status().DeferredReason; strings.Contains(why, "awaits its confirmation") {
				t.Errorf("the auto-installer waits: %q", why)
			}
		})
	}
	if why := ProbationHolds(State{Status: "installing", TargetVersion: "0.0.1"}, "0.0.1", 2*ProbationWindow-time.Second); why == "" {
		t.Error("ProbationHolds just inside twice the window holds nothing")
	}
}
