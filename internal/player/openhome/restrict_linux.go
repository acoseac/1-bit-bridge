//go:build linux

package openhome

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// restrictToInterface restricts the socket c, before its bind, to the
// interface whose index is index, with SO_BINDTOIFINDEX: the kernel then
// gives a listening socket bound to a device only a connection that
// arrived on that device (socket(7): only packets received from that
// interface are processed by the socket), where IPv4 is otherwise the weak
// host model and an address of one interface is reached through another.
// An unprivileged process may set it on a socket not bound to a device yet
// since Linux 5.7; before, and on a kernel that refuses it, the socket is
// left unrestricted and the reason said: admission's prefix check holds
// either way. It names the option, or "none: " and why.
func restrictToInterface(c syscall.RawConn, index int) string {
	if index <= 0 {
		return "none: the interface's index is not known"
	}
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX, index)
	}); err != nil {
		return "none: " + err.Error()
	}
	if serr != nil {
		return "none: the kernel refused SO_BINDTOIFINDEX (" + serr.Error() + ")"
	}
	return "SO_BINDTOIFINDEX"
}
