package dlna

import (
	"encoding/binary"
	"net/netip"
)

// An SSDP advertiser's M-SEARCH listener is bound to 0.0.0.0:1900, so it
// takes a unicast datagram sent to any address of this host, and the address
// it was SENT TO is what decides whether it is answered (servedAddrs, backlog
// B48). The kernel reports that address beside the datagram once asked
// (armDatagramDst): IP_PKTINFO on Linux and IP_RECVPKTINFO on macOS, through
// x/net's FlagDst (ssdp_dst_other.go), and IP_PKTINFO through WSARecvMsg on
// Windows, where x/net reads no control message (ssdp_dst_windows.go).

// wsaIPProtoIP and wsaIPPktinfo are Winsock's IPPROTO_IP and IP_PKTINFO, the
// level and type of the control message parseWSAPktinfo reads. A test on
// Windows pins them to x/sys/windows' values.
const (
	wsaIPProtoIP = 0
	wsaIPPktinfo = 19
)

// wsaInPktinfoSize is sizeof(IN_PKTINFO): the destination's IN_ADDR, then
// the arrival interface's index, a ULONG.
const wsaInPktinfoSize = 8

// parseWSAPktinfo reads the destination address out of the control data
// WSARecvMsg returned, laid out by Winsock's WSA_CMSG macros on a platform
// whose pointers are ptrSize bytes: a WSACMSGHDR (cmsg_len, a SIZE_T, then
// cmsg_level and cmsg_type, two INTs), its data at the header's size rounded
// up to ptrSize, and the next header at cmsg_len rounded up to ptrSize.
// Windows' every architecture is little-endian. ok is false when no
// IP_PKTINFO message is there, or the data is cut short.
//
// It decodes bytes rather than casting the buffer to x/sys/windows' structs,
// so it needs no alignment of the buffer and runs, and is tested, on every
// platform.
func parseWSAPktinfo(oob []byte, ptrSize int) (netip.Addr, bool) {
	if ptrSize != 4 && ptrSize != 8 {
		return netip.Addr{}, false
	}
	align := func(n int) int { return (n + ptrSize - 1) &^ (ptrSize - 1) }
	hdrSize := ptrSize + 8
	dataOff := align(hdrSize)
	for off := 0; off+hdrSize <= len(oob); {
		var msgLen int
		if ptrSize == 8 {
			l := binary.LittleEndian.Uint64(oob[off:])
			if l > uint64(len(oob)) {
				return netip.Addr{}, false
			}
			msgLen = int(l)
		} else {
			msgLen = int(binary.LittleEndian.Uint32(oob[off:]))
		}
		if msgLen < dataOff || off+msgLen > len(oob) {
			return netip.Addr{}, false
		}
		level := int32(binary.LittleEndian.Uint32(oob[off+ptrSize:]))
		typ := int32(binary.LittleEndian.Uint32(oob[off+ptrSize+4:]))
		if level == wsaIPProtoIP && typ == wsaIPPktinfo && msgLen-dataOff >= wsaInPktinfoSize {
			return netip.AddrFrom4([4]byte(oob[off+dataOff : off+dataOff+4])), true
		}
		off += align(msgLen)
	}
	return netip.Addr{}, false
}
