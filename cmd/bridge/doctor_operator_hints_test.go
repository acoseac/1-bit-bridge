package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrintedHintsSpeakToTheOperator drives `bridge doctor` and `bridge init`
// into each state whose hint named a field of doctor.Deps, a note for whoever
// calls that package, and requires the printed report to carry the check's
// warn with no such note. Measured with the real binary on 2026-09-28, each
// printed one:
//
//   - a config naming :0, the ephemeral-port mode config.validatePort
//     accepts, and a public init's --listen-address naming it: "pass
//     Deps.port-apiPort";
//   - `bridge doctor` before `bridge init`, which finds no config: "pass
//     Deps.DataDir so doctor can inspect cert state";
//   - the same with no home directory: "pass Deps.ConfigDir so doctor can
//     verify write access".
//
// A loopback init's --listen-address is a row too: it names :0 as legally as
// a public one's, now that a loopback run reads the flag (initAddresses).
func TestPrintedHintsSpeakToTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		// check is the line the state gives a warn.
		check string
		// says is a phrase of the hint the operator must be printed under
		// that line. Without it the test passes a report that prints no hint
		// at all, since nothing then names a Deps field either (CodeRabbit).
		says string
		run  func(t *testing.T) string
	}{
		{"bridge doctor over a config naming port 0", "port-api", "picks a free port each time the bridge starts", func(t *testing.T) string {
			isolateConfigEnv(t)
			dir := t.TempDir()
			writeInstallConfig(t, dir, testLibrary(t), ":0", "127.0.0.1:0")
			return runDoctor(t, "--config", filepath.Join(dir, "bridge.yaml"))
		}},
		{"bridge doctor before bridge init", "tls-cert", "`bridge init` mints one on a first install", func(t *testing.T) string {
			isolateConfigEnv(t)
			return runDoctor(t)
		}},
		{"bridge doctor with no home directory", "config-dir", "the default config directory could not be resolved", func(t *testing.T) string {
			isolateConfigEnv(t)
			prev := defaultConfigDirFn
			defaultConfigDirFn = func() (string, error) { return "", errors.New("$HOME is not defined") }
			t.Cleanup(func() { defaultConfigDirFn = prev })
			return runDoctor(t)
		}},
		{"a public init writing port 0", "port-api", "picks a free port each time the bridge starts", func(t *testing.T) string {
			_, out := runInit(t, publicFirstInstallArgs(filepath.Join(t.TempDir(), "cfg"), 0, freeLoopbackPort(t))...)
			return out
		}},
		{"a loopback init writing port 0", "port-api", "picks a free port each time the bridge starts", func(t *testing.T) string {
			_, out := runInit(t, loopbackFirstInstallArgs(t, filepath.Join(t.TempDir(), "cfg"), 0, freeLoopbackPort(t))...)
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.run(t)
			defer logRunOnFailure(t, out)
			if l := reportLine(out, tc.check); !strings.Contains(l, "[warn]") {
				t.Errorf("%s says %q, want the warn this state gives", tc.check, l)
			}
			if h := hintUnder(out, tc.check); !strings.Contains(h, tc.says) {
				t.Errorf("the hint under %s is %q, want one saying %q", tc.check, h, tc.says)
			}
			if strings.Contains(out, "Deps.") {
				t.Errorf("the report names a field of doctor.Deps, which means nothing to its reader")
			}
		})
	}
}

// hintUnder returns the hint line printReport puts under check's line (it
// begins "↳"), or "" when the next line is not one.
func hintUnder(report, check string) string {
	lines := strings.Split(report, "\n")
	for i, l := range lines {
		if strings.Contains(l, " "+check+" ") {
			if i+1 < len(lines) {
				if h := strings.TrimSpace(lines[i+1]); strings.HasPrefix(h, "↳") {
					return h
				}
			}
			return ""
		}
	}
	return ""
}

// runDoctor runs `bridge doctor` with args and returns what it printed,
// without colour.
func runDoctor(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut strings.Builder
	doctorCmd(args, &out, &errOut)
	return stripANSI(out.String() + errOut.String())
}
