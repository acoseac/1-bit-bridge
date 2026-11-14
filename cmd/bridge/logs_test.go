package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// The lines the tests' two logs hold, apart, so that what bridge logs prints
// says which file it read.
const (
	serviceLogLines = "the service's first line\nthe service's last line\n"
	userLogLines    = "a line of a bridge its sign-in launcher started\n"
)

// windowsLogsOnThisHost is a Windows host for bridge logs on the test's own:
// its PROGRAMDATA and LOCALAPPDATA are folders of the test's, so that its two
// logs are files bridge logs opens, made with this host's filepath.Join (the
// Windows path forms are TestTheWindowsLogChoiceInWindowsPaths'), and its
// install check answers inst.
type windowsLogsOnThisHost struct {
	host    logsHost
	service string // the service's log
	user    string // the user's log
}

func newWindowsLogsOnThisHost(t *testing.T, inst packaging.WindowsInstall) windowsLogsOnThisHost {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "Users", "me")
	dirs := packaging.WindowsLogDirs{
		ProgramData:  filepath.Join(root, "ProgramData"),
		LocalAppData: filepath.Join(home, "AppData", "Local"),
		Home:         func() (string, error) { return home, nil },
		Join:         filepath.Join,
	}
	user, err := dirs.UserLog()
	if err != nil {
		t.Fatal(err)
	}
	return windowsLogsOnThisHost{
		host:    windowsLogsHost(dirs, func() packaging.WindowsInstall { return inst }),
		service: dirs.ServiceLog(),
		user:    user,
	}
}

// windowsLogsHost is a Windows host whose directories are dirs and whose
// install check is install: DefaultLogPath there is the user's log, as
// packaging's TestTheWindowsLogsAreTheHostsOwn holds it on Windows.
func windowsLogsHost(dirs packaging.WindowsLogDirs, install func() packaging.WindowsInstall) logsHost {
	return logsHost{goos: "windows", defaultLog: dirs.UserLog, dirs: dirs, install: install}
}

// writeLogFile writes body to path, making its folder.
func writeLogFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runBridgeLogs runs bridge logs on host, answering its exit code, its stdout
// and its stderr.
func runBridgeLogs(t *testing.T, host logsHost, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := logsCmdWith(context.Background(), args, &stdout, &stderr, host)
	return code, stdout.String(), stderr.String()
}

// requireLogsRun fails the test unless bridge logs exited with code and
// printed stdout and stderr exactly.
func requireLogsRun(t *testing.T, code int, stdout, stderr string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("bridge logs exited %d, want %d", code, wantCode)
	}
	if stdout != wantStdout {
		t.Errorf("bridge logs printed\n%q\nwant\n%q", stdout, wantStdout)
	}
	if stderr != wantStderr {
		t.Errorf("bridge logs said on stderr\n%q\nwant\n%q", stderr, wantStderr)
	}
}

var (
	serviceRegistered = packaging.WindowsInstall{Service: packaging.ServiceRegistered}
	launcherInstalled = packaging.WindowsInstall{Service: packaging.ServiceNotRegistered, Startup: true}
	nothingInstalled  = packaging.WindowsInstall{Service: packaging.ServiceNotRegistered}
)

