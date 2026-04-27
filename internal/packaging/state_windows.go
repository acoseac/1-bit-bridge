//go:build windows

package packaging

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// installedKindForOS probes the Windows install state. SCM is checked
// first via mgr.Connect + OpenService — if a service registration is
// found we return KindWindowsSCM regardless of whether the Startup
// shortcut also exists (re-installs can leave both behind; SCM wins).
// On Connect failures other than ERROR_ACCESS_DENIED the function
// returns the error so a real SCM-down state is visible. On access
// denied (non-elevated) we fall through to the Startup-folder file
// probe — that's the user-context install path and is readable
// without admin.
func installedKindForOS() (ServiceKind, error) {
	m, err := mgr.Connect()
	if err == nil {
		s, openErr := m.OpenService(ServiceLabel)
		if openErr == nil {
			s.Close()
			m.Disconnect()
			return KindWindowsSCM, nil
		}
		m.Disconnect()
		// Service not registered with SCM — fall through to the
		// Startup-folder probe.
	} else if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return KindNone, err
	}
	path, err := windowsStartupShortcutPath()
	if err != nil {
		// Could not even resolve the Startup-folder path
		// (e.g. %APPDATA% unset). Treat as "no install we can see".
		return KindNone, nil
	}
	if _, err := os.Stat(path); err == nil {
		return KindWindowsStartup, nil
	}
	return KindNone, nil
}

// adminProbe holds the cached result of IsAdmin's filesystem probe.
// The probe opens a privileged device handle which is cheap but not
// free, and the menu redraws the launch frame several times per
// session — once-and-cache is the right tradeoff. Elevation does
// not change inside a single process lifetime.
var adminProbe struct {
	once sync.Once
	is   bool
}

// IsAdmin probes whether the current process is running with admin
// rights. Stdlib-only — no golang.org/x/sys/windows token APIs:
//
//  1. os.Open(`\\.\PHYSICALDRIVE0`) — opening a physical-drive
//     device handle requires SeBackupPrivilege which is granted
//     only to admins by default. Succeeds → admin.
//  2. Fallback to PHYSICALDRIVE1 in case drive-0 isn't present
//     (rare, e.g. some hypervisor / Windows Sandbox configurations
//     where the bare drive enumerator differs).
//  3. Final fallback: try mgr.Connect() with full read/write
//     access — same access-denied semantics as the SCM probe in
//     service_windows.go. Connect succeeds only as admin.
//
// If every probe returns "not exist", report false conservatively.
// Worst case: a real admin in a stripped environment sees the
// "(Requires Administrator)" hint anyway and the actual install
// proceeds successfully when they pick the option.
func IsAdmin() bool {
	adminProbe.once.Do(func() {
		if h, err := os.Open(`\\.\PHYSICALDRIVE0`); err == nil {
			h.Close()
			adminProbe.is = true
			return
		}
		if h, err := os.Open(`\\.\PHYSICALDRIVE1`); err == nil {
			h.Close()
			adminProbe.is = true
			return
		}
		if m, err := mgr.Connect(); err == nil {
			m.Disconnect()
			adminProbe.is = true
			return
		}
		adminProbe.is = false
	})
	return adminProbe.is
}

// IsRoot is the POSIX-only euid==0 probe; on Windows it returns
// false. The Windows analogue is IsAdmin (UAC elevation), which has
// different semantics than POSIX root — keep them as distinct
// concepts so callers don't conflate the gates.
func IsRoot() bool { return false }
