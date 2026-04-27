package packaging

import "errors"

// ErrSystemInstallNeedsRoot is returned by Stop / Restart when the
// detected install is a system-level launchd LaunchDaemon or systemd
// system unit and the current process can't drive it. The menu
// surfaces this as a friendly "Re-launch as root or convert to a
// user-context install" hint rather than calling user-domain
// commands that would silently no-op against the wrong namespace.
var ErrSystemInstallNeedsRoot = errors.New("system-level install detected; re-run as root or convert to a user-context install")

// Stop asks the service manager to stop the running bridge service
// (launchd / systemd / SCM) but leaves the install in place. A
// follow-up Start (via the OS itself or via Restart below) brings
// it back. No-op when nothing is installed — returns nil so menu
// callers can call this without a prior IsInstalled() check.
//
// On Windows, calling Stop without admin returns the platform's
// access-denied error wrapped — the menu surfaces this as a
// "Re-launch as Administrator" hint rather than a stack trace.
//
// On POSIX, calling Stop against a system-level install returns
// ErrSystemInstallNeedsRoot — see the comment on that sentinel.
func Stop() error {
	kind, _ := InstalledKind()
	if kind == KindNone {
		return nil
	}
	if kind == KindLaunchdSystem || kind == KindSystemdSystem {
		return ErrSystemInstallNeedsRoot
	}
	return stopForOS(kind)
}

// Restart stops then re-starts the installed service. Implemented
// per platform because the launchd/systemd/SCM APIs differ — the
// dispatcher here just keeps the public surface tidy. Returns nil
// when nothing is installed; same system-install gate as Stop.
func Restart() error {
	kind, _ := InstalledKind()
	if kind == KindNone {
		return nil
	}
	if kind == KindLaunchdSystem || kind == KindSystemdSystem {
		return ErrSystemInstallNeedsRoot
	}
	return restartForOS(kind)
}
