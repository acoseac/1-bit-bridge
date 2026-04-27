//go:build !windows

package packaging

import (
	"os"
	"path/filepath"
	"runtime"
)

// installedKindForOS probes filesystem locations under the current
// user's home for a launchd plist (darwin) or systemd unit (linux).
// User-level paths are checked first because they're the supported
// install location. The system-level path is checked only as a
// fallback for accidental sudo installs — the menu surfaces these
// kinds so the operator notices and can move the install back to
// user context.
func installedKindForOS() (ServiceKind, error) {
	switch runtime.GOOS {
	case "darwin":
		if userPath, err := launchdPlistPath(); err == nil {
			if _, statErr := os.Stat(userPath); statErr == nil {
				return KindLaunchdUser, nil
			}
		}
		// /Library/LaunchDaemons is OS-managed; readable by any
		// process. A stat failure here just means "not present".
		systemPath := filepath.Join("/Library", "LaunchDaemons", ServiceLabel+".plist")
		if _, err := os.Stat(systemPath); err == nil {
			return KindLaunchdSystem, nil
		}
	case "linux":
		if userPath, err := systemdUnitPath(); err == nil {
			if _, statErr := os.Stat(userPath); statErr == nil {
				return KindSystemdUser, nil
			}
		}
		systemPath := filepath.Join("/etc", "systemd", "system", ServiceLabel+".service")
		if _, err := os.Stat(systemPath); err == nil {
			return KindSystemdSystem, nil
		}
	}
	return KindNone, nil
}

// IsAdmin is the Windows-only elevation probe; on POSIX it returns
// true so callers that gate UX on admin rights don't need build-tagged
// dispatch — the gate is irrelevant when the relevant install paths
// are user-context (launchd user agent, systemd --user).
func IsAdmin() bool { return true }

// IsRoot reports whether the current process is running with euid 0.
// Used to warn against `sudo bridge` on POSIX, where running as root
// resolves $HOME to /root and silently breaks the bridge config dir
// resolution at next user-context launch.
func IsRoot() bool { return os.Geteuid() == 0 }
