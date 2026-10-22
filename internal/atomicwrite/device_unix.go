//go:build unix

package atomicwrite

import (
	"io/fs"
	"syscall"
)

// devicesKnown is whether a FileInfo here carries the filesystem it lies on
// (st_dev), so that the strict writer can refuse a mount of another
// filesystem below its root by it.
const devicesKnown = true

// device is the filesystem fi lies on, its st_dev, widened: Dev is an int32
// on darwin and a uint64 on linux, and equal devices stay equal.
func device(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
