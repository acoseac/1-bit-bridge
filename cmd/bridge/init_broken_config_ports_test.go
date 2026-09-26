package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// `bridge init --yes --force` over a config that does not load, the re-init
// meant to replace it, while the install's bridge is still serving.
//
// Measured on 2026-09-25 (#1022's log entry): a bridge live on 7788 / 7789,
// its config then broken on disk by one misspelt key, and the re-init run as
// the bridge's user exited 1, "[FAIL] port-api :7788 in use" and "[FAIL]
// port-admin :7789 in use", leaving the config broken. The preflight takes
// the install's ports and pid file from its config, and with none that
// loads it graded init's defaults with no pid file behind them: the
// bridge's own listeners read as another process.
//
// The test process plays the bridge. It holds the ports and its pid is the
// one recorded in <data>/server.pid, the file `bridge serve` writes under
// the data dir. init always writes that data dir, <dir>/data, so it knows
// where the file is without reading the config.

// TestInitReplacesABrokenConfigWhileItsBridgeHoldsTheDefaultPorts is the
// reported case: the re-init must go through and write its config.
func TestInitReplacesABrokenConfigWhileItsBridgeHoldsTheDefaultPorts(t *testing.T) {
	holdDefaultPortsOrSkip(t)
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getpid())

	code, out := reinitBrokenInstall(t, cfgDir, lib)
	if code != 0 {
		t.Fatalf("init exited %d replacing a config that does not load, with its own bridge on the "+
			"default ports:\n%s", code, out)
	}
	assertConfigReplaced(t, cfgDir)
}

// TestInitOverABrokenConfigRefusesADefaultPortItsBridgeIsNotSeenHolding is
// the control: the pid file names a bridge that is running, and something
// else holds the default ports init is about to write.
//
// With no config to say which ports that bridge binds, its being alive says
// nothing about these ports. checkPort's liveness arm would still excuse
// them wherever its probe could not rule that bridge out, as a warn or (on
// Linux, where the listener runs as this user) an ok, and init would save a
// port its restarted bridge cannot bind: #970's defect. Only the bridge
// seen listening there may excuse a port.
//
// The recorded pid is this test binary's parent (the `go test` that is
// waiting for it): alive for the whole test, and holding neither port. The
// test process holds them, as the other process.
func TestInitOverABrokenConfigRefusesADefaultPortItsBridgeIsNotSeenHolding(t *testing.T) {
	holdDefaultPortsOrSkip(t)
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getppid())

	code, out := reinitBrokenInstall(t, cfgDir, lib)
	if code == 0 {
		t.Fatalf("init exited 0 over a config that does not load, saving default ports another process "+
			"holds, because the bridge recorded in the data dir is alive:\n%s", out)
	}
	for _, name := range []string{"port-api", "port-admin"} {
		if !strings.Contains(out, name) {
			t.Errorf("the refusal does not name %s:\n%s", name, out)
		}
	}
	assertConfigUnchanged(t, cfgDir)
}

// TestInitOverABrokenConfigRecognisesItsBridgeOnThePortsItWrites is the
// second port pass's half. A public re-init writes ports other than the
// defaults, and the second pass grades them. It cleared the pid file for a
// port that changed, because a live bridge binds what its config says and
// a changed port is one that config does not name (#970). With no config
// that loads, "changed" is measured against init's defaults, and the
// bridge's own ports are refused.
func TestInitOverABrokenConfigRecognisesItsBridgeOnThePortsItWrites(t *testing.T) {
	requireDefaultPortsFreeOrSkip(t)
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getpid())
	api, admin := holdLoopbackPortAsBridge(t), holdLoopbackPortAsBridge(t)

	code, out := reinitBrokenInstall(t, cfgDir, lib, publicReinitArgs(api, admin)...)
	if code != 0 {
		t.Fatalf("public init exited %d replacing a config that does not load, with its own bridge on "+
			"the ports it writes:\n%s", code, out)
	}
	assertConfigReplaced(t, cfgDir)
}

// TestInitOverABrokenConfigRefusesAWrittenPortItsBridgeIsNotSeenHolding is
// the control for the test above, on the second pass: the recorded bridge
// is alive and something else holds the ports the public re-init writes.
func TestInitOverABrokenConfigRefusesAWrittenPortItsBridgeIsNotSeenHolding(t *testing.T) {
	requireDefaultPortsFreeOrSkip(t)
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getppid())
	api, admin := holdLoopbackPortAsBridge(t), holdLoopbackPortAsBridge(t)

	code, out := reinitBrokenInstall(t, cfgDir, lib, publicReinitArgs(api, admin)...)
	if code == 0 {
		t.Fatalf("public init exited 0 over a config that does not load, saving ports another process "+
			"holds, because the bridge recorded in the data dir is alive:\n%s", out)
	}
	if !strings.Contains(out, "these are the ports this init would write") {
		t.Errorf("the refusal did not come from the second port pass:\n%s", out)
	}
	for _, name := range []string{"port-api", "port-admin"} {
		if !strings.Contains(out, name) {
			t.Errorf("the refusal does not name %s:\n%s", name, out)
		}
	}
	assertConfigUnchanged(t, cfgDir)
}

// brokenTypo is the line that breaks the config: config.Load decodes with
// KnownFields, so one misspelt key is enough, the typo a hand edit makes.
const brokenTypo = "libraryNmae: typo\n"

