//go:build windows

package packaging

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// stopForOS asks the SCM to stop the bridge service. Returns nil
// when no service is registered (idempotent). Wraps access-denied
// so the caller's friendly "Re-launch as Administrator" path can
// classify it via errors.Is(err, windows.ERROR_ACCESS_DENIED).
//
// Startup-folder installs are not "running services" from SCM's
// view — they are user processes — so this function ignores them.
// Stopping a Startup-folder bridge is the operator quitting the
// foreground process, not a packaging concern.
func stopForOS() error {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("connect SCM (need admin?): %w", err)
		}
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceLabel)
	if err != nil {
		// Service not registered — nothing to stop.
		return nil
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		// Already stopped is acceptable.
		st, qerr := s.Query()
		if qerr == nil && st.State == svc.Stopped {
			return nil
		}
		return fmt.Errorf("stop service: %w", err)
	}
	return waitForServiceStopped(s, 10*time.Second)
}

// restartForOS stops then starts the SCM service. Implemented as a
// pair of sync calls (rather than a single SCM Restart command)
// because Windows SCM doesn't expose a one-shot restart — every
// Windows service tool synthesises it as Stop+poll-stopped+Start.
// Same access-denied wrapping as stopForOS so the caller can route
// to the elevation hint.
func restartForOS() error {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("connect SCM (need admin?): %w", err)
		}
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceLabel)
	if err != nil {
		// Nothing to restart.
		return nil
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err == nil {
		if werr := waitForServiceStopped(s, 10*time.Second); werr != nil {
			return fmt.Errorf("wait stop: %w", werr)
		}
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}
