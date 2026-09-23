package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

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
	}{
		{"no lookup", nil, OK, []string{"skipped"}},
		{"loaded", &ConfigFile{Path: path}, OK, []string{path}},
		{"none found", &ConfigFile{Tried: []string{path, "/home/b/.config/1-bit-bridge/bridge.yaml"}},
			OK, []string{"none found", path, "/home/b/.config/1-bit-bridge/bridge.yaml"}},
		{"none found, nothing tried", &ConfigFile{}, OK, []string{"none found"}},
		{"there and does not load", &ConfigFile{Path: path, LoadErr: parseErr},
			Fail, []string{path, "does not load", "line 6: field libraryNmae not found"}},
		{"there and not readable by this user", &ConfigFile{Path: path, LoadErr: permErr},
			Warn, []string{path, "not readable by this user"}},
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
