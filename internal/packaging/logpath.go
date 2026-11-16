package packaging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The bridge's log on Windows is one of two files, by how the bridge was
// started (backlog B618). The Windows service writes
// %PROGRAMDATA%\1-bit-bridge\bridge.log, which cmd/bridge's redirectServiceIO
// opens before serve starts: machine-wide, since the service runs as
// LocalSystem, whose %LOCALAPPDATA% is under
// C:\Windows\System32\config\systemprofile. A bridge the Startup-folder
// launcher starts, or bridge init's own start of one, writes DefaultLogPath,
// %LOCALAPPDATA%\1-bit-bridge\bridge.log, through the launcher's redirection.
// A bridge run in the foreground writes neither: it logs to its terminal.

// WindowsLogDirs are what the two Windows logs' paths are made from. Each is
// given, never read here, so that one function makes the paths for the
// service, for bridge logs, for the console and for a test on any host.
type WindowsLogDirs struct {
	// ProgramData is %PROGRAMDATA%; empty is C:\ProgramData, as the service
	// takes it.
	ProgramData string
	// LocalAppData is %LOCALAPPDATA%; empty is the home directory's
	// AppData\Local, as DefaultLogPath takes it.
	LocalAppData string
	// Home answers the home directory. It is asked only where LocalAppData
	// is empty; nil answers that the home directory is not known.
	Home func() (string, error)
	// Join joins a path's elements: filepath.Join on Windows, and a Windows
	// joiner of a test's on another host, whose filepath.Join makes no
	// Windows path. Nil is filepath.Join.
	Join func(elem ...string) string
}

// HostWindowsLogDirs are this process's: its environment's PROGRAMDATA and
// LOCALAPPDATA, its home directory and filepath.Join.
func HostWindowsLogDirs() WindowsLogDirs {
	return WindowsLogDirs{
		ProgramData:  os.Getenv("PROGRAMDATA"),
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		Home:         os.UserHomeDir,
		Join:         filepath.Join,
	}
}

// defaultProgramData is %PROGRAMDATA% where the environment has none: a
// well-known absolute path, so that a service whose environment some
// automation stripped still logs where bridge logs looks.
const defaultProgramData = `C:\ProgramData`

// errNoHome is UserLog's answer where LocalAppData is empty and no Home is
// given.
var errNoHome = errors.New("the home directory is not known")

func (d WindowsLogDirs) join(elem ...string) string {
	if d.Join == nil {
		return filepath.Join(elem...)
	}
	return d.Join(elem...)
}

// ServiceLogDir is the folder of the Windows service's log, which
// InstallWindowsService prepares and the service makes again at each start.
func (d WindowsLogDirs) ServiceLogDir() string {
	programData := d.ProgramData
	if programData == "" {
		programData = defaultProgramData
	}
	return d.join(programData, productName)
}

// ServiceLog is the file the bridge's Windows service writes its log to.
func (d WindowsLogDirs) ServiceLog() string {
	return d.join(d.ServiceLogDir(), bridgeLogFileName)
}

// UserLog is the file a bridge the Startup-folder launcher or bridge init
// starts writes its log to: DefaultLogPath's answer on Windows. The home
// directory is asked only where LocalAppData is empty, so a home that cannot
// be read fails the user's log alone.
func (d WindowsLogDirs) UserLog() (string, error) {
	if d.LocalAppData != "" {
		return d.join(d.LocalAppData, productName, bridgeLogFileName), nil
	}
	if d.Home == nil {
		return "", errNoHome
	}
	home, err := d.Home()
	if err != nil {
		return "", err
	}
	return d.join(home, "AppData", "Local", productName, bridgeLogFileName), nil
}

// OpenServiceLog opens the service's log as the service writes to it: made
// where it is not there, appended to, written only, 0644. It is the service's
// one open, so a test that reads the log while it is held holds it so.
func OpenServiceLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// ServiceDisplayName is the name Services shows the bridge's Windows service
// by. Its service name, which sc.exe takes, is ServiceLabel.
const ServiceDisplayName = "1-bit-bridge"

// ServiceRegistration says whether the bridge's Windows service is
// registered with the service manager, as far as a check could tell.
type ServiceRegistration int

