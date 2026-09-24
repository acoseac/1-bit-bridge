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
// the group kill, and in 1 of 42 with it. That one fits a child forked
// while the signal was being delivered, which a group kill can miss.
// The caller's wait covers it: the escaped process holds the output
// pipes, so Wait returns only once it has exited. That is also why
// Cmd.WaitDelay is NOT set here. It would unblock Wait by closing those
// pipes, abandoning exactly the process the wait is there to outlast.
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
		// Cancel runs only after a successful Start, so Process is set,
		// and with Setpgid the group id is the child's pid.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.ESRCH):
			// Nothing is left in the group: the call finished before the
			// cancel reached it. exec reads ErrProcessDone as exactly
			// that and reports the command's own exit status.
			return os.ErrProcessDone
		default:
			// The group could not be signalled. Kill the leader, which is
			// all CommandContext did before, so a failed group kill never
			// leaves the CLI itself running.
			return cmd.Process.Kill()
		}
	}
}
