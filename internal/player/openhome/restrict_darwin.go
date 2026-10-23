//go:build darwin

package openhome

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// restrictToInterface restricts the socket c, before its bind, to the
// interface whose index is index, with IP_BOUND_IF, which an unprivileged
// process may set. Measured on macOS (darwin 27, 2026-10-23): it scopes what
// the socket sends, so a connection that arrived by another interface is
// answered out of the bound one and never completes from a peer not on
// that link (a loopback listener bound to en0, dialled on loopback, timed
// out), and it does not refuse a segment by the interface it arrived on (a
// listener on en0's address bound to en0 took this host's own connection,
// which arrives on lo0). It names the option, or "none: " and why.
func restrictToInterface(c syscall.RawConn, index int) string {
	if index <= 0 {
		return "none: the interface's index is not known"
	}
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, index)
	}); err != nil {
		return "none: " + err.Error()
	}
	if serr != nil {
		return "none: the kernel refused IP_BOUND_IF (" + serr.Error() + ")"
	}
	return "IP_BOUND_IF"
}
