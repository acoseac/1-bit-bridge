package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthCmd(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"200 healthy", http.StatusOK, 0},
		{"500 unhealthy", http.StatusInternalServerError, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/health" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()
			cfgPath := writeHealthTestConfig(t, ts.Listener.Addr().String())
			got := healthCmd(context.Background(), []string{"--config", cfgPath}, io.Discard, io.Discard)
			if got != tc.want {
				t.Errorf("healthCmd exit = %d, want %d", got, tc.want)
			}
		})
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
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
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
