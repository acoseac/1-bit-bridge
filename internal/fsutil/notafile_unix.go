//go:build unix

package fsutil

import (
	"errors"
	"os"
	"syscall"
)

// openNoWait opens name read-only without waiting, and reports whether the
// file it returns is left nonblocking.
//
// O_NONBLOCK is what makes the open of a named pipe return at once rather
// than wait for a writer. O_NOCTTY keeps a terminal reached here, by a link
// swapped in after a caller's stat, from becoming the process's controlling
// terminal: a systemd service is a session leader with none, and on Linux
// opening a terminal without the flag hands it one.
//
// O_NONBLOCK changes one answer for a regular file: while another process
// holds a lease on it (Samba's kernel oplocks, an NFS server's delegation),
// the open fails with EWOULDBLOCK instead of waiting for the lease to be
// broken, which a plain open does, for up to lease-break-time (45 s by
// default). That failure is answered with the plain open the bridge always
// made, which waits as it always did. A named pipe's nonblocking open never
// answers EWOULDBLOCK, so the fallback is not a way back to the wait this
// function exists to avoid.
func openNoWait(name string) (*os.File, bool, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err == nil {
		return f, true, nil
	}
	// EWOULDBLOCK is EAGAIN on Linux and macOS, and not on every unix Go
	// builds for, so both are asked.
	if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, err
	}
	f, err = os.Open(name)
	return f, false, err
}

// setBlocking clears the O_NONBLOCK openNoWait set, once the file is known
// to be one. POSIX leaves the flag without effect on a regular file's reads,
// but not every file system is POSIX: a FUSE daemon (rclone, sshfs) is
// handed the file's flags with every read, and one that honours the flag can
// answer EAGAIN where a plain open's read would have waited for the data.
func setBlocking(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = syscall.SetNonblock(int(fd), false)
	}); err != nil {
		return err
	}
	return setErr
}
