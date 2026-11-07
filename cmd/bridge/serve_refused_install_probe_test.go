package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The early rollback's question to its backup, at its bound (backlog B706).
// B706 left production as it was, so these are pinning tests: production's
// bound, and what a backup that does not answer within it comes to.

// runAsBridgeSilentEnv, a duration, makes a copy of the test binary say
// nothing for that long before it runs as the bridge: a backup that does not
// answer `version` within the probe's bound. The copy ends by itself once the
// pause is over, whether or not the probe killed it first. Like the others,
// its name does not start with BRIDGE_ (run_as_bridge_test.go says why).
const runAsBridgeSilentEnv = "CMDBRIDGE_TEST_RUN_AS_BRIDGE_SILENT"

// holdIfSilent is the copy's pause, before anything else of its run.
func holdIfSilent() {
	if d, err := time.ParseDuration(os.Getenv(runAsBridgeSilentEnv)); err == nil && d > 0 {
		time.Sleep(d)
	}
}

// TestTheEarlyRollbackGivesItsBackupTenSecondsInProduction is a pinning test
// (backlog B706): with no bound of a test's, the early rollback gives its
// backup updater.VersionProbeTimeout, 10 s, to say its version, as it did
// before B706. A test's longer bound is serveOpts' alone.
func TestTheEarlyRollbackGivesItsBackupTenSecondsInProduction(t *testing.T) {
	if got := backupProbeBound(serveOpts{}); got != 10*time.Second {
		t.Errorf("the early rollback gives its backup %s in production, want 10s", got)
	}
	if got := backupProbeBound(serveOpts{backupProbeTimeout: time.Minute}); got != time.Minute {
		t.Errorf("the early rollback gives its backup %s where a test gave it a minute", got)
	}
}

// TestABackupSilentPastItsBoundIsAskedAgainAtTheNextStart is a pinning test
// (backlog B706: production is unchanged). It pins what an early rollback
// comes to when its backup does not answer `version` within its bound. Serve
// refuses its settings as it always has, exit 2. The binary, the backup and
// the install marker stay byte for byte as they were, and serve writes
// nothing of its own. Its updater line says the backup did not say its
// version within that bound. The next start, the one a service manager makes,
// asks the backup again, and once the backup answers it puts it back. The
// bound here is the seam's, a second, so the test does not wait out
// production's ten: TestTheEarlyRollbackGivesItsBackupTenSecondsInProduction
// pins those.
func TestABackupSilentPastItsBoundIsAskedAgainAtTheNextStart(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	// A minute of silence, far past the second it is given.
	t.Setenv(runAsBridgeSilentEnv, time.Minute.String())
	code, stderr := r.serve(t, serveOpts{backupProbeTimeout: time.Second})
	if code != 2 {
		t.Fatalf("serve over a backup that does not answer = %d, want 2, the settings' refusal as before; stderr=%s", code, stderr)
	}
	files.requireUnchanged(t)
	requireNoStoreOpened(t, r.dataDir)
	if said := updaterLines(stderr); !strings.Contains(said, "did not say its version: it did not answer within 1s") {
		t.Errorf("serve did not say that the backup did not answer within its bound; its updater lines: %q; stderr=%s", said, stderr)
	}

	// The next start, as a service manager makes it, the backup answering.
	t.Setenv(runAsBridgeSilentEnv, "")
	code, stderr = r.serve(t, serveOpts{})
	if code != supervision.RestartExitCode {
		t.Fatalf("the next start = %d, want %d: it asks the backup again and puts it back; stderr=%s",
			code, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the next start, want %s", got, previousVersion)
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || !st.RollbackStarted || st.RejectedVersion != version.ServerVersion {
		t.Errorf("the marker after the next start = %+v (%v), want the rollback's record, holding this version back", st, err)
	}
}
