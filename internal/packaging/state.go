package packaging

import (
	"os"
	"path/filepath"
)

// ServiceKind identifies which service-manager artifact is installed
// for the bridge on the current host. The user/system split matters
// because the install paths differ — a user agent lives under the
// user's home dir and runs at logon, a system daemon lives under
// shared OS dirs and runs at boot under root / LocalSystem.
type ServiceKind int

const (
	// KindNone — no service-manager artifact found.
	KindNone ServiceKind = iota
	// KindLaunchdUser — ~/Library/LaunchAgents/<label>.plist.
	// The supported macOS install path; runs at logon as the user.
	KindLaunchdUser
	// KindLaunchdSystem — /Library/LaunchDaemons/<label>.plist.
	// Rare and only present from a sudo install, which the menu
	// warns against because the bridge config dir lives under
	// the user's home and won't resolve correctly under root.
	KindLaunchdSystem
	// KindSystemdUser — ~/.config/systemd/user/<label>.service.
	// The supported Linux install path.
	KindSystemdUser
	// KindSystemdSystem — /etc/systemd/system/<label>.service.
	// Rare and operator-installed; same caveat as KindLaunchdSystem.
	KindSystemdSystem
	// KindWindowsSCM — registered with Windows SCM.
	// Requires admin to install/uninstall; survives logout.
	KindWindowsSCM
	// KindWindowsStartup — Startup-folder .cmd launcher.
	// User-context install; runs at logon while user is logged in.
	KindWindowsStartup
)

// Description returns a short, human-readable label for the service
// kind, suitable for a CLI status line. Distinct strings per kind
// so a UI never collapses two install kinds into the same display.
func (k ServiceKind) Description() string {
	switch k {
	case KindLaunchdUser:
		return "auto-start (macOS LaunchAgent)"
	case KindLaunchdSystem:
		return "background service (macOS LaunchDaemon)"
	case KindSystemdUser:
		return "auto-start (Linux systemd --user)"
	case KindSystemdSystem:
		return "background service (Linux systemd system)"
	case KindWindowsSCM:
		return "background service (SCM)"
	case KindWindowsStartup:
		return "auto-start (Startup folder)"
	default:
		return "not installed"
	}
}

// InstalledKind reports which service-manager artifact (if any) is
// installed on the current host. Best-effort: file-existence probes
// on darwin/linux, SCM-then-Startup probe on windows. Returns
// KindNone with nil error when nothing is found. Errors are surfaced
// only when a probe failed in a way that prevents detection (e.g.
// $HOME unresolvable) — a genuine "no service" state is not an error.
//
// On Windows, SCM is probed first; if SCM access is denied (no admin)
// the Startup-folder file probe still runs. Both can coexist on the
// same machine across reinstalls; SCM wins precedence in this report.
func InstalledKind() (ServiceKind, error) {
	return installedKindForOS()
}

// IsInstalled is a convenience wrapper around InstalledKind. Returns
// true when any non-None kind is detected; swallows the error so
// callers that only need the boolean can stay terse.
func IsInstalled() bool {
	k, _ := InstalledKind()
	return k != KindNone
}

// IsInitialized reports whether bridge.yaml exists at the platform-default
// config path. Returns the resolved config-file path either way so the
// caller can re-use it (e.g. as the --config arg for a manual launch).
func IsInitialized() (cfgPath string, ok bool) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", false
	}
	cfgPath = filepath.Join(dir, "bridge.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		return cfgPath, false
	}
	return cfgPath, true
}
