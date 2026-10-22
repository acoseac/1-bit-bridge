//go:build linux

package atomicwrite

import (
	"fmt"
	"io"
	"os"
)

// mountInfoPath is where the kernel lists this process's mounts (proc(5)).
const mountInfoPath = "/proc/self/mountinfo"

// maxMountInfoBytes bounds the read of the mount table: some 100,000 mounts,
// past what any host the player runs on mounts.
const maxMountInfoBytes = 16 << 20

// platformMountTable is the kernel's list of this process's mount points,
// each spelled from the process's root as the kernel resolves it, bind
// mounts of a filesystem onto itself included, which show no device of
// their own. A table past maxMountInfoBytes is an error, as one that cannot
// be read is: neither rules a mount out.
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
