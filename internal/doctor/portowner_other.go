//go:build !linux

package doctor

import (
	"fmt"
	"os"
)

// hiddenListenerOfThisUser is the non-Linux stub for checkPort's
// last-resort port attribution. It always answers "don't know" so the
// caller falls through to its Warn.
//
// The real implementation (portowner_linux.go) reads the `uid` column of
// /proc/net/tcp{,6}, and the descriptors of the processes this user can
// read under /proc, which exist only on Linux. It is there to rescue one
// specific deployment shape — a bridge granted cap_net_bind_service so it
// can bind :443 unprivileged, whose resulting dumpable=0 blocks port→pid
// attribution — and that shape is Linux-only by construction: macOS has no
// file capabilities, and on Windows the native GetExtendedTcpTable probe
// answers directly with no equivalent restriction.
//
// Returning (false, nil) rather than an error is deliberate: a nil error
// means "asked and got no match", which lands on the same Warn as a real
// no-match. Reporting a mechanism error here would imply something is
// broken on hosts where there is simply nothing to ask.
func hiddenListenerOfThisUser(int) (bool, error) { return false, nil }

// pidListensOnPort is the non-Linux stub for the /proc attribution that
// isPIDListeningOnPort asks where no usable lsof resolved, and after an
// lsof miss. There is no socket table to walk here: macOS ships lsof in its
// base system, so a Mac reaches the first only with lsof removed, and
// Windows attributes natively (GetExtendedTcpTable, doctor_windows.go) and
// never calls it.
//
// (false, nil) is "asked and got no match", lsof's own answer for a port it
// cannot attribute. The sighting says nothing looked, which leaves the
// recorded bridge possible: without lsof the caller puts the missing lsof in
// front of it, and after lsof's miss it adds nothing to lsof's account
// (procSecondOpinion).
func pidListensOnPort(int, int) (bool, ownerSighting, error) {
	return false, ownerSighting{saw: "nothing else here matches a process to a port"}, nil
}

// blindSpot is what lsof cannot see off Linux, for the account of a port it
// did not attribute (ownerSighting.blind): run by anyone but root, macOS's
// lsof lists only that user's processes, since the kernel gives a process's
// descriptors to its own user and to root. Measured 2026-09-26 on macOS 27:
// `lsof -iTCP -sTCP:LISTEN` run as a user leaves out the listeners of root's
// launchd and kdc, which `netstat -anv` shows. Windows compiles this and
// never calls it: its probe has no blind spot (listenerTableSighting).
func blindSpot() string {
	uid := os.Geteuid()
	if uid == 0 {
		return ""
	}
	return fmt.Sprintf("lsof run as uid %d rather than root sees only that user's processes", uid)
}
