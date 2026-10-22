//go:build linux

package conf

import (
	"io/fs"
	"syscall"
	"time"
)

// statusChange is the inode's change time (st_ctim): a write, a rename, a
// chmod and setting the modification time each move it, and no caller can
// set it back, as one can the modification time.
func statusChange(fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Ctim.Unix()), true
}
