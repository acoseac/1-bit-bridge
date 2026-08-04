package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// defaultConfigDirFn is the platform config-dir lookup, as a var so
// tests can point it at a temp dir. Production never reassigns it.
//
// The seam is not optional convenience: without it these tests read
// whatever is installed on the machine running them, so a developer with
// a real bridge at the platform path sees different results from CI,
// which has none. That is a test whose outcome depends on the
// environment rather than the code. Same shape as renameFunc in
// internal/manifest and commandContext in internal/tailscale.
var defaultConfigDirFn = packaging.DefaultConfigDir

// resolveConfigPath picks which bridge.yaml a command should read, in
// precedence order:
//
//  1. an explicit --config
//  2. ./bridge.yaml, if it exists
//  3. the platform config dir's bridge.yaml
//
// LOCAL BEFORE PLATFORM, and the reason is compatibility rather than
// ergonomics. Every command previously defaulted to the bare relative
// string "bridge.yaml", so today's behaviour is ALWAYS cwd-relative.
// Putting ./bridge.yaml first means no existing invocation changes
// meaning and the platform path is purely an additive fallback. The
// reverse order would silently repoint anyone who runs the CLI from a
// directory holding its own config — a test fixture, a second instance —
// at a different database, which is the kind of change that is only
// noticed after it has done something.
//
// The returned path is not guaranteed to exist: `ok` reports whether a
// file was actually found, so a caller that CREATES the config (init,
// serve --init-if-missing) can use the resolved location while a caller
// that READS it can fail with a message naming everywhere it looked.
func resolveConfigPath(explicit string) (path string, ok bool) {
	if explicit != "" {
		_, err := os.Stat(explicit)
		return explicit, err == nil
	}
	if _, err := os.Stat(defaultConfigPath); err == nil {
		return defaultConfigPath, true
	}
	if dir, err := defaultConfigDirFn(); err == nil && dir != "" {
		platform := filepath.Join(dir, defaultConfigPath)
		if _, err := os.Stat(platform); err == nil {
			return platform, true
		}
		// Nothing found. Return the platform path as the "where it
		// should live" answer — that is what init writes and what an
		// operator following the error should create.
		return platform, false
	}
	return defaultConfigPath, false
}

// configSearchPaths lists, in order, every location resolveConfigPath
// would consult for the given explicit value. Used to build a not-found
// error that names them, so "config not found" is actionable instead of
// prompting a hunt.
func configSearchPaths(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	paths := []string{defaultConfigPath}
	if dir, err := defaultConfigDirFn(); err == nil && dir != "" {
		paths = append(paths, filepath.Join(dir, defaultConfigPath))
	}
	return paths
}

// errConfigNotFound builds the not-found error, naming every path tried.
func errConfigNotFound(explicit string) error {
	tried := configSearchPaths(explicit)
	if len(tried) == 1 {
		return fmt.Errorf("no config at %s", tried[0])
	}
	return fmt.Errorf("no config found; tried: %s", strings.Join(tried, ", "))
}

// loadCLIConfig resolves the path per resolveConfigPath and loads it,
// returning the config and the path it came from (several callers need
// the path afterwards — to Save it back, or to include it in a backup
// set).
//
// The shared replacement for what used to be
// `config.Load(*configPath)` against a hardcoded "bridge.yaml" default
// at ~17 call sites, none of which could find a config installed at the
// platform location unless the operator happened to be standing in the
// right directory.
func loadCLIConfig(explicit string) (*config.Config, string, error) {
	path, ok := resolveConfigPath(explicit)
	if !ok {
		return nil, path, errConfigNotFound(explicit)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, path, fmt.Errorf("load %s: %w", path, err)
	}
	return cfg, path, nil
}
