package upload

import "github.com/acoseac/1-bit-bridge/internal/transcode"

// defaultFreeBytes reuses the transcode probe rather than reimplementing the
// walk-to-nearest-existing-ancestor logic. That walk matters here for the same
// reason it does there: a staging directory is created lazily, so a bare
// statfs on a path that does not exist yet would ENOENT rather than report the
// volume.
func defaultFreeBytes(dir string) (int64, error) {
	return transcode.AvailableDiskSpaceNearest(dir)
}
