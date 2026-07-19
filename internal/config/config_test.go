package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// writeConfig writes YAML to a tmp dir and returns (configPath, libraryRoot).
// The library root is a real directory inside the tmp, so Validate's Stat
// checks pass without mocking the filesystem.
func writeConfig(t *testing.T, yaml string) (configPath, libraryRoot string) {
	t.Helper()
	dir := t.TempDir()
	libraryRoot = filepath.Join(dir, "Music")
	if err := os.MkdirAll(libraryRoot, 0o755); err != nil {
		t.Fatalf("mkdir libraryRoot: %v", err)
	}
	configPath = filepath.Join(dir, "bridge.yaml")
	rendered := strings.ReplaceAll(yaml, "{{LIBRARY_ROOT}}", libraryRoot)
	if err := os.WriteFile(configPath, []byte(rendered), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath, libraryRoot
}

func TestLoadHappyPathAllFields(t *testing.T) {
	configPath, libRoot := writeConfig(t, `
libraryRoots:
  - {{LIBRARY_ROOT}}
listenAddress: "127.0.0.1:9000"
dataDir: /tmp/bridge-data
tlsCertPath: /tmp/bridge.crt
tlsKeyPath: /tmp/bridge.key
scanIntervalSec: 600
libraryName: "Test Library"
`)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LibraryRoots) != 1 || cfg.LibraryRoots[0] != libRoot {
		t.Errorf("LibraryRoots = %v, want [%q]", cfg.LibraryRoots, libRoot)
	}
	if cfg.ListenAddress != "127.0.0.1:9000" {
		t.Errorf("ListenAddress = %q", cfg.ListenAddress)
	}
	if cfg.DataDir != "/tmp/bridge-data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.TLSCertPath != "/tmp/bridge.crt" || cfg.TLSKeyPath != "/tmp/bridge.key" {
		t.Errorf("TLS paths = %q / %q", cfg.TLSCertPath, cfg.TLSKeyPath)
	}
	if cfg.ScanIntervalSec != 600 {
		t.Errorf("ScanIntervalSec = %d", cfg.ScanIntervalSec)
	}
	if cfg.ScanInterval() != 10*time.Minute {
		t.Errorf("ScanInterval = %v, want 10m", cfg.ScanInterval())
	}
	if cfg.LibraryName != "Test Library" {
		t.Errorf("LibraryName = %q", cfg.LibraryName)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	configPath, _ := writeConfig(t, `
libraryRoots:
  - {{LIBRARY_ROOT}}
`)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddress != DefaultListenAddress {
		t.Errorf("ListenAddress = %q, want default %q", cfg.ListenAddress, DefaultListenAddress)
	}
	if cfg.ScanIntervalSec != DefaultScanIntervalSec {
		t.Errorf("ScanIntervalSec = %d, want default %d", cfg.ScanIntervalSec, DefaultScanIntervalSec)
	}
	if cfg.LibraryName != DefaultLibraryName {
		t.Errorf("LibraryName = %q, want default %q", cfg.LibraryName, DefaultLibraryName)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("DataDir should be resolved to absolute, got %q", cfg.DataDir)
	}
	if !strings.HasSuffix(cfg.DataDir, "data") {
		t.Errorf("DataDir should end with 'data', got %q", cfg.DataDir)
	}
}

func TestLoadResolvesRelativePathsAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	// Library root is a subdir of the tmp; config references it relatively.
	libRel := "Music"
	libAbs := filepath.Join(dir, libRel)
	if err := os.MkdirAll(libAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(configPath, []byte("libraryRoots:\n  - "+libRel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LibraryRoots[0] != libAbs {
		t.Errorf("relative libraryRoot: got %q, want %q", cfg.LibraryRoots[0], libAbs)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/bridge.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist-wrapped error, got %v", err)
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p, []byte("::: not yaml :::"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	configPath, _ := writeConfig(t, `
libraryRoots:
  - {{LIBRARY_ROOT}}
libraryRootz: "typo"
`)
	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "libraryRootz") {
		t.Errorf("expected typo to be caught, got %v", err)
	}
}

func TestValidateEmptyLibraryRoots(t *testing.T) {
	cfg := &Config{ListenAddress: ":7788", ScanIntervalSec: 3600}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "libraryRoots") {
		t.Errorf("expected libraryRoots error, got %v", err)
	}
}

// TestValidatePassesNonexistentLibraryRoot pins the post-A2-refactor
// contract: Validate() is a pure shape check and no longer stats
// individual library roots. Filesystem-accessibility is the
// caller's decision via CheckLibraryRootsAccessible (the regression
// target — bridge.ars.md's public-mode VPS layout where the daemon
// runs against a FUSE mount root can't stat, so the stat-in-Validate
// shape took down `sudo bridge update` even though update doesn't
// need library access).
func TestValidatePassesNonexistentLibraryRoot(t *testing.T) {
	cfg := &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected Validate() to ignore root existence, got %v", err)
	}
}

func TestValidatePassesLibraryRootIsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{LibraryRoots: []string{file}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected Validate() to ignore is-directory, got %v", err)
	}
}

