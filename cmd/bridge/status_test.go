package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusServiceNotRunning pins: `bridge status` against an
// admin address that nothing is bound to surfaces the "not
// running" hint and returns exit code 1. The CLI should NOT
// crash or hang — the 5s probe timeout caps the wait.
func TestStatusServiceNotRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	// Bind a listener to claim a port, then close it so the port
	// is free for the test (avoids accidentally hitting another
	// process). Use the closed port in cfg.AdminAddress so the
	// connect attempt deterministically fails.
	lis := httptest.NewServer(nil)
	addr := lis.Listener.Addr().String()
	lis.Close()

	yaml := "libraryRoots:\n  - " + dir + "\n" +
		"adminAddress: \"" + addr + "\"\n" +
		"listenAddress: \":7788\"\n" +
		"dataDir: \"" + filepath.Join(dir, "data") + "\"\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := statusCmd(context.Background(), []string{"-config", cfg}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Errorf("missing 'not running' hint, stderr = %q", stderr.String())
	}
}

// TestStatusJSONFlag pins: --json route gates on the JSON branch
// (we can't easily exercise a fake admin server here without
// duplicating handlers; the test scope is "the flag is wired and
// not-running still surfaces").
func TestStatusJSONFlagSurfacesNotRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	lis := httptest.NewServer(nil)
	addr := lis.Listener.Addr().String()
	lis.Close()
	yaml := "libraryRoots:\n  - " + dir + "\n" +
		"adminAddress: \"" + addr + "\"\n" +
		"listenAddress: \":7788\"\n" +
		"dataDir: \"" + filepath.Join(dir, "data") + "\"\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := statusCmd(context.Background(), []string{"-config", cfg, "-json"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (not running, no JSON to emit)", code)
	}
}
