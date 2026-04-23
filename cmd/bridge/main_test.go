package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeValidConfig drops a minimal-but-valid bridge.yaml next to a real
// library root. Returns the config path.
func writeValidConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("libraryRoots:\n  - "+lib+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func runCapture(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	code = run(args, &so, &se)
	return so.String(), se.String(), code
}

func TestVersion(t *testing.T) {
	stdout, stderr, code := runCapture(t, "version")
	if code != 0 {
		t.Fatalf("version exit code = %d, want 0; stderr=%q", code, stderr)
	}
	want := fmt.Sprintf("1-bit-bridge %s (protocol v%d)", ServerVersion, ProtocolVersion)
	if !strings.Contains(stdout, want) {
		t.Errorf("version stdout = %q, want to contain %q", stdout, want)
	}
}

func TestHelpVariants(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		stdout, _, code := runCapture(t, arg)
		if code != 0 {
			t.Errorf("%q exit code = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "Subcommands:") {
			t.Errorf("%q stdout missing usage block: %q", arg, stdout)
		}
	}
}

func TestNoArgsShowsUsageAndReturns2(t *testing.T) {
	_, stderr, code := runCapture(t)
	if code != 2 {
		t.Errorf("no-args exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("no-args stderr missing usage block: %q", stderr)
	}
}

func TestUnknownSubcommandReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "frobnicate")
	if code != 2 {
		t.Errorf("unknown exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown subcommand: frobnicate") {
		t.Errorf("unknown stderr missing error: %q", stderr)
	}
}

func TestServeStubLoadsConfigAndReturns1(t *testing.T) {
	cfgPath := writeValidConfig(t)
	stdout, stderr, code := runCapture(t, "serve", "--config", cfgPath)
	if code != 1 {
		t.Errorf("serve stub exit code = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "config loaded") {
		t.Errorf("serve stdout missing config summary: %q", stdout)
	}
	if !strings.Contains(stderr, "not yet implemented") {
		t.Errorf("serve stderr missing marker: %q", stderr)
	}
}

func TestServeMissingConfigReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "serve", "--config", "/nonexistent/bridge.yaml")
	if code != 2 {
		t.Errorf("serve missing-config exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config load failed") {
		t.Errorf("serve missing-config stderr: %q", stderr)
	}
}

func TestServeAddrOverride(t *testing.T) {
	cfgPath := writeValidConfig(t)
	stdout, _, _ := runCapture(t, "serve", "--config", cfgPath, "--addr", "127.0.0.1:1234")
	if !strings.Contains(stdout, "listen=127.0.0.1:1234") {
		t.Errorf("serve --addr override not honored: %q", stdout)
	}
}

func TestPairStubReturns1(t *testing.T) {
	_, stderr, code := runCapture(t, "pair", "--name", "test")
	if code != 1 {
		t.Errorf("pair stub exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not yet implemented") {
		t.Errorf("pair stub stderr missing marker: %q", stderr)
	}
}

func TestScanStubLoadsConfigAndReturns1(t *testing.T) {
	cfgPath := writeValidConfig(t)
	_, stderr, code := runCapture(t, "scan", "--config", cfgPath)
	if code != 1 {
		t.Errorf("scan stub exit code = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "not yet implemented") {
		t.Errorf("scan stub stderr missing marker: %q", stderr)
	}
}

func TestScanMissingConfigReturns2(t *testing.T) {
	_, stderr, code := runCapture(t, "scan", "--config", "/nonexistent/bridge.yaml")
	if code != 2 {
		t.Errorf("scan missing-config exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config load failed") {
		t.Errorf("scan missing-config stderr: %q", stderr)
	}
}

func TestProtocolVersionIsOne(t *testing.T) {
	// PROTOCOL.md is the source of truth; the constant must match v1 until a
	// breaking wire change bumps both. If you bump ProtocolVersion, update
	// PROTOCOL.md AND the iOS-repo mirror in the same PR cycle (see
	// CONTRIBUTING.md — Mirror-PR rule).
	if ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1; did you forget the Mirror-PR?", ProtocolVersion)
	}
}