// TestValidate_RejectsNegativeSweepIntervals pins the standardized
// negative-interval handling (r1 review): the sweep intervals are
// *int (nil=default, 0=disabled), and a negative is a misconfiguration
// that must fail loudly — matching update.checkIntervalHours /
// backup.intervalHours — rather than silently clamp to disabled.
func TestValidate_RejectsNegativeSweepIntervals(t *testing.T) {
	neg := -5
	base := func() *Config {
		return &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	}
	// Sanity: the base config validates cleanly so the negative is the
	// only thing under test.
	if err := base().Validate(); err != nil {
		t.Fatalf("base config should validate, got %v", err)
	}
	t.Run("variantSweepIntervalSec", func(t *testing.T) {
		cfg := base()
		cfg.Integrity.VariantSweepIntervalSec = &neg
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "variantSweepIntervalSec") {
			t.Fatalf("expected variantSweepIntervalSec error, got %v", err)
		}
	})
	t.Run("orphanSidecarSweepIntervalSec", func(t *testing.T) {
		cfg := base()
		cfg.Integrity.OrphanSidecarSweepIntervalSec = &neg
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "orphanSidecarSweepIntervalSec") {
			t.Fatalf("expected orphanSidecarSweepIntervalSec error, got %v", err)
		}
	})
}

// TestValidate_ArtworkCacheMaxBytes pins the artwork-cache cap shape check:
// zero is the valid unbounded default, a positive byte cap validates, a
// negative value is rejected as a typo.
func TestValidate_ArtworkCacheMaxBytes(t *testing.T) {
	base := func() *Config {
		return &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	}
	t.Run("zero is unbounded default", func(t *testing.T) {
		cfg := base()
		cfg.Artwork.CacheMaxBytes = 0
		if err := cfg.Validate(); err != nil {
			t.Fatalf("zero cap should validate, got %v", err)
		}
	})
	t.Run("positive cap validates", func(t *testing.T) {
		cfg := base()
		cfg.Artwork.CacheMaxBytes = 2 << 30 // 2 GiB
		if err := cfg.Validate(); err != nil {
			t.Fatalf("positive cap should validate, got %v", err)
		}
	})
	t.Run("negative rejected", func(t *testing.T) {
		cfg := base()
		cfg.Artwork.CacheMaxBytes = -1
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "artwork.cacheMaxBytes") {
			t.Fatalf("expected artwork.cacheMaxBytes error, got %v", err)
		}
	})
}

// TestValidate_RejectsNegativeDLNADiscoveryIntervals pins the r1-review
// negative-interval standardization for the dlna.discovery block (gated
// on Discovery.Enabled, alongside the existing TTL-vs-interval check).
func TestValidate_RejectsNegativeDLNADiscoveryIntervals(t *testing.T) {
	base := func() *Config {
		c := &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
		c.DLNA.Discovery.Enabled = true
		return c
	}
	// Sanity: discovery enabled with default intervals validates, so the
	// negative is the only thing under test.
	if err := base().Validate(); err != nil {
		t.Fatalf("discovery-enabled base should validate, got %v", err)
	}
	t.Run("msearchIntervalSeconds", func(t *testing.T) {
		cfg := base()
		cfg.DLNA.Discovery.MSearchIntervalSeconds = -30
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "msearchIntervalSeconds") {
			t.Fatalf("expected msearchIntervalSeconds error, got %v", err)
		}
	})
	t.Run("rendererTTLSeconds", func(t *testing.T) {
		cfg := base()
		cfg.DLNA.Discovery.RendererTTLSeconds = -60
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rendererTTLSeconds") {
			t.Fatalf("expected rendererTTLSeconds error, got %v", err)
		}
	})
}

func TestCheckLibraryRootsAccessibleNonexistent(t *testing.T) {
	cfg := &Config{LibraryRoots: []string{"/nonexistent"}}
	errs := cfg.CheckLibraryRootsAccessible()
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
	if errs[0].Path != "/nonexistent" {
		t.Errorf("Path = %q, want %q", errs[0].Path, "/nonexistent")
	}
	if !errors.Is(errs[0].Err, os.ErrNotExist) {
		t.Errorf("Err = %v, want os.ErrNotExist wrapping", errs[0].Err)
	}
}

func TestCheckLibraryRootsAccessibleIsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{LibraryRoots: []string{file}}
	errs := cfg.CheckLibraryRootsAccessible()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "not a directory") {
		t.Errorf("expected 'not a directory' error, got %v", errs)
	}
}

func TestCheckLibraryRootsAccessibleAllPresent(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{LibraryRoots: []string{dir}}
	if errs := cfg.CheckLibraryRootsAccessible(); len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

// TestLoadSucceedsWithInaccessibleLibraryRoot is the
// bridge.ars.md regression target at the Load() level (the
// surface `bridge update` / `bridge status` actually invoke).
// The YAML references a library root whose stat returns EACCES,
// and Load() must succeed — otherwise public-mode operators
// can't run `sudo bridge update` against a FUSE-mounted library
// inaccessible to root.
func TestLoadSucceedsWithInaccessibleLibraryRoot(t *testing.T) {
	leaf := makeInaccessibleLibraryRoot(t)
	dir := t.TempDir() // distinct from the leaf's tmpdir — holds the YAML

	configPath := filepath.Join(dir, "bridge.yaml")
	yaml := "libraryRoots:\n  - " + leaf + "\nlistenAddress: \":7788\"\ndataDir: " + dir + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed against an inaccessible library root — `sudo bridge update` would break: %v", err)
	}
	// Confirm the fields `bridge update` reads are populated.
	if cfg.DataDir == "" {
		t.Error("DataDir empty — update would fail to locate the token store")
	}
	if cfg.ListenAddress == "" {
		t.Error("ListenAddress empty — applyDefaults didn't run")
	}
	// And confirm the new method DOES catch the EACCES at runtime
	// — startup decides whether that's fatal.
	errs := cfg.CheckLibraryRootsAccessible()
	if len(errs) != 1 {
		t.Errorf("CheckLibraryRootsAccessible: got %d errors, want 1: %v", len(errs), errs)
	}
}

