package main

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// lsofDeadlineReserve is what lsofTakesTheTestsDeadline leaves of the test's
// deadline for the test to fail and clean up in, as internal/doctor's own
// tests leave (realProbeUsesTheTestDeadline).
const lsofDeadlineReserve = 5 * time.Second

// lsofTakesTheTestsDeadline gives every lsof the doctor spawns until the test
// ends (bridge init's preflight, bridge doctor and doctor.Run alike, each
// asking the real lsof whether the bridge recorded in a data dir holds a port
// the test holds) the time the test has left, less lsofDeadlineReserve, where
// that is longer than the doctor's own 2 s bound (doctor.SetProbeLimitForTest;
// backlog B872 part 2). On a loaded runner lsof outlives that bound, and the
// probe cut short reads as no answer: a re-init over a config that does not
// load then exits 1, its own bridge's ports read as another process's
// (TestTheReinitsLsofProbeTakesTheTestsDeadline). Every cmd/bridge test that
// reaches the real lsof calls it, through reinitBrokenInstall and
// doctorLineOf or itself; the census that listed them is in the engineering
// log, 2026-11-14. Process-global: a test that calls it MUST NOT call
// t.Parallel().
func lsofTakesTheTestsDeadline(t *testing.T) {
	t.Helper()
	t.Cleanup(doctor.SetProbeLimitForTest(t.Deadline, lsofDeadlineReserve))
}
