package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// TestBridgeLogsOffWindowsIsAsItWas is a guard: on macOS and Linux bridge
// logs reads DefaultLogPath and says what it always said, word for word, but
// for the hint where that file is not there, which since 2026-11-18 names no
// `bridge serve` run in the foreground, which writes no log file: it says the
// service manager creates the file where the bridge's service writes it, and
// that a systemd unit sending the bridge's output to the journal is read with
// journalctl, in one sentence on both systems; and it asks nothing of a
// Windows install.
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
					"  where the bridge's service writes this file, the service manager creates it when it starts the service, "+
					"so run `bridge start` and retry; where a systemd unit sends the bridge's output to the journal instead, "+
					"read it with journalctl.\n")

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

// TestNoProductionCodeReassignsTheLogsInstallCheck: logsInstallCheck is the
// entry points' seam, which their tests set
// (TestBridgeLogsReadsTheHostsLogThroughItsEntryPoint,
// TestBridgeDoctorGradesTheHostsLogThroughItsEntryPoint), and production asks
// the check its one declaration names, packaging.CheckWindowsInstall: no Go
// file of this package but a test assigns it or takes its address, and that
// declaration writes it no type but, at most, an unnamed func type, which has
// no methods: any other type written (a named type, a selector, a generic
// instantiation) may be a defined type, whose pointer methods would change it
// with no & written. The walk reads every file the go tool builds here,
// whatever its platform, and must find that declaration. It reads each file
// and its source through seamFindingsIn, which
// TestTheLogsInstallCheckPinSeesEachWayToChangeIt holds to planted sources,
// each parsed with its identifiers resolved by the file's lexical scopes, so
// that a local of the seam's name is told from the package's variable (a type
// switch's case type lists, which the parser resolves where the switch's
// guard is declared, read past that guard): go/types would need the package
// type-checked, and every platform's files together do not check as one
// package.
func TestNoProductionCodeReassignsTheLogsInstallCheck(t *testing.T) {
	const seam = "logsInstallCheck"
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var declared []token.Position
	var changed []string
	parsed := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		decls, findings := seamFindingsIn(fset, f, src, seam)
		declared = append(declared, decls...)
		for _, c := range findings {
			switch c.kind {
			case seamDeclaredAsAnother:
				t.Errorf("%s declares %s as something other than packaging.CheckWindowsInstall", c.pos, seam)
			case seamDeclaredWithAType:
				t.Errorf("%s declares %s with a written type other than an unnamed func type (a named type, a "+
					"selector or a generic instantiation), which may have a method with a pointer receiver that "+
					"changes it, its call taking the address with no &; declared with no type, or with an unnamed "+
					"func type, its type has no methods", c.pos, seam)
			default:
				changed = append(changed, c.String())
			}
		}
	}
	if parsed < 20 || len(declared) != 1 {
		t.Fatalf("parsed %d files and found %s declared at %q, want once: the walk no longer reads the "+
			"package's own declaration of it, so it proves nothing", parsed, seam, declared)
	}
	for _, c := range changed {
		t.Errorf("%s: production code changes %s, which only a test of an entry point may set; production "+
			"asks packaging.CheckWindowsInstall, its declaration's", c, seam)
	}
}

// seamFinding is one place where a file declares the logs install check's
// seam otherwise than logs.go declares it, or changes it.
type seamFinding struct {
	pos  token.Position
	kind seamFindingKind
}

// String is the finding as the pin reports a change: its position, then what
// the file does there.
func (f seamFinding) String() string {
	return f.pos.String() + " " + string(f.kind)
}

// seamFindingKind is what a seamFinding found, in the words the pin reports a
// change in.
type seamFindingKind string

// The kinds of seamFinding.
const (
	// seamDeclaredAsAnother is a declaration of the seam whose value is not
	// packaging.CheckWindowsInstall, or that gives it none.
	seamDeclaredAsAnother seamFindingKind = "declares it as something other than packaging.CheckWindowsInstall"
	// seamDeclaredWithAType is a declaration that writes the seam a type other
	// than an unnamed func type (a named type, a selector pkg.Type, a generic
	// instantiation): such a type may be a defined type, whose method with a
	// pointer receiver can change the seam, a call of that method taking its
	// address with no & written. An unnamed func type, bare or in
	// parentheses, has no methods and is no finding (isUnnamedFuncType).
	seamDeclaredWithAType seamFindingKind = "declares it with a written type other than an unnamed func type"
	// seamAssigned is the package's seam on the left of an assignment, or as
	// a range clause's key or value. A := there declares a local of the
	// seam's name, never the seam, and is no finding.
	seamAssigned seamFindingKind = "assigns it"
	// seamAddressTaken is the package's seam as the operand of &.
	seamAddressTaken seamFindingKind = "takes its address"
)

