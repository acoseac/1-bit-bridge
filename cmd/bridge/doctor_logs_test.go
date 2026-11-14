package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// doctorsLogLine runs a doctor's report over no config (isolateConfigEnv), its
// log-file-size line grading the log logs chooses, and answers that line.
func doctorsLogLine(t *testing.T, logs doctorLogs) doctor.Check {
	t.Helper()
	isolateConfigEnv(t)
	return findCheck(t, doctor.Run(context.Background(), buildDoctorDepsWith("", false, doctorDACHost(), logs)), "log-file-size")
}

// consolesLogLine runs the console's doctor as serve wires it,
// adminDoctorRunnerFor, over the serving process's DAC host, the logs host
// given and no config (isolateConfigEnv), and answers the log-file-size line
// of the report it hands the console.
func consolesLogLine(t *testing.T, host logsHost) doctor.Check {
	t.Helper()
	isolateConfigEnv(t)
	for _, c := range adminDoctorRunnerFor("", nil, nil, servingDACHost(hostTrees, nil), host)(context.Background()).Checks {
		if c.Name == "log-file-size" {
			return doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
		}
	}
	t.Fatal("the console's doctor has no log-file-size line")
	return doctor.Check{}
}

// gradedAs is the log-file-size line's summary for a log at path holding body.
func gradedAs(body, path string) string {
	return fmt.Sprintf("%d B (%s)", len(body), path)
}

// terminalsOK is the log-file-size line's summary where no log is named: a
// bridge that logs to its terminal. It is the line's still, as before round 2,
// where a log's path cannot be made off Windows or in the console's doctor.
const terminalsOK = "no log file configured (logs go to the terminal)"

// requireLogLine fails the test unless the log-file-size line is status
// summary, with hint under it. Where the line wanted names a log or says why
// none could be named, the terminal's ok fails it in words of its own too.
func requireLogLine(t *testing.T, got doctor.Check, status doctor.Status, summary, hint string) {
	t.Helper()
	if got.Status != status || got.Summary != summary || got.Hint != hint {
		t.Errorf("the log-file-size line is %s %q (hint %q),\nwant %s %q (hint %q)", got.Status, got.Summary, got.Hint,
			status, summary, hint)
	}
	if summary != terminalsOK && got.Summary == terminalsOK {
		t.Errorf("the line reads as a bridge that logs to its terminal")
	}
}

// TestBridgeDoctorFromAShellGradesTheServicesLogWhileItIsRegistered is backlog
// B618's round 2: bridge doctor, run from a shell, is no service, so it graded
// the user's log under %LOCALAPPDATA% on a Windows service install, where
// `bridge logs` and the console inside the service read the service's under
// %PROGRAMDATA%. It grades the log `bridge logs` reads: the service's while
// the service is registered, the sign-in launcher there too or not.
func TestBridgeDoctorFromAShellGradesTheServicesLogWhileItIsRegistered(t *testing.T) {
	for _, inst := range []packaging.WindowsInstall{
		serviceRegistered,
		{Service: packaging.ServiceRegistered, Startup: true},
	} {
		t.Run(fmt.Sprintf("launcher %v", inst.Startup), func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, inst)
			writeLogFile(t, w.service, serviceLogLines)
			writeLogFile(t, w.user, userLogLines)
			requireLogLine(t, doctorsLogLine(t, doctorLogs{host: w.host}), doctor.OK, gradedAs(serviceLogLines, w.service), "")
		})
	}
}

// TestBridgeDoctorFromAShellGradesTheUserLogOfAStartupFolderInstall is a
// guard, green before round 2: with the sign-in launcher installed and the
// service not, bridge doctor grades the user's log, which the launcher
// redirects to.
func TestBridgeDoctorFromAShellGradesTheUserLogOfAStartupFolderInstall(t *testing.T) {
	w := newWindowsLogsOnThisHost(t, launcherInstalled)
	writeLogFile(t, w.service, serviceLogLines)
	writeLogFile(t, w.user, userLogLines)
	requireLogLine(t, doctorsLogLine(t, doctorLogs{host: w.host}), doctor.OK, gradedAs(userLogLines, w.user), "")
}

