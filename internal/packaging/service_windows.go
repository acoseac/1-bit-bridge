//go:build windows

package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// InstallWindowsService registers the bridge as a Windows Service via
// SCM. The service runs as LocalSystem by default (which means it
// can't access %USERPROFILE% music paths — callers that want
// user-specific library roots should stick with the Startup-folder
// install from PR-1).
//
// Returns the installed service name (always ServiceLabel) on success.
// Requires the calling process to be running elevated (UAC admin) —
// non-admin callers get a "access is denied" error from the SCM.
func InstallWindowsService(p Params) (string, error) {
	// Connect to the local SCM. The connection is read/write; on a
	// non-elevated process this returns "Access is denied" which we
	// surface to init's caller verbatim (clear enough hint).
	m, err := mgr.Connect()
	if err != nil {
		return "", fmt.Errorf("connect SCM (need admin?): %w", err)
	}
	defer m.Disconnect()

	// If an existing service with this name is registered, stop and
	// delete it first so install is idempotent. `bridge init --service`
	// re-runs should upgrade the binary path without requiring a
	// separate uninstall step.
	//
	// Windows' Delete is deferred until every handle to the service
	// is closed AND the service is in the STOPPED state. Skipping the
	// Stop step leaves a running service in "marked for delete" limbo:
	// CreateService below fails with ERROR_SERVICE_MARKED_FOR_DELETE
	// until the service finally stops (which it won't, because nothing
	// asked it to). The Control(svc.Stop) + poll-for-stopped fixes it.
	if s, err := m.OpenService(ServiceLabel); err == nil {
		_, _ = s.Control(svc.Stop)
		if werr := waitForServiceStopped(s, 10*time.Second); werr != nil {
			// Can't proceed to Delete without a clean stop — the
			// service would stay in "marked for delete" limbo until
			// reboot, and CreateService below would fail with
			// ERROR_SERVICE_MARKED_FOR_DELETE. Better to surface
			// the actual failure than silently fall through into
			// that failure mode.
			s.Close()
			return "", fmt.Errorf("stop existing service %q: %w", ServiceLabel, werr)
		}
		_ = s.Delete()
		s.Close()
		waitForServiceGone(m, ServiceLabel, 3*time.Second)
	}

	// Resolve the data dir for the service's log file. Co-located with
	// %PROGRAMDATA%\1-bit-bridge\ to match the service's
	// redirectServiceIO target in cmd/bridge/service_io_windows.go.
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	dataDir := filepath.Join(programData, "1-bit-bridge")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("ensure ProgramData dir: %w", err)
	}

	config := mgr.Config{
		DisplayName:      "1-bit-bridge",
		Description:      "Companion server for the 1-bit iOS music player.",
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DelayedAutoStart: true, // boot-time non-essential — don't block login
	}

	// The SCM expects the binary arg list as a single string; the Go
	// wrapper takes additional args as a slice and joins with spaces.
	// Our ImagePath is `bridge.exe serve --config <cfg>`.
	s, err := m.CreateService(ServiceLabel, p.BinaryPath, config,
		"serve", "--config", p.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Start the service immediately so the operator doesn't have to
	// reboot to see the bridge come up. Failure here is non-fatal —
	// the service is installed, it'll start on next boot.
	if err := s.Start(); err != nil {
		return ServiceLabel, fmt.Errorf("service installed but failed to start: %w", err)
	}
	return ServiceLabel, nil
}

// UninstallWindowsService stops and removes the SCM service. Missing
// service is not an error — idempotent uninstall.
func UninstallWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceLabel)
	if err != nil {
		// Missing = already uninstalled.
		return nil
	}
	defer s.Close()
	// Stop first so the Delete call doesn't leave a zombie process.
	// Ignore stop errors — the service might already be stopped, or
	// be in a stopping state.
	//
	// Note: svc.Stop is the Go-side constant for SERVICE_CONTROL_STOP
	// (= 1). The earlier `s.Control(6)` here sent SERVICE_CONTROL_
	// PARAMCHANGE instead, which is a no-op for a service that
	// doesn't handle it — Delete then deferred, uninstall effectively
	// silently no-oped until the next reboot.
	_, _ = s.Control(svc.Stop)
	if werr := waitForServiceStopped(s, 10*time.Second); werr != nil {
		// Same rationale as install: Delete waits for Stopped, so a
		// failed-to-stop service will reproduce the marked-for-delete
		// bug. Bubble the error so the operator sees why and can
		// intervene (kill the process, reboot) rather than the
		// uninstall silently completing + the service coming back.
		return fmt.Errorf("stop service %q: %w", ServiceLabel, werr)
	}
	return s.Delete()
}

// waitForServiceGone polls the SCM until OpenService returns "service
// does not exist", or the timeout expires. Protects Install from
// racing the SCM's async Delete.
func waitForServiceGone(m *mgr.Mgr, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := m.OpenService(name)
		if err != nil {
			return
		}
		s.Close()
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForServiceStopped polls the service's Query() state until it
// reports svc.Stopped or the timeout expires. SCM only finalises a
// Delete once the service is actually stopped, so we block here so
// the caller can proceed with its next step (either recreate in the
// install path, or return to the user in the uninstall path).
//
// Returns nil on clean stop. A Query error is returned as-is (the
// service may have vanished — caller's choice whether that's an OK
// outcome). A deadline exceed is surfaced as a wrapped error; the
// prior behaviour of returning silently on timeout was what caused
// install/uninstall to fall through into Delete on a still-running
// service, reproducing the ERROR_SERVICE_MARKED_FOR_DELETE case PR #30
// set out to fix.
func waitForServiceStopped(s *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service status: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for service to stop after %s", timeout)
}