// seamFindingsIn reads one parsed file for the logs install check's seam, the
// package variable named seam: where the file declares it, and each finding,
// in the file's order. f must be parsed from src with its identifiers resolved
// (no parser.SkipObjectResolution): the parser resolves each one by the file's
// lexical scopes, and an identifier is the seam only where it resolves to the
// package's variable (isSeamIdent). A local of the seam's name, declared by :=
// or var, as a parameter, a result or a receiver, or by a range, type switch
// or select clause, resolves to its own declaration, so its writes and its
// address are no finding; past its scope, and on the right of the declaration
// that makes it, where that scope has not begun, the name is the package's
// variable again. A type switch's case type lists lie outside the scope of the
// variable its guard declares, which the language declares in each clause's
// body alone, while the parser resolves them where it declared that guard:
// there an identifier is read past the guard (caseListBindings). Only the
// file's top-level var specs declare the seam: a top-level type, const or func
// of its name, or an import, is no variable, no write to it or its address
// compiles, and none of them compiles beside logs.go's declaration, so it is
// no declaration of the seam and no finding. An identifier is read with its
// parentheses taken off, so `(seam) = f` and `&(seam)` are `seam = f` and
// `&seam`.
func seamFindingsIn(fset *token.FileSet, f *ast.File, src []byte, seam string) (declared []token.Position, findings []seamFinding) {
	if f.Scope == nil {
		panic(fset.Position(f.Pos()).Filename + " was parsed with parser.SkipObjectResolution: its identifiers are not resolved to their scopes")
	}
	pkg := importNameOf(f, "github.com/acoseac/1-bit-bridge/internal/packaging")
	// The package's variable as this file's identifiers resolve to it: the
	// object of the file's own top-level declaration where it declares the
	// seam, and nil where another file does, since the parser leaves an
	// identifier no scope of the file declares unresolved. (In a file that
	// declares a type, const or func of its name at top level instead, which
	// does not compile beside the seam, it is that declaration's object.)
	obj := f.Scope.Lookup(seam)
	rebound := caseListBindings(fset, f, src, seam)
	topLevel := map[*ast.ValueSpec]bool{}
	for _, d := range f.Decls {
		if gen, ok := d.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			for _, spec := range gen.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					topLevel[vs] = true
				}
			}
		}
	}
	found := func(at ast.Node, kind seamFindingKind) {
		findings = append(findings, seamFinding{pos: fset.Position(at.Pos()), kind: kind})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			// A function's var or const declares a local of its own, and a
			// top-level const no variable.
			if !topLevel[n] {
				return true
			}
			for i, id := range n.Names {
				if id.Name != seam {
					continue
				}
				declared = append(declared, fset.Position(id.Pos()))
				if pkg == "" || len(n.Values) != len(n.Names) || !isSelector(n.Values[i], pkg, "CheckWindowsInstall") {
					found(id, seamDeclaredAsAnother)
				}
				if n.Type != nil && !isUnnamedFuncType(n.Type) {
					found(id, seamDeclaredWithAType)
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if isSeamIdent(lhs, seam, obj, rebound) {
					found(lhs, seamAssigned)
				}
			}
		case *ast.RangeStmt:
			for _, target := range []ast.Expr{n.Key, n.Value} {
				if isSeamIdent(target, seam, obj, rebound) {
					found(target, seamAssigned)
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND && isSeamIdent(n.X, seam, obj, rebound) {
				found(n.X, seamAddressTaken)
			}
		}
		return true
	})
	return declared, findings
}