// makeInaccessibleLibraryRoot returns a leaf directory whose
// `os.Stat` produces EACCES — built by chmod 0o000'ing the leaf's
// parent. Mirrors the FUSE-mount-without-allow_other shape the
// bridge.ars.md regression target lives in. Skips the test on
// platforms where POSIX directory modes don't apply (Windows) or
// where the caller bypasses them (root on Unix); detected via a
// behaviour probe after the chmod, not a static
// `runtime.GOOS == "windows"` / `os.Getuid() == 0` gate. The
// probe is robust to a runtime hardening change in either
// direction (Gemini high on PR #302).
func makeInaccessibleLibraryRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	parent := filepath.Join(dir, "locked-parent")
	leaf := filepath.Join(parent, "music")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir leaf: %v", err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	// Restore perms so t.TempDir cleanup can walk it after the test.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if _, err := os.Stat(leaf); err == nil {
		t.Skip("filesystem permissions restriction not enforced (Windows, or running as root on Unix)")
	}
	return leaf
}

// TestCheckLibraryRootsAccessiblePermissionDenied is the
// bridge.ars.md regression target: stat returns EACCES (the FUSE
// mount with allow_other off, as seen by root). The check reports
// ONE wrapped error per inaccessible root; Validate() on the same
// input succeeds because library-root accessibility is no longer
// part of the shape contract.
func TestCheckLibraryRootsAccessiblePermissionDenied(t *testing.T) {
	leaf := makeInaccessibleLibraryRoot(t)

	cfg := &Config{LibraryRoots: []string{leaf}}
	errs := cfg.CheckLibraryRootsAccessible()
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
	if !errors.Is(errs[0].Err, os.ErrPermission) {
		t.Errorf("Err = %v, want os.ErrPermission wrapping", errs[0].Err)
	}
	// Crucially: Validate() should NOT have failed on the same input.
	loopback := &Config{LibraryRoots: []string{leaf}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	if err := loopback.Validate(); err != nil {
		t.Errorf("Validate() should not stat library roots; got %v", err)
	}
}

func TestValidateScanIntervalNegative(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{LibraryRoots: []string{dir}, ListenAddress: ":7788", ScanIntervalSec: -1}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "scanIntervalSec") {
		t.Errorf("expected scanIntervalSec error, got %v", err)
	}
}

func TestValidateTLSMustBePairedCertOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		ScanIntervalSec: 3600,
		TLSCertPath:     "/tmp/cert.pem",
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tlsCertPath and tlsKeyPath") {
		t.Errorf("expected TLS-paired error, got %v", err)
	}
}

func TestValidateTLSMustBePairedKeyOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		ScanIntervalSec: 3600,
		TLSKeyPath:      "/tmp/key.pem",
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tlsCertPath and tlsKeyPath") {
		t.Errorf("expected TLS-paired error, got %v", err)
	}
}

func TestValidateBadListenAddress(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   "not-a-host-port",
		ScanIntervalSec: 3600,
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "listenAddress") {
		t.Errorf("expected listenAddress error, got %v", err)
	}
}

func TestLoadShippedExample(t *testing.T) {
	// The config/bridge.yaml.example we ship to users must itself parse (modulo
	// its libraryRoots being /Users/me/Music, which won't exist in CI). So we
	// copy it to tmp, swap the libraryRoots to a real dir, and Load.
	examplePath := filepath.Join("..", "..", "config", "bridge.yaml.example")
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("example config not found (running from nonstandard dir?): %v", err)
	}
	configPath, _ := writeConfig(t, strings.ReplaceAll(
		string(raw),
		"/Users/me/Music",
		"{{LIBRARY_ROOT}}",
	))
	if _, err := Load(configPath); err != nil {
		t.Errorf("shipped example failed to Load: %v", err)
	}
}

func TestScanIntervalConversion(t *testing.T) {
	c := &Config{ScanIntervalSec: 45}
	if c.ScanInterval() != 45*time.Second {
		t.Errorf("ScanInterval = %v", c.ScanInterval())
	}
}

func TestLoadAppliesAdminAddressDefault(t *testing.T) {
	configPath, _ := writeConfig(t, `
libraryRoots:
  - {{LIBRARY_ROOT}}
`)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminAddress != DefaultAdminAddress {
		t.Errorf("AdminAddress = %q, want default %q", cfg.AdminAddress, DefaultAdminAddress)
	}
}

func TestValidateAdminAddressLoopbackOK(t *testing.T) {
	dir := t.TempDir()
	for _, addr := range []string{"127.0.0.1:7789", "[::1]:7789", "localhost:7789"} {
		cfg := &Config{
			LibraryRoots:    []string{dir},
			ListenAddress:   ":7788",
			AdminAddress:    addr,
			ScanIntervalSec: 3600,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected error %v", addr, err)
		}
	}
}

