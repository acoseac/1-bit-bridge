package backup_test

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestTheDirectoryRemedyPerPlatform pins the one remedy a failure at the
// data directory alone takes: on unix every access the directory's steps
// use, and the search of each directory above it, or an immutable or
// append-only flag or an ACL cleared; on Windows, whose access a mode bit
// does not name, full control of the directory.
func TestTheDirectoryRemedyPerPlatform(t *testing.T) {
	const dir = "/srv/bridge"
	for goos, want := range map[string]string{
		"linux":   "restore read, write and search (execute) access to the data directory /srv/bridge and each directory above it, or clear an immutable or append-only flag or ACL on it",
		"darwin":  "restore read, write and search (execute) access to the data directory /srv/bridge and each directory above it, or clear an immutable or append-only flag or ACL on it",
		"windows": "restore full control of the data directory /srv/bridge",
	} {
		if got := backup.DirectoryRemedy(goos, dir); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}
