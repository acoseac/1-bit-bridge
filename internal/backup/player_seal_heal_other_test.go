//go:build !unix

package backup_test

import (
	"io/fs"
	"testing"
)

// platformSealObstructions is empty off unix, where no test makes a named
// pipe.
var platformSealObstructions []string

// obstructSealOnThisPlatform has no seal entry of its own off unix.
func obstructSealOnThisPlatform(t *testing.T, _, kind string) {
	t.Helper()
	t.Fatalf("no seal obstruction %q on this platform", kind)
}

// copySpecialEntry has no special entry to copy off unix.
func copySpecialEntry(t *testing.T, _ string, m fs.FileMode) {
	t.Helper()
	t.Fatalf("no copy of a %v entry on this platform", m)
}