func TestValidateAdminAddressRejectsNonLoopback(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		addr, wantSubstr string
	}{
		{":7789", "empty host"},
		{"0.0.0.0:7789", "not a loopback"},
		{"192.168.1.5:7789", "not a loopback"},
		{"example.com:7789", "loopback"},
		{"127.0.0.1", "missing port"}, // net.SplitHostPort's own error
	} {
		cfg := &Config{
			LibraryRoots:    []string{dir},
			ListenAddress:   ":7788",
			AdminAddress:    tc.addr,
			ScanIntervalSec: 3600,
		}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("Validate(%q): expected error, got nil", tc.addr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("Validate(%q): error %q missing substring %q", tc.addr, err.Error(), tc.wantSubstr)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bridge.yaml")
	orig := &Config{
		LibraryRoots:    []string{libRoot},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(dir, "data"),
		ScanIntervalSec: 1800,
		LibraryName:     "Home",
	}
	if err := orig.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if loaded.LibraryName != orig.LibraryName ||
		loaded.ListenAddress != orig.ListenAddress ||
		loaded.AdminAddress != orig.AdminAddress ||
		loaded.ScanIntervalSec != orig.ScanIntervalSec ||
		len(loaded.LibraryRoots) != 1 ||
		loaded.LibraryRoots[0] != libRoot {
		t.Errorf("round-trip mismatch:\n  orig:   %+v\n  loaded: %+v", orig, loaded)
	}
}

func TestSaveAtomicRename(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		LibraryRoots:    []string{libRoot},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(dir, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "H",
	}
	if err := cfg.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// No .bridge-*.yaml leftovers in the parent dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bridge-") {
			t.Errorf("temp file leaked: %q", e.Name())
		}
	}
}

// PROTOCOL.md says "intervalHours: 0 disables the periodic ticker".
// Pre-fixup, applyDefaults clobbered an explicit 0 with the default,
// so the operator couldn't disable from YAML — they had to remove
// the section, and any future Save() round-trip would re-add it.
// The pointer-typed IntervalHours preserves the distinction; this
// test locks it in so a future "simplify config" refactor can't
// silently re-introduce the bug.
func TestBackupIntervalHoursOmittedAppliesDefault(t *testing.T) {
	libRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfgPath,
		[]byte("libraryRoots:\n  - "+libRoot+"\nlistenAddress: ':7788'\nadminAddress: 127.0.0.1:7789\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Backup.EffectiveIntervalHours(); got != DefaultBackupIntervalHours {
		t.Errorf("absent intervalHours: EffectiveIntervalHours=%d, want %d", got, DefaultBackupIntervalHours)
	}
	if got := c.Backup.EffectiveKeep(); got != DefaultBackupKeep {
		t.Errorf("absent keep: EffectiveKeep=%d, want %d", got, DefaultBackupKeep)
	}
}

func TestBackupIntervalHoursExplicitZeroDisables(t *testing.T) {
	libRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfgPath,
		[]byte("libraryRoots:\n  - "+libRoot+"\nlistenAddress: ':7788'\nadminAddress: 127.0.0.1:7789\nbackup:\n  intervalHours: 0\n  keep: 5\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Backup.EffectiveIntervalHours(); got != 0 {
		t.Errorf("explicit intervalHours=0: EffectiveIntervalHours=%d, want 0 (disabled)", got)
	}
	if got := c.Backup.EffectiveKeep(); got != 5 {
		t.Errorf("explicit keep=5: EffectiveKeep=%d, want 5", got)
	}
}

// TestEffectiveKeepZeroVsNegative pins the documented contract (bridge02-03
// review, finding D): `keep: 0` (and an omitted section, which omitempty
// makes indistinguishable) fall back to DefaultBackupKeep — zero does NOT
// disable pruning. A negative value passes through verbatim; backup.Prune
// treats keep <= 0 as "retain everything", so `keep: -1` is the disable
// sentinel.
func TestEffectiveKeepZeroVsNegative(t *testing.T) {
	if got := (BackupConfig{Keep: 0}).EffectiveKeep(); got != DefaultBackupKeep {
		t.Errorf("keep=0: EffectiveKeep=%d, want DefaultBackupKeep=%d (zero does not disable)", got, DefaultBackupKeep)
	}
	if got := (BackupConfig{Keep: -1}).EffectiveKeep(); got != -1 {
		t.Errorf("keep=-1: EffectiveKeep=%d, want -1 (disable sentinel passed through to Prune)", got)
	}
	if got := (BackupConfig{Keep: 5}).EffectiveKeep(); got != 5 {
		t.Errorf("keep=5: EffectiveKeep=%d, want 5", got)
	}
}

func TestBackupIntervalHoursPositiveOverrides(t *testing.T) {
	libRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfgPath,
		[]byte("libraryRoots:\n  - "+libRoot+"\nlistenAddress: ':7788'\nadminAddress: 127.0.0.1:7789\nbackup:\n  intervalHours: 6\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Backup.EffectiveIntervalHours(); got != 6 {
		t.Errorf("explicit intervalHours=6: EffectiveIntervalHours=%d, want 6", got)
	}
}

func TestBackupIntervalHoursNegativeRejected(t *testing.T) {
	libRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfgPath,
		[]byte("libraryRoots:\n  - "+libRoot+"\nlistenAddress: ':7788'\nadminAddress: 127.0.0.1:7789\nbackup:\n  intervalHours: -3\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil {
		t.Errorf("Load(intervalHours=-3) should fail validation, got nil")
	}
}

