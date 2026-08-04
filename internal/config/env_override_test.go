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
		"dataDir: " + yamlStr(filepath.Join(dir, "yaml-data")) + "\n" +
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

// TestEnvOverridesBool exercises the ParseBool-backed env toggles
// directly against applyEnvOverrides (the string-var Load test above
// already proves Load calls it). Covers the three container toggles:
// BRIDGE_UPSCALE_ENABLED / BRIDGE_ANALYSIS_ENABLED (added for the
// Docker audio-tools image) and BRIDGE_DISABLE_HTTP3 (previously
// untested — backfilled here). The load-bearing case is "garbage
// leaves the prior value untouched": a value ParseBool can't decode
// (e.g. "yes", "on") must be silently ignored, NOT coerced to false,
// so a typo can't flip a feature off behind the operator's back.
func TestEnvOverridesBool(t *testing.T) {
	// Every case seeds one bool field, sets one env var, and asserts
	// the post-override value. field() returns a pointer so the same
	// accessor both seeds and reads.
	cases := []struct {
		name  string
		key   string
		value string // "" means the var is left unset
		seed  bool
		want  bool
		field func(*Config) *bool
	}{
		// BRIDGE_UPSCALE_ENABLED
		{"upscale true from false", "BRIDGE_UPSCALE_ENABLED", "true", false, true, func(c *Config) *bool { return &c.Upscale.Enabled }},
		{"upscale false from true", "BRIDGE_UPSCALE_ENABLED", "false", true, false, func(c *Config) *bool { return &c.Upscale.Enabled }},
		{"upscale 1 from false", "BRIDGE_UPSCALE_ENABLED", "1", false, true, func(c *Config) *bool { return &c.Upscale.Enabled }},
		{"upscale garbage keeps seed true", "BRIDGE_UPSCALE_ENABLED", "yes", true, true, func(c *Config) *bool { return &c.Upscale.Enabled }},
		{"upscale garbage keeps seed false", "BRIDGE_UPSCALE_ENABLED", "maybe", false, false, func(c *Config) *bool { return &c.Upscale.Enabled }},
		{"upscale unset keeps seed true", "BRIDGE_UPSCALE_ENABLED", "", true, true, func(c *Config) *bool { return &c.Upscale.Enabled }},
		// BRIDGE_ANALYSIS_ENABLED
		{"analysis true from false", "BRIDGE_ANALYSIS_ENABLED", "true", false, true, func(c *Config) *bool { return &c.Analysis.Enabled }},
		{"analysis false from true", "BRIDGE_ANALYSIS_ENABLED", "false", true, false, func(c *Config) *bool { return &c.Analysis.Enabled }},
		{"analysis garbage keeps seed true", "BRIDGE_ANALYSIS_ENABLED", "on", true, true, func(c *Config) *bool { return &c.Analysis.Enabled }},
		{"analysis unset keeps seed false", "BRIDGE_ANALYSIS_ENABLED", "", false, false, func(c *Config) *bool { return &c.Analysis.Enabled }},
		// BRIDGE_DISABLE_HTTP3 (boy-scout: previously had no coverage)
		{"http3 true from false", "BRIDGE_DISABLE_HTTP3", "true", false, true, func(c *Config) *bool { return &c.DisableHTTP3 }},
		{"http3 false from true", "BRIDGE_DISABLE_HTTP3", "false", true, false, func(c *Config) *bool { return &c.DisableHTTP3 }},
		{"http3 garbage keeps seed true", "BRIDGE_DISABLE_HTTP3", "nope", true, true, func(c *Config) *bool { return &c.DisableHTTP3 }},
		{"http3 unset keeps seed true", "BRIDGE_DISABLE_HTTP3", "", true, true, func(c *Config) *bool { return &c.DisableHTTP3 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear all three keys so a value leaked from the dev
			// machine's shell can't skew a case that expects "unset".
			for _, k := range []string{
				"BRIDGE_UPSCALE_ENABLED", "BRIDGE_ANALYSIS_ENABLED", "BRIDGE_DISABLE_HTTP3",
			} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			if tc.value != "" {
				t.Setenv(tc.key, tc.value)
			}
			c := &Config{}
			p := tc.field(c)
			*p = tc.seed
			c.applyEnvOverrides()
			if got := *tc.field(c); got != tc.want {
				t.Errorf("%s=%q seed=%v: got %v, want %v", tc.key, tc.value, tc.seed, got, tc.want)
			}
		})
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
		"dataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n")
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
