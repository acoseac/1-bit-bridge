//go:build darwin

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// pidListensOnPort is macOS's second look at the recorded pid
// (procOwnerFunc), asked after lsof, asked who listens on port, ran cleanly
// without naming pid: lsof asked about pid itself, for every TCP listener it
// holds, which ownListenersSighting reads. It is what /proc's read of the
// pid's own descriptors is on Linux (portowner_linux.go), and
// procSecondOpinion merges the two the same way.
//
// Without it nothing on macOS ruled a pid out, since lsof's first answer
// cannot exclude a process it may not see (lsofSighting). A bridge still
// running on the ports of the config it started with, whose config was then
// edited to a port another process holds, warned, `bridge doctor --config`
// (the runbook's check before a restart) exited 0, and the restart could not
// bind: #1029's row ML4, whose holder ran as this user, and the same with a
// holder of root's. lsof lists such a bridge's own listeners, on its old
// ports, and none on the new one.
//
// Two other readings were measured on macOS 27 and not taken. The pid's uid
// (kern.proc.pid): lsof run as uid 501 read every one of the 614 processes
// whose effective uid was 501, the app-sandboxed and hardened-runtime ones
// among them, and none of the 211 others, so a clean miss about a pid of
// lsof's own uid could rule it out. But an lsof that a sandbox denies
// process info reads no process and exits 1 all the same, silently, and that
// rule would FAIL a sandboxed doctor's own bridge on its own port. And
// `netstat -anv`, which names every listener's pid from a shell: run by a Go
// process, started from a shell or by launchd, it printed no TCP socket.
//
// Where no lsof resolved there is nothing to ask, since lsof is the reader
// here; macOS ships it in its base system, so a Mac reaches that only with
// lsof removed. A run that fails is an error, which leaves lsof's first
// account as it was (procSecondOpinion). It is bounded by probeTimeout
// around ctx, as the first run is: lsof stat()s mount points before it looks
// at any process, and a wedged network mount would hold it.
func pidListensOnPort(ctx context.Context, port, pid int) (bool, ownerSighting, error) {
	if lsofPath == "" {
		return false, ownerSighting{saw: nothingElseMatches}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := lsofCommand(probeCtx, lsofPath, "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-F", "n").Output()
	if err != nil {
		// Exit 1 is lsof's "matched nothing", unless the run was cut short:
		// a killed lsof can exit with any status (isPIDListeningOnPort).
		var exitErr *exec.ExitError
		if probeCtx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return false, ownerSighting{}, fmt.Errorf("lsof listing of pid %d's listeners: %w", pid, err)
		}
		out = nil
	}
	found, seen := ownListenersSighting(out, port, pid)
	return found, seen, nil
}
