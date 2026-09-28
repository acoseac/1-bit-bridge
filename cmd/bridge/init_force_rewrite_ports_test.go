package main

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// `bridge init --yes --force` over an install whose config loads: the one
// rewrite that is certain before the preflight runs. The preflight grades the
// install that is there (#963), its ports included, and a port the rewrite
// moves off is none of the install's concern once the new config is saved.
//
// Measured with the real binary on 2026-09-28: an install on 127.0.0.1:X and
// :Y, its bridge stopped, another process holding X, and a --force rewrite
// moving the API to A exited 1 on "[FAIL] port-api :X in use", a port the
// rewrite abandons, as a public run and as a loopback one. The preflight now
// leaves such a port ungraded (doctor.Deps.AbandonedPorts); the second port
// pass grades what the rewrite writes in its place, as it did; and a port the
// rewrite keeps is graded as before.

// portMovingRewrites are the two postures a --force rewrite can write, each as
// the flags that move the install onto api and admin.
var portMovingRewrites = []struct {
	name  string
	flags func(t *testing.T, api, admin int) []string
}{
	{"a loopback rewrite", func(t *testing.T, api, admin int) []string {
		return []string{"--library", testLibrary(t),
			"--listen-address", loopbackAddr(api), "--admin-address", loopbackAddr(admin)}
	}},
	{"a public rewrite", func(t *testing.T, api, admin int) []string {
		return []string{"--public", "--domain", "example.test", "--admin-tls-proxy",
			"--listen-address", loopbackAddr(api), "--admin-address", loopbackAddr(admin)}
	}},
}

// forceRewrite runs `bridge init --yes --force --no-service` over the
// install at cfgDir with the given flags.
func forceRewrite(t *testing.T, cfgDir string, flags ...string) (int, string) {
	t.Helper()
	return runInit(t, append([]string{"--yes", "--force", "--no-service", "--dir", cfgDir,
		"--name", "Rewritten"}, flags...)...)
}

// strangerOnTheInstallsListenPort writes a loopback install on two free
// ports, its bridge stopped, and holds its listen port as another process.
// It returns the config dir and the two ports.
func strangerOnTheInstallsListenPort(t *testing.T) (cfgDir string, api, admin int) {
	t.Helper()
	cfgDir = filepath.Join(t.TempDir(), "cfg")
	api, admin = freeLoopbackPort(t), freeLoopbackPort(t)
	writeLoopbackInstall(t, cfgDir, testLibrary(t), api, admin)
	holdLoopbackPort(t, api)
	return cfgDir, api, admin
}

// TestInitForceRewriteIsNotRefusedOverAPortItMovesOff is the reported case,
// in both postures: the rewrite moves both ports, and another process holds
// the install's old listen port.
func TestInitForceRewriteIsNotRefusedOverAPortItMovesOff(t *testing.T) {
	for _, posture := range portMovingRewrites {
		t.Run(posture.name, func(t *testing.T) {
			cfgDir, oldAPI, _ := strangerOnTheInstallsListenPort(t)
			api, admin := freeLoopbackPort(t), freeLoopbackPort(t)

			code, out := forceRewrite(t, cfgDir, posture.flags(t, api, admin)...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a --force rewrite moving the API from :%d to :%d exited %d while another process "+
					"holds :%d, a port the rewritten config does not name", oldAPI, api, code, oldAPI)
			}
			cfg := loadInstallConfig(t, cfgDir)
			if cfg.ListenAddress != loopbackAddr(api) || cfg.AdminAddress != loopbackAddr(admin) {
				t.Errorf("saved %q and %q, want the rewrite's %s and %s",
					cfg.ListenAddress, cfg.AdminAddress, loopbackAddr(api), loopbackAddr(admin))
			}
		})
	}
}

// TestInitForceRewriteStillRefusesAPortItKeeps is the control the change
// must not lose: a rewrite that KEEPS the install's listen port writes a port
// another process holds, and the restarted bridge could not bind it. The
// preflight refuses it as the install's, before anything is written, and
// since the rewrite writes that port, it says under the report that these are
// the ports this init would write: the check's own hint names a bridge.yaml
// the run is about to replace, where the flags choose another.
//
// Kept in the other role too: a rewrite moving the admin console onto the
// install's old listen port binds that port again, so it is not abandoned.
// Were the list built per role (every port whose role changed), the old
// listen port would be listed, and the second pass would then carry the list
// while grading that same port as the new admin port, and wave it through.
func TestInitForceRewriteStillRefusesAPortItKeeps(t *testing.T) {
	for _, shape := range []struct {
		name string
		// ports are the rewrite's API and admin port, given the install's
		// listen port another process holds.
		ports func(t *testing.T, kept int) (api, admin int)
	}{
		{"as the listen port", func(t *testing.T, kept int) (int, int) { return kept, freeLoopbackPort(t) }},
		{"as the admin port", func(t *testing.T, kept int) (int, int) { return freeLoopbackPort(t), kept }},
	} {
		for _, posture := range portMovingRewrites {
			t.Run(posture.name+" keeping it "+shape.name, func(t *testing.T) {
				cfgDir, kept, _ := strangerOnTheInstallsListenPort(t)
				api, admin := shape.ports(t, kept)

				code, out := forceRewrite(t, cfgDir, posture.flags(t, api, admin)...)
				defer logRunOnFailure(t, out)
				assertRewriteRefused(t, cfgDir, out, code, kept)
				if !strings.Contains(out, portsThisInitWrites()) {
					t.Errorf("the refusal does not say the port is one this init would write: no %q",
						portsThisInitWrites())
				}
			})
		}
	}
}

