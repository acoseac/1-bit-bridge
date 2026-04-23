// Package config loads and validates bridge.yaml (library roots, listen
// address, TLS paths, scan interval).
//
// Relative paths in the config file resolve against the config file's own
// directory, matching how most Unix tools handle config-relative paths.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config mirrors the on-disk bridge.yaml shape. See config/bridge.yaml.example.
type Config struct {
	LibraryRoots    []string `yaml:"libraryRoots"`
	ListenAddress   string   `yaml:"listenAddress"`
	AdminAddress    string   `yaml:"adminAddress,omitempty"`
	DataDir         string   `yaml:"dataDir"`
	TLSCertPath     string   `yaml:"tlsCertPath,omitempty"`
	TLSKeyPath      string   `yaml:"tlsKeyPath,omitempty"`
	ScanIntervalSec int      `yaml:"scanIntervalSec"`
	LibraryName     string   `yaml:"libraryName"`
}

// Defaults applied when a field is absent or zero-valued.
const (
	DefaultListenAddress   = ":7788"
	DefaultAdminAddress    = "127.0.0.1:7789"
	DefaultDataDir         = "./data"
	DefaultScanIntervalSec = 3600
	DefaultLibraryName     = "1-bit Bridge"
)

// Load parses a bridge.yaml file, fills defaults, resolves relative paths
// against the config file's directory, and validates. A returned *Config is
// ready to hand to downstream subsystems.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typo-catcher: unknown YAML keys fail the load
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs config path %q: %w", path, err)
	}
	cfg.resolvePaths(filepath.Dir(absPath))
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultListenAddress
	}
	if c.AdminAddress == "" {
		c.AdminAddress = DefaultAdminAddress
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.ScanIntervalSec == 0 {
		c.ScanIntervalSec = DefaultScanIntervalSec
	}
	if c.LibraryName == "" {
		c.LibraryName = DefaultLibraryName
	}
}

func (c *Config) resolvePaths(baseDir string) {
	for i, r := range c.LibraryRoots {
		if r != "" && !filepath.IsAbs(r) {
			c.LibraryRoots[i] = filepath.Join(baseDir, r)
		}
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}
	if c.TLSCertPath != "" && !filepath.IsAbs(c.TLSCertPath) {
		c.TLSCertPath = filepath.Join(baseDir, c.TLSCertPath)
	}
	if c.TLSKeyPath != "" && !filepath.IsAbs(c.TLSKeyPath) {
		c.TLSKeyPath = filepath.Join(baseDir, c.TLSKeyPath)
	}
}

// Validate checks invariants the server relies on. Called automatically by
// Load; exposed for tests and for callers that construct Config in memory.
func (c *Config) Validate() error {
	if len(c.LibraryRoots) == 0 {
		return errors.New("libraryRoots: must have at least one entry")
	}
	for _, r := range c.LibraryRoots {
		if r == "" {
			return errors.New("libraryRoots: entries must not be empty")
		}
		info, err := os.Stat(r)
		if err != nil {
			return fmt.Errorf("libraryRoots[%q]: %w", r, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("libraryRoots[%q]: not a directory", r)
		}
	}
	if c.ScanIntervalSec < 1 {
		return fmt.Errorf("scanIntervalSec: must be >= 1, got %d", c.ScanIntervalSec)
	}
	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return errors.New("tlsCertPath and tlsKeyPath: must be set together, or both empty")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("listenAddress %q: %w", c.ListenAddress, err)
	}
	if err := validateLoopbackAddress(c.AdminAddress); err != nil {
		return fmt.Errorf("adminAddress %q: %w", c.AdminAddress, err)
	}
	return nil
}

// validateLoopbackAddress enforces that the admin listener binds only to a
// loopback interface. Accepts "127.0.0.1:N", "[::1]:N", and "localhost:N" —
// an empty host (":N" = all interfaces) or any non-loopback IP is rejected.
// The admin console has no auth layer; loopback binding is the trust boundary.
func validateLoopbackAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if port == "" {
		return errors.New("port must not be empty")
	}
	if host == "" {
		return errors.New("host must be a loopback address (127.0.0.1, ::1, or localhost); an empty host binds all interfaces")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("host %q is not a loopback address", host)
		}
		return nil
	}
	if host != "localhost" {
		return fmt.Errorf("host %q must be a loopback address (127.0.0.1, ::1, or localhost)", host)
	}
	return nil
}

// ScanInterval returns scanIntervalSec as a time.Duration.
func (c *Config) ScanInterval() time.Duration {
	return time.Duration(c.ScanIntervalSec) * time.Second
}

// Save atomically writes c as YAML to path (temp file + rename). Parent
// directory must exist. Comments and fields unknown to this schema are not
// preserved — callers that want to keep hand-authored comments should not
// use Save. `bridge init` and admin-console edits are the intended callers.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-*.yaml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	return nil
}
