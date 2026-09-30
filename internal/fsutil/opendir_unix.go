//go:build unix

package fsutil

import (
	"os"
	"syscall"
)

// openDir opens name with O_DIRECTORY, which the kernel checks before it
// opens anything: what is not a directory is refused with ENOTDIR, a named
// pipe included, without waiting for a writer.
func openDir(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
}
