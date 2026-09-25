package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestDoctorDoesNotGradeTheDefaultPortsOfAConfigItCannotLoad drives
// buildDoctorDeps and doctor.Run over a config that was named or found and
// did not load, with the DEFAULT ports held.
//
// That config is where the ports and the pid file come from, so doctor is
// left grading 7788 / 7789 with no pid file. Measured on 2026-09-25 (bridge
// serve as uid 1000, `bridge doctor --config <its config>` as uid 1001, in
// the stock golang image): config-file warned that the file is not
// readable by this user, and port-api and port-admin FAILed "another
// process owns this port" against the bridge's own listeners. The port
// lines now say they were not checked and why, and config-file keeps the
// one verdict about the config.
//
// The two permission rows are the reported case, a config this user may
// not read and one under a directory it may not traverse (`bridge init`
// makes the config dir 0700). The other two are the same rule: a --config
// that is not there, and a config that does not load, which is the
// runbook's validate-before-restart run on an edit with a typo in it,
// beside a bridge still serving on its ports.
func TestDoctorDoesNotGradeTheDefaultPortsOfAConfigItCannotLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		// denied marks the rows that need mode bits to deny this user.
		denied bool
		// install writes the config doctor is pointed at and returns the
		// --config to pass ("" for the working directory's).
		install func(t *testing.T, cwd string) string
		want    doctor.Status // config-file's verdict
		reason  string        // the port lines' reason
	}{
		{"a found config this user cannot read", true, func(t *testing.T, cwd string) string {
			chmodForTest(t, writeInstallAt(t, cwd, "local-track.flac"), 0)
			return ""
		}, doctor.Warn, "not readable by this user"},
		{"a named config under a directory this user cannot traverse", true, func(t *testing.T, _ string) string {
			locked := filepath.Join(t.TempDir(), "locked")
			named := writeInstallAt(t, locked, "local-track.flac")
			chmodForTest(t, locked, 0)
			// Registered after t.TempDir, so it runs first: RemoveAll
			// cannot descend into a directory with no permissions.
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			return named
		}, doctor.Warn, "not readable by this user"},
		{"a named config that is not there", false, func(t *testing.T, _ string) string {
			return filepath.Join(t.TempDir(), "typo", "bridge.yml")
		}, doctor.Fail, "does not exist"},
		{"a found config that does not load", false, func(t *testing.T, cwd string) string {
			// config.Load decodes with KnownFields, so one misspelt key is a
			// load failure: the typo a hand edit makes.
			if err := os.WriteFile(filepath.Join(cwd, defaultConfigPath), []byte("libraryNmae: typo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return ""
		}, doctor.Fail, "does not load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.denied && runtime.GOOS == "windows" {
				t.Skip("mode bits do not deny a read or a traversal on Windows")
			}
			if tc.denied && os.Geteuid() == 0 {
				t.Skip("root reads a mode-0000 file and traverses a mode-0000 directory, so the config " +
					"loads and doctor grades its own ports; this row needs a user the mode bits deny")
			}
			cwd, _ := isolateConfigEnv(t)
			d := buildDoctorDeps(tc.install(t, cwd))
			// The reported shape: the defaults, and no pid file to recognise
			// the bridge's own listeners by.
			if d.APIPort != 7788 || d.AdminPort != 7789 || d.OwnPIDFile != "" {
				t.Fatalf("Deps carry ports %d / %d and pid file %q; want the defaults 7788 / 7789 and none, "+
					"the state a config that did not load leaves doctor in", d.APIPort, d.AdminPort, d.OwnPIDFile)
			}
			holdLoopbackPort(t, d.APIPort)
			holdLoopbackPort(t, d.AdminPort)

			rep := doctor.Run(context.Background(), d)
			if c := findCheck(t, rep, "config-file"); c.Status != tc.want {
				t.Errorf("config-file = %s %q, want %s", c.Status, c.Summary, tc.want)
			}
			for _, name := range []string{"port-api", "port-admin"} {
				c := findCheck(t, rep, name)
				if c.Status != doctor.OK || !strings.HasPrefix(c.Summary, "not checked: ") ||
					!strings.Contains(c.Summary, tc.reason) {
					t.Errorf("%s = %s %q (hint %q) with the default port held; want ok \"not checked: …%s…\", "+
						"since the port the config sets is unknown", name, c.Status, c.Summary, c.Hint, tc.reason)
				}
			}
		})
	}
}

// chmodForTest sets path's mode, failing the test if it cannot.
func chmodForTest(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// holdLoopbackPort makes sure port is bound on 127.0.0.1 for the rest of
// the test: by the test when it is free, and otherwise by whatever already
// holds it (a bridge running on this host, say), which is the same fact to
// doctor. A port that neither binds nor answers is a host this test cannot
// set up, and that fails it rather than letting it pass on a free port.
func holdLoopbackPort(t *testing.T, port int) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err == nil {
		t.Cleanup(func() { _ = lis.Close() })
		return
	}
	conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
	if dialErr != nil {
		t.Fatalf("could not bind %s (%v), and nothing answers on it (%v)", addr, err, dialErr)
	}
	_ = conn.Close()
}
