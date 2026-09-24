//go:build !windows

package tailscale

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// stopTreeOnCancel makes a cancelled context stop every process a CLI
// call started, not only the one it exec'd.
//
// exec.CommandContext's default Cancel kills the direct child, and the
// direct child is not always the CLI. The standalone macOS app's
// "Install CLI" helper puts a two-line shell script at
// /usr/local/bin/tailscale that runs the app binary WITHOUT exec, so the
// kill lands on /bin/sh while the app, the process that writes the cert
// files, carries on as an orphan holding the output pipes. Wait then
// blocks until that orphan exits, and whatever it writes lands after the
// caller was told the call was cancelled. That is how `bridge serve`
// kept writing <dataDir>/tls after it had returned.
//
// Setpgid makes the child the leader of a new process group, which its
// descendants inherit, and Cancel signals the whole group. Measured
// against that wrapper, cancelling `status --json` 2 to 20 ms in: the
// app went on to produce output after the kill in 36 of 42 runs without
// the group kill, and in 1 of 42 with it.
//
// A group kill can miss a child the wrapper is forking at that instant.
// Linux's copy_process restarts a fork a group signal lands in; on
// macOS, a stand-in whose inner script writes a marker a second later
// escaped in 45 of 4,830 cancels spread over 0 to 4 ms, in three runs.
// The caller's wait covers that case:
// the escaped child inherits the output pipes, so Wait returns only
// once it has exited (8 of 8 escapes, measured with pipes attached),
// and a caller that joins the goroutine making the call gets the write
// before it returns, never after. That is why Cmd.WaitDelay is NOT set
// here. It would unblock Wait by closing those pipes, abandoning
// exactly the process the wait is there to outlast.
//
// The group is this call's alone, so the kill reaches nothing else. The
// cost is that a terminal's Ctrl-C no longer reaches the CLI directly;
// it reaches the bridge, whose context cancel then kills the group.
func stopTreeOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// Ask os.Process first. exec can run Cancel after Wait has reaped
		// the leader, and from then on its pid, and with it the group id,
		// may belong to another process. os.Process orders its own
		// signals against the reap and answers ErrProcessDone past it,
		// which a bare kill(-pid) cannot.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return err
		}
		// Setpgid made the group id the child's pid.
		err := killGroup(cmd.Process.Pid)
		if errors.Is(err, syscall.ESRCH) {
			// Nothing is left in the group: the call finished before the
			// cancel reached it. exec reads ErrProcessDone as exactly
			// that and reports the command's own exit status.
			return os.ErrProcessDone
		}
		return err
	}
}

// killGroup sends SIGKILL to every process in group pgid. A variable only
// so a test can see whether Cancel reached it; production never reassigns
// it, the same convention as commandContext.
var killGroup = func(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
