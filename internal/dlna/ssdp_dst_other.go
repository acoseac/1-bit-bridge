//go:build !windows

package dlna

import (
	"net"
	"net/netip"

	"golang.org/x/net/ipv4"
)

// datagramDstSpace is the out-of-band buffer a datagram's destination needs:
// one IP_PKTINFO control message, as x/net sizes it for this platform.
var datagramDstSpace = len(ipv4.NewControlMessage(ipv4.FlagDst))

// armDatagramDst asks the kernel to report each datagram's destination
// address on c: IP_PKTINFO on Linux, IP_RECVPKTINFO on macOS and the BSDs
// (x/net's FlagDst, which both deliver as an IP_PKTINFO message).
func armDatagramDst(c *net.UDPConn) error {
	return ipv4.NewPacketConn(c).SetControlMessage(ipv4.FlagDst, true)
}

// parseDatagramDst reads the destination address out of a datagram's
// out-of-band data. ok is false when it carries none.
func parseDatagramDst(oob []byte) (netip.Addr, bool) {
	var cm ipv4.ControlMessage
	if err := cm.Parse(oob); err != nil || cm.Dst == nil {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(cm.Dst)
	return a.Unmap(), ok
}
