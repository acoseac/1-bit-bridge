//go:build !windows

package main

import "github.com/acoseac/1-bit-bridge/internal/proctest"

// standInExited reports whether the stand-in fpcalc has exited. kill(pid, 0)
// alone treats a zombie as alive, so the answer goes through proctest.
func standInExited(pid int) (bool, string) {
	return proctest.Exited(pid)
}
