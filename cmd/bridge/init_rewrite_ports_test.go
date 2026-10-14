package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestInitRewriteKeepsTheAddressesItDoesNotName: a loopback install on its
// own ports, rewritten with --yes --force and no address flag, keeps both.
// printKept lists them. The preflight grades a kept port as the install's
// in TestInitRewriteGradesAKeptListenPortAsTheInstalls.
func TestInitRewriteKeepsTheAddressesItDoesNotName(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := testLibrary(t)
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)
	writeLoopbackInstall(t, cfgDir, lib, api, admin)

	code, out := forceRewrite(t, cfgDir, "--library", lib)
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	saved := loadInstallConfig(t, cfgDir)
	if saved.ListenAddress != loopbackAddr(api) || saved.AdminAddress != loopbackAddr(admin) {
		t.Errorf("saved %s and %s, want the install's %s and %s",
			saved.ListenAddress, saved.AdminAddress, loopbackAddr(api), loopbackAddr(admin))
	}
	assertAddressKept(t, out, "listenAddress", loopbackAddr(api))
	assertAddressKept(t, out, "adminAddress", loopbackAddr(admin))
}

// TestInitRewriteGradesAKeptListenPortAsTheInstalls: another process holds
// the install's listen port. A rewrite that names no address flag grades
// that port as one it still writes, refuses, and leaves the config as it
// was. It does not call the port one this rewrite moves off.
func TestInitRewriteGradesAKeptListenPortAsTheInstalls(t *testing.T) {
	cfgDir, api, _ := strangerOnTheInstallsListenPort(t)
	before := readConfigFile(t, cfgDir)

	code, out := forceRewrite(t, cfgDir, "--library", testLibrary(t))
	defer logRunOnFailure(t, out)
	assertRewriteRefused(t, cfgDir, out, code, api)
	if got := readConfigFile(t, cfgDir); got != before {
		t.Error("the refused rewrite changed the config")
	}
	if strings.Contains(out, "moves off") {
		t.Error("the rewrite treats the install's listen port as one it moves off")
	}
}

// TestInitRewriteWritesTheListenAddressItNames: a rewrite that names
// --listen-address writes that address, and the admin address it does not
// name stays the loopback default. Neither is listed as kept.
func TestInitRewriteWritesTheListenAddressItNames(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := testLibrary(t)
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)
	writeLoopbackInstall(t, cfgDir, lib, api, admin)
	named := freeLoopbackPort(t)

	code, out := forceRewrite(t, cfgDir, "--skip-doctor", "--library", lib, "--listen-address", loopbackAddr(named))
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	saved := loadInstallConfig(t, cfgDir)
	if saved.ListenAddress != loopbackAddr(named) {
		t.Errorf("listenAddress = %q, want %s", saved.ListenAddress, loopbackAddr(named))
	}
	if saved.AdminAddress != config.DefaultAdminAddress {
		t.Errorf("adminAddress = %q, want the default %s", saved.AdminAddress, config.DefaultAdminAddress)
	}
	if _, ok := keptValue(out, "listenAddress"); ok {
		t.Error("the rewrite lists the listen address it was given as one it kept")
	}
	if _, ok := keptValue(out, "adminAddress"); ok {
		t.Error("the rewrite lists the admin address, and it wrote the default")
	}
}

// TestInitRewriteOfAnotherPostureWritesThatPosturesPorts: a loopback install
// on its own ports, rewritten into public mode with no address flag, writes
// public mode's ports. The install's ports are not kept.
func TestInitRewriteOfAnotherPostureWritesThatPosturesPorts(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := testLibrary(t)
	api, admin := freeLoopbackPort(t), freeLoopbackPort(t)
	writeLoopbackInstall(t, cfgDir, lib, api, admin)

	code, out := forceRewrite(t, cfgDir, "--skip-doctor", "--public", "--domain", "localhost", "--admin-tls-proxy")
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	saved := loadInstallConfig(t, cfgDir)
	if saved.ListenAddress != ":443" || saved.AdminAddress != config.DefaultAdminAddress {
		t.Errorf("saved %s and %s, want public mode's :443 and %s",
			saved.ListenAddress, saved.AdminAddress, config.DefaultAdminAddress)
	}
	if _, ok := keptValue(out, "listenAddress"); ok {
		t.Error("the rewrite lists a listen address it kept, and it wrote public mode's")
	}
}

// assertAddressKept requires printKept to list key as value.
func assertAddressKept(t *testing.T, out, key, value string) {
	t.Helper()
	got, ok := keptValue(out, key)
	if !ok || got != value {
		t.Errorf("kept %s = %q, present %v, want %s", key, got, ok, value)
	}
}
