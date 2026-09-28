package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestInitLoopbackRunWritesTheAddressesItIsGiven: a loopback `bridge init`
// writes the --listen-address and --admin-address it is given, as a public run
// always has (initAddresses). Until 2026-09-28 a loopback run read neither: it
// saved :7788 and 127.0.0.1:7789 and exited 0 without a word, whatever it was
// given (measured with the real binary: both flags on free loopback ports, and
// --admin-address 0.0.0.0:47812, each saved the defaults). An operator whose
// 7788 is taken, by a second bridge say, could set up the install only by
// editing bridge.yaml after init had graded and saved the defaults.
//
// One row per shape of address a loopback install may bind: both on
// 127.0.0.1, and the API on every interface (its default's shape) with the
// console on localhost, which the loopback rule accepts. A rewrite is a row
// too: a --force rewrite of a loopback install writes the run's addresses in
// place of the install's, as it writes every other setting the run names.
func TestInitLoopbackRunWritesTheAddressesItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name string
		// addrs are the listen and admin address the run is given, on the
		// two free ports.
		addrs func(api, admin int) (string, string)
		// rewrite runs init over a loopback install on two other ports.
		rewrite bool
	}{
		{"a first install on loopback ports", func(api, admin int) (string, string) {
			return loopbackAddr(api), loopbackAddr(admin)
		}, false},
		{"a first install with the API on every interface", func(api, admin int) (string, string) {
			return ":" + strconv.Itoa(api), "localhost:" + strconv.Itoa(admin)
		}, false},
		{"a --force rewrite of a loopback install", func(api, admin int) (string, string) {
			return loopbackAddr(api), loopbackAddr(admin)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := testLibrary(t)
			if tc.rewrite {
				writeLoopbackInstall(t, cfgDir, lib, freeLoopbackPort(t), freeLoopbackPort(t))
			}
			listen, admin := tc.addrs(freeLoopbackPort(t), freeLoopbackPort(t))

			code, out := runInit(t, "--yes", "--force", "--no-service", "--dir", cfgDir, "--library", lib,
				"--listen-address", listen, "--admin-address", admin)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a loopback init on %s and %s exited %d", listen, admin, code)
			}
			cfg := loadInstallConfig(t, cfgDir)
			if cfg.ListenAddress != listen || cfg.AdminAddress != admin {
				t.Errorf("saved listenAddress %q and adminAddress %q, want %q and %q: the run's flags",
					cfg.ListenAddress, cfg.AdminAddress, listen, admin)
			}
		})
	}
}

// writeLoopbackInstall writes the config of a loopback install at cfgDir
// listening on the two given loopback ports (writeInstallConfig).
func writeLoopbackInstall(t *testing.T, cfgDir, lib string, api, admin int) {
	t.Helper()
	writeInstallConfig(t, cfgDir, lib, loopbackAddr(api), loopbackAddr(admin))
}

// writeInstallConfig writes the config of an install at cfgDir, as `bridge
// init` lays one out (bridge.yaml beside its data dir), named "Existing" and
// listening on the two given addresses, with its data dir made and no bridge
// running: no pid file.
func writeInstallConfig(t *testing.T, cfgDir, lib, listen, admin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfgDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"listenAddress: \"" + listen + "\"\n" +
		"adminAddress: \"" + admin + "\"\n" +
		"libraryName: Existing\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