// TestBridgeLogsReadsTheServicesLogWhileTheServiceIsRegistered is backlog
// B618: on a Windows service install bridge logs read the user's log under
// %LOCALAPPDATA%, which the service never writes, while the service wrote
// %PROGRAMDATA%'s; it said the file did not exist, or showed one an earlier
// install left. It reads the service's log now, whatever else is installed,
// and says so in one line on stderr where the user's log is there too, and
// only there.
func TestBridgeLogsReadsTheServicesLogWhileTheServiceIsRegistered(t *testing.T) {
	for _, c := range []struct {
		name     string
		inst     packaging.WindowsInstall
		userToo  bool
		wantNote bool
	}{
		{name: "both logs there", inst: serviceRegistered, userToo: true, wantNote: true},
		{name: "the service's log alone", inst: serviceRegistered},
		{name: "both install artifacts and both logs", inst: packaging.WindowsInstall{Service: packaging.ServiceRegistered, Startup: true},
			userToo: true, wantNote: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, c.inst)
			writeLogFile(t, w.service, serviceLogLines)
			if c.userToo {
				writeLogFile(t, w.user, userLogLines)
			}
			code, stdout, stderr := runBridgeLogs(t, w.host)
			wantStderr := ""
			if c.wantNote {
				wantStderr = fmt.Sprintf("logs: reading the Windows service's log, %s; %s, the log of a bridge its sign-in launcher starts, is there too\n",
					w.service, w.user)
			}
			requireLogsRun(t, code, stdout, stderr, 0, serviceLogLines, wantStderr)
		})
	}
}

// TestBridgeLogsReadsTheUserLogOfAStartupFolderInstall is a guard, green at
// main: where the Startup-folder launcher is installed and the service is
// not, bridge logs reads the user's log, which the launcher redirects to, and
// says nothing more.
func TestBridgeLogsReadsTheUserLogOfAStartupFolderInstall(t *testing.T) {
	w := newWindowsLogsOnThisHost(t, launcherInstalled)
	writeLogFile(t, w.user, userLogLines)
	code, stdout, stderr := runBridgeLogs(t, w.host)
	requireLogsRun(t, code, stdout, stderr, 0, userLogLines, "")
}

// TestBridgeLogsSaysWhichLogItReadsWhereTheServiceIsNotRegistered: with the
// service not registered bridge logs reads the user's log, and where the
// service's log is there too, one an uninstalled service left, it says which
// it read, in one line on stderr.
func TestBridgeLogsSaysWhichLogItReadsWhereTheServiceIsNotRegistered(t *testing.T) {
	for _, inst := range []packaging.WindowsInstall{launcherInstalled, nothingInstalled} {
		t.Run(fmt.Sprintf("launcher %v", inst.Startup), func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, inst)
			writeLogFile(t, w.service, serviceLogLines)
			writeLogFile(t, w.user, userLogLines)
			code, stdout, stderr := runBridgeLogs(t, w.host)
			requireLogsRun(t, code, stdout, stderr, 0, userLogLines,
				fmt.Sprintf("logs: reading %s, as the bridge's Windows service is not installed; %s, the service's log, is there too\n",
					w.user, w.service))
		})
	}
}

// TestBridgeLogsSaysWhereTheServicesLogWillBe: with the service registered
// and its log not there yet, bridge logs names that log's path and how to see
// whether the service runs, Services and sc.exe by the service's own names,
// never `bridge start` (which a standard user cannot run against a service)
// nor `bridge status` (which asks the invoking user's config, not the
// service); and it reads no other log in its place, a user's log left there
// included.
func TestBridgeLogsSaysWhereTheServicesLogWillBe(t *testing.T) {
	for _, userToo := range []bool{false, true} {
		t.Run(fmt.Sprintf("a user's log there %v", userToo), func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, serviceRegistered)
			if userToo {
				writeLogFile(t, w.user, userLogLines)
			}
			code, stdout, stderr := runBridgeLogs(t, w.host)
			requireLogsRun(t, code, stdout, stderr, 1, "",
				"logs: "+w.service+" does not exist (yet)\n"+
					"  the bridge's Windows service writes it once it starts; see whether the service runs in Services, "+
					"where it is \"1-bit-bridge\", or with `sc.exe query com.acoseac.1-bit-bridge`.\n")
		})
	}
}

