//go:build linux

package atomicwrite

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// mountInfoPath is where the kernel lists this process's mounts (proc(5)).
const mountInfoPath = "/proc/self/mountinfo"

// maxMountInfoBytes bounds the read of the mount table: some 100,000 mounts,
// past what any host the player runs on mounts.
const maxMountInfoBytes = 16 << 20

// platformMountID is the mount path lies on, by the ID the kernel gives it
// (statx's STATX_MNT_ID, Linux 5.8), a link at the end of the path not
// followed. The ID is the mount's, whatever the path's spelling: on a
// filesystem that folds case, path/Player reaches the mount at path/player
// and reports its ID, where a mount table names it by one spelling alone
// (Codex on round 7). A bind mount of a filesystem onto itself has an ID of
// its own, which its device does not show. ok is false where the kernel
// gives no ID: one older than 5.8, which leaves the field out of the mask,
// or a statx a seccomp policy refuses (ENOSYS, EPERM); any other error is
// the stat's.
func platformMountID(path string) (uint64, bool, error) {
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

// platformMountTable is the kernel's list of this process's mount points,
// each spelled from the process's root as the kernel resolves it, bind
// mounts of a filesystem onto itself included, which show no device of
// their own. A table past maxMountInfoBytes is an error, as one that cannot
// be read is: neither rules a mount out. It is the probe's fallback where
// the kernel gives no mount IDs (platformMountID), and it names each mount
// point by one spelling.
func platformMountTable() (map[string]bool, error) {
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxMountInfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMountInfoBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", mountInfoPath, maxMountInfoBytes)
	}
	return parseMountInfo(data)
}
