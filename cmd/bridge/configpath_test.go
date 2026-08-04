package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateConfigEnv makes the resolver's two inputs — the working
// directory and the platform config dir — both point at fresh temp dirs,
// so the test describes the code rather than whatever bridge happens to
// be installed on the machine running it.
func isolateConfigEnv(t *testing.T) (cwd, platform string) {
	t.Helper()
	cwd, platform = t.TempDir(), t.TempDir()
	chdir(t, cwd)
	prev := defaultConfigDirFn
	defaultConfigDirFn = func() (string, error) { return platform, nil }
	t.Cleanup(func() { defaultConfigDirFn = prev })
	return cwd, platform
}

// chdir moves into dir for the test and restores afterwards. The
// resolver consults ./bridge.yaml, so cwd is an input.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// An explicit --config always wins, existing or not: the caller named a
// file and a silent fallback to some other config would be worse than an
// error about the one they asked for.
func TestResolveConfigPathExplicitAlwaysWins(t *testing.T) {
	dir, _ := isolateConfigEnv(t)
	// A local bridge.yaml that must NOT be preferred over the explicit one.
	if err := os.WriteFile("bridge.yaml", []byte("libraryName: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(explicit, []byte("libraryName: explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveConfigPath(explicit)
	if got != explicit || !ok {
		t.Errorf("resolveConfigPath(%q) = (%q,%v), want (%q,true)", explicit, got, ok, explicit)
	}

	missing := filepath.Join(dir, "nope.yaml")
	got, ok = resolveConfigPath(missing)
	if got != missing || ok {
		t.Errorf("a named-but-absent config must be reported as itself and "+
			"not-found, got (%q,%v)", got, ok)
	}
}

// ./bridge.yaml comes BEFORE the platform path. This is the
// compatibility guarantee, not a preference: every command used to
// default to the bare relative string, so local-first means no existing
// invocation changes meaning.
func TestResolveConfigPathPrefersLocalOverPlatform(t *testing.T) {
	_, _ = isolateConfigEnv(t)
	if err := os.WriteFile("bridge.yaml", []byte("libraryName: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveConfigPath("")
	if !ok {
		t.Fatal("a bridge.yaml in the working directory must be found")
	}
	if got != defaultConfigPath {
		t.Errorf("resolveConfigPath(\"\") = %q, want %q — resolving to the "+
			"platform path while a local config exists would silently repoint "+
			"anyone running from a directory with its own config at a "+
			"different database", got, defaultConfigPath)
	}
}

// With no local config, fall through to the platform location — the
// whole point of the change. Previously every command looked only at
// ./bridge.yaml, so a config installed by `bridge init` was invisible
// unless the operator happened to be standing in the right directory.
func TestResolveConfigPathFallsBackToPlatform(t *testing.T) {
	_, platform := isolateConfigEnv(t) // both dirs empty: no config anywhere

	got, ok := resolveConfigPath("")
	if ok {
		t.Fatalf("no config exists anywhere in this fixture, but resolve "+
			"reported found at %q", got)
	}
	if got == defaultConfigPath {
		t.Error("with no local config the resolver must name the platform " +
			"path, so the not-found message points at where init writes one")
	}
	if want := filepath.Join(platform, defaultConfigPath); got != want {
		t.Errorf("platform fallback = %q, want %q", got, want)
	}
}

// The not-found error has to name everywhere it looked, or "config not
// found" sends the operator hunting.
func TestErrConfigNotFoundNamesEveryPathTried(t *testing.T) {
	_, _ = isolateConfigEnv(t)

	err := errConfigNotFound("")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range configSearchPaths("") {
		if !strings.Contains(msg, want) {
			t.Errorf("not-found message %q omits %q", msg, want)
		}
	}
	if len(configSearchPaths("")) < 2 {
		t.Error("with no explicit --config the resolver should consult at " +
			"least the local and platform paths")
	}

	// An explicit path names only itself — listing alternatives the
	// resolver never consulted would be misleading.
	explicit := filepath.Join(t.TempDir(), "given.yaml")
	if got := configSearchPaths(explicit); len(got) != 1 || got[0] != explicit {
		t.Errorf("configSearchPaths(%q) = %v, want just that path", explicit, got)
	}
	if msg := errConfigNotFound(explicit).Error(); !strings.Contains(msg, explicit) {
		t.Errorf("explicit not-found message %q omits the path given", msg)
	}
}

// loadCLIConfig is what the ~17 call sites use; it must surface the
// tried-paths error rather than a bare open failure.
func TestLoadCLIConfigReportsWhereItLooked(t *testing.T) {
	_, _ = isolateConfigEnv(t)

	_, _, err := loadCLIConfig("")
	if err == nil {
		t.Fatal("want an error with no config present")
	}
	for _, want := range configSearchPaths("") {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("loadCLIConfig error %q omits %q", err.Error(), want)
		}
	}
}

// The resolved path is returned so callers that WRITE it back
// (`library add/remove` → cfg.Save) update the file they read rather
// than creating a new one in the working directory.
func TestLoadCLIConfigReturnsThePathItRead(t *testing.T) {
	dir, _ := isolateConfigEnv(t)
	yaml := fmt.Sprintf("libraryName: t\nlibraryRoots:\n  - %q\n", dir)
	if err := os.WriteFile("bridge.yaml", []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	_, path, err := loadCLIConfig("")
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	if path != defaultConfigPath {
		t.Errorf("returned path = %q, want %q — a caller that Saves must "+
			"write back to the file it read, not to a fresh one", path, defaultConfigPath)
	}
}