// TestBridgeDoctorFromAShellSaysWhereTheCheckCannotTell: where the install
// check cannot tell whether the service is registered, bridge doctor grades
// the log `bridge logs` reads then, the service's where it is there and the
// user's where only that is, and says so after the line's summary, ok or not;
// with neither there it says that, a warn, never an ok. A host with no install
// check reads as one that cannot tell.
func TestBridgeDoctorFromAShellSaysWhereTheCheckCannotTell(t *testing.T) {
	errCheck := errors.New("connect to the service manager: The RPC server is unavailable.")
	uncertain := "cannot tell whether the bridge's Windows service is installed (" + errCheck.Error() + ")"
	for _, c := range []struct {
		name                string
		serviceLog, userLog bool
		status              doctor.Status
		summary, hint       func(w windowsLogsOnThisHost) string
	}{
		{name: "both logs there", serviceLog: true, userLog: true, status: doctor.OK,
			summary: func(w windowsLogsOnThisHost) string {
				return gradedAs(serviceLogLines, w.service) + "; " + uncertain + ", so this is its log, the one `bridge logs` reads"
			}},
		{name: "the service's log alone", serviceLog: true, status: doctor.OK,
			summary: func(w windowsLogsOnThisHost) string {
				return gradedAs(serviceLogLines, w.service) + "; " + uncertain + ", so this is its log, the one `bridge logs` reads"
			}},
		{name: "the user's log alone", userLog: true, status: doctor.OK,
			summary: func(w windowsLogsOnThisHost) string {
				return gradedAs(userLogLines, w.user) + "; " + uncertain + ", and its log, " + w.service +
					", is not there, so this is the one `bridge logs` reads"
			}},
		{name: "neither log", status: doctor.Warn,
			summary: func(w windowsLogsOnThisHost) string {
				return "not checked: neither " + w.service + " nor " + w.user + " exists"
			},
			hint: func(windowsLogsOnThisHost) string {
				return uncertain + "; the service writes the first once it starts, and a bridge its sign-in launcher starts the second"
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, packaging.WindowsInstall{ServiceErr: errCheck})
			if c.serviceLog {
				writeLogFile(t, w.service, serviceLogLines)
			}
			if c.userLog {
				writeLogFile(t, w.user, userLogLines)
			}
			hint := ""
			if c.hint != nil {
				hint = c.hint(w)
			}
			requireLogLine(t, doctorsLogLine(t, doctorLogs{host: w.host}), c.status, c.summary(w), hint)
		})
	}
	t.Run("no install check", func(t *testing.T) {
		w := newWindowsLogsOnThisHost(t, packaging.WindowsInstall{})
		w.host.install = nil
		writeLogFile(t, w.service, serviceLogLines)
		requireLogLine(t, doctorsLogLine(t, doctorLogs{host: w.host}), doctor.OK,
			gradedAs(serviceLogLines, w.service)+"; cannot tell whether the bridge's Windows service is installed ("+
				errNoInstallCheck.Error()+"), so this is its log, the one `bridge logs` reads", "")
	})
}

// homelessWindowsLogs is a Windows logsHost whose home directory cannot be
// read, its LOCALAPPDATA unset, so the user's log has no path, and whose
// install check answers inst.
func homelessWindowsLogs(t *testing.T, errHome error, inst packaging.WindowsInstall) logsHost {
	return windowsLogsHost(packaging.WindowsLogDirs{
		ProgramData: filepath.Join(t.TempDir(), "ProgramData"),
		Home:        func() (string, error) { return "", errHome },
		Join:        filepath.Join,
	}, func() packaging.WindowsInstall { return inst })
}

// homelessLogs is a logsHost of goos, off Windows, whose DefaultLogPath
// cannot be made: `env -u HOME bridge doctor`.
func homelessLogs(goos string) logsHost {
	return logsHost{goos: goos, defaultLog: func() (string, error) { return "", errors.New("$HOME is not defined") }}
}

