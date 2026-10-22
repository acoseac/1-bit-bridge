//go:build linux || darwin

package alsa

import "syscall"

// openNonblock makes opening a FIFO return at once, and openDirectoryOnly
// makes a directory open of anything else fail at once (ENOTDIR).
const (
	openNonblock      = syscall.O_NONBLOCK
	openDirectoryOnly = syscall.O_DIRECTORY
)
