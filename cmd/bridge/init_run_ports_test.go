package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// `bridge init`'s preflight grades the ports the run writes wherever no
// install's config names its own: on a first install, and over a config that
// is there and does not load.
//
// It graded init's defaults there, 7788 and 7789, whatever the run wrote. A
// public run writes others (:443 or --listen-address; the admin console on
// 7789 or --admin-address), so another process on 7788 refused a public first
// install over a port it would never bind. Measured with the real binary on
// 2026-09-28, 127.0.0.1:7788 held by another process: a public first install
// on two free loopback ports, one on its defaults, and a public re-init over a
// config that does not load each exited 1 on "[FAIL] port-api :7788 in use";
// a loopback first install, which does write :7788, exited 1 as it should.

// TestInitPublicFirstInstallIsNotRefusedOverPortsItDoesNotWrite is the
// reported case: the defaults held, a public first install on two other
// ports.
func TestInitPublicFirstInstallIsNotRefusedOverPortsItDoesNotWrite(t *testing.T) {
	holdLoopbackPort(t, 7788)
	holdLoopbackPort(t, 7789)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)

	code, out := runInit(t, publicFirstInstallArgs(cfgDir, api, admin)...)
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a public first install on :%d and :%d exited %d with 7788 and 7789 held, ports it does "+
			"not write", api, admin, code)
	}
	cfg := loadInstallConfig(t, cfgDir)
	if want := loopbackAddr(api); cfg.ListenAddress != want {
		t.Errorf("listenAddress = %q, want %q", cfg.ListenAddress, want)
	}
	if want := loopbackAddr(admin); cfg.AdminAddress != want {
		t.Errorf("adminAddress = %q, want %q", cfg.AdminAddress, want)
	}
}

// TestInitOverABrokenConfigIsNotRefusedOverPortsItDoesNotWrite is the same
// over a config that does not load, the public re-init that replaces it. The
// data dir records a bridge that is running (this binary's parent, which holds
// none of the ports), and something else holds the defaults. Before, the
// preflight graded the defaults in #1027's attribution-only mode and refused:
// the recorded bridge is alive and is not seen on 7788.
func TestInitOverABrokenConfigIsNotRefusedOverPortsItDoesNotWrite(t *testing.T) {
	holdLoopbackPort(t, 7788)
	holdLoopbackPort(t, 7789)
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getppid())
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)

	code, out := reinitBrokenInstall(t, cfgDir, lib, publicReinitArgs(strconv.Itoa(api), strconv.Itoa(admin))...)
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a public re-init over a config that does not load, on :%d and :%d, exited %d with 7788 "+
			"and 7789 held, ports it does not write", api, admin, code)
	}
	assertConfigReplaced(t, cfgDir)
}

