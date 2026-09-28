package main

import (
	"errors"
	"os"
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
		run   func(t *testing.T) string
	}{
		{"bridge doctor over a config naming port 0", "port-api", func(t *testing.T) string {
			isolateConfigEnv(t)
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "bridge.yaml")
			body := "libraryRoots:\n  - " + testLibrary(t) + "\n" +
				"dataDir: " + filepath.Join(dir, "data") + "\n" +
				"listenAddress: \":0\"\n" +
				"adminAddress: \"127.0.0.1:0\"\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return runDoctor(t, "--config", cfgPath)
		}},
		{"bridge doctor before bridge init", "tls-cert", func(t *testing.T) string {
			isolateConfigEnv(t)
			return runDoctor(t)
		}},
		{"bridge doctor with no home directory", "config-dir", func(t *testing.T) string {
			isolateConfigEnv(t)
			prev := defaultConfigDirFn
			defaultConfigDirFn = func() (string, error) { return "", errors.New("$HOME is not defined") }
			t.Cleanup(func() { defaultConfigDirFn = prev })
			return runDoctor(t)
		}},
		{"a public init writing port 0", "port-api", func(t *testing.T) string {
			_, out := runInit(t, publicFirstInstallArgs(filepath.Join(t.TempDir(), "cfg"), 0, freeLoopbackPort(t))...)
			return out
		}},
		{"a loopback init writing port 0", "port-api", func(t *testing.T) string {
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
			if strings.Contains(out, "Deps.") {
				t.Errorf("the report names a field of doctor.Deps, which means nothing to its reader")
			}
		})
	}
}

// runDoctor runs `bridge doctor` with args and returns what it printed,
// without colour.
func runDoctor(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut strings.Builder
	doctorCmd(args, &out, &errOut)
	return stripANSI(out.String() + errOut.String())
}