// brokenInstall writes an install the way `bridge init` lays one out, a
// config dir holding bridge.yaml and data/, and breaks its config. It
// returns the config dir and the library root.
func brokenInstall(t *testing.T) (cfgDir, lib string) {
	t.Helper()
	tmp := t.TempDir()
	lib = filepath.Join(tmp, "Music")
	cfgDir = filepath.Join(tmp, "cfg")
	for _, d := range []string{lib, filepath.Join(cfgDir, "data")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"libraryName: Existing\n" + brokenTypo
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgDir, lib
}

// recordBridgePID writes pid where `bridge serve` records its own, under
// the data dir init writes.
func recordBridgePID(t *testing.T, cfgDir string, pid int) {
	t.Helper()
	path := filepath.Join(cfgDir, "data", serverPIDFileName)
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reinitBrokenInstall runs `bridge init --yes --force --no-service` over
// the install, with any extra flags, and returns its exit code and output.
func reinitBrokenInstall(t *testing.T, cfgDir, lib string, extra ...string) (int, string) {
	t.Helper()
	args := append([]string{
		"--yes", "--force", "--no-service",
		"--dir", cfgDir, "--library", lib, "--name", "Rewritten",
	}, extra...)
	var out, errOut bytes.Buffer
	code := initCmd(args, strings.NewReader(""), &out, &errOut)
	return code, "--- stdout ---\n" + out.String() + "\n--- stderr ---\n" + errOut.String()
}

// publicReinitArgs makes the re-init a public one on the two given loopback
// ports, which are not init's defaults, so the second port pass grades them.
func publicReinitArgs(api, admin string) []string {
	return []string{
		"--public", "--domain", "example.test", "--admin-tls-proxy",
		"--listen-address", "127.0.0.1:" + api,
		"--admin-address", "127.0.0.1:" + admin,
	}
}

// assertConfigReplaced checks the config is init's, not the broken one.
func assertConfigReplaced(t *testing.T, cfgDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "libraryNmae") || !strings.Contains(string(raw), "Rewritten") {
		t.Errorf("the config was not replaced:\n%s", raw)
	}
}

// assertConfigUnchanged checks a refusal left the broken config alone.
func assertConfigUnchanged(t *testing.T, cfgDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "libraryNmae") {
		t.Errorf("the config was rewritten despite the refusal:\n%s", raw)
	}
}

// holdDefaultPortsOrSkip binds init's default ports on loopback for the
// rest of the test, as the bridge whose pid the test records. Unlike
// holdLoopbackPort it cannot accept a port something else already holds:
// that process is not the recorded bridge, so the test would be about
// whoever holds it rather than about init.
func holdDefaultPortsOrSkip(t *testing.T) {
	t.Helper()
	for _, port := range []int{7788, 7789} {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("cannot hold %s (%v): another process on this host has it, and this test needs "+
				"to hold it itself, as the bridge whose pid it records", addr, err)
		}
		t.Cleanup(func() { _ = lis.Close() })
	}
}

// requireDefaultPortsFreeOrSkip skips unless init's default ports are free.
// The preflight grades them whatever the re-init writes, as it does for a
// first install, so another process on them would refuse the run before
// the second pass these tests are about.
func requireDefaultPortsFreeOrSkip(t *testing.T) {
	t.Helper()
	for _, port := range []int{7788, 7789} {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("%s is held by another process on this host (%v), and the preflight grades it "+
				"before the second port pass this test is about", addr, err)
		}
		_ = lis.Close()
	}
}

// holdLoopbackPortAsBridge binds an ephemeral loopback port for the rest of
// the test and returns it.
func holdLoopbackPortAsBridge(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	_, port, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestInitPreflightPointsAnUnloadableConfigAtItsDataDirsPidFile is the
// wiring the end-to-end tests above rely on, one row per way a config that
// is there can fail to load. The ports stay init's defaults, since nothing
// in the config can be read; the pid file is the one `bridge serve` writes
// under the data dir init writes; and OwnPIDPortsUnknown confines it to
// the bridge seen listening, since its ports are unknown.
func TestInitPreflightPointsAnUnloadableConfigAtItsDataDirsPidFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		// denied marks the row that needs mode bits to deny this user.
		denied bool
		unload func(t *testing.T, cfgPath string)
	}{
		{"does not load", false, func(*testing.T, string) {}},
		// Unreadable, to this user, by mode bits.
		{"not readable", true, func(t *testing.T, cfgPath string) { chmodForTest(t, cfgPath, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.denied {
				skipUnlessModeBitsDeny(t, "root reads a mode-0000 file, so the config reaches the decoder "+
					"and fails as the other row does")
			}
			cfgDir, _ := brokenInstall(t)
			cfgPath := filepath.Join(cfgDir, "bridge.yaml")
			tc.unload(t, cfgPath)
			dataDir := filepath.Join(cfgDir, "data")

			d := doctor.Deps{DataDir: dataDir, APIPort: 7788, AdminPort: 7789}
			withExistingInstallDeps(&d, cfgPath)
			if d.APIPort != 7788 || d.AdminPort != 7789 {
				t.Errorf("ports = %d/%d, want init's 7788/7789 — nothing in the config was read", d.APIPort, d.AdminPort)
			}
			if want := filepath.Join(dataDir, serverPIDFileName); d.OwnPIDFile != want {
				t.Errorf("OwnPIDFile = %q, want %q — without it the bridge this re-init replaces reads "+
					"as another process on its own ports", d.OwnPIDFile, want)
			}
			if !d.OwnPIDPortsUnknown {
				t.Error("OwnPIDPortsUnknown = false — the recorded bridge's being alive would then excuse " +
					"ports nothing says it binds")
			}
		})
	}
}
