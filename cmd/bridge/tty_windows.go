//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// initTerminal opts the current process's stdout into ANSI escape
// processing. Without this, pre-Win10-Anniversary conhost (and some
// legacy configs on newer Windows) would print our SGR sequences as
// literal `[95m1-BIT BRIDGE` garbage. Best-effort: a GetConsoleMode
// failure (not a console / failed) leaves the original mode intact —
// no harm, color stays disabled because colorEnabled's TTY check
// already filters non-console stdouts. Idempotent; repeat calls just
// OR the bit again.
//
// Why x/sys/windows instead of stdlib syscall: stdlib's syscall on
// Windows exports GetConsoleMode but NOT SetConsoleMode (the package
// is frozen, and SetConsoleMode landed in x/sys after the freeze).
// The bridge's internal/packaging Windows code already imports
// x/sys/windows for SCM lifecycle (PR #1's lifecycle_windows.go and
// the original service_windows.go) — using it here adds no new dep.
func initTerminal() {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return
	}
	_ = windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
