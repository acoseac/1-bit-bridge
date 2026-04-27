package packaging

// Stop asks the service manager to stop the running bridge service
// (launchd / systemd / SCM) but leaves the install in place. A
// follow-up Start (via the OS itself or via Restart below) brings
// it back. No-op when nothing is installed — returns nil so menu
// callers can call this without a prior IsInstalled() check.
//
// On Windows, calling Stop without admin returns the platform's
// access-denied error wrapped — the menu surfaces this as a
// "Re-launch as Administrator" hint rather than a stack trace.
func Stop() error {
	return stopForOS()
}

// Restart stops then re-starts the installed service. Implemented
// per platform because the launchd/systemd/SCM APIs differ — the
// dispatcher here just keeps the public surface tidy. Returns nil
// when nothing is installed.
func Restart() error {
	return restartForOS()
}