func TestValidateCustomEndpoints_HappyPath(t *testing.T) {
	in := []string{
		"https://bridge.example.com:7788",
		"https://192.168.1.10:7788",
		"https://[fe80::1]:7788",
	}
	kept, warns := ValidateCustomEndpoints(in)
	if len(kept) != 3 {
		t.Errorf("expected 3 kept, got %d: %v", len(kept), kept)
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
}

func TestValidateCustomEndpoints_DropsHTTP(t *testing.T) {
	in := []string{
		"http://bridge.example.com:7788",  // wrong scheme
		"https://bridge.example.com:7788", // ok
	}
	kept, warns := ValidateCustomEndpoints(in)
	if len(kept) != 1 || kept[0] != "https://bridge.example.com:7788" {
		t.Errorf("kept = %v, want one HTTPS entry", kept)
	}
	if len(warns) != 1 {
		t.Errorf("expected 1 warning for http://, got %v", warns)
	}
}

func TestValidateCustomEndpoints_DropsMalformed(t *testing.T) {
	in := []string{
		"not-a-url",                 // missing scheme
		":://broken",                // gibberish
		"https://",                  // missing host
		"https://valid.example.com", // ok
	}
	kept, warns := ValidateCustomEndpoints(in)
	if len(kept) != 1 || kept[0] != "https://valid.example.com" {
		t.Errorf("kept = %v, want one valid entry", kept)
	}
	if len(warns) < 2 {
		t.Errorf("expected >= 2 warnings, got %v", warns)
	}
}

func TestConfigCloneIsDeep(t *testing.T) {
	// Auto-populate every exported field so assertNoSharedPointers never
	// silently skips a nil slice or pointer. If a future field is added to
	// Config (or any nested struct) without updating this test, fillNonZero
	// will fill it in and the walk will catch a missing deep-copy.
	cfg := &Config{}
	fillNonZero(reflect.ValueOf(cfg).Elem())

	clone := Clone(cfg)
	if clone == nil {
		t.Fatal("Clone returned nil")
	}
	seen := map[uintptr]string{}
	assertNoSharedPointers(t, reflect.ValueOf(cfg).Elem(), reflect.ValueOf(clone).Elem(), "Config", seen)

	// spot-check mutability isolation
	cfg.LibraryRoots[0] = "/mutated"
	if clone.LibraryRoots[0] == "/mutated" {
		t.Fatal("clone LibraryRoots shares backing array")
	}
	if cfg.Backup.IntervalHours != nil && clone.Backup.IntervalHours != nil {
		*cfg.Backup.IntervalHours = 99
		if *clone.Backup.IntervalHours == 99 {
			t.Fatal("clone Backup.IntervalHours shares pointer")
		}
	}
}

func assertNoSharedPointers(t *testing.T, a, b reflect.Value, path string, seen map[uintptr]string) {
	t.Helper()
	if !a.IsValid() || !b.IsValid() {
		return
	}
	if a.Type() != b.Type() {
		t.Fatalf("%s: type mismatch %v vs %v", path, a.Type(), b.Type())
	}
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			assertNoSharedPointers(t, a.Field(i), b.Field(i), path+"."+field.Name, seen)
		}
	case reflect.Slice:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Len() > 0 && b.Len() > 0 {
			pa := a.Pointer()
			pb := b.Pointer()
			if pa == pb {
				t.Fatalf("%s: shared slice backing array", path)
			}
			key := pa
			if key == 0 {
				key = uintptr(unsafe.Pointer(a.UnsafePointer()))
			}
			if prev, ok := seen[key]; ok {
				t.Fatalf("%s: slice backing array already seen at %s", path, prev)
			}
			seen[key] = path
		}
		for i := 0; i < a.Len() && i < b.Len(); i++ {
			assertNoSharedPointers(t, a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), seen)
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Fatalf("%s: shared pointer", path)
		}
		assertNoSharedPointers(t, a.Elem(), b.Elem(), path+"*", seen)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return
		}
		assertNoSharedPointers(t, a.Elem(), b.Elem(), path+"(iface)", seen)
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Fatalf("%s: shared map header", path)
		}
	}
}

// fillNonZero recursively assigns a non-zero value to every exported field in
// v (which must be a settable reflect.Value of Kind Struct). This ensures
// TestConfigCloneIsDeep covers future pointer/slice fields automatically —
// assertNoSharedPointers silently skips nil values, so an unset field in the
// fixture would create a coverage blind spot.
func fillNonZero(v reflect.Value) {
	if v.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			if f.String() == "" {
				f.SetString("x")
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if f.Int() == 0 {
				f.SetInt(1)
			}
		case reflect.Bool:
			if !f.Bool() {
				f.SetBool(true)
			}
		case reflect.Slice:
			if f.IsNil() {
				sl := reflect.MakeSlice(f.Type(), 1, 1)
				elem := sl.Index(0)
				// Recursively seed the first element so future Config
				// fields that hold slices of structs / pointers /
				// nested containers don't silently bypass
				// `assertNoSharedPointers` — that walker skips nil
				// values, so an unset element would be a coverage
				// blind spot. Coderabbit Major on PR #234 (initial
				// struct + pointer cases); Gemini medium on PR #236
				// added Int / Bool / nested Slice / Map for
				// completeness.
				switch elem.Kind() {
				case reflect.String:
					elem.SetString("x")
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					elem.SetInt(1)
				case reflect.Bool:
					elem.SetBool(true)
				case reflect.Struct:
					fillNonZero(elem)
				case reflect.Pointer:
					nv := reflect.New(elem.Type().Elem())
					if nv.Elem().Kind() == reflect.Struct {
						fillNonZero(nv.Elem())
					}
					elem.Set(nv)
				case reflect.Slice:
					// Nested slice (e.g. `[][]T`) — allocate so the
					// walker descends rather than short-circuiting
					// on a nil inner slice. The walker handles the
					// deeper recursion itself.
					elem.Set(reflect.MakeSlice(elem.Type(), 1, 1))
				case reflect.Map:
					// Map header allocation — same rationale; the
					// walker only checks `shared map header` at this
					// level, so an empty initialised map is enough.
					elem.Set(reflect.MakeMap(elem.Type()))
				}
				f.Set(sl)
			}
		case reflect.Pointer:
			if f.IsNil() {
				nv := reflect.New(f.Type().Elem())
				switch nv.Elem().Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					nv.Elem().SetInt(1)
				case reflect.Struct:
					fillNonZero(nv.Elem())
				}
				f.Set(nv)
			} else if f.Elem().Kind() == reflect.Struct {
				fillNonZero(f.Elem())
			}
		case reflect.Struct:
			fillNonZero(f)
		}
	}
}

