//go:build unix

package backup_test

import (
	"io/fs"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// platformSealObstructions are the seal entries obstructSeal makes only on
// unix: a named pipe, which an open for reading would wait on for a writer.
var platformSealObstructions = []string{"named_pipe"}

// obstructSealOnThisPlatform makes the unix seal entry kind at path.
func obstructSealOnThisPlatform(t *testing.T, path, kind string) {
	t.Helper()
	if kind != "named_pipe" {
		t.Fatalf("no seal obstruction %q", kind)
	}
	fsutiltest.MakeFIFO(t, path)
}

// copySpecialEntry copies an entry that is neither a link, a directory nor a
// regular file to dst: a named pipe, as a named pipe.
func copySpecialEntry(t *testing.T, dst string, m fs.FileMode) {
	t.Helper()
	if m&fs.ModeNamedPipe == 0 {
		t.Fatalf("no copy of a %v entry", m)
	}
	fsutiltest.MakeFIFO(t, dst)
}
