//go:build windows

package dlna

import (
	"math/bits"
	"net"
	"net/netip"

	"golang.org/x/sys/windows"
)

// datagramDstSpace is the out-of-band buffer a datagram's destination needs:
// WSA_CMSG_SPACE(sizeof(IN_PKTINFO)), 24 bytes on 64-bit Windows, with room
// to spare.
const datagramDstSpace = 64

// armDatagramDst asks Winsock to report each datagram's destination address
// on c (IP_PKTINFO). x/net reads no control message on Windows, so this sets
// the option itself, and net.UDPConn.ReadMsgUDP, which calls WSARecvMsg,
// returns the message.
func armDatagramDst(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_PKTINFO, 1)
	}); err != nil {
		return err
	}
	return serr
}

// parseDatagramDst reads the destination address out of a datagram's
// out-of-band data (parseWSAPktinfo). ok is false when it carries none.
func parseDatagramDst(oob []byte) (netip.Addr, bool) {
	return parseWSAPktinfo(oob, bits.UintSize/8)
}
