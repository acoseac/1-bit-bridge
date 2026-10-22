//go:build darwin

package atomicwrite

import (
	"io/fs"
	"syscall"
)

// platformMedium takes every root on macOS: it has neither tmpfs nor
// overlayfs, and a RAM disk is a volume like any other, which statfs does not
// tell from a disk. The player, the strict writer's one caller, runs on Linux
// alone.
func platformMedium(string, uint64) error { return nil }

// mountIDsExpected is whether this platform gives a mount's identity, so that
// the strict writer refuses (ErrMountsUnknown) where it gets none.
const mountIDsExpected = true

// platformMountID is the mount path lies on, by the filesystem ID statfs
// gives it (f_fsid), whatever the path's spelling: each mount has its own,
// its volume's device or one the kernel makes unique among the mounts
// (vfs_getnewfsid), and statfs answers for the mount the path reaches.
// statfs follows a link at the end of the path; the strict writer asks it of
// no link (plain).
func platformMountID(path string) (uint64, bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return uint64(uint32(st.Fsid.Val[0]))<<32 | uint64(uint32(st.Fsid.Val[1])), true, nil
}
