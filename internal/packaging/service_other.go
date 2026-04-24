//go:build !windows

package packaging

import "errors"

// ErrServiceInstallUnsupported is returned when InstallWindowsService is
// called on a non-Windows host. Callers check the error and fall back
// to launchd/systemd (unix) or the Startup-folder launcher (windows).
var ErrServiceInstallUnsupported = errors.New("Windows Service install only supported on windows")

// InstallWindowsService is a non-Windows stub. Always returns
// ErrServiceInstallUnsupported. The signature matches the windows
// build so callers don't need build-constrained dispatch code.
func InstallWindowsService(_ Params) (string, error) {
	return "", ErrServiceInstallUnsupported
}

// UninstallWindowsService is a non-Windows stub. Always returns nil so
// a best-effort uninstall doesn't fail across platforms.
func UninstallWindowsService() error { return nil }
