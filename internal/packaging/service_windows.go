//go:build windows

package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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

	// If an existing service with this name is registered, delete it
	// first so install is idempotent. `bridge init --service` re-runs
	// should upgrade the binary path without requiring a separate
	// uninstall step.
	if s, err := m.OpenService(ServiceLabel); err == nil {
		_ = s.Delete()
		s.Close()
		// Give SCM a moment to finish the delete before the Create. The
		// docs say Delete is asynchronous; in practice 200ms is enough,
		// but we wait up to 3s to be safe.
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
	_, _ = s.Control(6) // SERVICE_CONTROL_STOP = 6
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
