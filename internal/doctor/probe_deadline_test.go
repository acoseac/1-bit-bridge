package doctor

import (
	"testing"
	"time"
)

// realProbeUsesTheTestDeadline gives a real lsof the time this test has
// left when that is longer than probeTimeout, and probeTimeout otherwise
// (SetProbeLimitForTest, which cmd/bridge's tests take too).
// isPIDListeningOnPort and the macOS second look read probeLimit, which
// production leaves at probeTimeout (2s). A loaded runner misses that bound
// and turns a slow lsof into a failed port verdict (backlog B338). The
// timeout-contract tests leave the default, so they still end at 2s.
func realProbeUsesTheTestDeadline(t *testing.T) {
	t.Helper()
	t.Cleanup(SetProbeLimitForTest(t.Deadline, 5*time.Second))
}