// TestBridgeDoctorSaysWhenTheLogsPathCannotBeMade: on Windows, bridge doctor
// from a shell says why a log's path cannot be made, a warn, never the ok of a
// bridge that logs to its terminal, which the empty path it left once read
// as. That warn is the Windows shell's alone (round 3): off Windows, from a
// shell or in the console, and in the console's doctor on Windows, run as
// serve runs it (adminDoctorRunnerFor), a path that cannot be made names no
// file, the terminal's ok, as before round 2.
func TestBridgeDoctorSaysWhenTheLogsPathCannotBeMade(t *testing.T) {
	errHome := errors.New("%USERPROFILE% is not defined")
	cannotBeMade := "not checked: the log's path cannot be made (" + errHome.Error() + ")"
	t.Run("the service not registered", func(t *testing.T) {
		requireLogLine(t, doctorsLogLine(t, doctorLogs{host: homelessWindowsLogs(t, errHome, launcherInstalled)}), doctor.Warn,
			cannotBeMade, "")
	})
	t.Run("the check cannot tell and the service's log is not there", func(t *testing.T) {
		errCheck := errors.New("connect to the service manager: Access is denied.")
		host := homelessWindowsLogs(t, errHome, packaging.WindowsInstall{ServiceErr: errCheck})
		requireLogLine(t, doctorsLogLine(t, doctorLogs{host: host}), doctor.Warn,
			"not checked: "+host.dirs.ServiceLog()+" is not there, and the other log's path cannot be made ("+errHome.Error()+")",
			"cannot tell whether the bridge's Windows service is installed ("+errCheck.Error()+")")
	})
	t.Run("the console's doctor outside the service", func(t *testing.T) {
		host := homelessWindowsLogs(t, errHome, serviceRegistered)
		host.isService = func() bool { return false }
		requireLogLine(t, consolesLogLine(t, host), doctor.OK, terminalsOK, "")
	})
	for _, goos := range []string{"darwin", "linux"} {
		t.Run("off Windows on "+goos, func(t *testing.T) {
			requireLogLine(t, doctorsLogLine(t, doctorLogs{host: homelessLogs(goos)}), doctor.OK, terminalsOK, "")
			requireLogLine(t, consolesLogLine(t, homelessLogs(goos)), doctor.OK, terminalsOK, "")
		})
	}
}

// TestTheConsolesDoctorGradesItsOwnLogBesideARegisteredService is a guard,
// green before round 2 and kept by it, run since round 3 through the console's
// doctor as serve wires it (adminDoctorRunnerFor), whose console flag is
// production's own: the console's doctor grades the log its own process
// writes, so a console of a bridge run outside the service, beside a
// registered service, grades the user's log, and inside the service the
// service's; it asks no install check, which is bridge doctor's alone.
func TestTheConsolesDoctorGradesItsOwnLogBesideARegisteredService(t *testing.T) {
	for _, inService := range []bool{false, true} {
		t.Run(fmt.Sprintf("inside the service %v", inService), func(t *testing.T) {
			w := newWindowsLogsOnThisHost(t, serviceRegistered)
			writeLogFile(t, w.service, serviceLogLines)
			writeLogFile(t, w.user, userLogLines)
			asked := 0
			w.host.install = func() packaging.WindowsInstall { asked++; return serviceRegistered }
			w.host.isService = func() bool { return inService }
			want := gradedAs(userLogLines, w.user)
			if inService {
				want = gradedAs(serviceLogLines, w.service)
			}
			requireLogLine(t, consolesLogLine(t, w.host), doctor.OK, want, "")
			if asked != 0 {
				t.Errorf("the console's doctor asked the install check %d times; it grades its own process's log", asked)
			}
		})
	}
}

