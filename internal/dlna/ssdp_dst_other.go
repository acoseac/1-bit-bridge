//go:build !windows

package dlna

import (
	"net"
	"net/netip"

	"golang.org/x/net/ipv4"
)

// datagramMetaSpace is the out-of-band buffer a datagram's destination and
// arrival index need: on Linux and macOS one IP_PKTINFO control message
// carries both, as x/net sizes it for this platform.
var datagramMetaSpace = len(ipv4.NewControlMessage(ipv4.FlagDst | ipv4.FlagInterface))

// armDatagramDst asks the kernel to report each datagram's destination
// address on c: IP_PKTINFO on Linux, IP_RECVPKTINFO on macOS and the BSDs
// (x/net's FlagDst, which both deliver as an IP_PKTINFO message).
func armDatagramDst(c *net.UDPConn) error {
	return ipv4.NewPacketConn(c).SetControlMessage(ipv4.FlagDst, true)
}

// armDatagramArrival asks the kernel to report the index of the interface
// each datagram arrived on: x/net's FlagInterface, the same IP_PKTINFO (Linux)
// or IP_RECVPKTINFO (macOS) message as the destination, asked for by a call
// of its own so that its refusal leaves the destination armed.
func armDatagramArrival(c *net.UDPConn) error {
	return ipv4.NewPacketConn(c).SetControlMessage(ipv4.FlagInterface, true)
}

// parseDatagramMeta reads the destination address and the arrival index out
// of a datagram's out-of-band data: in_pktinfo's ipi_addr, the destination
// the datagram's header carries (never ipi_spec_dst, the local address a
// reply would route from), and its ipi_ifindex. Control data that does not
// parse reports neither, and an IP_PKTINFO message cut short is passed over
// by x/net's parser, so it reports neither either.
func parseDatagramMeta(oob []byte) datagramMeta {
	var cm ipv4.ControlMessage
	if err := cm.Parse(oob); err != nil {
		return datagramMeta{}
	}
	var m datagramMeta
	if cm.Dst != nil {
		if a, ok := netip.AddrFromSlice(cm.Dst); ok {
			m.dst, m.dstOK = a.Unmap(), true
		}
	}
	if cm.IfIndex > 0 {
		m.ifIndex, m.ifOK = metaIndex(uint64(cm.IfIndex))
	}
	return m
}
