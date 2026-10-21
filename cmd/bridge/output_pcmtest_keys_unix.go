//go:build unix

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// pcmTestStoppableFile reads f, standard input, through poll, so that a
// stop cuts a read short: each Read waits for input on f or for a byte on
// a wake-up pipe, which interrupt writes. release closes the pipe once the
// reader has finished. Nothing about f changes: no flag is set on a
// descriptor the shell shares.
func pcmTestStoppableFile(f *os.File) (io.Reader, func(), func(), bool) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return nil, nil, nil, false
	}
	unix.CloseOnExec(p[0])
	unix.CloseOnExec(p[1])
	r := &pcmTestPollReader{f: f, fd: int(f.Fd()), wake: p[0]}
	interrupt := func() { _, _ = unix.Write(p[1], []byte{1}) }
	release := func() {
		_ = unix.Close(p[0])
		_ = unix.Close(p[1])
	}
	return r, interrupt, release, true
}

// pcmTestPollReader reads its file only once poll says it has input, and
// not at all once its wake-up pipe has a byte. It keeps f, whose
// descriptor it reads.
type pcmTestPollReader struct {
	f        *os.File
	fd, wake int
}

func (r *pcmTestPollReader) Read(b []byte) (int, error) {
	fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}, {Fd: int32(r.wake), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if fds[1].Revents != 0 {
			return 0, errPCMTestKeysStopped
		}
		if fds[0].Revents == 0 {
			continue
		}
		n, err := unix.Read(r.fd, b)
		switch {
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}
