package atomicwrite

import (
	"strconv"
	"strings"
)

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
