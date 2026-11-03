//go:build darwin

package updater

import (
	"errors"
	"os"
	"syscall"
)

// plainFsync is the ordinary fsync(2) of d, which syncWithFallback asks for
// where Sync's F_FULLFSYNC is not supported in a way Go does not retry
// itself: Go's darwin Sync falls back to fsync on ENOTSUP alone (#64215).
func plainFsync(d *os.File) func() error {
	return func() error {
		rc, err := d.SyscallConn()
		if err != nil {
			return &os.PathError{Op: "fsync", Path: d.Name(), Err: err}
		}
		var opErr error
		if err := rc.Control(func(fd uintptr) {
			for {
				opErr = syscall.Fsync(int(fd))
				if !errors.Is(opErr, syscall.EINTR) {
					return
				}
			}
		}); err != nil {
			return &os.PathError{Op: "fsync", Path: d.Name(), Err: err}
		}
		if opErr != nil {
			return &os.PathError{Op: "fsync", Path: d.Name(), Err: opErr}
		}
		return nil
	}
}
