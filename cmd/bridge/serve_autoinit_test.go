package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestBaseConfigProducesValidLoopbackConfig guards the extraction of the
// shared loopback seed builder (used by both `bridge init` and serve's
// --init-if-missing): it must produce a Validate-passing loopback config.
func TestBaseConfigProducesValidLoopbackConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig([]string{dir}, "Test Library", filepath.Join(dir, "data"))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseConfig should Validate: %v", err)
	}
	if cfg.AdminAddress != config.DefaultAdminAddress {
		t.Errorf("AdminAddress = %q, want loopback default %q", cfg.AdminAddress, config.DefaultAdminAddress)
	}
	if cfg.ListenAddress != config.DefaultListenAddress {
		t.Errorf("ListenAddress = %q, want %q", cfg.ListenAddress, config.DefaultListenAddress)
	}
}

// TestWriteAutoInitConfigSeedsSparseDefaults checks the auto-init seed: it
// creates the parent dir (MkdirAll — config.Save's CreateTemp needs it),
// writes the /library default, and the result loads end-to-end.
func TestWriteAutoInitConfigSeedsSparseDefaults(t *testing.T) {
	// Nested path whose parent doesn't exist yet — exercises MkdirAll.
	cfgPath := filepath.Join(t.TempDir(), "sub", "bridge.yaml")
	if err := writeAutoInitConfig(cfgPath); err != nil {
		t.Fatalf("writeAutoInitConfig: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if !strings.Contains(string(raw), autoInitDefaultRoot) {
		t.Errorf("seed should contain default root %q; got:\n%s", autoInitDefaultRoot, raw)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("config.Load of the seed failed: %v", err)
	}
}

// TestAutoInitSeedDoesNotCaptureEnv proves the load-bearing "no state
// capture" contract: the seed is written WITHOUT reading BRIDGE_* env, so at
// runtime env wins (injected by config.Load's applyEnvOverrides) while the
// on-disk YAML keeps the sparse /library fallback. If the seed instead baked
// the day-one env values, a later docker-compose that dropped a variable
// would surprisingly keep the old value.
func TestAutoInitSeedDoesNotCaptureEnv(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := writeAutoInitConfig(cfgPath); err != nil {
		t.Fatalf("writeAutoInitConfig: %v", err)
	}
	t.Setenv("BRIDGE_LIBRARY_ROOTS", "/music")
	t.Setenv("BRIDGE_LIBRARY_NAME", "Env Name")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.LibraryRoots) != 1 || cfg.LibraryRoots[0] != "/music" {
		t.Errorf("runtime LibraryRoots = %v, want [/music] (env should win)", cfg.LibraryRoots)
	}
	if cfg.LibraryName != "Env Name" {
		t.Errorf("runtime LibraryName = %q, want the env value", cfg.LibraryName)
	}
	// The persisted YAML must still carry the sparse defaults, NOT the env
	// values — otherwise dropping the env var later would surprise.
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "/music") || strings.Contains(string(raw), "Env Name") {
		t.Errorf("seed YAML captured env state (should stay sparse):\n%s", raw)
	}
}
