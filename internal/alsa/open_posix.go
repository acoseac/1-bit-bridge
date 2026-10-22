//go:build unix

package alsa

import "syscall"

// openNonblock makes opening a FIFO return at once, and openDirectoryOnly
// makes a directory open of anything else fail at once (ENOTDIR). Every
// system the unix tag stands for has FIFOs, and syscall defines both flags
// on every one Go builds (TestOpenFlagsAreBuiltForEveryUnix).
const (
	openNonblock      = syscall.O_NONBLOCK
	openDirectoryOnly = syscall.O_DIRECTORY
)