// isSeamIdent reports whether e, its parentheses taken off, is an identifier
// named seam that is the package's variable: as rebound reads it, where the
// parser bound it to a type switch's guard from that switch's case type lists
// (caseListBindings), and otherwise where the parser resolved it to obj, the
// package's variable as seamFindingsIn finds it in the file. An identifier
// that a local of that name declares, or that names one, holds the local's
// object instead. A nil e, the key or the value a range clause leaves out, is
// not.
func isSeamIdent(e ast.Expr, seam string, obj *ast.Object, rebound map[*ast.Ident]bool) bool {
	if e == nil {
		return false
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok || id.Name != seam {
		return false
	}
	if isSeam, ok := rebound[id]; ok {
		return isSeam
	}
	return id.Obj == obj
}

// caseListBindings reads each identifier of the seam's name that the parser
// binds where the language does not: one in a type switch's case type list
// that the parser bound to the guard of a switch whose lists hold it
// (caseListGuards). The language declares a guard's variable in each clause's
// body alone, while go/parser's resolver walks a clause's list in the scope
// where it declared the guard (resolver.go, TypeSwitchStmt and CaseClause).
// It maps each such identifier to whether it is the package's variable: the
// parser's answer for src parsed again with those guards' names blanked, so
// that they declare nothing and the parser resolves the name in the scopes
// around their switches, the switch's initializer, an enclosing function's
// parameters and locals and an outer type switch's case variable among them.
// An identifier a declaration inside the list binds, a func literal's
// parameter there, is bound as the language binds it and is left out.
func caseListBindings(fset *token.FileSet, f *ast.File, src []byte, seam string) map[*ast.Ident]bool {
	held := map[*ast.Ident][]*ast.Ident{}
	caseListGuards(f, seam, nil, held)
	name := fset.File(f.Pos()).Name()
	rebound := map[*ast.Ident]bool{}
	for id, guards := range held {
		if !slices.ContainsFunc(guards, func(g *ast.Ident) bool { return g.Obj == id.Obj }) {
			continue
		}
		blanked := bytes.Clone(src)
		for _, g := range guards {
			at := fset.Position(g.Pos()).Offset
			if at+len(seam) > len(src) || string(src[at:at+len(seam)]) != seam {
				panic(name + ": the source given is not the one its file was parsed from")
			}
			copy(blanked[at:], "_"+strings.Repeat(" ", len(seam)-1))
		}
		again := token.NewFileSet()
		f2, err := parser.ParseFile(again, name, blanked, 0)
		if err != nil {
			panic(fmt.Sprintf("%s does not parse with a type switch's guard blanked: %v", name, err))
		}
		at := fset.Position(id.Pos()).Offset
		var same *ast.Ident
		ast.Inspect(f2, func(n ast.Node) bool {
			if x, ok := n.(*ast.Ident); ok && again.Position(x.Pos()).Offset == at {
				same = x
			}
			return same == nil
		})
		if same == nil || same.Name != seam {
			panic(fmt.Sprintf("%s parsed again holds no %s at offset %d", name, seam, at))
		}
		rebound[id] = same.Obj == f2.Scope.Lookup(seam)
	}
	return rebound
}

// caseListGuards maps in out each identifier named seam below root that lies
// in the case type list of a type switch whose guard declares that name, to
// the guards of every such switch whose lists hold it, the innermost last:
// held is the guards of the lists root lies in. A switch's initializer, its
// guard and its clauses' bodies lie in none of its lists.
func caseListGuards(root ast.Node, seam string, held []*ast.Ident, out map[*ast.Ident][]*ast.Ident) {
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == seam && len(held) > 0 {
				out[n] = held
			}
		case *ast.TypeSwitchStmt:
			inLists := held
			if guard, ok := n.Assign.(*ast.AssignStmt); ok && len(guard.Lhs) == 1 {
				if id, ok := guard.Lhs[0].(*ast.Ident); ok && id.Name == seam {
					inLists = append(slices.Clip(held), id)
				}
			}
			if n.Init != nil {
				caseListGuards(n.Init, seam, held, out)
			}
			caseListGuards(n.Assign, seam, held, out)
			for _, s := range n.Body.List {
				clause := s.(*ast.CaseClause)
				for _, e := range clause.List {
					caseListGuards(e, seam, inLists, out)
				}
				for _, s := range clause.Body {
					caseListGuards(s, seam, held, out)
				}
			}
			return false
		}
		return true
	})
}

// isUnnamedFuncType reports whether a declaration's written type e, its
// parentheses taken off, is a func type written out, as
// `func() packaging.WindowsInstall` is: an unnamed type, on which no method
// can be declared (a method's receiver is a defined type), so a variable of
// it has no method with a pointer receiver to change it.
func isUnnamedFuncType(e ast.Expr) bool {
	_, ok := ast.Unparen(e).(*ast.FuncType)
	return ok
}