// TestBridgeLogsSaysWhatWritesTheUserLog: where the user's log is not there,
// bridge logs names its path and what would write it, the sign-in launcher
// where that is installed, and otherwise that a bridge run in the foreground
// logs to its terminal; never `bridge serve` as the way to make the file,
// which a foreground run never makes.
func TestBridgeLogsSaysWhatWritesTheUserLog(t *testing.T) {
	for _, c := range []struct {
		name string
		inst packaging.WindowsInstall
		want func(user string) string
	}{
		{name: "the sign-in launcher installed", inst: launcherInstalled, want: func(user string) string {
			return "logs: " + user + " does not exist (yet)\n" +
				"  the bridge's sign-in launcher, com.acoseac.1-bit-bridge.cmd in your Startup folder, writes it once it starts the bridge, at sign-in.\n"
		}},
		{name: "nothing installed", inst: nothingInstalled, want: func(user string) string {
			return "logs: " + user + " does not exist\n" +
				"  neither the bridge's Windows service nor its sign-in launcher is installed to write it; " +
				"a bridge run in the foreground logs to its terminal.\n"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, c.inst)
			code, stdout, stderr := runBridgeLogs(t, w.host)
			requireLogsRun(t, code, stdout, stderr, 1, "", c.want(w.user))
			if strings.Contains(stderr, "bridge serve") || strings.Contains(stderr, "bridge start") {
				t.Errorf("bridge logs offered a command as the way to make the file: %q", stderr)
			}
		})
	}
}

// TestBridgeLogsReadsTheServicesLogWhereTheCheckCannotTell: where the install
// check could not tell whether the service is registered, bridge logs never
// takes the user's log in silence: it reads the service's log where that is
// there, the user's where it is not, says which in one line with the check's
// reason, and names both paths where neither is there. A host with no install
// check reads as one that could not tell.
func TestBridgeLogsReadsTheServicesLogWhereTheCheckCannotTell(t *testing.T) {
	errCheck := errors.New("connect to the service manager: The RPC server is unavailable.")
	cannotTell := packaging.WindowsInstall{ServiceErr: errCheck}
	why := "cannot tell whether the bridge's Windows service is installed (" + errCheck.Error() + ")"
	for _, c := range []struct {
		name                string
		serviceLog, userLog bool
		wantCode            int
		wantStdout          string
		wantStderr          func(w windowsLogsOnThisHost) string
	}{
		{name: "both logs there", serviceLog: true, userLog: true, wantStdout: serviceLogLines,
			wantStderr: func(w windowsLogsOnThisHost) string {
				return "logs: " + why + "; reading its log, " + w.service + ", which is there; " +
					w.user + ", the log of a bridge its sign-in launcher starts, is there too\n"
			}},
		{name: "the service's log alone", serviceLog: true, wantStdout: serviceLogLines,
			wantStderr: func(w windowsLogsOnThisHost) string {
				return "logs: " + why + "; reading its log, " + w.service + ", which is there\n"
			}},
		{name: "the user's log alone", userLog: true, wantStdout: userLogLines,
			wantStderr: func(w windowsLogsOnThisHost) string {
				return "logs: " + why + "; its log, " + w.service + ", is not there, so reading " + w.user + "\n"
			}},
		{name: "neither log", wantCode: 1,
			wantStderr: func(w windowsLogsOnThisHost) string {
				return "logs: neither " + w.service + " nor " + w.user + " exists (" + why + ")\n" +
					"  the service writes the first once it starts, and a bridge its sign-in launcher starts the second; " +
					"a bridge run in the foreground logs to its terminal.\n"
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, cannotTell)
			if c.serviceLog {
				writeLogFile(t, w.service, serviceLogLines)
			}
			if c.userLog {
				writeLogFile(t, w.user, userLogLines)
			}
			code, stdout, stderr := runBridgeLogs(t, w.host)
			requireLogsRun(t, code, stdout, stderr, c.wantCode, c.wantStdout, c.wantStderr(w))
		})
	}
	t.Run("no install check", func(t *testing.T) {
		w := newWindowsLogsOnThisHost(t, cannotTell)
		w.host.install = nil
		writeLogFile(t, w.service, serviceLogLines)
		code, stdout, stderr := runBridgeLogs(t, w.host)
		requireLogsRun(t, code, stdout, stderr, 0, serviceLogLines,
			"logs: cannot tell whether the bridge's Windows service is installed ("+errNoInstallCheck.Error()+
				"); reading its log, "+w.service+", which is there\n")
	})
}

