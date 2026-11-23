//go:build darwin

package consolelan

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Hold is the net.ListenConfig Control that holds a socket, before its
// bind, to the interface whose index is index: IP_BOUND_IF, read back
// (HoldUsing), which scopes what the socket sends, its SYN-ACK included, to
// that interface, so a handshake completes only with a peer that interface
// reaches (measured on macOS, internal/player/openhome/restrict_darwin.go).
// An IPv6 socket is refused: IPV6_BOUND_IF has never been measured, so a
// ULA there is not_supported_here (Decide). Every refusal is a
// *RefusalError, and Listen then closes the socket.
func Hold(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error {
	if ipv6 {
		return func(_, _ string, _ syscall.RawConn) error {
			return &RefusalError{Reason: ReasonNotSupportedHere}
		}
	}
	return HoldUsing(OptionCalls{Name: "IP_BOUND_IF", Set: setBoundIf, Get: getBoundIf})(index, ipv6)
}

func setBoundIf(fd uintptr, index int) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, index)
}

func getBoundIf(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF)
}

// HostModel has no macOS table: the strong host model is Windows'.
func HostModel(ipv6 bool) (Table, error) {
	_ = ipv6
	return nil, &RefusalError{Reason: ReasonNotSupportedHere}
}
