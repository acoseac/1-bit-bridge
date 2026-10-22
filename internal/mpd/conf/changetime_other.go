//go:build !linux && !darwin

package conf

import (
	"io/fs"
	"time"
)

// statusChange has no inode change time to read here: the modification
// time is all a change shows (the reading this serves runs on Linux).
func statusChange(fs.FileInfo) (time.Time, bool) { return time.Time{}, false }
