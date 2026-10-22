package atomicwrite

import (
	"strconv"
	"strings"
)

// mountIDFrom is the mount ID statx gives path, or where statx gives none the
// one fdinfo gives: Linux's platformMountID, with statxMountID and
// fdinfoMountID. An error from either is the answer, never a reason to ask
// the other. It is platform-neutral so its tests run everywhere.
func mountIDFrom(path string, statx, fdinfo func(string) (uint64, bool, error)) (uint64, bool, error) {
	if id, ok, err := statx(path); err != nil || ok {
		return id, ok, err
	}
	return fdinfo(path)
}

// parseFdinfoMountID is the mount ID a /proc/<pid>/fdinfo/<fd> listing gives
// its descriptor, the number on its "mnt_id:" line (proc(5), Linux 3.15), and
// false where the listing has no such line, or one that does not read as a
// number. It is platform-neutral so its tests read captured listings
// everywhere; only Linux reads the kernel's (fdinfoMountID).
func parseFdinfoMountID(data []byte) (uint64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "mnt_id" {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		return id, err == nil
	}
	return 0, false
}
