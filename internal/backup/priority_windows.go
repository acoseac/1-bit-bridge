//go:build windows

package backup

import "golang.org/x/sys/windows"

var procSetThreadPriority = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadPriority")

// threadModeBackgroundBegin is SetThreadPriority's background mode, which
// lowers a thread's CPU, I/O and memory priority.
const threadModeBackgroundBegin = 0x00010000

// lowerThreadPriority puts the calling OS thread alone, which its caller has
// locked and never unlocks (runLowPriority), in background mode. Best
// effort: a refusal leaves the thread as it was.
func lowerThreadPriority() {
	if procSetThreadPriority.Find() != nil {
		return
	}
	_, _, _ = procSetThreadPriority.Call(uintptr(windows.CurrentThread()), threadModeBackgroundBegin)
}
