//go:build !windows

package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withoutLsof puts the package in the state resolveLsof leaves on a host
// with no lsof: lsofPath empty. That is the resolution itself rather than
// a predicate about it, so a test using it takes the branch such a host
// takes. The stock golang image has no lsof, nor do Debian and Ubuntu
// minimal installs (the package is Priority: standard) or their container
// images. Every CI runner has one, which is why a verdict that depended on
// it was seen only in a container run.
func withoutLsof(t *testing.T) {
	t.Helper()
	orig := lsofPath
	t.Cleanup(func() { lsofPath = orig })
	lsofPath = ""
}

// TestPortCheckWithoutLsofFailsAPortNoLiveBridgeOfOursHolds.
//
// checkPort used to end in `if !portProbeAvailable() { return warn(…) }`,
// goreview F9's answer to a live bridge the probe could not attribute
// (#429). #640's liveness arm has answered that case since: a recorded pid
// that is alive gets ok or warn before anything falls through. So what
// still reached the fallback was a bound port with no live pid of ours to
// attribute it to. lsof could not change that answer (with no pid it is
// never asked, and a dead pid holds nothing for it to find), yet its
// absence turned the Fail into a warn, and `bridge init` on such a host
// saved a port another process held. Its second port pass clears
// OwnPIDFile and a first install has none: #970's defect, back on every
// host without lsof.
func TestPortCheckWithoutLsofFailsAPortNoLiveBridgeOfOursHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pidFile func(t *testing.T) string
	}{
		// The caller saying no bridge of ours can hold the port: init's
		// second pass, a first install, `bridge doctor` with no config.
		{"no pid file given", func(*testing.T) string { return "" }},
		// A stopped bridge: serve removes its pid file on the way out.
		{"pid file absent", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "server.pid")
		}},
		// A bridge that stopped without removing it. #640 pinned this
		// one as a Fail, with the probe forced available.
		{"recorded pid not running", func(t *testing.T) string {
			withPIDAlive(t, false)
			return writePIDFile(t, 4242)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withoutLsof(t)
			port := bindPort(t)
			c := checkPort(t.Context(), "port-test", port, tc.pidFile(t))
			if c.Status != Fail {
				t.Errorf("bound port, no live pid of ours, no lsof: got %v (%s / %s), want fail",
					c.Status, c.Summary, c.Hint)
			}
		})
	}
}

// TestPortCheckWithoutLsofStillAnswersALiveRecordedPID is the control for
// the test above: taking the fallback out must not make every bound port
// on such a host a Fail. A recorded pid that is alive still reaches #640's
// arm, which answers for a live bridge the probe cannot attribute, with or
// without lsof.
func TestPortCheckWithoutLsofStillAnswersALiveRecordedPID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owned bool
		want  Status
	}{
		{"listener owned by this user", true, OK},
		{"owner not attributable", false, Warn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withoutLsof(t)
			withPIDAlive(t, true)
			withPortOwner(t, tc.owned, nil)
			port := bindPort(t)
			c := checkPort(t.Context(), "port-test", port, writePIDFile(t, 4242))
			if c.Status != tc.want {
				t.Errorf("bound port, recorded pid alive, no lsof: got %v (%s / %s), want %v",
					c.Status, c.Summary, c.Hint, tc.want)
			}
		})
	}
}

// TestPortCheckWithoutLsofAttributesALiveBridgeFromProc: on Linux the
// attribution itself does not need lsof. isPIDListeningOnPort reads the
// same kernel tables lsof does (pidListensOnPort), so a live bridge that
// holds its port is named as ours, rather than reaching the liveness arm's
// uid answer, whose wording blames a capability-bound binary for what was
// a missing tool.
func TestPortCheckWithoutLsofAttributesALiveBridgeFromProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("attribution without lsof reads /proc, which only Linux has")
	}
	withoutLsof(t)
	c := checkPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, os.Getpid()))
	if c.Status != OK || !strings.Contains(c.Summary, "bound by our own bridge") {
		t.Errorf("our own listener, no lsof: got %v (%s), want ok \"bound by our own bridge\"", c.Status, c.Summary)
	}
}

// TestChosenPortWithoutLsofRecognisesTheRecordedBridge is the reported
// case on a Linux host without lsof, which the stock golang image, and
// Debian's and Ubuntu's minimal installs, are. checkChosenPort excuses a
// port only when the recorded bridge is seen listening on it; were seeing
// it to need lsof, the re-init that replaces a broken config would refuse
// the bridge's own port here and go through where lsof is installed.
func TestChosenPortWithoutLsofRecognisesTheRecordedBridge(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("attribution without lsof reads /proc, which only Linux has")
	}
	withoutLsof(t)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, os.Getpid()))
	if c.Status != OK {
		t.Errorf("the recorded bridge holds the port, no lsof: got %v (%s / %s), want ok", c.Status, c.Summary, c.Hint)
	}
}
