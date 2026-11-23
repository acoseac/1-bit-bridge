//go:build linux

package consolelan

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Hold is the net.ListenConfig Control that holds a socket, before its
// bind, to the interface whose index is index: SO_BINDTOIFINDEX, which an
// unprivileged process may set on a socket not yet bound to a device since
// Linux 5.7, read back with getsockopt (HoldUsing). An index of 0 or less, a
// kernel that refuses the option and a read-back that names another index
// each return a *RefusalError, so Listen closes the socket and nothing
// listens: the console never listens unrestricted, where OpenHome's helper
// does (internal/player/openhome/restrict_linux.go). ipv6 changes nothing
// here: the option is SOL_SOCKET's, and the IPv6 listener lookup compares it
// too.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	return HoldUsing(OptionCalls{Name: "SO_BINDTOIFINDEX", Set: setBindToIfindex, Get: getBindToIfindex})(index, ipv6)
}

// setBindToIfindex and getBindToIfindex are the option's two calls.
func setBindToIfindex(fd uintptr, index int) error {
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX, index)
}

func getBindToIfindex(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX)
}

// HostModel has no Linux table: the strong host model is Windows'.
func HostModel(ipv6 bool) (Table, error) {
	_ = ipv6
	return nil, &RefusalError{Reason: ReasonNotSupportedHere}
}
