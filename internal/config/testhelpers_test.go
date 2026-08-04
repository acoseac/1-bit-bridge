package config

import (
	"os"
	"path/filepath"
	"strings"
)

// yamlStr renders s as a SINGLE-quoted YAML scalar.
//
// Test fixtures build bridge.yaml by string concatenation and used to
// wrap paths in DOUBLE quotes. A double-quoted YAML scalar processes
// backslash escapes, so a Windows path is not merely inconvenient — it
// is a parse error: `dataDir: "C:\Users\RUNNER~1\..."` makes YAML read
// `\U` as a unicode escape and fail with "did not find expected
// hexdecimal number". Every config test that wrote a temp path into a
// quoted field failed on Windows for that reason alone, with nothing
// wrong in the product.
//
// Single-quoted scalars process no escapes at all; the only special
// character is `'`, escaped by doubling. That makes this safe for any
// native path on any platform.
func yamlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// absTestPath builds an absolute path that is absolute on the HOST, not
// just on POSIX.
//
// Several fixtures hardcoded "/tmp/bridge-data" to assert that an
// absolute path in bridge.yaml survives Load verbatim. On Windows
// "/tmp/bridge-data" is NOT absolute (filepath.IsAbs wants a volume), so
// resolvePaths correctly rewrote it relative to the config dir and the
// assertion failed — testing the platform, not the behaviour. Anchoring
// to the volume of the working directory keeps the intent ("absolute
// stays absolute") portable.
func absTestPath(parts ...string) string {
	root := string(filepath.Separator)
	if wd, err := os.Getwd(); err == nil {
		if vol := filepath.VolumeName(wd); vol != "" {
			root = vol + string(filepath.Separator)
		}
	}
	return filepath.Join(append([]string{root}, parts...)...)
}
