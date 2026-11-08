//go:build darwin

package backup

import "golang.org/x/sys/unix"

// lowerThreadPriority puts the calling OS thread alone, which its caller has
// locked and never unlocks (runLowPriority), in the background band: Darwin's
// setpriority(PRIO_DARWIN_THREAD, 0, PRIO_DARWIN_BG) lowers the thread's CPU
// and I/O priority. Best effort.
func lowerThreadPriority() {
	_ = unix.Setpriority(unix.PRIO_DARWIN_THREAD, 0, unix.PRIO_DARWIN_BG)
}