func TestValidateCustomEndpoints_TrimsAndSkipsBlanks(t *testing.T) {
	in := []string{
		"  https://a.example.com  ",
		"",
		"   ",
		"https://b.example.com",
	}
	kept, _ := ValidateCustomEndpoints(in)
	if len(kept) != 2 {
		t.Errorf("kept = %v, want 2 trimmed entries", kept)
	}
	// Trim is observable: the kept entry must be the trimmed form.
	if kept[0] != "https://a.example.com" {
		t.Errorf("kept[0] = %q, want trimmed", kept[0])
	}
}

func TestValidateCustomEndpoints_Dedupes(t *testing.T) {
	in := []string{
		"https://a.example.com:7788",
		"https://a.example.com:7788", // duplicate
		"https://b.example.com:7788",
	}
	kept, _ := ValidateCustomEndpoints(in)
	if len(kept) != 2 {
		t.Errorf("kept = %v, want 2 deduped entries", kept)
	}
}

// TestValidateCustomEndpoints_DedupesTrailingSlash pins the
// trailing-slash dedup that url.String() alone misses: it treats an
// empty path ("https://h:7788") and a root path ("https://h:7788/") as
// distinct, so both paste-friendly variants would otherwise survive.
// (r1 review fix — the dedup key normalises a bare "/" path to "".)
func TestValidateCustomEndpoints_DedupesTrailingSlash(t *testing.T) {
	in := []string{
		"https://a.example.com:7788",
		"https://a.example.com:7788/", // same endpoint, trailing slash
	}
	kept, _ := ValidateCustomEndpoints(in)
	if len(kept) != 1 {
		t.Errorf("kept = %v, want 1 (trailing-slash variant should dedupe)", kept)
	}
}

// TestValidateCustomEndpoints_RejectsOversizedHost verifies the SAN-bloat
// guard: each accepted hostname is added to the generated TLS cert's
// SAN list, so a hostile / typo'd entry with a multi-kilobyte hostname
// would otherwise balloon the cert binary on every reload.
func TestValidateCustomEndpoints_RejectsOversizedHost(t *testing.T) {
	// 256-char label exceeds the 255-char cap by one. RFC 1035 max FQDN.
	longLabel := strings.Repeat("a", maxCustomEndpointHostLen-len(".example.com")+1)
	longHost := longLabel + ".example.com"
	in := []string{
		"https://" + longHost + ":7788",
		"https://ok.example.com:7788",
	}
	kept, warns := ValidateCustomEndpoints(in)
	if len(kept) != 1 || kept[0] != "https://ok.example.com:7788" {
		t.Errorf("kept = %v, want only the short host", kept)
	}
	if len(warns) != 1 {
		t.Errorf("expected 1 warning for oversized host, got %d: %v", len(warns), warns)
	}
}

