package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `bridge fingerprint` and enrich.enabled (backlog B617): the command builds
// its own AcoustID client, so a run that would look up reads the switch
// itself as it starts: BRIDGE_ENRICH_ENABLED where it says on or off, else
// the config --config names, else the config at the default path, else on.
// Off, it sends nothing, before the key, the probe or a decode, and exits 1;
// --no-lookup sends nothing and is not refused. --config, and the switch a
// run follows while it runs, are fingerprint_live_switch_test.go's.

const (
	// fingerprintOffRefusal begins the command's refusal while off.
	fingerprintOffRefusal = "Enrichment is switched off ("
	// fingerprintProbed is what a run that reached the probe says with no
	// fpcalc on PATH.
	fingerprintProbed = "Install fpcalc"
)

// fingerprintCLI runs `bridge fingerprint` through run, as a shell would,
// in a directory with no bridge.yaml unless the test wrote one and with no
// fpcalc on PATH, so a run that reaches the probe says so.
func fingerprintCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"fingerprint"}, args...), &stdout, &stderr)
	return code, stderr.String()
}

// writeSwitchConfig writes a config that loads, with enrich holding the
// enrich block's lines, at path.
func writeSwitchConfig(t *testing.T, path, enrich string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") + "\n"
	if enrich != "" {
		yaml += "enrich:\n" + enrich
	}
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fingerprintSwitchCase is where a run finds its switch: the variable's
// value (empty says nothing), and the enrich block of ./bridge.yaml and of
// the platform's config, each written only when not empty.
type fingerprintSwitchCase struct {
	name, env, local, platform string
}

// setUp gives the run its switch, in a working directory and a platform
// directory of the test's own, and returns the platform config's path.
func (c fingerprintSwitchCase) setUp(t *testing.T) string {
	t.Helper()
	_, platform := isolateConfigEnv(t)
	clearEnrichSwitchEnv(t)
	if c.env != "" {
		t.Setenv("BRIDGE_ENRICH_ENABLED", c.env)
	}
	if c.local != "" {
		writeSwitchConfig(t, "bridge.yaml", c.local)
	}
	platformPath := filepath.Join(platform, "bridge.yaml")
	if c.platform != "" {
		writeSwitchConfig(t, platformPath, c.platform)
	}
	return platformPath
}

// clearEnrichSwitchEnv removes BRIDGE_ENRICH_ENABLED for the test, so a value
// a developer's shell carries cannot decide a case.
func clearEnrichSwitchEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BRIDGE_ENRICH_ENABLED", "")
	os.Unsetenv("BRIDGE_ENRICH_ENABLED")
}

// TestBridgeFingerprintRefusesALookupWhileEnrichmentIsOff: switched off by
// the variable, by ./bridge.yaml or by the platform's config, a run with a
// key sends nothing: it exits 1 naming what switched it off, before it
// probes fpcalc or reads a file. The variable decides over a config that
// says the other. A config that is there and does not load refuses the
// lookup too, naming the file: whether the operator switched it off cannot
// be told.
func TestBridgeFingerprintRefusesALookupWhileEnrichmentIsOff(t *testing.T) {
	for _, c := range []struct {
		fingerprintSwitchCase
		source string // what the refusal names; "<platform>" for that path
	}{
		{fingerprintSwitchCase{name: "the variable", env: "off"}, "BRIDGE_ENRICH_ENABLED"},
		{fingerprintSwitchCase{name: "./bridge.yaml", local: "  enabled: false\n"}, "enrich.enabled in bridge.yaml"},
		{fingerprintSwitchCase{name: "the platform's config", platform: "  enabled: false\n"}, "enrich.enabled in <platform>"},
		{fingerprintSwitchCase{name: "the variable over a config that says on", env: "No", local: "  enabled: true\n"}, "BRIDGE_ENRICH_ENABLED"},
	} {
		t.Run(c.name, func(t *testing.T) {
			platformPath := c.setUp(t)
			code, stderr := fingerprintCLI(t, "--key", "k", "missing.flac")
			want := fingerprintOffRefusal + strings.ReplaceAll(c.source, "<platform>", platformPath) + ")"
			if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "--no-lookup") {
				t.Errorf("exit %d, stderr:\n%s\nwant exit 1 and %q with the way to --no-lookup", code, stderr, want)
			}
			if strings.Contains(stderr, fingerprintProbed) || strings.Contains(stderr, "missing.flac") {
				t.Errorf("a refused run probed fpcalc or read a file:\n%s", stderr)
			}
		})
	}
	t.Run("a config that does not load", func(t *testing.T) {
		fingerprintSwitchCase{name: "unloadable"}.setUp(t)
		if err := os.WriteFile("bridge.yaml", []byte("enrich: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stderr := fingerprintCLI(t, "--key", "k", "missing.flac")
		if code != 1 || !strings.Contains(stderr, "The config at bridge.yaml does not load") || !strings.Contains(stderr, "--no-lookup") {
			t.Errorf("exit %d, stderr:\n%s\nwant exit 1 naming the config that does not load", code, stderr)
		}
		if strings.Contains(stderr, fingerprintProbed) {
			t.Errorf("a refused run probed fpcalc:\n%s", stderr)
		}
	})
}

// TestBridgeFingerprintLooksUpWhileEnrichmentIsOn: on by default, by the
// variable over a config that says off, or by the config, the run passes the
// switch and goes on to the probe, which says fpcalc is missing here; and a
// --no-lookup run while off is not refused, since it sends nothing.
func TestBridgeFingerprintLooksUpWhileEnrichmentIsOn(t *testing.T) {
	for _, c := range []struct {
		fingerprintSwitchCase
		args []string
	}{
		{fingerprintSwitchCase{name: "no variable and no config"}, []string{"--key", "k"}},
		{fingerprintSwitchCase{name: "the variable over a config that says off", env: "ON", local: "  enabled: false\n"}, []string{"--key", "k"}},
		{fingerprintSwitchCase{name: "a config that says nothing", local: "  deezerEnabled: false\n"}, []string{"--key", "k"}},
		{fingerprintSwitchCase{name: "--no-lookup while off", env: "off"}, []string{"--no-lookup"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.setUp(t)
			code, stderr := fingerprintCLI(t, append(c.args, "missing.flac")...)
			if code != 1 || !strings.Contains(stderr, fingerprintProbed) {
				t.Errorf("exit %d, stderr:\n%s\nwant the run to reach the probe", code, stderr)
			}
			if strings.Contains(stderr, fingerprintOffRefusal) || strings.Contains(stderr, "does not load") {
				t.Errorf("the switch refused a run it should not have:\n%s", stderr)
			}
		})
	}
}
