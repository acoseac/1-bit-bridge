package packaging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// windowsStartupShortcutPath returns the per-user Startup-folder path
// where bridge's .cmd launcher lives.
//
//	%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\1-bit-bridge.cmd
//
// A `.cmd` (plain text) rather than a `.lnk` (binary, shell-link COM
// structure) avoids the `IShellLink` dance while still running on
// logon — Windows Explorer launches every .cmd in Startup at logon
// the same way it launches shortcuts.
func windowsStartupShortcutPath() (string, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return "", fmt.Errorf("%%APPDATA%% is unset — can't locate Startup folder")
	}
	return filepath.Join(appdata,
		"Microsoft", "Windows", "Start Menu", "Programs", "Startup",
		ServiceLabel+".cmd"), nil
}

// installWindowsStartup renders a `.cmd` launcher into the user's
// Startup folder. The launcher runs `bridge serve --config <path>` in
// a minimized window so the user doesn't see a stray console pop up on
// logon, with stdout/stderr both going to the log file for
// troubleshooting.
func installWindowsStartup(p Params) (string, error) {
	path, err := windowsStartupShortcutPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("ensure Startup folder: %w", err)
	}
	body, err := render("startup.cmd.tmpl", p)
	if err != nil {
		return "", err
	}
	// os.WriteFile truncates, which is the idempotent behaviour we want:
	// re-running init overwrites the old launcher with the new one.
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return path, fmt.Errorf("write launcher: %w", err)
	}
	return path, nil
}

// uninstallWindowsStartup removes the Startup-folder launcher. Missing
// is not an error (idempotent uninstall).
func uninstallWindowsStartup() (string, error) {
	path, err := windowsStartupShortcutPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return path, err
	}
	return path, nil
}
