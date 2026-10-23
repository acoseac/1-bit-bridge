//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// playRawKeys puts the terminal f is into a mode that hands each key over
// as it is pressed, with no echo: canonical input and echo off, signals
// kept, so Ctrl-C still stops the run. It reports false, changing nothing,
// for a file that is not a terminal. The descriptor is taken through
// SyscallConn, never Fd, which would make a non-blocking file blocking on
// the open file description every holder shares (pcmTestStoppableFile).
// restore puts the terminal back as it was.
func playRawKeys(f *os.File) (restore func(), ok bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, false
	}
	fd := -1
	if err := rc.Control(func(d uintptr) { fd = int(d) }); err != nil {
		return nil, false
	}
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, false
	}
	raw := *old
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }, true
}
