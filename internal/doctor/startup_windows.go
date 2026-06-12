//go:build windows

package doctor

import "golang.org/x/sys/windows"

// knownStartupDir resolves the per-user Startup folder via the
// SHGetKnownFolderPath API (FOLDERID_Startup). This honours roaming /
// redirected enterprise profiles that the hardcoded
// %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup path can miss.
// Returns ("", false) on any failure so the caller falls back to the
// env-based construction.
func knownStartupDir() (string, bool) {
	p, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0)
	if err != nil || p == "" {
		return "", false
	}
	return p, true
}
