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

// tryInstallWindowsService is the elevation-aware wrapper used by
// `Install` on Windows. The non-Windows stub returns ("", nil) so
// the dispatch in packaging.go falls through to the unix-side
// install paths (which are what actually runs on darwin/linux —
// the call site is gated on `runtime.GOOS == "windows"`, this stub
// only exists to keep the symbol resolvable at compile time).
func tryInstallWindowsService(_ Params) (string, error) { return "", nil }

// tryUninstallWindowsService mirrors tryInstallWindowsService for
// the uninstall path. Always nil on non-Windows.
func tryUninstallWindowsService() error { return nil }