// TestBridgeLogsChoosesTheServicesLogWithoutTheHome: choosing the service's
// log needs nothing of the user's path, so a home directory that cannot be
// read, with LOCALAPPDATA empty, leaves the service's log read and the other
// unnamed; with the service not registered it fails as DefaultLogPath fails.
func TestBridgeLogsChoosesTheServicesLogWithoutTheHome(t *testing.T) {
	errHome := errors.New("%USERPROFILE% is not defined")
	dirsWithoutHome := func(t *testing.T) packaging.WindowsLogDirs {
		return packaging.WindowsLogDirs{
			ProgramData: filepath.Join(t.TempDir(), "ProgramData"),
			Home:        func() (string, error) { return "", errHome },
			Join:        filepath.Join,
		}
	}
	t.Run("the service registered", func(t *testing.T) {
		dirs := dirsWithoutHome(t)
		writeLogFile(t, dirs.ServiceLog(), serviceLogLines)
		host := windowsLogsHost(dirs, func() packaging.WindowsInstall { return serviceRegistered })
		code, stdout, stderr := runBridgeLogs(t, host)
		requireLogsRun(t, code, stdout, stderr, 0, serviceLogLines, "")
	})
	t.Run("the service not registered", func(t *testing.T) {
		dirs := dirsWithoutHome(t)
		writeLogFile(t, dirs.ServiceLog(), serviceLogLines)
		host := windowsLogsHost(dirs, func() packaging.WindowsInstall { return launcherInstalled })
		code, stdout, stderr := runBridgeLogs(t, host)
		requireLogsRun(t, code, stdout, stderr, 1, "", "logs: resolve path: "+errHome.Error()+"\n")
	})
}

// TestBridgeLogsReadsTheServicesLogWhileTheServiceHoldsIt: bridge logs reads
// the service's log while a handle holds it open as the service does
// (packaging.OpenServiceLog, redirectServiceIO's open). Its subject is
// Windows, where both opens' sharing modes decide whether the read is let
// in; elsewhere nothing refuses it.
func TestBridgeLogsReadsTheServicesLogWhileTheServiceHoldsIt(t *testing.T) {
	w := newWindowsLogsOnThisHost(t, serviceRegistered)
	if err := os.MkdirAll(filepath.Dir(w.service), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := packaging.OpenServiceLog(w.service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if _, err := held.WriteString(serviceLogLines); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runBridgeLogs(t, w.host)
	requireLogsRun(t, code, stdout, stderr, 0, serviceLogLines, "")
	// The service goes on writing, and bridge logs reads that too.
	const more = "a line the service wrote while it was read\n"
	if _, err := held.WriteString(more); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runBridgeLogs(t, w.host, "-n", "1")
	requireLogsRun(t, code, stdout, stderr, 0, more, "")
}

// TestBridgeLogsOffWindowsIsAsItWas is a guard, green at main: on macOS and
// Linux bridge logs reads DefaultLogPath and says what it always said, word
// for word, and asks nothing of a Windows install.
func TestBridgeLogsOffWindowsIsAsItWas(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "1-bit-bridge", "bridge.log")
			host := logsHost{
				goos:       goos,
				defaultLog: func() (string, error) { return path, nil },
				install: func() packaging.WindowsInstall {
					t.Errorf("bridge logs on %s asked what Windows has installed", goos)
					return packaging.WindowsInstall{}
				},
			}
			code, stdout, stderr := runBridgeLogs(t, host)
			requireLogsRun(t, code, stdout, stderr, 1, "",
				"logs: "+path+" does not exist (yet)\n"+
					"  the file is created on first service start; run `bridge start` (or `bridge serve` in the foreground) and retry.\n")

			writeLogFile(t, path, "one\ntwo\nthree\n")
			code, stdout, stderr = runBridgeLogs(t, host, "-n", "2")
			requireLogsRun(t, code, stdout, stderr, 0, "two\nthree\n", "")

			errHome := errors.New("$HOME is not defined")
			host.defaultLog = func() (string, error) { return "", errHome }
			code, stdout, stderr = runBridgeLogs(t, host)
			requireLogsRun(t, code, stdout, stderr, 1, "", "logs: resolve path: "+errHome.Error()+"\n")
		})
	}
}

