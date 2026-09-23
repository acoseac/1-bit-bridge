package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigDirCheckDoesNotCreateTheDirectoryOfANamedConfigThatIsNotThere
// pins both sides of the config-dir guard. A config the caller NAMED that is
// not there leaves the check nothing to vouch for, and creating the named
// file's directory left a typo'd `--config /x/bridge.yml`'s /x behind,
// reported ok. A config that is merely not there YET (the no-flag and
// launcher lookups, which record where they looked rather than an error)
// still gets the directory `bridge init` will write to created and probed.
func TestConfigDirCheckDoesNotCreateTheDirectoryOfANamedConfigThatIsNotThere(t *testing.T) {
	named := filepath.Join(t.TempDir(), "typo")
	c := checkConfigDir(t.Context(), Deps{
		ConfigDir: named,
		ConfigFile: &ConfigFile{Path: filepath.Join(named, "bridge.yml"),
			LoadErr: &fs.PathError{Op: "stat", Path: named, Err: fs.ErrNotExist}},
	})
	if c.Status != OK || !strings.Contains(c.Summary, "not checked") {
		t.Errorf("config-dir = %s %q for a named config that is not there, want ok \"not checked\"", c.Status, c.Summary)
	}
	if _, err := os.Stat(named); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("config-dir created %s for a named config that is not there (stat: %v)", named, err)
	}

	preSetup := filepath.Join(t.TempDir(), "1-bit-bridge")
	c = checkConfigDir(t.Context(), Deps{
		ConfigDir:  preSetup,
		ConfigFile: &ConfigFile{Tried: []string{filepath.Join(preSetup, "bridge.yaml")}},
	})
	if c.Status != OK || c.Summary != preSetup {
		t.Errorf("config-dir = %s %q before setup, want ok naming %s", c.Status, c.Summary, preSetup)
	}
	if fi, err := os.Stat(preSetup); err != nil || !fi.IsDir() {
		t.Errorf("config-dir did not create %s before setup (stat: %v)", preSetup, err)
	}
}

// TestCheckConfigFile pins every branch of checkConfigFile, and the two
// distinctions that make it worth having: a config that is MISSING is ok
// while one that is there and does not load FAILS, and a permission
// failure (a fact about who ran doctor) only warns.
func TestCheckConfigFile(t *testing.T) {
	const path = "/data/bridge.yaml"
	// The shape config.Load returns: a YAML decode error spans lines.
	parseErr := errors.New(`parse config "/data/bridge.yaml": yaml: unmarshal errors:
  line 6: field libraryNmae not found in type config.Config`)
	// And the shape os.ReadFile gives it for an unreadable file, wrapped
	// the way config.Load wraps it.
	permErr := fmt.Errorf("read config %q: %w", path,
		&fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission})

	cases := []struct {
		name string
		file *ConfigFile
		want Status
		has  []string
		// lacks pins WHICH branch answered where two can share a word:
		// fs.ErrNotExist's own text is "file does not exist", so the
		// generic does-not-load line would satisfy a "does not exist"
		// substring on its own.
		lacks []string
	}{
		{"no lookup", nil, OK, []string{"skipped"}, nil},
		{"loaded", &ConfigFile{Path: path}, OK, []string{path}, nil},
		{"none found", &ConfigFile{Tried: []string{path, "/home/b/.config/1-bit-bridge/bridge.yaml"}},
			OK, []string{"none found", path, "/home/b/.config/1-bit-bridge/bridge.yaml"}, nil},
		{"none found, nothing tried", &ConfigFile{}, OK, []string{"none found"}, nil},
		{"there and does not load", &ConfigFile{Path: path, LoadErr: parseErr},
			Fail, []string{path, "does not load", "line 6: field libraryNmae not found"}, nil},
		{"there and not readable by this user", &ConfigFile{Path: path, LoadErr: permErr},
			Warn, []string{path, "not readable by this user"}, nil},
		// A path the caller NAMED that is not there: the shape os.Stat
		// returns for it.
		{"named and not there", &ConfigFile{Path: path,
			LoadErr: &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}},
			Fail, []string{path, "does not exist"}, []string{"does not load"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := checkConfigFile(t.Context(), Deps{ConfigFile: tc.file})
			if c.Name != checkNameConfigFile {
				t.Errorf("Name = %q, want %q", c.Name, checkNameConfigFile)
			}
			if c.Status != tc.want {
				t.Errorf("Status = %s, want %s (summary %q)", c.Status, tc.want, c.Summary)
			}
			for _, s := range tc.has {
				if !strings.Contains(c.Summary, s) {
					t.Errorf("Summary %q does not contain %q", c.Summary, s)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(c.Summary, s) {
					t.Errorf("Summary %q contains %q: the wrong branch answered", c.Summary, s)
				}
			}
			// The report prints one line per check.
			if strings.ContainsAny(c.Summary, "\r\n") {
				t.Errorf("Summary spans lines: %q", c.Summary)
			}
			if c.Status != OK && c.Hint == "" {
				t.Error("a warn or fail carries no hint")
			}
		})
	}
}
