//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openLockFileNoFollow is OpenLockFileNoFollow's open on these platforms:
// open(2) on the whole path with O_NOFOLLOW, which refuses a link at the last
// element with ELOOP however it was asked (O_CREAT included, so a link to
// nothing makes nothing), and O_NONBLOCK, so a named pipe there is opened at
// once and refused by its kind rather than waited on. The descriptor's kind is
// asked of the descriptor itself.
func openLockFileNoFollow(path string, perm os.FileMode, write bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if write {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	}
	var fd int
	var err error
	for {
		fd, err = unix.Open(path, flags, uint32(perm.Perm()))
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ELOOP) {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLockFileNotAFile}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLockFileNotAFile}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openRefusedForWriting is OpenRefusedForWriting's platform half: EROFS.
func openRefusedForWriting(err error) bool {
	return errors.Is(err, unix.EROFS)
}
