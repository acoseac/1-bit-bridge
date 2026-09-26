package doctor

import (
	"os"
	"strconv"
	"strings"
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
// arm answers ok or warn. Without it nothing names the port. The install
// may have moved off init's defaults (often because something else holds
// 7788), so its bridge is alive on its own ports while another process
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
		// pid 4242 is not the holder, so no probe names it. Alive, with the
		// listener running as this user: the Linux uid arm's ok.
		{"recorded bridge alive, listener runs as this user", func(t *testing.T) (int, string) {
			withPIDAlive(t, true)
			withPortOwner(t, true, nil)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, OK},
		// Alive, and nothing can say who holds the port: the liveness
		// arm's warn.
		{"recorded bridge alive, holder unattributable", func(t *testing.T) (int, string) {
			withPIDAlive(t, true)
			withPortOwner(t, false, nil)
			return bindPort(t), writePIDFile(t, 4242)
		}, Fail, Warn},
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
			for _, unknown := range []bool{true, false} {
				want := tc.wantKnown
				if unknown {
					want = tc.want
				}
				d := Deps{APIPort: port, OwnPIDFile: pidFile, OwnPIDPortsUnknown: unknown}
				rep := RunPortChecks(t.Context(), d, true, false)
				if len(rep.Checks) != 1 {
					t.Fatalf("RunPortChecks returned %d checks, want the one port-api", len(rep.Checks))
				}
				if c := rep.Checks[0]; c.Status != want {
					t.Errorf("OwnPIDPortsUnknown=%v: got %v (%s / %s), want %v",
						unknown, c.Status, c.Summary, c.Hint, want)
				}
			}
		})
	}
}

// TestChosenPortRefusalNamesTheRecordedBridge pins the hint on the row
// that is most likely to be the operator's own bridge: running, with a
// port nothing can attribute to it. The FAIL stands, since nothing says
// the port is that bridge's, but the hint has to say which bridge was
// found and how to settle it, or it reads "another process owns this
// port" about what may be the operator's own listener.
func TestChosenPortRefusalNamesTheRecordedBridge(t *testing.T) {
	withPIDAlive(t, true)
	withPortOwner(t, false, nil)
	pidFile := writePIDFile(t, 4242)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), pidFile)
	if c.Status != Fail {
		t.Fatalf("got %v (%s), want fail", c.Status, c.Summary)
	}
	for _, want := range []string{pidFile, "pid " + strconv.Itoa(4242), "stop that bridge"} {
		if !strings.Contains(c.Hint, want) {
			t.Errorf("hint does not say %q: %s", want, c.Hint)
		}
	}
}
