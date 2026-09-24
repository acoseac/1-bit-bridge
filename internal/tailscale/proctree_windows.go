//go:build windows

package tailscale

import "os/exec"

// stopTreeOnCancel leaves CommandContext's default Cancel in place on
// Windows. There are no process groups to signal, and the path
// resolveBinary finds there, tailscale.exe under Program Files, is the
// CLI itself rather than a script that starts it, so killing the direct
// child already stops the process that writes the cert files. A
// wrapper there (a .cmd on PATH) would need a job object; nothing ships
// one.
func stopTreeOnCancel(*exec.Cmd) {
	// Deliberately empty: CommandContext's default kill already reaches
	// the writer here, for the reason above.
}
