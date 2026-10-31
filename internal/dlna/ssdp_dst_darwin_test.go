package dlna

import (
	"encoding/binary"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"
)

// platformControlData is what x/net's macOS table reads that its Linux table
// does not (B610). The table sets every entry here, so an IPPROTO_IP message
// of type 0 matches none and is passed over like any type x/net does not
// read, and an IP_PKTINFO beside it stands. And x/net reads the separate
// destination and interface messages macOS writes for IP_RECVDSTADDR and
// IP_RECVIF, alone or together, as it always has, though the listener asks
// for IP_RECVPKTINFO.
func platformControlData() []controlDataCase {
	pkt := pktinfoAt(7, ssdpGroup4)
	zero := unixCmsg(unix.IPPROTO_IP, 0, []byte{0, 0, 0, 0}, -1)
	dstaddr := unixCmsg(unix.IPPROTO_IP, unix.IP_RECVDSTADDR, peer4[:], -1)
	sdl := make([]byte, unix.SizeofSockaddrDatalink) // a sockaddr_dl: sdl_len, sdl_family, sdl_index, ...
	sdl[0], sdl[1] = unix.SizeofSockaddrDatalink, unix.AF_LINK
	binary.NativeEndian.PutUint16(sdl[2:], 9)
	recvif := unixCmsg(unix.IPPROTO_IP, unix.IP_RECVIF, sdl, -1)
	return []controlDataCase{
		{"type_0_alone", zero, datagramMeta{}, false},
		{"type_0_before_a_pktinfo", slices.Concat(zero, pkt), pktinfoMeta(ssdpGroup4, 7), false},
		{"type_0_after_a_pktinfo", slices.Concat(pkt, zero), pktinfoMeta(ssdpGroup4, 7), false},
		{"type_0_between_two_pktinfos", slices.Concat(pkt, zero, pktinfoAt(9, peer4)), pktinfoMeta(peer4, 9), false},
		{"an_ip_recvdstaddr_message", dstaddr, datagramMeta{dst: netip.AddrFrom4(peer4), dstOK: true}, false},
		{"an_ip_recvif_message", recvif, datagramMeta{ifIndex: 9, ifOK: true}, false},
		{"an_ip_recvdstaddr_and_an_ip_recvif_message", slices.Concat(dstaddr, recvif), pktinfoMeta(peer4, 9), false},
	}
}
