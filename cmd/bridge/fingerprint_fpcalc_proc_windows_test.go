//go:build windows

package main

import (
	"fmt"
	"os"
)

// standInExited reports whether the stand-in fpcalc has exited. Windows has
// no kill(pid, 0). OpenProcess through Kill fails once the process is gone,
// and succeeds while it is still running.
func standInExited(pid int) (bool, string) {
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return true, fmt.Sprint(err)
	}
	if err := p.Kill(); err != nil {
		return true, err.Error()
	}
	return false, "TerminateProcess succeeded; the stand-in was still running"
}
