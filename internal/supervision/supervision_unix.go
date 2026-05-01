//go:build !windows

package supervision

import "os"

// isSupervisedForOS checks the macOS / Linux supervisor environment
// variables. Either set means we're running under a supervisor that
// will relaunch us after os.Exit (launchd KeepAlive / systemd
// Restart=always, both of which the bridge's `init` command writes
// into the unit files it ships). Neither set → unsupervised; the
// admin UI must not promise auto-relaunch.
//
// Tested via `supervision_unix_test.go` which sets / unsets the
// vars in-process — no subprocess fork required.
func isSupervisedForOS() bool {
	// macOS launchd sets XPC_SERVICE_NAME for plist-installed jobs.
	// Present for both daemons (`/Library/LaunchDaemons`) and user
	// agents (`~/Library/LaunchAgents`).
	if os.Getenv("XPC_SERVICE_NAME") != "" {
		return true
	}
	// systemd sets INVOCATION_ID for services it spawns. The
	// documented signal — companion `JOURNAL_STREAM` is set under
	// the same condition but `INVOCATION_ID` is the canonical
	// "systemd service" probe per `systemd.exec(5)`.
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	return false
}
