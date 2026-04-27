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

// isServiceMissing classifies an OpenService error as the canonical
// "service does not exist" case — a true idempotent no-op for our
// stop/restart paths. Any other error (access-denied, RPC fault,
// SCM-marked-for-delete) needs to bubble up so the caller can
// distinguish a missing service from a misbehaving one.
func isServiceMissing(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
}

// stopForOS asks the SCM to stop the bridge service. The kind
// argument is unused on Windows (Stop's dispatcher only forwards
// KindWindowsSCM here; Startup-folder installs are user processes,
// not SCM services, so they're ignored). Wraps access-denied so the
// caller's friendly "Re-launch as Administrator" path can classify
// it via errors.Is(err, windows.ERROR_ACCESS_DENIED).
func stopForOS(_ ServiceKind) error {
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
		if isServiceMissing(err) {
			// Truly idempotent no-op.
			return nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("open service (need admin?): %w", err)
		}
		// Real SCM fault — RPC down, service marked-for-delete, etc.
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		// Stop on an already-stopped service returns
		// ERROR_SERVICE_NOT_ACTIVE (or sometimes _CANNOT_ACCEPT_CTRL).
		// Query the actual state — if it's Stopped or stopping, we're
		// done; otherwise the error is a real fault.
		st, qerr := s.Query()
		if qerr == nil && (st.State == svc.Stopped || st.State == svc.StopPending) {
			if st.State == svc.StopPending {
				return waitForServiceStopped(s, 10*time.Second)
			}
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
// Same access-denied wrapping as stopForOS. Stop failures are
// classified the same way: already-stopped is acceptable, anything
// else surfaces before we attempt Start (so the user doesn't see
// a misleading start-error masking a real stop fault).
func restartForOS(_ ServiceKind) error {
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
		if isServiceMissing(err) {
			// Nothing to restart — caller's contract via Restart's
			// KindNone short-circuit means we shouldn't reach here,
			// but the SCM probe could race a parallel Uninstall.
			return nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("open service (need admin?): %w", err)
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		// Same classification as stopForOS — already stopped is OK,
		// stop-pending we wait through, anything else bubbles up.
		st, qerr := s.Query()
		if qerr != nil || (st.State != svc.Stopped && st.State != svc.StopPending) {
			return fmt.Errorf("stop service: %w", err)
		}
		if st.State == svc.StopPending {
			if werr := waitForServiceStopped(s, 10*time.Second); werr != nil {
				return fmt.Errorf("wait stop: %w", werr)
			}
		}
	} else {
		if werr := waitForServiceStopped(s, 10*time.Second); werr != nil {
			return fmt.Errorf("wait stop: %w", werr)
		}
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}
