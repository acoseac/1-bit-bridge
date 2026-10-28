//go:build unix

package atomicwrite

import (
	"syscall"
	"testing"
)

// platformObstructions are the entries obstruction plants only on unix: a
// named pipe, which an open for reading would wait on for a writer.
var platformObstructions = []string{"named_pipe"}

// obstructionOnThisPlatform plants the unix entry kind at path.
func obstructionOnThisPlatform(t *testing.T, path, kind string) {
	t.Helper()
	if kind != "named_pipe" {
		t.Fatalf("no obstruction %q", kind)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
