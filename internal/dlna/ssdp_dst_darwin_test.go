package dlna

import (
	"encoding/binary"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"
)

// platformControlData is what x/net's macOS table reads that its Linux table
// does not (B610): the separate destination and interface messages macOS
// writes for IP_RECVDSTADDR and IP_RECVIF, alone or together, as it always
// has, though the listener asks for IP_RECVPKTINFO. (The table sets every
// entry here, so x/net always passed an IPPROTO_IP message of type 0 over;
// since v0.62.0 it does on Linux too, and those rows are
// portableControlData's, backlog B615.)
func platformControlData() []controlDataCase {
	dstaddr := unixCmsg(unix.IPPROTO_IP, unix.IP_RECVDSTADDR, peer4[:], -1)
	sdl := make([]byte, unix.SizeofSockaddrDatalink) // a sockaddr_dl: sdl_len, sdl_family, sdl_index, ...
	sdl[0], sdl[1] = unix.SizeofSockaddrDatalink, unix.AF_LINK
	binary.NativeEndian.PutUint16(sdl[2:], 9)
	recvif := unixCmsg(unix.IPPROTO_IP, unix.IP_RECVIF, sdl, -1)
	return []controlDataCase{
		{"an_ip_recvdstaddr_message", dstaddr, datagramMeta{dst: netip.AddrFrom4(peer4), dstOK: true}},
		{"an_ip_recvif_message", recvif, datagramMeta{ifIndex: 9, ifOK: true}},
		{"an_ip_recvdstaddr_and_an_ip_recvif_message", slices.Concat(dstaddr, recvif), pktinfoMeta(peer4, 9)},
	}
}
