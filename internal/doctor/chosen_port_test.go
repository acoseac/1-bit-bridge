package doctor

import (
	"os"
	"testing"
)

// TestChosenPortIsExcusedOnlyByTheRecordedBridgeSeenListening drives both
// port ladders over the same facts, through RunPortChecks as `bridge init`
// calls it: once with OwnPIDPortsUnknown, as init grades a port over an
// install whose config did not load, and once without, as it grades the
// ports of a config that loaded.
//
// With the config, a held port behind a live recorded bridge is probably
// that bridge's, because its config names the port: checkPort's liveness
// arm answers ok or warn, unless the probe saw enough to rule the bridge
// out, which is a FAIL there too. Without it nothing names the port. The
// install may have moved off init's defaults (because something else holds
// 7788, say), so its bridge is alive on its own ports while another process
// holds the one init writes, and excusing that port saves a config the
// restarted bridge cannot bind. Only the bridge seen listening there says
// otherwise, so every other row is a FAIL.
func TestChosenPortIsExcusedOnlyByTheRecordedBridgeSeenListening(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup holds the port, or leaves it free, and returns it with the
		// pid file to record ("" for none).
		setup     func(t *testing.T) (port int, pidFile string)
		want      Status // ports unknown
		wantKnown Status // the ordinary ladder, the control
	}{
		// This process holds the port and is the recorded pid, so the
		// probe sees it listening: lsof on macOS and Linux, /proc on Linux
		// without lsof, GetExtendedTcpTable on Windows.
		{"the recorded bridge seen listening", func(t *testing.T) (int, string) {
			return bindPort(t), writePIDFile(t, os.Getpid())
		}, OK, OK},
		// pid 4242 is not the holder, so no probe names it, and the probe
		// is forced to a miss that cannot rule it out, as a capability-bound
		// bridge's is. Alive, with the listener running as this user: the
		// Linux uid arm's ok.
		{"recorded bridge alive, listener runs as this user", func(t *testing.T) (int, string) {
			withUnattributedMiss(t)
			withPIDAlive(t, true)
			withHiddenListener(t, true, nil)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, OK},
		// Alive, and nothing can say who holds the port: the liveness
		// arm's warn.
		{"recorded bridge alive, holder unattributable", func(t *testing.T) (int, string) {
			withUnattributedMiss(t)
			withPIDAlive(t, true)
			withHiddenListener(t, false, nil)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, Warn},
		// Alive, and the probe saw enough to rule it out (/proc read all of
		// its descriptors, or Windows' table named the holders): the port
		// is another process's, whoever the listener runs as.
		{"recorded bridge alive, ruled out, listener runs as this user", func(t *testing.T) (int, string) {
			withOwnerProbe(t, false, ownerSighting{saw: "/proc shows no descriptor of pid 4242 listening on this port", ruledOut: true}, nil)
			withPIDAlive(t, true)
			withHiddenListener(t, true, nil)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, Fail},
		{"recorded bridge not running", func(t *testing.T) (int, string) {
			withPIDAlive(t, false)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, Fail},
		{"no pid file recorded", func(t *testing.T) (int, string) {
			return bindPort(t), ""
		}, Fail, Fail},
		{"port free", func(t *testing.T) (int, string) {
			return mustFreePort(t), writePIDFile(t, os.Getpid())
		}, OK, OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, pidFile := tc.setup(t)
			assertAPIPortVerdict(t, Deps{APIPort: port, OwnPIDFile: pidFile, OwnPIDPortsUnknown: true}, tc.want)
			assertAPIPortVerdict(t, Deps{APIPort: port, OwnPIDFile: pidFile}, tc.wantKnown)
		})
	}
}

// assertAPIPortVerdict grades d's API port through RunPortChecks, as
// `bridge init` does, and requires the one check it returns to be want.
func assertAPIPortVerdict(t *testing.T, d Deps, want Status) {
	t.Helper()
	rep := RunPortChecks(t.Context(), d, true, false)
	if len(rep.Checks) != 1 {
		t.Fatalf("RunPortChecks returned %d checks, want the one port-api", len(rep.Checks))
	}
	if c := rep.Checks[0]; c.Status != want {
		t.Errorf("OwnPIDPortsUnknown=%v: got %v (%s / %s), want %v",
			d.OwnPIDPortsUnknown, c.Status, c.Summary, c.Hint, want)
	}
}