// TestValidateCustomEndpoints_AcceptsBoundaryHost pins the boundary at
// exactly maxCustomEndpointHostLen — the hostname is allowed, not
// rejected, when its length matches the constant.
func TestValidateCustomEndpoints_AcceptsBoundaryHost(t *testing.T) {
	// Construct a hostname of exactly maxCustomEndpointHostLen characters.
	// "<label>.example.com" where label fills the remaining space.
	const tld = ".example.com"
	labelLen := maxCustomEndpointHostLen - len(tld)
	host := strings.Repeat("a", labelLen) + tld
	if len(host) != maxCustomEndpointHostLen {
		t.Fatalf("test bug: host len = %d, want %d", len(host), maxCustomEndpointHostLen)
	}
	kept, warns := ValidateCustomEndpoints([]string{"https://" + host + ":7788"})
	if len(kept) != 1 {
		t.Errorf("kept = %v, want boundary host accepted", kept)
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
}

// TestConfigValidatePrunesCustomEndpoints verifies that Validate()
// rewrites the slice in-place — invalid entries are dropped without
// failing the whole config load.
func TestConfigValidatePrunesCustomEndpoints(t *testing.T) {
	libRoot := t.TempDir()
	c := &Config{
		LibraryRoots:    []string{libRoot},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 600,
		LibraryName:     "test",
		CustomEndpoints: []string{
			"https://valid.example.com",
			"http://wrong-scheme.example.com",
			"garbage",
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate should not fail on bad entries: %v", err)
	}
	if len(c.CustomEndpoints) != 1 {
		t.Errorf("post-Validate CustomEndpoints = %v, want only the valid entry", c.CustomEndpoints)
	}
}

// TestValidateRejectsBadTailscaleMode pins that typos in the
// tailscale.mode field surface at config-load time — same surface
// as every other field's validation — instead of slipping through to
// the lifecycle wiring deep inside `bridge serve`. Gemini medium
// on PR #249.
func TestValidateRejectsBadTailscaleMode(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    DefaultAdminAddress, // Validate insists on a loopback addr
		ScanIntervalSec: 3600,
		Tailscale:       TailscaleConfig{Mode: "tnset"}, // typo
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate() to reject a typo'd tailscale.mode, got nil")
	}
	if !strings.Contains(err.Error(), "tailscale.mode") {
		t.Errorf("error should mention tailscale.mode, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote("tnset")) {
		t.Errorf("error should preserve the original input %q verbatim, got %v", "tnset", err)
	}
}

// TestValidateAcceptsKnownTailscaleModes asserts the inverse — that
// Validate() doesn't accidentally start rejecting the three known
// modes (case + whitespace variants included) under the wire-in.
func TestValidateAcceptsKnownTailscaleModes(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"", "cli", "tsnet", "disabled", "  CLI  ", "\tdisabled\n", "TSNet"}
	for _, mode := range cases {
		cfg := &Config{
			LibraryRoots:    []string{dir},
			ListenAddress:   ":7788",
			AdminAddress:    DefaultAdminAddress,
			ScanIntervalSec: 3600,
			Tailscale:       TailscaleConfig{Mode: mode},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() rejected known mode %q: %v", mode, err)
		}
	}
}

// TestTailscaleEffectiveMode covers the mode-string validation in
// TailscaleConfig.EffectiveMode. The yaml Mode field is a free-form
// string but only three values are valid; anything else (typo'd or
// future-flag-from-newer-config) MUST return an error rather than
// silently fall through to the cli default — a typo like
// `mode: tnset` would otherwise look like it activated tsnet when
// it actually did nothing.
//
// The parser tolerates leading/trailing whitespace and case
// differences (`mode: " TSNet "` → `tsnet`) — common from
// hand-edited YAML — but preserves the original (untrimmed) value
// in the error message so a typo report doesn't mislead with an
// invisible-whitespace explanation.
func TestTailscaleEffectiveMode(t *testing.T) {
	cases := []struct {
		in      string
		want    TailscaleMode
		wantErr bool
	}{
		// Canonical inputs.
		{"", TailscaleModeCLI, false},
		{"cli", TailscaleModeCLI, false},
		{"tsnet", TailscaleModeTsnet, false},
		{"disabled", TailscaleModeDisabled, false},
		// Whitespace tolerance — common after a yaml merge / format-on-save.
		{"  cli", TailscaleModeCLI, false},
		{"tsnet  ", TailscaleModeTsnet, false},
		{"  disabled  ", TailscaleModeDisabled, false},
		{"\tcli\n", TailscaleModeCLI, false},
		{"   ", TailscaleModeCLI, false}, // whitespace-only collapses to empty → CLI default
		// Case tolerance — operator typed Title Case or upper case.
		{"CLI", TailscaleModeCLI, false},
		{"TSNET", TailscaleModeTsnet, false},
		{"TSNet", TailscaleModeTsnet, false}, // mixed case from a previous version of this comment's docs
		{"Disabled", TailscaleModeDisabled, false},
		// Real errors — must still trip the validation gate.
		{"tnset", "", true},     // typo
		{"enabled", "", true},   // user might guess "enabled" as the inverse of "disabled"
		{"on", "", true},        // truthy synonym
		{"   tnset ", "", true}, // trimmed but still a typo — error message must carry the original
		{"TS-NET", "", true},    // hyphenated guess
	}
	for _, c := range cases {
		got, err := (TailscaleConfig{Mode: c.in}).EffectiveMode()
		if c.wantErr {
			if err == nil {
				t.Errorf("EffectiveMode(%q) want error, got %v", c.in, got)
				continue
			}
			// Error message preserves the ORIGINAL (untrimmed) input
			// so the operator sees what they actually typed.
			if !strings.Contains(err.Error(), strconv.Quote(c.in)) {
				t.Errorf("EffectiveMode(%q) error message %q must contain the original input verbatim", c.in, err.Error())
			}
			continue
		}
		if err != nil {
			t.Errorf("EffectiveMode(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("EffectiveMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// mkLoopbackConfig returns a minimal fully-valid loopback Config so the
// validation-hardening tests below can mutate one field and assert on the
// verdict without every other check tripping first.
func mkLoopbackConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
	}
}

// TestValidateScanIntervalUpperBound pins B37: a huge scanIntervalSec is
// REJECTED by Validate rather than reaching scanner.RunPeriodic, where
// time.Duration(n)*time.Second overflows int64 into a negative Duration and
// panics time.NewTicker at startup.
func TestValidateScanIntervalUpperBound(t *testing.T) {
	cases := []struct {
		name    string
		secs    int
		wantErr bool
	}{
		{"typical", 3600, false},
		{"one-year-ceiling-accepted", maxIntervalSeconds, false},
		{"just-over-ceiling-rejected", maxIntervalSeconds + 1, true},
		{"overflow-huge-rejected", 1 << 40, true}, // *time.Second overflows int64 → negative → NewTicker panic
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mkLoopbackConfig(t)
			cfg.ScanIntervalSec = tc.secs
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "scanIntervalSec") {
					t.Fatalf("secs=%d: want scanIntervalSec error, got %v", tc.secs, err)
				}
			} else if err != nil {
				t.Fatalf("secs=%d: want nil, got %v", tc.secs, err)
			}
		})
	}
}

