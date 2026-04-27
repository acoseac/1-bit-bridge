//go:build !windows

package packaging

import (
	"fmt"
	"os/exec"
	"runtime"
)

// stopForOS shells out to the user's service manager. We mirror the
// install path's command shape — `launchctl bootout gui/<uid>` on
// darwin, `systemctl --user stop` on linux. Errors from either
// command are wrapped with the combined output so the operator sees
// what the manager actually said (the launchctl/systemctl messages
// are usually self-explanatory).
func stopForOS() error {
	switch runtime.GOOS {
	case "darwin":
		path, err := launchdPlistPath()
		if err != nil {
			return fmt.Errorf("resolve plist path: %w", err)
		}
		// `bootout` returns non-zero when the agent isn't loaded —
		// that's the no-op case we want, so a non-zero exit isn't
		// fatal. Surface only when CombinedOutput's error suggests
		// a real failure (the agent IS loaded but couldn't be
		// stopped). We can't distinguish the two without parsing
		// stderr, so swallow + return nil — Restart will catch any
		// real "still running" state on the bootstrap step.
		_ = exec.Command("launchctl", "bootout", "gui/"+uidString(), path).Run()
		return nil
	case "linux":
		out, err := exec.Command("systemctl", "--user", "stop", ServiceLabel+".service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user stop: %v: %s", err, string(out))
		}
		return nil
	}
	return nil
}

// restartForOS bootstraps a freshly-stopped agent (darwin) or asks
// systemd to bounce the unit (linux). On platforms without a
// supported install path, returns nil.
func restartForOS() error {
	switch runtime.GOOS {
	case "darwin":
		path, err := launchdPlistPath()
		if err != nil {
			return fmt.Errorf("resolve plist path: %w", err)
		}
		_ = exec.Command("launchctl", "bootout", "gui/"+uidString(), path).Run()
		out, err := exec.Command("launchctl", "bootstrap", "gui/"+uidString(), path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, string(out))
		}
		return nil
	case "linux":
		out, err := exec.Command("systemctl", "--user", "restart", ServiceLabel+".service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user restart: %v: %s", err, string(out))
		}
		return nil
	}
	return nil
}