// TestTheLogsInstallCheckPinSeesEachWayToChangeIt holds seamFindingsIn, the
// reading TestNoProductionCodeReassignsTheLogsInstallCheck gives each file of
// the package, to planted files (backlog B618FU), each declaring the seam once,
// as logs.go does, but where its row is about the declaration or about a file
// that does not declare it (elsewhere), as every other file of the package.
// Each way a file can change the seam is one finding of its kind on the line
// planted: the five the pin read past before B618FU (an assignment and an
// address taken through parentheses, a range clause's value over a slice and
// its key over an iter.Seq, and a declaration of a named type whose pointer
// method sets it, called with no & written) and the three it caught (an
// assignment, an address taken, a declaration of another value). A file that
// only reads the seam, beside a named type whose pointer method sets another
// variable, has none, and so has a file that declares it with an unnamed func
// type written, bare or in parentheses, and only reads it: that type has no
// methods. And a local of the seam's name changes nothing of the seam (a :=, a
// range clause's :=, a later = to it, its address, a var, a parameter, a
// result, a func literal's parameter, a type switch's or a select case's
// define), with the seam declared in the file or not; while a write after such
// a local's scope ends, and a write or the seam's address on the right of the
// := that declares the local, whose scope has not begun there, are one finding
// each. In a type switch's case type list, which the variable its guard
// declares does not reach, the seam's address is one finding, the seam
// declared in the file or not, and so it is in the list of a type switch
// inside another's; while there the address of a parameter, a local, the
// switch's initializer's local or an outer type switch's case variable of its
// name is that local's, and no finding. A top-level type, const or func of its
// name, or an import, in a file that does not declare the seam, is no
// declaration of it and no finding, and so is a composite literal's key of its
// name, while its address in the literal's value is one.
func TestTheLogsInstallCheckPinSeesEachWayToChangeIt(t *testing.T) {
	const header = `package main

import (
	"iter"
	"unsafe"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

`
	const asLogsGo = "var logsInstallCheck = packaging.CheckWindowsInstall\n\n"
	for _, c := range []struct {
		name string
		src  string          // the file after its imports
		want seamFindingKind // the one finding's, on the line marked "// planted"; "" is none
		// elsewhere is a file that does not declare the seam, as every file of
		// the package but logs.go: its identifier of the seam's name that no
		// scope of the file declares is the package's variable.
		elsewhere bool
	}{
		{name: "an assignment through parentheses", want: seamAssigned, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	(logsInstallCheck) = f // planted
}
`},
		{name: "an address taken through parentheses", want: seamAddressTaken, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	p := &(logsInstallCheck) // planted
	*p = f
}
`},
		{name: "a range clause's value over a slice", want: seamAssigned, src: asLogsGo + `func plant(fs []func() packaging.WindowsInstall) {
	for _, logsInstallCheck = range fs { // planted
	}
}
`},
		{name: "a range clause's key over an iter.Seq", want: seamAssigned, src: asLogsGo + `func plant(seq iter.Seq[func() packaging.WindowsInstall]) {
	for logsInstallCheck = range seq { // planted
	}
}
`},
		{name: "a declaration of a named type whose pointer method sets it", want: seamDeclaredWithAType, src: `type installCheck func() packaging.WindowsInstall

func (c *installCheck) set(f installCheck) { *c = f }

var logsInstallCheck installCheck = packaging.CheckWindowsInstall // planted

func plant(f installCheck) {
	logsInstallCheck.set(f)
}
`},
		{name: "an assignment", want: seamAssigned, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck = f // planted
}
`},
		{name: "an address taken", want: seamAddressTaken, src: asLogsGo + `func plant() *func() packaging.WindowsInstall {
	return &logsInstallCheck // planted
}
`},
		{name: "a declaration of another value", want: seamDeclaredAsAnother, src: `var logsInstallCheck = func() packaging.WindowsInstall { return packaging.WindowsInstall{} } // planted
`},
		{name: "reads alone", src: asLogsGo + `type installCheck func() packaging.WindowsInstall

func (c *installCheck) set(f installCheck) { *c = f }

func plant(f installCheck) bool {
	inst := logsInstallCheck()
	check := logsInstallCheck
	var other installCheck = check
	other.set(f)
	return inst.Startup && logsInstallCheck != nil
}
`},
		{name: "a declaration with an unnamed func type written", src: `var logsInstallCheck func() packaging.WindowsInstall = packaging.CheckWindowsInstall

func plant() bool {
	return logsInstallCheck().Startup && logsInstallCheck != nil
}
`},
		{name: "a declaration with an unnamed func type written in parentheses", src: `var logsInstallCheck (func() packaging.WindowsInstall) = packaging.CheckWindowsInstall

func plant() bool {
	return logsInstallCheck().Startup && logsInstallCheck != nil
}
`},
		{name: "a local := that shadows it", src: asLogsGo + `func plant(f func() packaging.WindowsInstall) bool {
	logsInstallCheck := f
	return logsInstallCheck().Startup
}
`},
		{name: "a local range := that shadows it", src: asLogsGo + `func plant(fs []func() packaging.WindowsInstall) {
	for _, logsInstallCheck := range fs {
		_ = logsInstallCheck()
	}
}
`},
		{name: "a later = to that shadow", src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck := f
	logsInstallCheck = packaging.CheckWindowsInstall
	_ = logsInstallCheck()
}
`},
		{name: "the address of a shadow", src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck := packaging.CheckWindowsInstall
	p := &logsInstallCheck
	*p = f
	_ = logsInstallCheck()
}
`},
		{name: "a local var declaration that shadows it", src: asLogsGo + `type installCheck func() packaging.WindowsInstall