// TestTheDoctorsLogNoteSurvivesItsRendering: what the log-file-size line says
// of a check that cannot tell reaches the reader in bridge doctor's table and
// in its --json: on an ok line, after the summary, which the table prints
// (it prints a hint under a warn or a fail alone); with neither log there, as
// a warn whose hint the table prints too. On Windows a path that cannot be
// made is a warn in both, never the terminal's ok. Off Windows it is that ok
// in both, and in the report the console's doctor hands the console
// (adminDoctorRunnerFor) it is that ok on Windows too, as before round 2.
func TestTheDoctorsLogNoteSurvivesItsRendering(t *testing.T) {
	errCheck := errors.New("connect to the service manager: The RPC server is unavailable.")
	uncertain := "cannot tell whether the bridge's Windows service is installed (" + errCheck.Error() + ")"
	type rendered struct {
		line, hint string
		json       jsonDoctorCheck
	}
	render := func(t *testing.T, host logsHost) rendered {
		t.Helper()
		isolateConfigEnv(t)
		d := buildDoctorDepsWith("", false, doctorDACHost(), doctorLogs{host: host})
		var table, tableErr, js, jsErr bytes.Buffer
		runDoctorReport(d, false, false, &table, &tableErr)
		runDoctorReport(d, true, false, &js, &jsErr)
		var rep jsonDoctorReport
		if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
			t.Fatalf("bridge doctor --json printed no report: %v\n%s", err, js.String())
		}
		r := rendered{line: reportLine(stripANSI(table.String()), "log-file-size"), hint: hintUnder(stripANSI(table.String()), "log-file-size")}
		for _, c := range rep.Checks {
			if c.Name == "log-file-size" {
				r.json = c
			}
		}
		if r.line == "" || r.json.Name == "" {
			t.Fatalf("no log-file-size line: table\n%s\njson\n%s", table.String(), js.String())
		}
		return r
	}
	t.Run("the service's log, noted", func(t *testing.T) {
		w := newWindowsLogsOnThisHost(t, packaging.WindowsInstall{ServiceErr: errCheck})
		writeLogFile(t, w.service, serviceLogLines)
		r := render(t, w.host)
		note := uncertain + ", so this is its log, the one `bridge logs` reads"
		if !strings.HasPrefix(r.line, "[ok]") || !strings.HasSuffix(r.line, gradedAs(serviceLogLines, w.service)+"; "+note) {
			t.Errorf("the table's line is %q, want ok with the note after the summary", r.line)
		}
		if r.json.Status != "ok" || r.json.Summary != gradedAs(serviceLogLines, w.service)+"; "+note {
			t.Errorf("--json's line is %+v, want ok with the note after the summary", r.json)
		}
	})
	t.Run("neither log", func(t *testing.T) {
		w := newWindowsLogsOnThisHost(t, packaging.WindowsInstall{ServiceErr: errCheck})
		r := render(t, w.host)
		summary := "not checked: neither " + w.service + " nor " + w.user + " exists"
		hint := uncertain + "; the service writes the first once it starts, and a bridge its sign-in launcher starts the second"
		if !strings.HasPrefix(r.line, "[warn]") || !strings.HasSuffix(r.line, summary) || r.hint != "↳ "+hint {
			t.Errorf("the table's line is %q, hint %q; want the warn %q and its hint", r.line, r.hint, summary)
		}
		if r.json.Status != "warn" || r.json.Summary != summary || r.json.Hint != hint {
			t.Errorf("--json's line is %+v, want the warn and its hint", r.json)
		}
	})
	t.Run("a path that cannot be made", func(t *testing.T) {
		errHome := errors.New("%USERPROFILE% is not defined")
		r := render(t, homelessWindowsLogs(t, errHome, launcherInstalled))
		summary := "not checked: the log's path cannot be made (" + errHome.Error() + ")"
		if !strings.HasPrefix(r.line, "[warn]") || !strings.HasSuffix(r.line, summary) || r.json.Status != "warn" ||
			r.json.Summary != summary {
			t.Errorf("the table's line is %q and --json's %+v, want the warn %q", r.line, r.json, summary)
		}
		if strings.Contains(r.line+r.json.Summary, "logs go to the terminal") {
			t.Errorf("a path that cannot be made reads as a bridge that logs to its terminal")
		}
	})
	for _, goos := range []string{"darwin", "linux"} {
		t.Run("a path that cannot be made off Windows on "+goos, func(t *testing.T) {
			r := render(t, homelessLogs(goos))
			if !strings.HasPrefix(r.line, "[ok]") || !strings.HasSuffix(r.line, terminalsOK) || r.hint != "" {
				t.Errorf("the table's line is %q, hint %q; want the ok %q and no hint", r.line, r.hint, terminalsOK)
			}
			if r.json.Status != "ok" || r.json.Summary != terminalsOK || r.json.Hint != "" {
				t.Errorf("--json's line is %+v, want the ok %q", r.json, terminalsOK)
			}
		})
	}
	t.Run("a path that cannot be made in the console's doctor", func(t *testing.T) {
		windows := homelessWindowsLogs(t, errors.New("%USERPROFILE% is not defined"), serviceRegistered)
		windows.isService = func() bool { return false }
		for _, c := range []struct {
			name string
			host logsHost
		}{
			{"on windows outside the service", windows},
			{"on linux", homelessLogs("linux")},
		} {
			t.Run(c.name, func(t *testing.T) {
				requireLogLine(t, consolesLogLine(t, c.host), doctor.OK, terminalsOK, "")
			})
		}
	})
}

