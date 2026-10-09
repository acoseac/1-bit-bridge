//go:build darwin

package main

import (
	"os"
	"strconv"
	"syscall"
)

// processHoldsFile reports whether this process has path open.
//
// os.ReadDir of /dev/fd stats every name, and on macOS fstatat of a
// descriptor that closed between the listing and the stat is EBADF, which
// fails the whole ReadDir. The kqueue watcher's libraryFiles already lists
// names only for that reason (internal/manifest/watcher_kqueue_test.go). A
// descriptor closed before its Fstat is skipped. Readlink of /dev/fd/N is
// EINVAL here, so the comparison is the file's device and inode.
func processHoldsFile(path string) (bool, error) {
	var target syscall.Stat_t
	if err := syscall.Stat(path, &target); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	d, err := os.Open("/dev/fd")
	if err != nil {
		return false, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		fd, convErr := strconv.Atoi(name)
		if convErr != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		if st.Dev == target.Dev && st.Ino == target.Ino {
			return true, nil
		}
	}
	return false, nil
}