func plant(f installCheck) {
	var logsInstallCheck installCheck = f
	logsInstallCheck = f
	_ = logsInstallCheck()
}
`},
		{name: "a parameter, a result and a func literal's parameter that shadow it", src: asLogsGo + `func plant(logsInstallCheck func() packaging.WindowsInstall) {
	logsInstallCheck = nil
	_ = &logsInstallCheck
}

func plantResult() (logsInstallCheck func() packaging.WindowsInstall) {
	logsInstallCheck = packaging.CheckWindowsInstall
	return
}

var plantLiteral = func(logsInstallCheck func() packaging.WindowsInstall) {
	logsInstallCheck = nil
}
`},
		{name: "a type switch's and a select case's defines that shadow it", src: asLogsGo + `func plant(v any, ch chan func() packaging.WindowsInstall) {
	switch logsInstallCheck := v.(type) {
	case func() packaging.WindowsInstall:
		logsInstallCheck = nil
		_ = &logsInstallCheck
	}
	select {
	case logsInstallCheck := <-ch:
		logsInstallCheck = nil
		_ = &logsInstallCheck
	default:
	}
}
`},
		{name: "a write after a shadow's scope ends", want: seamAssigned, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	if logsInstallCheck := f; logsInstallCheck != nil {
		logsInstallCheck = f
	}
	{
		var logsInstallCheck = f
		logsInstallCheck = f
		_ = logsInstallCheck
	}
	for _, logsInstallCheck := range []func() packaging.WindowsInstall{f} {
		logsInstallCheck = f
		_ = logsInstallCheck
	}
	logsInstallCheck = f // planted
}
`},
		{name: "its address taken on the right of a define that shadows it", want: seamAddressTaken, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck := &logsInstallCheck // planted
	*logsInstallCheck = f
}
`},
		{name: "a write on the right of a define that shadows it", want: seamAssigned, src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck := func() {
		logsInstallCheck = f // planted
	}
	logsInstallCheck()
}
`},
		{name: "a write in a file that does not declare it, after a shadow's scope ends", want: seamAssigned, elsewhere: true, src: `func plant(f func() packaging.WindowsInstall) {
	{
		logsInstallCheck := f
		logsInstallCheck = f
		_ = logsInstallCheck
	}
	logsInstallCheck = f // planted
}
`},
		{name: "a shadow in a file that does not declare it", elsewhere: true, src: `func plant(f func() packaging.WindowsInstall) {
	logsInstallCheck := f
	logsInstallCheck = f
	_ = &logsInstallCheck
}
`},
		{name: "its address in a type switch's case type list", want: seamAddressTaken, src: asLogsGo + `func plant(v any) {
	switch logsInstallCheck := v.(type) {
	case [unsafe.Sizeof(&logsInstallCheck)]byte: // planted
		_ = &logsInstallCheck
	}
}
`},
		{name: "its address in a type switch's case type list, in a file that does not declare it", want: seamAddressTaken, elsewhere: true, src: `func plant(v any) {
	switch logsInstallCheck := v.(type) {
	case [unsafe.Sizeof(&logsInstallCheck)]byte: // planted
		_ = &logsInstallCheck
	}
}
`},
		{name: "its address in the case type list of a type switch inside another's", want: seamAddressTaken, src: asLogsGo + `func plant(v, w any) {
	switch logsInstallCheck := v.(type) {
	case [unsafe.Sizeof(func() {
		switch logsInstallCheck := w.(type) {
		case [unsafe.Sizeof(&logsInstallCheck)]byte: // planted
			_ = &logsInstallCheck
		}
	})]byte:
		_ = &logsInstallCheck
	}
}
`},
		{name: "a parameter's address in a type switch's case type list", src: asLogsGo + `func plant(logsInstallCheck func() packaging.WindowsInstall, v any) {
	switch logsInstallCheck := v.(type) {
	case [unsafe.Sizeof(&logsInstallCheck)]byte:
		_ = &logsInstallCheck
	}
}
`},
		{name: "a local's address in a type switch's case type list", src: asLogsGo + `func plant(f func() packaging.WindowsInstall, v any) {
	logsInstallCheck := f
	switch logsInstallCheck := v.(type) {
	case [unsafe.Sizeof(&logsInstallCheck)]byte:
		_ = &logsInstallCheck
	}
	_ = logsInstallCheck
}
`},
		{name: "the address of the switch's initializer's local in its case type list", src: asLogsGo + `func plant(f func() packaging.WindowsInstall) {
	switch logsInstallCheck := f; logsInstallCheck := any(logsInstallCheck).(type) {
	case [unsafe.Sizeof(&logsInstallCheck)]byte:
		_ = &logsInstallCheck
	}
}
`},
		{name: "the address of an outer type switch's case variable in an inner one's case type list", src: asLogsGo + `func plant(v any) {
	switch logsInstallCheck := v.(type) {
	case func() packaging.WindowsInstall:
		switch logsInstallCheck := any(logsInstallCheck).(type) {
		case [unsafe.Sizeof(&logsInstallCheck)]byte:
			_ = &logsInstallCheck
		}
	}
}
`},
		{name: "a top-level type of its name, in a file that does not declare it", elsewhere: true, src: `type logsInstallCheck func() packaging.WindowsInstall

var plantCheck logsInstallCheck = packaging.CheckWindowsInstall
`},
		{name: "a top-level const of its name, in a file that does not declare it", elsewhere: true, src: `const logsInstallCheck = "the logs install check"

var plantName = logsInstallCheck
`},
		{name: "a top-level func of its name, in a file that does not declare it", elsewhere: true, src: `func logsInstallCheck() packaging.WindowsInstall {
	return packaging.CheckWindowsInstall()
}

var plantInstall = logsInstallCheck()
`},
		{name: "an import of its name, in a file that does not declare it", elsewhere: true, src: `import logsInstallCheck "fmt"

func plant() {
	logsInstallCheck.Println()
}
`},
		{name: "a composite literal's key of its name", src: asLogsGo + `type plantHolder struct{ logsInstallCheck func() packaging.WindowsInstall }

func plant(f func() packaging.WindowsInstall) plantHolder {
	return plantHolder{logsInstallCheck: f}
}
`},
		{name: "its address in a composite literal's value", want: seamAddressTaken, src: asLogsGo + `type plantHolder struct{ logsInstallCheck func() packaging.WindowsInstall }

func plant() plantHolder {
	return plantHolder{logsInstallCheck: *(&logsInstallCheck)} // planted
}
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := header + c.src
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "planted.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			declared, findings := seamFindingsIn(fset, f, []byte(src), "logsInstallCheck")
			wantDeclared := 1 // once, as logs.go declares it
			if c.elsewhere {
				wantDeclared = 0
			}
			if len(declared) != wantDeclared {
				t.Errorf("found logsInstallCheck declared at %q, want %d declaration(s)", declared, wantDeclared)
			}
			if c.want == "" {
				if len(findings) != 0 {
					t.Errorf("a file that leaves the package's seam as it is has findings %q, want none", findings)
				}
				return
			}
			marker := strings.Index(src, "// planted")
			if marker < 0 {
				t.Fatal("the row marks no line planted")
			}
			line := 1 + strings.Count(src[:marker], "\n")
			if len(findings) != 1 || findings[0].kind != c.want || findings[0].pos.Line != line {
				t.Errorf("findings %q, want one that %s, on line %d", findings, c.want, line)
			}
		})
	}
}

// TestTheConsolesLogIsTheFileItsProcessWrites is backlog B618's console half,
// for a console whose process runs as the installed bridge: the file that
// process writes (one run in the foreground writes none, and the file named
// may then be an earlier launch's). Inside the Windows service the console's
// export offered DefaultLogPath, LocalSystem's under
// C:\Windows\System32\config\systemprofile, while the service wrote
// %PROGRAMDATA%'s. Through bridge logs' own choice, with whether the process
// runs as the service in place of the install check, it offers the service's
// log inside the service, the user's otherwise, and DefaultLogPath off
// Windows, "" where that path cannot be made.
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
