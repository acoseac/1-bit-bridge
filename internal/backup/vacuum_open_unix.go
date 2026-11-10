//go:build !windows

package backup

import (
	"os"

	"golang.org/x/sys/unix"
)

// openVacuumOutput opens SQLite's output without following its last component
// or waiting on a substituted pipe. Its caller validates the descriptor's kind.
func openVacuumOutput(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