// TestBridgeLogsReadsTheHostsLogThroughItsEntryPoint drives `bridge logs`
// through run, on this host's own logsHost (hostLogs), its directories set
// by the environment the test gives it and its install check the test's
// (logsInstallCheck), which answers not registered, so that no host's own
// service manager decides the answer: DefaultLogPath on macOS and Linux,
// asking no check; on Windows the user's log too, asking it once. A guard,
// green at main.
func TestBridgeLogsReadsTheHostsLogThroughItsEntryPoint(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("PROGRAMDATA", filepath.Join(root, "ProgramData"))
	asked := 0
	prev := logsInstallCheck
	logsInstallCheck = func() packaging.WindowsInstall { asked++; return nothingInstalled }
	t.Cleanup(func() { logsInstallCheck = prev })
	path, err := packaging.DefaultLogPath()
	if err != nil {
		t.Fatal(err)
	}
	writeLogFile(t, path, userLogLines)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"logs"}, &stdout, &stderr)
	requireLogsRun(t, code, stdout.String(), stderr.String(), 0, userLogLines, "")
	wantAsked := 0
	if runtime.GOOS == "windows" {
		wantAsked = 1
	}
	if asked != wantAsked {
		t.Errorf("bridge logs asked the install check %d times, want %d", asked, wantAsked)
	}
	if runtime.GOOS == "windows" {
		if got := hostLogs().dirs.ServiceLog(); got != filepath.Join(root, "ProgramData", "1-bit-bridge", "bridge.log") {
			t.Errorf("the host's service log is %q, not under the PROGRAMDATA given", got)
		}
	}
}

// TestTheConsolesLogIsTheFileItsProcessWrites is backlog B618's console half:
// inside the Windows service the console's export offered DefaultLogPath,
// LocalSystem's under C:\Windows\System32\config\systemprofile, while the
// service wrote %PROGRAMDATA%'s. Through bridge logs' own choice, with
// whether the process runs as the service in place of the install check, it
// offers the service's log inside the service, the user's otherwise, and
// DefaultLogPath off Windows, "" where that path cannot be made.
func TestTheConsolesLogIsTheFileItsProcessWrites(t *testing.T) {
	w := newWindowsLogsOnThisHost(t, nothingInstalled)
	asService := w.host
	asService.isService = func() bool { return true }
	if got := consoleLogPath(asService); got != w.service {
		t.Errorf("inside the service the console's log is %q, want the service's, %q", got, w.service)
	}
	notService := w.host
	notService.isService = func() bool { return false }
	if got := consoleLogPath(notService); got != w.user {
		t.Errorf("outside the service the console's log is %q, want the user's, %q", got, w.user)
	}
	if got := consoleLogPath(w.host); got != w.user {
		t.Errorf("with no service check the console's log is %q, want the user's, %q", got, w.user)
	}
	homeless := windowsLogsHost(packaging.WindowsLogDirs{
		ProgramData: filepath.Join(t.TempDir(), "ProgramData"),
		Home:        func() (string, error) { return "", errors.New("%USERPROFILE% is not defined") },
		Join:        filepath.Join,
	}, nil)
	if got := consoleLogPath(homeless); got != "" {
		t.Errorf("with no user's path to be made the console's log is %q, want none", got)
	}
	homeless.isService = func() bool { return true }
	if got, want := consoleLogPath(homeless), homeless.dirs.ServiceLog(); got != want {
		t.Errorf("inside the service with no home the console's log is %q, want the service's, %q", got, want)
	}
	for _, goos := range []string{"darwin", "linux"} {
		elsewhere := logsHost{goos: goos, defaultLog: func() (string, error) { return "/the/default.log", nil },
			isService: func() bool { return true }}
		if got := consoleLogPath(elsewhere); got != "/the/default.log" {
			t.Errorf("on %s the console's log is %q, want DefaultLogPath's", goos, got)
		}
		elsewhere.defaultLog = func() (string, error) { return "", errors.New("$HOME is not defined") }
		if got := consoleLogPath(elsewhere); got != "" {
			t.Errorf("on %s with no DefaultLogPath the console's log is %q, want none", goos, got)
		}
	}
}