// TestBridgeDoctorGradesTheHostsLogThroughItsEntryPoint drives bridge doctor
// --json through doctorCmd on this host's own logsHost (hostLogs), its
// directories set by the environment the test gives it and its install check
// the test's (logsInstallCheck), which answers registered: on Windows the
// line grades the service's log under the PROGRAMDATA given, asking that
// check once; elsewhere DefaultLogPath, asking none. A guard off Windows.
func TestBridgeDoctorGradesTheHostsLogThroughItsEntryPoint(t *testing.T) {
	isolateConfigEnv(t)
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
	logsInstallCheck = func() packaging.WindowsInstall { asked++; return serviceRegistered }
	t.Cleanup(func() { logsInstallCheck = prev })
	userLog, err := packaging.DefaultLogPath()
	if err != nil {
		t.Fatal(err)
	}
	writeLogFile(t, userLog, userLogLines)
	serviceLog := packaging.HostWindowsLogDirs().ServiceLog()
	writeLogFile(t, serviceLog, serviceLogLines)

	var out, errOut bytes.Buffer
	doctorCmd([]string{"--json"}, &out, &errOut)
	var rep jsonDoctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("bridge doctor --json printed no report: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	want, wantAsked := gradedAs(userLogLines, userLog), 0
	if runtime.GOOS == "windows" {
		want, wantAsked = gradedAs(serviceLogLines, serviceLog), 1
	}
	var got jsonDoctorCheck
	for _, c := range rep.Checks {
		if c.Name == "log-file-size" {
			got = c
		}
	}
	if got.Status != "ok" || got.Summary != want {
		t.Errorf("bridge doctor's log-file-size line is %+v, want ok %q", got, want)
	}
	if asked != wantAsked {
		t.Errorf("bridge doctor asked the install check %d times, want %d", asked, wantAsked)
	}
}

// TestAStoppedServiceIsRegistered (backlog B618; round 2 moved it here from
// internal/packaging, where it could not fail): a stopped service is still the
// installed bridge. Its log, older than the one a bridge its sign-in launcher
// wrote since, is the one `bridge logs` reads and bridge doctor grades: the
// install check decides, never which file is newer, and asks no service's
// state (internal/packaging's TestTheInstallCheckAsksOnlyWhatAStandardUserHolds
// holds its calls to a connect, an open and their closes).
func TestAStoppedServiceIsRegistered(t *testing.T) {
	w := newWindowsLogsOnThisHost(t, serviceRegistered)
	writeLogFile(t, w.service, serviceLogLines)
	writeLogFile(t, w.user, userLogLines)
	stopped := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(w.service, stopped, stopped); err != nil {
		t.Fatal(err)
	}
	if svc, user := mtime(t, w.service), mtime(t, w.user); !svc.Before(user) {
		t.Fatalf("the stopped service's log (%v) is not older than the user's (%v)", svc, user)
	}
	code, stdout, stderr := runBridgeLogs(t, w.host)
	requireLogsRun(t, code, stdout, stderr, 0, serviceLogLines,
		fmt.Sprintf("logs: reading the Windows service's log, %s; %s, the log of a bridge its sign-in launcher starts, is there too\n",
			w.service, w.user))
	requireLogLine(t, doctorsLogLine(t, doctorLogs{host: w.host}), doctor.OK, gradedAs(serviceLogLines, w.service), "")
}

// mtime is path's modification time.
func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}
