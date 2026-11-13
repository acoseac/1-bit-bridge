package packaging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// windowsJoin joins as Windows's filepath.Join joins the clean elements these
// tests give it, on any host: with one backslash between them. A host's own
// filepath.Join makes no Windows path off Windows.
func windowsJoin(elem ...string) string {
	return strings.Join(elem, `\`)
}

// joiner is a way of joining a path's elements, named.
type joiner struct {
	name string
	join func(elem ...string) string
}

// windowsJoiners are the joiners the path tests make their paths with: the
// tests' own on every host and, on Windows, Windows's filepath.Join too, which
// must agree with it.
func windowsJoiners() []joiner {
	joiners := []joiner{{name: "a Windows joiner", join: windowsJoin}}
	if runtime.GOOS == "windows" {
		joiners = append(joiners, joiner{name: "filepath.Join", join: filepath.Join})
	}
	return joiners
}

// errNoHomeHere is a home directory that cannot be read.
var errNoHomeHere = errors.New("the test's home cannot be read")

// TestTheWindowsLogsAreMadeFromTheirDirectories pins the two Windows logs'
// paths in Windows's form (backlog B618): the service's under %PROGRAMDATA%,
// C:\ProgramData where the environment has none, as the service's
// redirectServiceIO took it; the user's under %LOCALAPPDATA%, the home
// directory's AppData\Local where the environment has none, as DefaultLogPath
// took it, the home asked only then.
func TestTheWindowsLogsAreMadeFromTheirDirectories(t *testing.T) {
	home := func(t *testing.T) func() (string, error) {
		return func() (string, error) { return `C:\Users\me`, nil }
	}
	homeNotAsked := func(t *testing.T) func() (string, error) {
		return func() (string, error) {
			t.Errorf("the home directory was asked although LOCALAPPDATA is set")
			return "", errNoHomeHere
		}
	}
	for _, j := range windowsJoiners() {
		for _, c := range []struct {
			name                  string
			programData, localApp string
			home                  func(t *testing.T) func() (string, error)
			serviceDir, service   string
			user                  string
		}{
			{
				name:        "both directories set",
				programData: `D:\ProgramData`, localApp: `C:\Users\me\AppData\Local`, home: homeNotAsked,
				serviceDir: `D:\ProgramData\1-bit-bridge`, service: `D:\ProgramData\1-bit-bridge\bridge.log`,
				user: `C:\Users\me\AppData\Local\1-bit-bridge\bridge.log`,
			},
			{
				name:        "neither set",
				programData: "", localApp: "", home: home,
				serviceDir: `C:\ProgramData\1-bit-bridge`, service: `C:\ProgramData\1-bit-bridge\bridge.log`,
				user: `C:\Users\me\AppData\Local\1-bit-bridge\bridge.log`,
			},
			{
				name:        "LOCALAPPDATA elsewhere than the home's",
				programData: `C:\ProgramData`, localApp: `E:\Local`, home: homeNotAsked,
				serviceDir: `C:\ProgramData\1-bit-bridge`, service: `C:\ProgramData\1-bit-bridge\bridge.log`,
				user: `E:\Local\1-bit-bridge\bridge.log`,
			},
		} {
			t.Run(j.name+"/"+c.name, func(t *testing.T) {
				d := WindowsLogDirs{ProgramData: c.programData, LocalAppData: c.localApp, Home: c.home(t), Join: j.join}
				if got := d.ServiceLogDir(); got != c.serviceDir {
					t.Errorf("ServiceLogDir() = %q, want %q", got, c.serviceDir)
				}
				if got := d.ServiceLog(); got != c.service {
					t.Errorf("ServiceLog() = %q, want %q", got, c.service)
				}
				got, err := d.UserLog()
				if err != nil || got != c.user {
					t.Errorf("UserLog() = %q, %v; want %q", got, err, c.user)
				}
			})
		}
	}
}

// TestTheUserLogNeedsTheHomeOnlyWithoutLocalAppData: a home directory that
// cannot be read fails the user's log where LOCALAPPDATA is empty, and
// nothing else, the service's log included.
func TestTheUserLogNeedsTheHomeOnlyWithoutLocalAppData(t *testing.T) {
	for _, home := range []struct {
		name string
		fn   func() (string, error)
		want error
	}{
		{name: "a home that cannot be read", fn: func() (string, error) { return "", errNoHomeHere }, want: errNoHomeHere},
		{name: "no home given", fn: nil, want: errNoHome},
	} {
		t.Run(home.name, func(t *testing.T) {
			d := WindowsLogDirs{Home: home.fn, Join: windowsJoin}
			if got, err := d.UserLog(); !errors.Is(err, home.want) || got != "" {
				t.Errorf("UserLog() = %q, %v; want %v", got, err, home.want)
			}
			if got, want := d.ServiceLog(), `C:\ProgramData\1-bit-bridge\bridge.log`; got != want {
				t.Errorf("ServiceLog() = %q, want %q", got, want)
			}
		})
	}
}

// The access rights the service manager stand-in grants and refuses, by
// Windows's values (winsvc.h, winnt.h): TestTheStandInsRightsAreWindowsOwn
// holds them to x/sys/windows's on Windows.
const (
	scManagerEnumerateService  = 0x0004
	scManagerQueryLockStatus   = 0x0010
	scManagerAllAccess         = 0xF003F
	serviceQueryConfig         = 0x0001
	serviceEnumerateDependents = 0x0008
	serviceInterrogate         = 0x0080
	serviceUserDefinedControl  = 0x0100
	serviceAllAccess           = 0xF01FF
	readControl                = 0x00020000
)

// The rights a standard user holds under the default security Windows gives
// the service manager and a service CreateService registers (Microsoft's
// "Service Security and Access Rights": an authenticated user's).
const (
	standardUserManagerRights = scManagerConnect | scManagerEnumerateService | scManagerQueryLockStatus | readControl
	standardUserServiceRights = serviceQueryConfig | serviceQueryStatus | serviceEnumerateDependents |
		serviceInterrogate | serviceUserDefinedControl | readControl
)

// errAccessDeniedHere and errNoSuchServiceHere stand in for Windows's
// ERROR_ACCESS_DENIED and ERROR_SERVICE_DOES_NOT_EXIST.
var (
	errAccessDeniedHere  = errors.New("Access is denied.")
	errNoSuchServiceHere = errors.New("The specified service does not exist as an installed service.")
)

// scmStandIn is the Windows service manager as a caller holding the rights
// given meets it: an open asking for more than they grant is refused with
// access denied, as Windows refuses it, a name it holds no service by with
// does-not-exist, and every call is recorded. Its services' states are what
// a status query would answer, which the install check never asks.
type scmStandIn struct {
	managerRights, serviceRights uint32
	// services are the registered services, by name, and their states.
	services map[string]string
	// connectErr and openErr, where set, are the connect's and a registered
	// service's open's answers whatever the rights.
	connectErr, openErr error
	asked               []string
	open                int
}

func (s *scmStandIn) manager() serviceManager {
	return serviceManager{
		connect: func(access uint32) (uintptr, error) {
			s.asked = append(s.asked, fmt.Sprintf("connect %#x", access))
			if s.connectErr != nil {
				return 0, s.connectErr
			}
			if access&^s.managerRights != 0 {
				return 0, errAccessDeniedHere
			}
			s.open++
			return 1, nil
		},
		open: func(manager uintptr, name string, access uint32) (uintptr, error) {
			s.asked = append(s.asked, fmt.Sprintf("open %s %#x", name, access))
			if manager != 1 {
				return 0, fmt.Errorf("open through handle %d, not the manager's", manager)
			}
			if _, ok := s.services[name]; !ok {
				return 0, errNoSuchServiceHere
			}
			if s.openErr != nil {
				return 0, s.openErr
			}
			if access&^s.serviceRights != 0 {
				return 0, errAccessDeniedHere
			}
			s.open++
			return 2, nil
		},
		closeHandle: func(h uintptr) {
			s.asked = append(s.asked, fmt.Sprintf("close %d", h))
			s.open--
		},
		notRegistered: func(err error) bool { return errors.Is(err, errNoSuchServiceHere) },
	}
}

// TestTheInstallCheckAsksOnlyWhatAStandardUserHolds is backlog B618's
// non-elevated case: a standard user, who holds of the service manager and of
// the bridge's service only what their default security grants, is told the
// service is registered, where InstalledKind's mgr.Connect, which asks for
// all access, is refused and reads as no service.
func TestTheInstallCheckAsksOnlyWhatAStandardUserHolds(t *testing.T) {
	for _, c := range []struct {
		name                         string
		managerRights, serviceRights uint32
	}{
		{name: "a standard user", managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights},
		{name: "an elevated administrator", managerRights: scManagerAllAccess, serviceRights: serviceAllAccess},
	} {
		t.Run(c.name, func(t *testing.T) {
			scm := &scmStandIn{managerRights: c.managerRights, serviceRights: c.serviceRights,
				services: map[string]string{ServiceLabel: "running"}}
			got, err := scm.manager().registration(ServiceLabel)
			if got != ServiceRegistered || err != nil {
				t.Errorf("registration = %v, %v; want registered", got, err)
			}
			want := []string{"connect 0x1", "open " + ServiceLabel + " 0x4", "close 2", "close 1"}
			if fmt.Sprint(scm.asked) != fmt.Sprint(want) {
				t.Errorf("the check asked %q, want %q", scm.asked, want)
			}
			if scm.open != 0 {
				t.Errorf("%d handles left open", scm.open)
			}
		})
	}
	// The stand-in refuses what InstalledKind asks a standard user for, as
	// Windows does: what makes the first row above a finding.
	scm := &scmStandIn{managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights,
		services: map[string]string{ServiceLabel: "running"}}
	if _, err := scm.manager().connect(scManagerAllAccess); !errors.Is(err, errAccessDeniedHere) {
		t.Errorf("the stand-in granted a standard user all access to the manager: %v", err)
	}
}

// TestTheInstallCheckTellsNotRegisteredFromCannotTell: a name the service
// manager has no service by is not registered; a connect refused or out of
// reach, and a service the manager will not open, can tell nothing and say
// why, so that bridge logs never takes them for no service.
func TestTheInstallCheckTellsNotRegisteredFromCannotTell(t *testing.T) {
	errRPC := errors.New("The RPC server is unavailable.")
	for _, c := range []struct {
		name       string
		scm        *scmStandIn
		want       ServiceRegistration
		wantErr    error
		wantPrefix string
	}{
		{name: "no service by the name",
			scm:  &scmStandIn{managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights},
			want: ServiceNotRegistered},
		{name: "the manager out of reach",
			scm:  &scmStandIn{connectErr: errRPC, services: map[string]string{ServiceLabel: "running"}},
			want: ServiceRegistrationUnknown, wantErr: errRPC, wantPrefix: "connect to the service manager: "},
		{name: "the manager refuses even a connect",
			scm:  &scmStandIn{managerRights: 0, services: map[string]string{ServiceLabel: "running"}},
			want: ServiceRegistrationUnknown, wantErr: errAccessDeniedHere, wantPrefix: "connect to the service manager: "},
		{name: "a service whose security refuses its status",
			scm:  &scmStandIn{managerRights: standardUserManagerRights, serviceRights: 0, services: map[string]string{ServiceLabel: "running"}},
			want: ServiceRegistrationUnknown, wantErr: errAccessDeniedHere, wantPrefix: "open the service " + ServiceLabel + ": "},
		{name: "a service the manager fails to open",
			scm: &scmStandIn{managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights,
				services: map[string]string{ServiceLabel: "running"}, openErr: errRPC},
			want: ServiceRegistrationUnknown, wantErr: errRPC, wantPrefix: "open the service " + ServiceLabel + ": "},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.scm.manager().registration(ServiceLabel)
			if got != c.want {
				t.Errorf("registration = %v, want %v", got, c.want)
			}
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("registration's error = %v, want none", err)
			case c.wantErr != nil && (!errors.Is(err, c.wantErr) || !strings.HasPrefix(err.Error(), c.wantPrefix)):
				t.Errorf("registration's error = %v, want %q then %v", err, c.wantPrefix, c.wantErr)
			}
			if c.scm.open != 0 {
				t.Errorf("%d handles left open", c.scm.open)
			}
		})
	}
	t.Run("no service manager", func(t *testing.T) {
		got, err := serviceManager{}.registration(ServiceLabel)
		if got != ServiceRegistrationUnknown || !errors.Is(err, errNoServiceManager) {
			t.Errorf("registration = %v, %v; want unknown, %v", got, err, errNoServiceManager)
		}
	})
}

// TestAStoppedServiceIsRegistered is backlog B618's stopped service: the
// check asks whether the service is registered, never whether it runs, so a
// stopped service's log is still the one bridge logs reads.
func TestAStoppedServiceIsRegistered(t *testing.T) {
	scm := &scmStandIn{managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights,
		services: map[string]string{ServiceLabel: "stopped"}}
	if got, err := scm.manager().registration(ServiceLabel); got != ServiceRegistered || err != nil {
		t.Errorf("registration of a stopped service = %v, %v; want registered", got, err)
	}
}

// TestBothInstallArtifactsAreReported is backlog B618's install of one kind
// over the other: the service registered and the Startup-folder launcher
// there are both reported, and bridge logs reads the service's log
// (chooseWindowsLog); neither, either alone, and a launcher whose path cannot
// be made each read as they are.
func TestBothInstallArtifactsAreReported(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), ServiceLabel+".cmd")
	if err := os.WriteFile(launcher, []byte("@echo off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), ServiceLabel+".cmd")
	registered := map[string]string{ServiceLabel: "running"}
	for _, c := range []struct {
		name     string
		services map[string]string
		launcher func() (string, error)
		want     WindowsInstall
	}{
		{name: "both", services: registered, launcher: func() (string, error) { return launcher, nil },
			want: WindowsInstall{Service: ServiceRegistered, Startup: true}},
		{name: "the service alone", services: registered, launcher: func() (string, error) { return gone, nil },
			want: WindowsInstall{Service: ServiceRegistered}},
		{name: "the launcher alone", launcher: func() (string, error) { return launcher, nil },
			want: WindowsInstall{Service: ServiceNotRegistered, Startup: true}},
		{name: "neither", launcher: func() (string, error) { return gone, nil },
			want: WindowsInstall{Service: ServiceNotRegistered}},
		{name: "a launcher whose path cannot be made", services: registered,
			launcher: func() (string, error) { return "", errors.New("%APPDATA% is unset") },
			want:     WindowsInstall{Service: ServiceRegistered}},
	} {
		t.Run(c.name, func(t *testing.T) {
			scm := &scmStandIn{managerRights: standardUserManagerRights, serviceRights: standardUserServiceRights,
				services: c.services}
			if got := checkWindowsInstall(scm.manager(), c.launcher, os.Stat); got != c.want {
				t.Errorf("checkWindowsInstall = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestTheInstallCheckOffWindowsTellsNothing: off Windows the host has no
// service manager, so the check answers unknown, never not registered.
func TestTheInstallCheckOffWindowsTellsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has a service manager: TestTheServiceManagerAnswersTheInstallCheck asks it")
	}
	inst := CheckWindowsInstall()
	if inst.Service != ServiceRegistrationUnknown || !errors.Is(inst.ServiceErr, errNoServiceManager) {
		t.Errorf("CheckWindowsInstall() off Windows = %+v, want unknown, %v", inst, errNoServiceManager)
	}
}

// TestOpenServiceLogAppendsAsTheServiceWrites: the service's open makes its
// log, keeps what is there and writes after it.
func TestOpenServiceLogAppendsAsTheServiceWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.log")
	for _, line := range []string{"a first start\n", "a second start\n"} {
		f, err := OpenServiceLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a first start\na second start\n" {
		t.Errorf("the service's log holds %q", got)
	}
}
