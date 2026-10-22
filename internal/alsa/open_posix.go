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

// rootPath is the name a tree's root is opened by: dir/., which path
// resolution refuses with ENOTDIR before it opens anything unless dir is a
// directory, so a FIFO in the root's place is never opened
// (TestARootSwappedForAFIFOBeforeItsOpenIsRefusedPromptly).
func rootPath(dir string) string { return dir + "/." }
