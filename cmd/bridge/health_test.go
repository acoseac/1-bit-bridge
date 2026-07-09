package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestHealthCmdListenerUp: a bound listener → exit 0 (healthy).
func TestHealthCmdListenerUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfgPath := writeHealthTestConfig(t, ln.Addr().String())
	if got := healthCmd(context.Background(), []string{"--config", cfgPath}, io.Discard, io.Discard); got != 0 {
		t.Errorf("healthCmd (listener up) exit = %d, want 0", got)
	}
}

// TestHealthCmdConnectionRefused: nothing listening → exit 1 (unhealthy).
func TestHealthCmdConnectionRefused(t *testing.T) {
	cfgPath := writeHealthTestConfig(t, "127.0.0.1:1")
	if got := healthCmd(context.Background(), []string{"--config", cfgPath}, io.Discard, io.Discard); got != 1 {
		t.Errorf("healthCmd (refused) exit = %d, want 1", got)
	}
}

// TestHealthCmdWildcardDialsLoopback: a wildcard bind (":PORT") must be
// probed on 127.0.0.1:PORT, not a literal empty host.
func TestHealthCmdWildcardDialsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := writeHealthTestConfig(t, ":"+port)
	if got := healthCmd(context.Background(), []string{"--config", cfgPath}, io.Discard, io.Discard); got != 0 {
		t.Errorf("healthCmd (wildcard bind) exit = %d, want 0", got)
	}
}

// writeHealthTestConfig writes a minimal valid loopback config whose API
// listenAddress points at the given host:port, and returns its path.
func writeHealthTestConfig(t *testing.T, listenAddr string) string {
	t.Helper()
	dir := t.TempDir()
	// Clear any leaked BRIDGE_* so the seeded listenAddress isn't overridden.
	for _, k := range []string{"BRIDGE_LISTEN_ADDRESS", "BRIDGE_LIBRARY_ROOTS", "BRIDGE_DATA_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg := baseConfig([]string{dir}, "Health Test", filepath.Join(dir, "data"))
	cfg.ListenAddress = listenAddr
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return cfgPath
}
