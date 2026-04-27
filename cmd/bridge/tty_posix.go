//go:build !windows

package main

// initTerminal is a no-op on POSIX — macOS Terminal.app, iTerm2,
// gnome-terminal, xterm, ssh dumb terminals all parse ANSI natively
// (or, in the dumb-terminal case, are already excluded by
// colorEnabled's TERM=dumb check). The Windows build of this file
// (`tty_windows.go`) does the SetConsoleMode dance.
func initTerminal() {}
