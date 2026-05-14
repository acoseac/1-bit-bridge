//go:build !windows

package packaging

import (
	"bytes"
	"fmt"
	"os/exec"
)

// runSystemctlUser runs `systemctl --user <verb> <ServiceLabel>.service`
// and wraps a non-zero exit with the combined output. Shared by
// stopForOS / startForOS / restartForOS so the verb-specific
// boilerplate doesn't duplicate three ways (SonarCloud per-PR
// duplication gate caught this).
func runSystemctlUser(verb string) error {
	out, err := exec.Command("systemctl", systemdUserFlag, verb, ServiceLabel+systemdUnitSuffix).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", verb, err, string(out))
	}
	return nil
}

// stopForOS shells out to the user's service manager. The kind
// argument carries the InstalledKind classification from Stop's
// dispatcher — only user-context kinds reach here (system kinds
// surface as ErrSystemInstallNeedsRoot upstream). We mirror the
// install path's command shape — `launchctl bootout gui/<uid>` on
// darwin, `systemctl --user stop` on linux. Errors from either
// command are wrapped with the combined output so the operator sees
// what the manager actually said.
func stopForOS(kind ServiceKind) error {
	switch kind {
	case KindLaunchdUser:
		path, err := launchdPlistPath()
		if err != nil {
			return fmt.Errorf(plistPathErrFormat, err)
		}
		// `bootout` returns non-zero with a "Could not find specified
		// service" message when the agent isn't loaded — that's the
		// no-op case we want and surfacing it as an error confuses
		// callers. Capture combined output and only swallow when the
		// stderr matches the not-loaded sentinel; any other failure
		// returns a wrapped error.
		out, err := exec.Command("launchctl", "bootout", "gui/"+uidString(), path).CombinedOutput()
		if err != nil && !bytes.Contains(out, []byte("Could not find")) && !bytes.Contains(out, []byte("not currently loaded")) {
			return fmt.Errorf("launchctl bootout: %v: %s", err, string(out))
		}
		return nil
	case KindSystemdUser:
		return runSystemctlUser("stop")
	}
	return nil
}

// startForOS asks the service manager to boot the installed bridge.
// `launchctl bootstrap` on darwin (idempotent — a not-already-loaded
// agent loads, an already-loaded one returns "service already
// loaded" which we swallow). `systemctl --user start` on linux,
// idempotent against an already-running unit.
func startForOS(kind ServiceKind) error {
	switch kind {
	case KindLaunchdUser:
		path, err := launchdPlistPath()
		if err != nil {
			return fmt.Errorf(plistPathErrFormat, err)
		}
		out, err := exec.Command("launchctl", "bootstrap", "gui/"+uidString(), path).CombinedOutput()
		if err != nil && !bytes.Contains(out, []byte("service already loaded")) && !bytes.Contains(out, []byte("Bootstrap failed: 17: File exists")) {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, string(out))
		}
		return nil
	case KindSystemdUser:
		return runSystemctlUser("start")
	}
	return nil
}

// restartForOS bootstraps a freshly-stopped agent (darwin) or asks
// systemd to bounce the unit (linux). Only handles user-context kinds;
// system kinds short-circuit upstream in Restart.
func restartForOS(kind ServiceKind) error {
	switch kind {
	case KindLaunchdUser:
		path, err := launchdPlistPath()
		if err != nil {
			return fmt.Errorf(plistPathErrFormat, err)
		}
		// bootout-then-bootstrap. bootout's not-loaded case is fine
		// (same swallow pattern as stopForOS); the real test is the
		// bootstrap step succeeding — if the agent failed to stop
		// for a real reason, bootstrap will fail too with a clear
		// "service already loaded" message.
		out, err := exec.Command("launchctl", "bootout", "gui/"+uidString(), path).CombinedOutput()
		if err != nil && !bytes.Contains(out, []byte("Could not find")) && !bytes.Contains(out, []byte("not currently loaded")) {
			return fmt.Errorf("launchctl bootout: %v: %s", err, string(out))
		}
		out, err = exec.Command("launchctl", "bootstrap", "gui/"+uidString(), path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, string(out))
		}
		return nil
	case KindSystemdUser:
		return runSystemctlUser("restart")
	}
	return nil
}
