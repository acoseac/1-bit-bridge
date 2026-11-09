//go:build windows

package main

import "os"

// openMarkerForSync opens the marker at path for its sync in place
// (syncMarkerFile) for writing: on Windows Sync is FlushFileBuffers, which
// needs a handle opened so. A marker serve may not write there, its
// read-only attribute or an ACL, is therefore never synced in place, and a
// start whose record does not hold its identity yet never takes it up: the
// record is written without it, the data dir protected by its database and
// its lock file alone, and the marker left as it was
// (TestAReadOnlyMarkerIsNeverTakenUpOnWindows). No path a marker has names a
// named pipe there, which an open could wait on.
func openMarkerForSync(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}