const (
	// ServiceRegistrationUnknown is a check that could not tell, its error
	// saying why: the zero value, so that a check that ran nothing tells
	// nothing.
	ServiceRegistrationUnknown ServiceRegistration = iota
	// ServiceNotRegistered says the service manager has no service by the
	// bridge's name.
	ServiceNotRegistered
	// ServiceRegistered says it has one, running or not.
	ServiceRegistered
)

// WindowsInstall is what the log readers ask of a Windows install: whether
// the service is registered, and whether the Startup-folder launcher is
// there. Both can be, since an install of one kind over the other leaves
// both behind, and the service, which starts at boot, is then the bridge
// that runs.
type WindowsInstall struct {
	Service ServiceRegistration
	// ServiceErr says why the check could not tell; nil where it could.
	ServiceErr error
	// Startup says the Startup-folder launcher is there.
	Startup bool
}

// CheckWindowsInstall asks what of the bridge is installed on this host,
// with no elevation and nothing asked of the network: it connects to the
// service manager with SC_MANAGER_CONNECT alone and opens the service with
// SERVICE_QUERY_STATUS alone, which a standard user holds under the default
// security Windows gives the manager and a service CreateService registers
// (Microsoft's "Service Security and Access Rights"). That is why it is not
// InstalledKind, whose mgr.Connect and OpenService ask for all access, which
// only an elevated administrator is granted, and which reads any other
// failure as no service. Off Windows it tells nothing.
func CheckWindowsInstall() WindowsInstall {
	return checkWindowsInstall(hostServiceManager, windowsStartupShortcutPath, os.Stat)
}

// checkWindowsInstall is CheckWindowsInstall over the service manager, the
// launcher's path and the stat given.
func checkWindowsInstall(sm serviceManager, launcher func() (string, error), stat func(string) (os.FileInfo, error)) WindowsInstall {
	var inst WindowsInstall
	inst.Service, inst.ServiceErr = sm.registration(ServiceLabel)
	if path, err := launcher(); err == nil {
		if _, err := stat(path); err == nil {
			inst.Startup = true
		}
	}
	return inst
}

// serviceManager is the calls the install check makes of the Windows service
// manager: Windows's own (scm_windows.go), or a test's stand-in, which is how
// each of the check's cases runs on every host.
type serviceManager struct {
	// connect opens the service manager with the access asked for.
	connect func(access uint32) (uintptr, error)
	// open opens the service named name through the manager's handle, with
	// the access asked for.
	open func(manager uintptr, name string, access uint32) (uintptr, error)
	// closeHandle closes a handle connect or open answered.
	closeHandle func(h uintptr)
	// notRegistered says whether an error of open's says that no service has
	// the name: ERROR_SERVICE_DOES_NOT_EXIST.
	notRegistered func(err error) bool
}

// The access the install check asks for, by Windows's values: what a
// standard user holds of the service manager, and of a service CreateService
// registered.
const (
	scManagerConnect   uint32 = 0x0001 // SC_MANAGER_CONNECT
	serviceQueryStatus uint32 = 0x0004 // SERVICE_QUERY_STATUS
)

// errNoServiceManager is the install check's answer where there is no
// service manager to ask: off Windows.
var errNoServiceManager = errors.New("there is no Windows service manager to ask")

// registration asks whether a service named name is registered. A name the
// manager does not know is not registered; any other failure, a connect
// refused or out of reach, or a service the manager will not open, tells
// nothing, and says why.
func (sm serviceManager) registration(name string) (ServiceRegistration, error) {
	if sm.connect == nil || sm.open == nil {
		return ServiceRegistrationUnknown, errNoServiceManager
	}
	manager, err := sm.connect(scManagerConnect)
	if err != nil {
		return ServiceRegistrationUnknown, fmt.Errorf("connect to the service manager: %w", err)
	}
	defer sm.release(manager)
	service, err := sm.open(manager, name, serviceQueryStatus)
	if err != nil {
		if sm.notRegistered != nil && sm.notRegistered(err) {
			return ServiceNotRegistered, nil
		}
		return ServiceRegistrationUnknown, fmt.Errorf("open the service %s: %w", name, err)
	}
	sm.release(service)
	return ServiceRegistered, nil
}

// release closes h where the manager has a close.
func (sm serviceManager) release(h uintptr) {
	if sm.closeHandle != nil {
		sm.closeHandle(h)
	}
}
