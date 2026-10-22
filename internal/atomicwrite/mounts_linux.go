//go:build linux

package atomicwrite

import (
	"errors"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// platformMedium is the medium check on Linux (mediumFrom): statfs's f_type
// of the root, and for an overlay the upper layer the kernel's mount table
// (/proc/self/mountinfo) names on the line of the root's own mount ID.
func platformMedium(root string, mountID uint64) error {
	return mediumFrom(root, mountID, linuxStatfs, readMountInfo, mountIDOf)
}

// linuxStatfs is what the medium check reads of the filesystem path is on.
func linuxStatfs(path string) (fsStat, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsStat{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return fsStat{Type: uint32(st.Type), Bsize: int64(st.Bsize), Blocks: uint64(st.Blocks), Files: uint64(st.Files)}, nil
}

// readMountInfo is the kernel's mount table for this process's mount
// namespace.
func readMountInfo() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }

// mountIDsExpected is whether this platform gives a mount's identity, so that
// the strict writer refuses (ErrMountsUnknown) where it gets none.
const mountIDsExpected = true

// platformMountID is the mount path lies on, by the ID the kernel gives it, a
// link at the end of the path not followed: statx's STATX_MNT_ID (Linux 5.8),
// or where statx gives none (an older kernel, which leaves the field out of
// the mask, or a statx a seccomp policy refuses: ENOSYS, EPERM) the mnt_id
// /proc/self/fdinfo lists for a descriptor opened on the path (Linux 3.15).
// Both are the mount's ID whatever the path's spelling: on a filesystem that
// folds case, path/Player reaches a mount at path/player and reports its ID,
// where a mount table names the mount by one spelling (Codex on round 7 and
// round 8); and a bind mount of a filesystem onto itself has an ID of its
// own, which its device does not show. ok is false where neither gives one;
// any other error is the stat's or the open's.
func platformMountID(path string) (uint64, bool, error) {
	return mountIDFrom(path, statxMountID, fdinfoMountID)
}

// statxMountID is statx's mount ID for path, a link at the end not followed,
// and false where the kernel gives none.
func statxMountID(path string) (uint64, bool, error) {
	var st unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &st)
	switch {
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
		return 0, false, nil
	case err != nil:
		return 0, false, &fs.PathError{Op: "statx", Path: path, Err: err}
	case st.Mask&unix.STATX_MNT_ID == 0:
		return 0, false, nil
	}
	return st.Mnt_id, true, nil
}

// fdinfoMountID is the mnt_id the kernel lists in /proc/self/fdinfo for a
// descriptor opened on path with O_PATH, a link at the end not followed, an
// open that asks nothing of path's own permissions, as a stat does not. ok is
// false where the listing does not exist (no /proc) or holds no mnt_id, and
// any other failure to read it is the read's error (fdinfoMountIDFrom).
func fdinfoMountID(path string) (uint64, bool, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	return fdinfoMountIDFrom(os.ReadFile, "/proc/self/fdinfo/"+strconv.Itoa(fd))
}
