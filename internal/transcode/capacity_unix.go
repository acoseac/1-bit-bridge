//go:build !windows

package transcode

import (
	"fmt"
	"syscall"
)

// TotalDiskSpace returns the capacity of the volume that holds dir.
//
// Blocks (not Bavail) is the total, so a "% used" reading derived from this
// and AvailableDiskSpace describes the VOLUME rather than just the caller's
// share of it. That is the honest denominator for a fill bar: a proportion
// computed against anything else — indexed library bytes, say — reads as
// "disk almost empty" on a shared disk that is nearly full.
func TotalDiskSpace(dir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", dir, err)
	}
	return int64(stat.Blocks) * int64(stat.Bsize), nil
}
