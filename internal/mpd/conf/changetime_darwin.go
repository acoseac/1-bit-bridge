//go:build darwin

package conf

import (
	"io/fs"
	"syscall"
	"time"
)

// statusChange is the inode's change time (st_ctimespec), as on Linux.
func statusChange(fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Ctimespec.Unix()), true
}
