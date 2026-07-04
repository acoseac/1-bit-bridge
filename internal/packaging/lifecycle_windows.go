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

// openBridgeServiceForSCM is the shared connect-SCM + open-service
// preamble for every lifecycle action (stop / restart / start). Returns
// `(manager, service, isMissing, err)`:
//   - On success, the caller defers `m.Disconnect()` AND `s.Close()` and
//     proceeds with the lifecycle command (`Stop` / `Start`).
//   - `isMissing == true` is the idempotent "service not installed" case;
//     callers return nil immediately (Stop/Restart short-circuit, Start
//     reports nothing-to-start). manager + service are nil.
//   - Any other error is wrapped via the shared `scmConnectAdminErr` /
//     `scmConnectErr` / `scmOpenSvcAdminErr` / `scmOpenSvcErr` formats so
//     access-denied bubbles up with the operator-readable "(need admin?)"
//     hint. manager + service are nil.
//
// Extracted to eliminate the three-way duplicate of this preamble that
// stopForOS / restartForOS / startForOS each carried (CodeRabbit / Qodo
// would have flagged this as code-smell; SonarCloud's per-PR duplication
// gate did).
func openBridgeServiceForSCM() (*mgr.Mgr, *mgr.Service, bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, false, fmt.Errorf(scmConnectAdminErr, err)
		}
		return nil, nil, false, fmt.Errorf(scmConnectErr, err)
	}
	s, err := m.OpenService(ServiceLabel)
	if err != nil {
		m.Disconnect()
		if isServiceMissing(err) {
			return nil, nil, true, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, false, fmt.Errorf(scmOpenSvcAdminErr, err)
		}
		return nil, nil, false, fmt.Errorf(scmOpenSvcErr, err)
	}
	return m, s, false, nil
}

// errStartupFolderCLI builds the "can't <verb> a Startup-folder install
// via the SCM" error the three lifecycle ops return for
// KindWindowsStartup. A Startup-folder install is a plain user process
// launched from the Startup folder, not an SCM-managed service, so the
// CLI has no service handle to signal — a clear error beats the silent
// no-op the pre-fix SCM-missing path produced.
func errStartupFolderCLI(verb, hint string) error {
	return fmt.Errorf("cannot %s a Startup-folder install from the CLI; %s", verb, hint)
}

// stopForOS asks the SCM to stop the bridge service. A KindWindowsStartup
// install has no SCM service, so it returns a clear error rather than the
// silent no-op the SCM-missing path would otherwise produce. Wraps
// access-denied so the caller's friendly "Re-launch as Administrator"
// path can classify it via errors.Is(err, windows.ERROR_ACCESS_DENIED).
func stopForOS(kind ServiceKind) error {
	if kind == KindWindowsStartup {
		return errStartupFolderCLI("stop", "end the bridge.exe process in Task Manager")
	}
	m, s, missing, err := openBridgeServiceForSCM()
	if err != nil {
		return err
	}
	if missing {
		// Truly idempotent no-op.
		return nil
	}
	defer m.Disconnect()
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
// a misleading start-error masking a real stop fault). A
// KindWindowsStartup install has no SCM service to restart — return a
// clear error instead of the silent no-op the SCM-missing path produced.
func restartForOS(kind ServiceKind) error {
	if kind == KindWindowsStartup {
		return errStartupFolderCLI("restart", "end the bridge.exe process in Task Manager, then run `bridge serve`")
	}
	m, s, missing, err := openBridgeServiceForSCM()
	if err != nil {
		return err
	}
	if missing {
		// Nothing to restart — caller's contract via Restart's
		// KindNone short-circuit means we shouldn't reach here,
		// but the SCM probe could race a parallel Uninstall.
		return nil
	}
	defer m.Disconnect()
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

// startForOS asks the SCM to start the installed bridge service.
// Idempotent against an already-running service (returns nil if
// `s.Start()` reports ERROR_SERVICE_ALREADY_RUNNING). Same admin
// gate semantics as stopForOS / restartForOS. A KindWindowsStartup
// install has no SCM service — return a clear error rather than the
// silent no-op the SCM-missing path produced.
func startForOS(kind ServiceKind) error {
	if kind == KindWindowsStartup {
		return errStartupFolderCLI("start", "run `bridge serve` (the Startup entry launches automatically on next sign-in)")
	}
	m, s, missing, err := openBridgeServiceForSCM()
	if err != nil {
		return err
	}
	if missing {
		return nil
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}
