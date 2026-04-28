package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestEnvOverrides asserts that BRIDGE_* env vars win over the
// YAML values, that an unset var leaves the YAML untouched, and
// that BRIDGE_LIBRARY_ROOTS is colon-split with empty fragments
// dropped. Pinned because Docker/k8s deployments depend on this
// contract; an accidental regression here breaks every container
// deployment silently.
func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib2"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("libraryRoots:\n  - " + filepath.Join(dir, "yaml-lib") + "\n" +
		"listenAddress: \":7788\"\n" +
		"adminAddress: \"127.0.0.1:7789\"\n" +
		"dataDir: \"" + filepath.Join(dir, "yaml-data") + "\"\n" +
		"libraryName: \"yaml name\"\n")
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "yaml-lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BRIDGE_LISTEN_ADDRESS", ":9090")
	t.Setenv("BRIDGE_ADMIN_ADDRESS", "127.0.0.1:9091")
	t.Setenv("BRIDGE_DATA_DIR", filepath.Join(dir, "env-data"))
	t.Setenv("BRIDGE_LIBRARY_NAME", "env name")
	// Use os.PathListSeparator (`:` on POSIX, `;` on Windows) to
	// match the production code's split behaviour. Pre-fix the
	// test hard-coded `:` which passed on POSIX but would fail on
	// Windows where drive-letter paths can't be colon-split (Qodo
	// Bug post-merge on PR #85). Also exercises the trailing-
	// empty-fragment trim by appending a doubled separator.
	sep := string(os.PathListSeparator)
	t.Setenv("BRIDGE_LIBRARY_ROOTS",
		filepath.Join(dir, "lib1")+sep+filepath.Join(dir, "lib2")+sep+sep)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddress != ":9090" {
		t.Errorf("ListenAddress = %q, want :9090", cfg.ListenAddress)
	}
	if cfg.AdminAddress != "127.0.0.1:9091" {
		t.Errorf("AdminAddress = %q, want 127.0.0.1:9091", cfg.AdminAddress)
	}
	if cfg.DataDir != filepath.Join(dir, "env-data") {
		t.Errorf("DataDir = %q, want %s", cfg.DataDir, filepath.Join(dir, "env-data"))
	}
	if cfg.LibraryName != "env name" {
		t.Errorf("LibraryName = %q, want \"env name\"", cfg.LibraryName)
	}
	wantRoots := []string{filepath.Join(dir, "lib1"), filepath.Join(dir, "lib2")}
	if !reflect.DeepEqual(cfg.LibraryRoots, wantRoots) {
		t.Errorf("LibraryRoots = %v, want %v", cfg.LibraryRoots, wantRoots)
	}
}

// TestEnvOverridesAbsent asserts unset vars leave YAML untouched
// — guards against an over-eager always-overwrite implementation.
func TestEnvOverridesAbsent(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("libraryRoots:\n  - " + libDir + "\n" +
		"listenAddress: \":7788\"\n" +
		"adminAddress: \"127.0.0.1:7789\"\n" +
		"dataDir: \"" + filepath.Join(dir, "data") + "\"\n")
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	// Belt-and-suspenders: explicitly clear any leakage from the
	// process environment so this test is deterministic on a dev
	// machine that has BRIDGE_* set in their shell.
	for _, k := range []string{
		"BRIDGE_LISTEN_ADDRESS", "BRIDGE_ADMIN_ADDRESS",
		"BRIDGE_DATA_DIR", "BRIDGE_LIBRARY_NAME", "BRIDGE_LIBRARY_ROOTS",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k) // Setenv("") sets empty; Unsetenv removes the key entirely
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddress != ":7788" {
		t.Errorf("ListenAddress = %q, want :7788 (yaml)", cfg.ListenAddress)
	}
	if cfg.LibraryRoots[0] != libDir {
		t.Errorf("LibraryRoots[0] = %q, want %q (yaml)", cfg.LibraryRoots[0], libDir)
	}
}
