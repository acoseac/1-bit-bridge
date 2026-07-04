//go:build windows

package packaging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
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
		return "", fmt.Errorf(scmConnectAdminErr, err)
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
		sendStopIfRunning(s)
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
		// Wait (generously) for the SCM to finalize the async delete. A
		// timeout means the delete is genuinely stuck (a handle held open
		// elsewhere) — surface it rather than silently proceeding into a
		// CreateService that would fail with MARKED_FOR_DELETE. The
		// normal sub-second residual after "gone" is absorbed by
		// createServiceWithRetry below.
		if werr := waitForServiceGone(m, ServiceLabel, 5*time.Second); werr != nil {
			return "", werr
		}
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
	// Our ImagePath is `bridge.exe serve --config <cfg>`. Retry-wrapped
	// to absorb a residual MARKED_FOR_DELETE from the just-deleted prior
	// service (the SCM finalizes deletes asynchronously).
	s, err := createServiceWithRetry(m, config, p.BinaryPath,
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
		return fmt.Errorf(scmConnectErr, err)
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
	sendStopIfRunning(s)
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
// does not exist", or the timeout expires. Protects Install from racing
// the SCM's async Delete. Returns an error on timeout so the caller can
// surface a genuinely-stuck delete (e.g. a monitoring tool holding a
// handle) instead of silently proceeding into a CreateService that would
// fail with ERROR_SERVICE_MARKED_FOR_DELETE.
func waitForServiceGone(m *mgr.Mgr, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := m.OpenService(name)
		if err != nil {
			return nil
		}
		s.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("service %q still present after %s (SCM delete still pending)", name, timeout)
}

// createServiceWithRetry creates the bridge service, retrying briefly on
// ERROR_SERVICE_MARKED_FOR_DELETE. Even after waitForServiceGone reports
// the prior service gone, the SCM finalizes the Delete asynchronously on
// its own thread, so CreateService can transiently hit MARKED_FOR_DELETE
// for a few hundred ms. A bounded retry with a 100ms yield absorbs that
// window — a tight no-yield loop would burn every attempt inside a single
// scheduling quantum before the handle clears. Any other error returns
// immediately (no point spinning on a real fault).
func createServiceWithRetry(m *mgr.Mgr, cfg mgr.Config, binary string, args ...string) (*mgr.Service, error) {
	const maxAttempts = 20 // ~2s at 100ms
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		s, err := m.CreateService(ServiceLabel, binary, cfg, args...)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return nil, err
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("still marked for delete after %d attempts: %w", maxAttempts, lastErr)
}

// waitForServiceStopped polls the service's Query() state until it
// reports svc.Stopped or the timeout expires. SCM only finalises a
// Delete once the service is actually stopped, so we block here so
// the caller can proceed with its next step (either recreate in the
// install path, or return to the user in the uninstall path).
//
// Returns nil on clean stop. A Query error is wrapped with context
// and returned (`query service status: %w`) — callers use
// `errors.Is` / `errors.As` against the inner error to probe for
// "the service vanished mid-poll" vs. a real SCM fault. A deadline
// exceeded is surfaced as a wrapped error; the prior behaviour of
// returning silently on timeout was what caused install/uninstall
// to fall through into Delete on a still-running service,
// reproducing the ERROR_SERVICE_MARKED_FOR_DELETE case PR #30 set
// out to fix.
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

// sendStopIfRunning sends SERVICE_CONTROL_STOP only when the service
// isn't already Stopped, so an idempotent reinstall/uninstall over an
// already-stopped service doesn't spam ERROR_SERVICE_NOT_ACTIVE into
// the Windows Event Log. A Query error falls back to sending Stop
// (prior behaviour); waitForServiceStopped is the authoritative gate
// either way.
func sendStopIfRunning(s *mgr.Service) {
	if status, err := s.Query(); err == nil && status.State == svc.Stopped {
		return
	}
	_, _ = s.Control(svc.Stop)
}

// tryInstallWindowsService is the SCM-or-fallback entry point used
// by `packaging.Install` on Windows. Returns:
//
//   - (unitPath, nil) on a successful SCM install. The caller treats
//     this as a hard success.
//   - ("", nil) when SCM access is denied (no admin). The caller
//     falls through to the Startup-folder install.
//   - ("", err) for any genuine SCM failure (service exists but
//     can't be replaced, CreateService failed, etc.).
//
// `mgr.Connect()` returns `windows.ERROR_ACCESS_DENIED` when the
// calling process isn't running as administrator — the elevation
// probe. We classify with `errors.Is` against the typed sentinel
// (Gemini flagged the prior "swallow every connect error" path on
// PR #48 as too permissive). Other Connect errors (RPC down,
// SCM service stopped) are real failures and bubble up.
func tryInstallWindowsService(p Params) (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// Non-elevated: fall through to startup folder.
			return "", nil
		}
		return "", fmt.Errorf(scmConnectErr, err)
	}
	m.Disconnect()
	// We're elevated — proceed with the real install. Any error
	// from here is operator-actionable.
	return InstallWindowsService(p)
}

// tryUninstallWindowsService mirrors `tryInstallWindowsService` for
// the uninstall path. SCM access denied → return nil (no service
// to remove from a non-elevated context); other Connect errors
// bubble up so a genuine SCM-down state is visible. Idempotent:
// a not-registered service is also nil.
func tryUninstallWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil
		}
		return fmt.Errorf(scmConnectErr, err)
	}
	m.Disconnect()
	return UninstallWindowsService()
}
