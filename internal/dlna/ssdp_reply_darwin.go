package dlna

import (
	"net"

	"golang.org/x/sys/unix"
)

// pinStrictReply constrains the route before the first unconnected send.
func pinStrictReply(c ssdpUnicastSocket, index int) error {
	return setStrictReplyInterface(c, index, func(fd uintptr, index int) error {
		return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, index)
	})
}

func writeStrictReply(c ssdpUnicastSocket, b []byte, to *net.UDPAddr, _ SSDPEndpointSnapshot) (int, error) {
	return c.WriteToUDP(b, to)
}
