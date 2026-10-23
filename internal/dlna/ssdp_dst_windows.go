//go:build windows

package dlna

import (
	"math/bits"
	"net"

	"golang.org/x/sys/windows"
)

// datagramMetaSpace is the out-of-band buffer a datagram's destination and
// arrival index need: WSA_CMSG_SPACE(sizeof(IN_PKTINFO)), one message that
// carries both, 24 bytes on 64-bit Windows, with room to spare.
const datagramMetaSpace = 64

// armDatagramDst asks Winsock to report each datagram's destination address
// on c (IP_PKTINFO). x/net reads no control message on Windows, so this sets
// the option itself, and net.UDPConn.ReadMsgUDP, which calls WSARecvMsg,
// returns the message.
func armDatagramDst(c *net.UDPConn) error {
	return setPktinfo(c)
}

// armDatagramArrival asks Winsock to report the index of the interface each
// datagram arrived on: IN_PKTINFO's ipi_ifindex, in the same IP_PKTINFO
// message as the destination, asked for by a call of its own so that its
// refusal leaves the destination armed.
func armDatagramArrival(c *net.UDPConn) error {
	return setPktinfo(c)
}

// setPktinfo turns IP_PKTINFO on for c.
func setPktinfo(c *net.UDPConn) error {
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

// parseDatagramMeta reads the destination address and the arrival index out
// of a datagram's out-of-band data (parseWSAPktinfo, at this platform's
// pointer size).
func parseDatagramMeta(oob []byte) datagramMeta {
	return wsaDatagramMeta(oob, bits.UintSize/8)
}
