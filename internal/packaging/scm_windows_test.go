//go:build windows

package packaging

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestTheStandInsRightsAreWindowsOwn holds the access the install check asks
// for, and what its stand-in grants and refuses (logpath_test.go), to
// x/sys/windows's values.
func TestTheStandInsRightsAreWindowsOwn(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"SC_MANAGER_CONNECT", scManagerConnect, windows.SC_MANAGER_CONNECT},
		{"SERVICE_QUERY_STATUS", serviceQueryStatus, windows.SERVICE_QUERY_STATUS},
		{"SC_MANAGER_ENUMERATE_SERVICE", scManagerEnumerateService, windows.SC_MANAGER_ENUMERATE_SERVICE},
		{"SC_MANAGER_QUERY_LOCK_STATUS", scManagerQueryLockStatus, windows.SC_MANAGER_QUERY_LOCK_STATUS},
		{"SC_MANAGER_ALL_ACCESS", scManagerAllAccess, windows.SC_MANAGER_ALL_ACCESS},
		{"SERVICE_QUERY_CONFIG", serviceQueryConfig, windows.SERVICE_QUERY_CONFIG},
		{"SERVICE_ENUMERATE_DEPENDENTS", serviceEnumerateDependents, windows.SERVICE_ENUMERATE_DEPENDENTS},
		{"SERVICE_INTERROGATE", serviceInterrogate, windows.SERVICE_INTERROGATE},
		{"SERVICE_USER_DEFINED_CONTROL", serviceUserDefinedControl, windows.SERVICE_USER_DEFINED_CONTROL},
		{"SERVICE_ALL_ACCESS", serviceAllAccess, windows.SERVICE_ALL_ACCESS},
		{"READ_CONTROL", readControl, windows.READ_CONTROL},
	} {
		if c.got != c.want {
			t.Errorf("%s is %#x here and %#x in x/sys/windows", c.name, c.got, c.want)
		}
	}
}

// TestTheServiceManagerAnswersTheInstallCheck asks Windows's own service
// manager, with the check's access: a service every Windows registers is
// registered, and a name no service has is not, never unknown. CI's runner is
// elevated, so this shows the calls Windows answers, not a standard user's
// rights, which TestTheInstallCheckAsksOnlyWhatAStandardUserHolds models.
func TestTheServiceManagerAnswersTheInstallCheck(t *testing.T) {
	if got, err := hostServiceManager.registration("EventLog"); got != ServiceRegistered || err != nil {
		t.Errorf("registration of EventLog = %v, %v; want registered", got, err)
	}
	const none = ServiceLabel + ".b618-not-registered"
	if got, err := hostServiceManager.registration(none); got != ServiceNotRegistered || err != nil {
		t.Errorf("registration of %s = %v, %v; want not registered", none, got, err)
	}
}

// TestTheWindowsLogsAreTheHostsOwn: on Windows the user's log is
// DefaultLogPath, which the Startup-folder launcher redirects to, and the
// service's is %PROGRAMDATA%'s, or C:\ProgramData's where the environment has
// none, which redirectServiceIO opens.
func TestTheWindowsLogsAreTheHostsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	for _, local := range []string{filepath.Join(t.TempDir(), "Local"), ""} {
		t.Setenv("LOCALAPPDATA", local)
		want, err := DefaultLogPath()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := HostWindowsLogDirs().UserLog(); got != want || err != nil {
			t.Errorf("with LOCALAPPDATA %q the user's log is %q, %v; DefaultLogPath is %q", local, got, err, want)
		}
	}
	programData := t.TempDir()
	t.Setenv("PROGRAMDATA", programData)
	if got, want := HostWindowsLogDirs().ServiceLog(), filepath.Join(programData, "1-bit-bridge", "bridge.log"); got != want {
		t.Errorf("the service's log is %q, want %q", got, want)
	}
	t.Setenv("PROGRAMDATA", "")
	if got, want := HostWindowsLogDirs().ServiceLog(), `C:\ProgramData\1-bit-bridge\bridge.log`; got != want {
		t.Errorf("with no PROGRAMDATA the service's log is %q, want %q", got, want)
	}
}