// windowsJoin joins as Windows's filepath.Join joins the clean elements these
// tests give it, on any host: with one backslash between them. A host's own
// filepath.Join makes no Windows path off Windows.
func windowsJoin(elem ...string) string {
	return strings.Join(elem, `\`)
}

// TestTheWindowsLogChoiceInWindowsPaths holds bridge logs' and the console's
// choice to Windows's path forms, the paths made with a Windows joiner on any
// host and with Windows's filepath.Join too on Windows: the service's log
// under %PROGRAMDATA% or C:\ProgramData while the service is registered or
// the check cannot tell, the user's under %LOCALAPPDATA% or the home's
// AppData\Local otherwise, and the service's chosen with no home to be read.
func TestTheWindowsLogChoiceInWindowsPaths(t *testing.T) {
	joiners := map[string]func(elem ...string) string{"a Windows joiner": windowsJoin}
	if runtime.GOOS == "windows" {
		joiners["filepath.Join"] = filepath.Join
	}
	errHome := errors.New("%USERPROFILE% is not defined")
	const (
		service    = `C:\ProgramData\1-bit-bridge\bridge.log`
		serviceOnD = `D:\Shared\1-bit-bridge\bridge.log`
		user       = `C:\Users\me\AppData\Local\1-bit-bridge\bridge.log`
	)
	for name, join := range joiners {
		for _, c := range []struct {
			name                  string
			service               packaging.ServiceRegistration
			programData, localApp string
			homeErr               error
			read, other           string
			readErr, otherErr     error
		}{
			{name: "registered", service: packaging.ServiceRegistered,
				programData: `C:\ProgramData`, localApp: `C:\Users\me\AppData\Local`, read: service, other: user},
			{name: "registered, no environment", service: packaging.ServiceRegistered, read: service, other: user},
			{name: "registered, PROGRAMDATA elsewhere", service: packaging.ServiceRegistered,
				programData: `D:\Shared`, read: serviceOnD, other: user},
			{name: "registered, no home", service: packaging.ServiceRegistered, homeErr: errHome,
				read: service, otherErr: errHome},
			{name: "not registered", service: packaging.ServiceNotRegistered, read: user, other: service},
			{name: "not registered, no home", service: packaging.ServiceNotRegistered, homeErr: errHome,
				readErr: errHome, other: service},
			{name: "cannot tell", service: packaging.ServiceRegistrationUnknown, read: service, other: user},
		} {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				dirs := packaging.WindowsLogDirs{
					ProgramData:  c.programData,
					LocalAppData: c.localApp,
					Home: func() (string, error) {
						if c.homeErr != nil {
							return "", c.homeErr
						}
						return `C:\Users\me`, nil
					},
					Join: join,
				}
				got := chooseWindowsLog(c.service, dirs)
				if got.read != c.read || !errors.Is(got.readErr, c.readErr) || got.other != c.other || !errors.Is(got.otherErr, c.otherErr) {
					t.Errorf("chooseWindowsLog = %+v, want read %q (%v), other %q (%v)", got, c.read, c.readErr, c.other, c.otherErr)
				}
				if c.service == packaging.ServiceRegistrationUnknown {
					return
				}
				console := windowsLogsHost(dirs, nil)
				console.isService = func() bool { return c.service == packaging.ServiceRegistered }
				if got := consoleLogPath(console); got != c.read {
					t.Errorf("the console's log is %q, want %q", got, c.read)
				}
			})
		}
	}
}
