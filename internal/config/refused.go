package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// RefusedError is Load's refusal of a config it read: the file does not
// decode, or its settings do not normalize and validate. Every other failure
// of Load (a file it cannot read, a name being repointed, a path it cannot
// make absolute) is not one. Its Error is what Load has always said, and it
// unwraps to that error, so a caller that asks errors.Is or errors.As of it
// sees what it always saw.
//
// Reasons are the same refusal without the file's name, one line each, for a
// caller that must say why where no path may appear (backlog B613): the
// candidate's settings check (`bridge serve --validate-config`), whose lines
// the updater records and the console and `bridge update` show. They are as
// value-free as the refusal (B505).
type RefusedError struct {
	err    error // what Load returns, as it always has
	reason error // the same, without the file's name
}

func (e *RefusedError) Error() string { return e.err.Error() }

func (e *RefusedError) Unwrap() error { return e.err }

// Reasons are the refusal's lines without the file's name: each line of a
// yaml.v3 type error (`line 12: …`), or each line of the refusal's text.
func (e *RefusedError) Reasons() []string {
	var te *yaml.TypeError
	if errors.As(e.reason, &te) {
		return append([]string(nil), te.Errors...)
	}
	var out []string
	for _, line := range strings.Split(e.reason.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// dataDirEnv is the environment variable that overrides dataDir, as
// applyEnvOverrides derives it from the field.
var dataDirEnv = envName("dataDir")

// DataDirOf is the data directory Load would give the config at path, read
// from the file's dataDir key alone, for a caller that must find the data
// directory of a config Load refuses: serve's early rollback of an install
// whose new version refuses its settings (cmd/bridge), which reads the
// updater's marker there (backlog B613).
//
// It is Load's three steps for that key and nothing else: the key as yaml.v3
// decodes it into Config.DataDir, from the file's first document, an alias, a
// merge key or a tag resolved as Load's decode resolves them; DefaultDataDir
// for a value that is empty or absent (applyDefaults); BRIDGE_DATA_DIR when it
// is set and not empty (applyEnvOverrides); and a relative value joined to the
// directory of path as filepath.Abs gives it (resolvePaths), never the
// directory a link there leads to. Every other key is left unread, so the
// value another setting refuses, which is what brings a caller here, does not
// stop it. A file it cannot read, a document yaml.v3 cannot decode into the
// key, and a dataDir written twice or as anything but a scalar are errors:
// there the data directory is not certain, and a caller must not guess one.
func DataDirOf(path string) (string, error) {
	data, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		DataDir string `yaml:"dataDir"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return "", fmt.Errorf("read dataDir from config %q: %w", path, ValueFreeYAMLError(err))
	}
	dir := doc.DataDir
	if dir == "" {
		dir = DefaultDataDir
	}
	if v, ok := os.LookupEnv(dataDirEnv); ok && v != "" {
		dir = v
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs config path %q: %w", path, err)
	}
	return ResolvePath(filepath.Dir(abs), dir), nil
}
