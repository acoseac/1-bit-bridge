package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LibraryRootsOf is the library roots Load would give the config at path,
// whose bytes are data, read from its libraryRoots key alone and touching no
// root: for bridge doctor, whose load of the config may wait on a root that
// does not answer (Validate resolves each root where upscale.variantsDir or
// upscale.tempDir is set), and which probes these beside that load, so a run
// whose load does not finish still names the root that holds it (backlog
// B650).
//
// It is Load's steps for that key and nothing else, as DataDirOf is for
// dataDir: the key as yaml.v3 decodes it into Config.LibraryRoots, from the
// file's first document; BRIDGE_LIBRARY_ROOTS where it is set and not empty,
// split and trimmed as applyEnvOverrides takes it; and each entry resolved
// against the directory of path as filepath.Abs gives it (resolvePaths).
// Normalize and Validate change no root, so these are the roots a load that
// answers gives. A document yaml.v3 cannot decode into the key is an error.
func LibraryRootsOf(path string, data []byte) ([]string, error) {
	var doc struct {
		LibraryRoots []string `yaml:"libraryRoots"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read libraryRoots from config %q: %w", path, ValueFreeYAMLError(err))
	}
	c := Config{LibraryRoots: doc.LibraryRoots}
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
