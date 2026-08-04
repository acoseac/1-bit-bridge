package main

import "strings"

// yamlStr renders s as a SINGLE-quoted YAML scalar.
//
// See the twin in internal/config: test fixtures build bridge.yaml by
// concatenation, and a DOUBLE-quoted YAML scalar processes backslash
// escapes — so `dataDir: "C:\Users\RUNNER~1\..."` fails to parse at all
// ("\U" reads as a unicode escape). Single-quoted scalars process no
// escapes; `'` is the only special character, escaped by doubling.
func yamlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
