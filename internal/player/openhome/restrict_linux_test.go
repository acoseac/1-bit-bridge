//go:build linux

package openhome

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// boundInterface reads back the interface ln's socket is bound to
// (SO_BINDTOIFINDEX); readable is false where the kernel does not answer
// it.
func boundInterface(t *testing.T, ln net.Listener) (index int, readable bool) {
	t.Helper()
	sc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var gerr error
	if err := sc.Control(func(fd uintptr) {
		index, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX)
	}); err != nil {
		t.Fatal(err)
	}
	return index, gerr == nil
}
