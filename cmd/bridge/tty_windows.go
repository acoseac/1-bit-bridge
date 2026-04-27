//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// initTerminal opts the current process's stdout into ANSI escape
// processing and reports whether the flip succeeded. Without this,
// pre-Win10-Anniversary conhost (and some legacy configs on newer
// Windows) would print our SGR sequences as literal `[95m1-BIT
// BRIDGE` garbage. Returns true only when SetConsoleMode succeeded
// (or the mode flag was already set) — caller MUST gate color
// rendering on this return so a failed flip doesn't leave us
// emitting raw escapes that a legacy console can't parse. Idempotent.
//
// Why x/sys/windows instead of stdlib syscall: stdlib's syscall on
// Windows exports GetConsoleMode but NOT SetConsoleMode (the package
// is frozen, and SetConsoleMode landed in x/sys after the freeze).
// The bridge's internal/packaging Windows code already imports
// x/sys/windows for SCM lifecycle (PR #1's lifecycle_windows.go and
// the original service_windows.go) — using it here adds no new dep.
func initTerminal() bool {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		// Already enabled — don't bother flipping.
		return true
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}
