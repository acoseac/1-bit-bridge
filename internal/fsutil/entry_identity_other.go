//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
)

// entryIdentity is EntryIdentity off Windows: os.Lstat, whose stat carries
// the device and the inode as it is taken.
func entryIdentity(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}
