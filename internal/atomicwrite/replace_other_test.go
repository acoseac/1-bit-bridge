//go:build !unix

package atomicwrite

import "testing"

// platformObstructions is empty off unix, where no test makes a named pipe.
var platformObstructions []string

// obstructionOnThisPlatform has no entry of its own off unix.
func obstructionOnThisPlatform(t *testing.T, _, kind string) {
	t.Helper()
	t.Fatalf("no obstruction %q on this platform", kind)
}
