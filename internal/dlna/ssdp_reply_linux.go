package dlna

import (
	"net"

	"golang.org/x/net/ipv4"
)

// pinStrictReply needs no socket option on Linux: every send carries its
// outgoing interface in IP_PKTINFO, which requires no bind-to-device privilege.
func pinStrictReply(_ ssdpUnicastSocket, _ int) error { return nil }

// writeStrictReply supplies both ipi_ifindex and ipi_spec_dst. A source bind
// alone does not constrain the route on an overlapping subnet or a VPN.
func writeStrictReply(c ssdpUnicastSocket, b []byte, to *net.UDPAddr, from SSDPEndpointSnapshot) (int, error) {
	cm := ipv4.ControlMessage{IfIndex: from.Interface.Index, Src: net.IP(from.Local.Addr().AsSlice())}
	n, _, err := c.WriteMsgUDP(b, cm.Marshal(), to)
	return n, err
}