// TestInitForceRewriteGradesThePortItMovesTo: leaving the old port ungraded
// must not leave the new one so. Another process holds the install's old
// listen port AND the one the rewrite moves the API to. The refusal is the
// second port pass's, about the new port, which carries the preflight's Deps,
// abandoned ports and all.
func TestInitForceRewriteGradesThePortItMovesTo(t *testing.T) {
	for _, posture := range portMovingRewrites {
		t.Run(posture.name, func(t *testing.T) {
			cfgDir, oldAPI, _ := strangerOnTheInstallsListenPort(t)
			api := freeLoopbackPort(t)
			holdLoopbackPort(t, api)

			code, out := forceRewrite(t, cfgDir, posture.flags(t, api, freeLoopbackPort(t))...)
			defer logRunOnFailure(t, out)
			assertRewriteRefused(t, cfgDir, out, code, api)
			if strings.Contains(out, ":"+strconv.Itoa(oldAPI)+" in use") {
				t.Errorf("the refusal grades :%d, the port the rewrite moves off", oldAPI)
			}
		})
	}
}

// TestInitInteractiveRunGradesTheInstallsPortsBeforeItsPrompt: an
// interactive run over a config that loads is not certain to rewrite it. Its
// "Overwrite?" comes after the preflight, and a no keeps the config, whose
// ports are then the ones the bridge binds. So its preflight grades the
// install's ports as it always has, the ones its flags would move off
// included, and a refusal there is a verdict about the install, with no
// word about the run's ports.
func TestInitInteractiveRunGradesTheInstallsPortsBeforeItsPrompt(t *testing.T) {
	cfgDir, oldAPI, _ := strangerOnTheInstallsListenPort(t)
	var out, errOut strings.Builder
	code := initCmd([]string{"--no-service", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t))},
		strings.NewReader("Rewritten\ny\n"), &out, &errOut)
	printed := stripANSI(out.String() + errOut.String())
	defer logRunOnFailure(t, printed)
	if code != 1 {
		t.Fatalf("an interactive run exited %d while another process holds :%d, the install's listen port", code, oldAPI)
	}
	assertPortFailed(t, printed, "port-api", oldAPI)
	if strings.Contains(printed, portsThisInitWrites()) {
		t.Errorf("the refusal says the port is one this init would write, and the run may keep the install's")
	}
}

// assertRewriteRefused requires a --force rewrite to have exited 1 on a FAIL
// of port-api on port, a port it writes and another process holds, and to
// have left the install's config as it was.
func assertRewriteRefused(t *testing.T, cfgDir, out string, code, port int) {
	t.Helper()
	if code != 1 {
		t.Fatalf("the rewrite exited %d while another process holds :%d, a port it writes", code, port)
	}
	assertPortFailed(t, out, "port-api", port)
	if cfg := loadInstallConfig(t, cfgDir); cfg.LibraryName != "Existing" {
		t.Errorf("the config was rewritten despite the refusal: libraryName %q", cfg.LibraryName)
	}
}

// assertPortFailed requires the printed report's line for check to FAIL on
// port, the port another process holds.
func assertPortFailed(t *testing.T, out, check string, port int) {
	t.Helper()
	if l := reportLine(out, check); !strings.Contains(l, "[FAIL]") || !strings.Contains(l, ":"+strconv.Itoa(port)+" in use") {
		t.Errorf("%s says %q, want a FAIL on :%d", check, l, port)
	}
}

// TestPortsARewriteAbandons pins which of the install's ports a rewrite
// abandons: the ones it binds in neither role, each once. A port it keeps in
// the other role is not abandoned, since the bridge binds it again after a
// restart, and the second pass grades it as the port the run writes.
func TestPortsARewriteAbandons(t *testing.T) {
	const x, y, a, b = 7001, 7002, 7003, 7004
	for _, tc := range []struct {
		name                                       string
		installAPI, installAdmin, runAPI, runAdmin int
		want                                       []int
	}{
		{"nothing moves", x, y, x, y, nil},
		{"the API moves", x, y, a, y, []int{x}},
		{"the console moves", x, y, x, b, []int{y}},
		{"both move", x, y, a, b, []int{x, y}},
		{"the two swap", x, y, y, x, nil},
		{"the API moves onto the console's port", x, y, y, b, []int{x}},
		{"the console moves onto the API's port", x, y, a, x, []int{y}},
		{"an install on one port", x, x, a, b, []int{x}},
		{"an install on port 0 moving off it", 0, y, a, y, []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := portsARewriteAbandons(tc.installAPI, tc.installAdmin, tc.runAPI, tc.runAdmin)
			if !slices.Equal(got, tc.want) {
				t.Errorf("portsARewriteAbandons(%d, %d, %d, %d) = %v, want %v",
					tc.installAPI, tc.installAdmin, tc.runAPI, tc.runAdmin, got, tc.want)
			}
			for _, p := range got {
				if p == tc.runAPI || p == tc.runAdmin {
					t.Errorf("it abandons :%d, a port the rewrite writes, which the second pass must grade", p)
				}
			}
		})
	}
}
