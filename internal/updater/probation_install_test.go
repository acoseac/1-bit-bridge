//go:build !windows

package updater

import (
	"context"
	"testing"

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
	if why := upd.probationHolds(dataDir, live, "0.1.0"); why != "" {
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
