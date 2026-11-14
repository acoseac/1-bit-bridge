package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// LibraryRootsOf is the library roots Load would give the config at path,
// whose bytes are data, read from its libraryRoots key alone and touching no
// root: for bridge doctor, whose load of the config may wait on a root that
// does not answer (Validate resolves each root where upscale.variantsDir or
// upscale.tempDir is set), and which probes these beside that load, so a run
// whose load does not finish still names the root that holds it (backlog
// B650).
//
// It is Load's steps for that key and nothing else: the file decoded as Load
// decodes it (DecodeFile, so a file Load's decode refuses is refused here
// too, and a key reached by an alias or a merge is the one Load reaches);
// BRIDGE_LIBRARY_ROOTS where it is set and not empty, split and trimmed as
// applyEnvOverrides takes it; and each entry resolved against the directory
// of path as filepath.Abs gives it (resolvePaths). Normalize and Validate
// change no root, so these are the roots a load that answers gives.
func LibraryRootsOf(path string, data []byte) ([]string, error) {
	var c Config
	if err := DecodeFile(data, &c); err != nil {
		return nil, fmt.Errorf("read libraryRoots from config %q: %w", path, ValueFreeYAMLError(err))
	}
	for _, b := range envBindings() {
		if b.YAMLPath != "libraryRoots" {
			continue
		}
		if v, ok := os.LookupEnv(b.EnvVar); ok && v != "" {
			c.applyEnvValue(b, b.EnvVar, v)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs config path %q: %w", path, err)
	}
	for i, r := range c.LibraryRoots {
		c.LibraryRoots[i] = ResolvePath(filepath.Dir(abs), r)
	}
	return c.LibraryRoots, nil
}
