package dlna

import (
	"math/bits"
	"net"

	"golang.org/x/sys/windows"
)

// ssdpIPUnicastIF is IP_UNICAST_IF from ws2ipdef.h. Winsock takes the IPv4
// interface index in network byte order (both supported Windows targets are LE).
const ssdpIPUnicastIF = 31

func pinStrictReply(c ssdpUnicastSocket, index int) error {
	return setStrictReplyInterface(c, index, func(fd uintptr, index int) error {
		return windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ssdpIPUnicastIF, int(bits.ReverseBytes32(uint32(index))))
	})
}

func writeStrictReply(c ssdpUnicastSocket, b []byte, to *net.UDPAddr, _ SSDPEndpointSnapshot) (int, error) {
	return c.WriteToUDP(b, to)
}
