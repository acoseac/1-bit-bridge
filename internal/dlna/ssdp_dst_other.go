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
//
// It holds whatever it is handed, on every platform this file builds for
// (backlog B610). What it reports is what x/net's parse reports, and an input
// that makes that parse panic is control data that does not parse
// (parseControlMessage).
func parseDatagramMeta(oob []byte) datagramMeta {
	cm, ok := parseControlMessage(oob)
	if !ok {
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

// parseControlMessage is x/net's parse of oob, and ok is false when that
// parse returns an error or panics.
//
// x/net's parse alone does not hold. Its table of the IPPROTO_IP messages it
// reads is each platform's own, and an entry a platform leaves unset has a
// name and a length of 0 and no parse function. Parse compares each
// IPPROTO_IP message's type and length with the entries in turn, so a message
// of type 0 matches the first unset entry and calls the function it lacks.
// At x/net v0.61.0 only Darwin's table sets every entry: Linux's sets its TTL
// and packet-info entries alone, and the tables of the BSDs, Solaris, AIX and
// z/OS each leave one or more unset. No kernel writes such a message beside a
// datagram (every platform numbers its IPPROTO_IP options from 1), and a
// network peer cannot supply the ancillary bytes, but a panic here would end
// the process: the SSDP read loop runs with no recovery.
//
// The recover covers this one call and nothing else, and what it hides is
// the intended effect: x/net panicking on control data no kernel wrote, which
// then reports neither, whatever else oob holds.
func parseControlMessage(oob []byte) (cm ipv4.ControlMessage, ok bool) {
	defer func() {
		if recover() != nil {
			cm, ok = ipv4.ControlMessage{}, false
		}
	}()
	err := cm.Parse(oob)
	return cm, err == nil
}