// TestInitPreflightRefusesAPortTheRunWrites is the other half. The preflight
// grades the ports this run writes, so it refuses one another process holds,
// before init has written anything, and says under its report that these are
// the run's ports: the check's own hint is written for an install that is
// there, and there is none, or none whose config loads.
//
// One row per way the run's ports are chosen, which is what makes the
// preflight's port and the saved one the same by construction
// (initAddresses): a public run's flags, the admin port a public run defaults
// to, a loopback run's defaults, and a loopback run's flags.
//
// Before, the first row passed the preflight, which graded 7788 and 7789,
// and was refused by the second port pass after init had made its data dir;
// the second and third were refused by the preflight with no word on whose
// ports those were. The last passed and saved the defaults: a loopback run
// read neither address flag until 2026-09-28.
func TestInitPreflightRefusesAPortTheRunWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold holds the admin port the run writes and returns it.
		hold func(t *testing.T) int
		// args are the run's flags, over cfgDir, writing the admin port held.
		args func(t *testing.T, cfgDir string, admin int) []string
	}{
		{
			name: "a public first install on the ports its flags name",
			hold: func(t *testing.T) int {
				port := freeLoopbackPort(t)
				holdLoopbackPort(t, port)
				return port
			},
			args: func(t *testing.T, cfgDir string, admin int) []string {
				return publicFirstInstallArgs(cfgDir, freeLoopbackPort(t), admin)
			},
		},
		{
			// --listen-address keeps the run off :443, which a test cannot
			// count on binding or finding free on every runner.
			name: "a public first install on the admin port it defaults to",
			hold: func(t *testing.T) int {
				holdLoopbackPort(t, 7789)
				return 7789
			},
			args: func(t *testing.T, cfgDir string, _ int) []string {
				return []string{
					"--yes", "--no-service", "--dir", cfgDir,
					"--public", "--domain", "example.test", "--admin-tls-proxy",
					"--listen-address", loopbackAddr(freeLoopbackPort(t)),
				}
			},
		},
		{
			name: "a loopback first install",
			hold: func(t *testing.T) int {
				holdLoopbackPort(t, 7789)
				return 7789
			},
			args: func(t *testing.T, cfgDir string, _ int) []string {
				return []string{"--yes", "--no-service", "--dir", cfgDir, "--library", testLibrary(t)}
			},
		},
		{
			name: "a loopback first install on the ports its flags name",
			hold: func(t *testing.T) int {
				port := freeLoopbackPort(t)
				holdLoopbackPort(t, port)
				return port
			},
			args: func(t *testing.T, cfgDir string, admin int) []string {
				return loopbackFirstInstallArgs(t, cfgDir, freeLoopbackPort(t), admin)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			admin := tc.hold(t)

			code, out := runInit(t, tc.args(t, cfgDir, admin)...)
			defer logRunOnFailure(t, out)
			if code != 1 {
				t.Fatalf("init exited %d while another process holds :%d, the admin port it writes", code, admin)
			}
			if l := reportLine(out, "port-admin"); !strings.Contains(l, "[FAIL]") ||
				!strings.Contains(l, ":"+strconv.Itoa(admin)+" in use") {
				t.Errorf("port-admin says %q, want a FAIL on :%d, the port this run writes", l, admin)
			}
			if !strings.Contains(out, portsThisInitWrites()) {
				t.Errorf("the refusal does not say whose ports these are: no %q", portsThisInitWrites())
			}
			// The preflight's own probe makes the config dir; the data dir and
			// the config are init's to write, after the preflight.
			for _, name := range []string{"data", "bridge.yaml"} {
				if _, err := os.Stat(filepath.Join(cfgDir, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is there after the refusal (stat: %v): the run was refused after it "+
						"began writing, not by the preflight", name, err)
				}
			}
		})
	}
}

// TestInitSaysNothingOfTheRunsPortsWhereTheInstallsConfigLoads is the control
// for that line. Where the install's config loads, the preflight grades the
// install's own ports, and a refusal there IS a verdict about the install:
// here a re-run that keeps the config, whose listen port another process
// holds while the install's bridge is stopped.
func TestInitSaysNothingOfTheRunsPortsWhereTheInstallsConfigLoads(t *testing.T) {
	lib := testLibrary(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)
	holdLoopbackPort(t, api)
	writeLoopbackInstall(t, cfgDir, lib, api, admin)

	// --yes without --force keeps the config.
	code, out := runInit(t, "--yes", "--no-service", "--dir", cfgDir, "--library", lib)
	defer logRunOnFailure(t, out)
	if code != 1 {
		t.Fatalf("init exited %d while another process holds :%d, the install's own listen port", code, api)
	}
	if l := reportLine(out, "port-api"); !strings.Contains(l, "[FAIL]") ||
		!strings.Contains(l, ":"+strconv.Itoa(api)+" in use") {
		t.Errorf("port-api says %q, want a FAIL on :%d, the install's port", l, api)
	}
	if strings.Contains(out, "the ports this init would write") {
		t.Errorf("the refusal says its ports are the run's, and they are the install's")
	}
}

