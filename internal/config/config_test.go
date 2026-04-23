package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestValidateNonexistentLibraryRoot(t *testing.T) {
	cfg := &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", ScanIntervalSec: 3600}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for nonexistent library root")
	}
}

func TestValidateLibraryRootIsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{LibraryRoots: []string{file}, ListenAddress: ":7788", ScanIntervalSec: 3600}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' error, got %v", err)
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
