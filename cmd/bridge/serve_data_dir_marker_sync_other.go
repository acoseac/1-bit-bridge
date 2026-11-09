//go:build !windows

package main

import (
	"os"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// openMarkerForSync opens the marker at path for its sync in place
// (syncMarkerFile) for reading, as a file (fsutil.OpenAsFile): Linux and
// macOS sync a descriptor opened for reading, so a marker serve may read and
// not write (another user's, as where root owns the data dir and a member of
// its group serves it, or an immutable one) is synced and taken up; and a
// named pipe swapped in at its path is refused at once, where a plain open
// for reading would wait for a writer and hold serve's start. Until B741's
// round 3 the open asked to write on every OS, so such a marker was never
// taken up (the Claude API on #1441).
func openMarkerForSync(path string) (*os.File, error) {
	f, _, err := fsutil.OpenAsFile(path)
	return f, err
}