// TestInitRefusesAnAddressFlagTheConfigWouldRefuse: a --listen-address or
// --admin-address that the config's own check refuses, in the posture the run
// writes, is refused before the preflight, which has no port to grade for it.
// A public run's was refused at the validation before Save, exit 1, after a
// preflight that graded 7788 in its place and after init had made its
// directories. A loopback run's was not refused at all until 2026-09-28: the
// run read neither flag, and saved the defaults with exit 0.
//
// A loopback run's admin address must name a loopback host, as that
// install's adminAddress must (config.ValidateLoopbackAddress): its console
// has no login. A public run's may name any interface, which the public
// tests pin (TestInitPublicAdminAddressOverrideWinsInBothModes).
func TestInitRefusesAnAddressFlagTheConfigWouldRefuse(t *testing.T) {
	const noLogin = "without --public the admin console has no login"
	// The subtests are not named for the flags: t.TempDir's path carries the
	// test's name, and init prints the path, so an output that named the
	// flag would prove nothing.
	for _, tc := range []struct {
		name   string
		public bool
		flag   string
		addr   string
		want   []string
	}{
		{"public listen address with no port", true, "--listen-address", "443",
			[]string{"missing port in address"}},
		{"public admin address out of range", true, "--admin-address", "127.0.0.1:99999",
			[]string{"must be a number between 0 and 65535"}},
		{"loopback listen address with no port", false, "--listen-address", "443",
			[]string{"missing port in address"}},
		{"loopback admin address out of range", false, "--admin-address", "127.0.0.1:99999",
			[]string{"must be a number between 0 and 65535"}},
		{"loopback admin address on every interface", false, "--admin-address", "0.0.0.0:17789",
			[]string{`host "0.0.0.0" is not a loopback address`, noLogin}},
		{"loopback admin address with no host", false, "--admin-address", ":17789",
			[]string{"an empty host binds all interfaces", noLogin}},
		{"loopback admin address on a host name", false, "--admin-address", "bridge.example.test:17789",
			[]string{"must be a loopback address", noLogin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			args := []string{"--yes", "--no-service", "--dir", cfgDir, tc.flag, tc.addr}
			if tc.public {
				args = append(args, "--public", "--domain", "example.test", "--admin-tls-proxy")
			} else {
				// A library, so the run's only usage error is the address.
				args = append(args, "--library", testLibrary(t))
			}
			code, out := runInit(t, args...)
			defer logRunOnFailure(t, out)
			if code != 2 {
				t.Errorf("init exited %d on %s %q, want 2, a usage error", code, tc.flag, tc.addr)
			}
			named := tc.flag + " " + strconv.Quote(tc.addr)
			for _, want := range append([]string{named}, tc.want...) {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not name %s and why: no %q", named, want)
				}
			}
			// The loopback line is about the host, and only a host refusal
			// carries it: a port that does not parse is no question of login.
			if !slices.Contains(tc.want, noLogin) && strings.Contains(out, noLogin) {
				t.Errorf("the refusal of %s explains the loopback rule, and the address was refused for "+
					"something else", named)
			}
			if _, err := os.Stat(cfgDir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the config dir is there after the refusal (stat: %v): the run was refused after "+
					"its preflight, or later", err)
			}
		})
	}
}

// publicFirstInstallArgs are a public first install's flags, behind a proxy so
// no --email is needed, on the two given loopback ports.
func publicFirstInstallArgs(cfgDir string, api, admin int) []string {
	return []string{
		"--yes", "--no-service", "--dir", cfgDir,
		"--public", "--domain", "example.test", "--admin-tls-proxy",
		"--listen-address", loopbackAddr(api),
		"--admin-address", loopbackAddr(admin),
	}
}

// loopbackFirstInstallArgs are a loopback first install's flags, with an
// empty library, on the two given loopback ports.
func loopbackFirstInstallArgs(t *testing.T, cfgDir string, api, admin int) []string {
	return []string{
		"--yes", "--no-service", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", loopbackAddr(api),
		"--admin-address", loopbackAddr(admin),
	}
}

// loopbackAddr is 127.0.0.1:port.
func loopbackAddr(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}

// runInit runs `bridge init` with args and returns its exit code and both
// streams, without colour.
func runInit(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := initCmd(args, strings.NewReader(""), &out, &errOut)
	return code, stripANSI("--- stdout ---\n" + out.String() + "\n--- stderr ---\n" + errOut.String())
}
