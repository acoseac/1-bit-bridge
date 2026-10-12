//go:build windows

package proctest

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

// Exited reports whether process pid has exited, and what the answer rests
// on, for a failure message.
//
// It opens the process with SYNCHRONIZE only and waits with a zero timeout.
// It never signals the process or ends it. WAIT_OBJECT_0 means the process
// has exited. WAIT_TIMEOUT means it is running. OpenProcess failing with
// ERROR_INVALID_PARAMETER means there is no such process, so it has exited.
// Every other answer is "running", including the ones that mean "could not
// tell" (another OpenProcess error, WAIT_FAILED, an unexpected wait value):
// the callers poll until a deadline, so an answer that errs toward running
// fails the test, with its reason, and never passes a process that is still
// there.
//
// A Windows pid is a DWORD. A value that is not one is refused before any
// call, so the cast cannot truncate it into a pid that exists.
//
// Once the process has exited and every handle to it is closed, Windows can
// reuse its pid. A reused pid reads as running, which errs toward failing,
// never toward passing.
func Exited(pid int) (bool, string) {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false, fmt.Sprintf("%d is not a process id", pid)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return true, "OpenProcess: ERROR_INVALID_PARAMETER"
		}
		return false, "OpenProcess: " + err.Error()
	}
	defer windows.CloseHandle(h)
	// windows.WAIT_TIMEOUT is Errno 258, which is the wait status
	// WaitForSingleObject returns when the timeout expires. x/sys names
	// no separate wait-status constant for it.
	event, err := windows.WaitForSingleObject(h, 0)
	switch {
	case err != nil || event == windows.WAIT_FAILED:
		return false, fmt.Sprintf("WaitForSingleObject: %v (event %d)", err, event)
	case event == windows.WAIT_OBJECT_0:
		return true, "WaitForSingleObject = WAIT_OBJECT_0"
	case event == uint32(windows.WAIT_TIMEOUT):
		return false, "WaitForSingleObject = WAIT_TIMEOUT"
	default:
		return false, fmt.Sprintf("WaitForSingleObject = %d", event)
	}
}