// TestValidateAtlasMetaTTLUpperBound pins the B37 HOUR-unit ceiling — a units
// slip (capping hour fields at the seconds ceiling) would still overflow.
func TestValidateAtlasMetaTTLUpperBound(t *testing.T) {
	cfg := mkLoopbackConfig(t)
	cfg.Atlas.MetaTTLHours = maxIntervalHours + 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "metaTtlHours") {
		t.Fatalf("over-ceiling: want metaTtlHours error, got %v", err)
	}
	cfg.Atlas.MetaTTLHours = maxIntervalHours // exactly one year is fine
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ceiling should pass, got %v", err)
	}
	cfg.Atlas.MetaTTLHours = 0 // 0 = "use the default"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero (default) should pass, got %v", err)
	}
}

// TestValidatePort pins B39's port numericness/range check in isolation.
func TestValidatePort(t *testing.T) {
	cases := []struct {
		port    string
		wantErr bool
	}{
		{"", false},  // empty: the caller's own shape check owns it
		{"0", false}, // 0 = OS-assigned ephemeral port (documented mode)
		{"1", false},
		{"7788", false},
		{"65535", false},
		{"65536", true}, // above range
		{"99999", true}, // above range
		{"abc", true},   // non-numeric
		{"-1", true},    // parses but negative
	}
	for _, tc := range cases {
		if err := validatePort(tc.port); (err != nil) != tc.wantErr {
			t.Errorf("validatePort(%q) err=%v, wantErr=%v", tc.port, err, tc.wantErr)
		}
	}
}

// TestValidateRejectsBogusPort pins B39 end-to-end: a port net.SplitHostPort
// accepts but net.Listen would reject is caught at load time on both the
// listen and admin binds.
func TestValidateRejectsBogusPort(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"listen-out-of-range", func(c *Config) { c.ListenAddress = ":99999" }, "listenAddress"},
		{"listen-non-numeric", func(c *Config) { c.ListenAddress = ":abc" }, "listenAddress"},
		{"admin-non-numeric", func(c *Config) { c.AdminAddress = "127.0.0.1:abc" }, "adminAddress"},
		{"admin-out-of-range", func(c *Config) { c.AdminAddress = "127.0.0.1:70000" }, "adminAddress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mkLoopbackConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want %q error, got %v", tc.wantSub, err)
			}
		})
	}
	// A valid numeric port is accepted (the default base already binds :7788
	// / :7789, so a clean Validate confirms the check doesn't over-reject).
	if err := mkLoopbackConfig(t).Validate(); err != nil {
		t.Fatalf("valid ports should pass, got %v", err)
	}
}

// TestValidateWorkerCounts pins Q40: negative worker/queue counts are rejected
// loudly (zero stays the "use the default" sentinel).
func TestValidateWorkerCounts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string // "" = expect success
	}{
		{"upscale-workers-negative", func(c *Config) { c.Upscale.Workers = -1 }, "upscale.workers"},
		{"upscale-queuecap-negative", func(c *Config) { c.Upscale.QueueCap = -1 }, "upscale.queueCap"},
		{"analysis-workers-negative", func(c *Config) { c.Analysis.Workers = -1 }, "analysis.workers"},
		{"analysis-queuecap-negative", func(c *Config) { c.Analysis.QueueCap = -1 }, "analysis.queueCap"},
		{"zero-uses-default", func(c *Config) { c.Upscale.Workers = 0; c.Analysis.QueueCap = 0 }, ""},
		{"positive-ok", func(c *Config) {
			c.Upscale.Workers = 4
			c.Upscale.QueueCap = 100
			c.Analysis.Workers = 2
			c.Analysis.QueueCap = 100
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mkLoopbackConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want %q error, got %v", tc.wantSub, err)
			}
		})
	}
}

// TestValidateDLNAListenAddress pins B38: the DLNA bind is validated when the
// feature is on (and the default :7790 passes).
func TestValidateDLNAListenAddress(t *testing.T) {
	cfg := mkLoopbackConfig(t)
	cfg.DLNA.Enabled = true
	cfg.DLNA.ListenAddress = ":abc"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dlna.listenAddress") {
		t.Fatalf("bad dlna bind: want dlna.listenAddress error, got %v", err)
	}
	cfg.DLNA.ListenAddress = "" // EffectiveDLNAListenAddress → default :7790
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DLNA on + default bind should pass, got %v", err)
	}
}

// TestResolvePathCleansAbsolute pins Q39: an absolute path with a `..` segment
// is Cleaned (not stored verbatim), while an empty value stays empty rather
// than collapsing to ".".
func TestResolvePathCleansAbsolute(t *testing.T) {
	if got := resolvePath("/base", ""); got != "" {
		t.Errorf("resolvePath(_, %q) = %q, want empty", "", got)
	}
	// Build an absolute path with an un-cleaned ".." segment by concatenation
	// (filepath.Join would pre-clean it and make the assertion trivial).
	sep := string(filepath.Separator)
	absDirty := t.TempDir() + sep + ".." + sep + "private"
	cleaned := filepath.Clean(absDirty)
	if cleaned == absDirty {
		t.Fatalf("test setup: %q was already clean", absDirty)
	}
	if got := resolvePath("", absDirty); got != cleaned {
		t.Errorf("resolvePath(abs) = %q, want %q", got, cleaned)
	}
	rel := filepath.Join("sub", "x")
	if got, want := resolvePath("/base", rel), filepath.Join("/base", rel); got != want {
		t.Errorf("resolvePath(rel) = %q, want %q", got, want)
	}
}
