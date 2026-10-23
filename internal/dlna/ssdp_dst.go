package dlna

import (
	"encoding/binary"
	"math"
	"net/netip"
)

// An SSDP listener is bound to 0.0.0.0:1900, so it takes a unicast datagram
// sent to any address of this host, and the address it was SENT TO is what
// decides whether the MediaServer answers it (servedAddrs, backlog B48). The
// kernel reports that address beside the datagram once asked
// (armDatagramDst), and the index of the interface the datagram ARRIVED on
// once asked for that too (armDatagramArrival, B470 PR 5a1), which a strict
// registration needs and the MediaServer does not use: IP_PKTINFO on Linux
// and IP_RECVPKTINFO on macOS, through x/net's FlagDst and FlagInterface
// (ssdp_dst_other.go), and IP_PKTINFO through WSARecvMsg on Windows, where
// x/net reads no control message (ssdp_dst_windows.go). On all three one
// control message carries both, and each is still asked for by a call of its
// own, so a refusal of the second leaves the first armed.

// datagramMeta is what the kernel reported beside one datagram: the address
// it was sent to and the index of the interface it arrived on, each with
// whether it was reported. A field the kernel did not report, or reported
// cut short or malformed, is not valid, and nothing stands in for it: an
// index of 0 names no interface, and a listener's own interface is never
// taken for the one a datagram arrived on.
type datagramMeta struct {
	dst     netip.Addr
	dstOK   bool
	ifIndex int
	ifOK    bool
}

// metaIndex is a reported interface index as datagramMeta keeps it: valid
// only when it is positive and fits an int on every platform the bridge
// builds for.
func metaIndex(idx uint64) (int, bool) {
	if idx == 0 || idx > math.MaxInt32 {
		return 0, false
	}
	return int(idx), true
}

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

// parseWSAPktinfo reads the destination address and the arrival interface's
// index out of the control data WSARecvMsg returned, laid out by Winsock's
// WSA_CMSG macros on a platform whose pointers are ptrSize bytes: a
// WSACMSGHDR (cmsg_len, a SIZE_T, then cmsg_level and cmsg_type, two INTs),
// its data at the header's size rounded up to ptrSize, and the next header
// at cmsg_len rounded up to ptrSize. The data is an IN_PKTINFO: the
// destination's IN_ADDR, then the index, a little-endian ULONG (Windows'
// every architecture is little-endian). ok is false when no IP_PKTINFO
// message is there, or the data is cut short; an index of 0 is returned as
// it came, and names no interface.
//
// It decodes bytes rather than casting the buffer to x/sys/windows' structs,
// so it needs no alignment of the buffer and runs, and is tested, on every
// platform.
func parseWSAPktinfo(oob []byte, ptrSize int) (dst netip.Addr, ifIndex uint32, ok bool) {
	if ptrSize != 4 && ptrSize != 8 {
		return netip.Addr{}, 0, false
	}
	align := func(n int) int { return (n + ptrSize - 1) &^ (ptrSize - 1) }
	hdrSize := ptrSize + 8
	dataOff := align(hdrSize)
	for off := 0; off+hdrSize <= len(oob); {
		var msgLen int
		if ptrSize == 8 {
			l := binary.LittleEndian.Uint64(oob[off:])
			if l > uint64(len(oob)) {
				return netip.Addr{}, 0, false
			}
			msgLen = int(l)
		} else {
			msgLen = int(binary.LittleEndian.Uint32(oob[off:]))
		}
		if msgLen < dataOff || off+msgLen > len(oob) {
			return netip.Addr{}, 0, false
		}
		level := int32(binary.LittleEndian.Uint32(oob[off+ptrSize:]))
		typ := int32(binary.LittleEndian.Uint32(oob[off+ptrSize+4:]))
		if level == wsaIPProtoIP && typ == wsaIPPktinfo && msgLen-dataOff >= wsaInPktinfoSize {
			data := oob[off+dataOff:]
			return netip.AddrFrom4([4]byte(data[:4])), binary.LittleEndian.Uint32(data[4:8]), true
		}
		off += align(msgLen)
	}
	return netip.Addr{}, 0, false
}

// wsaDatagramMeta is the datagramMeta of control data WSARecvMsg returned on
// a platform whose pointers are ptrSize bytes (parseWSAPktinfo).
func wsaDatagramMeta(oob []byte, ptrSize int) datagramMeta {
	dst, idx, ok := parseWSAPktinfo(oob, ptrSize)
	if !ok {
		return datagramMeta{}
	}
	m := datagramMeta{dst: dst, dstOK: true}
	m.ifIndex, m.ifOK = metaIndex(uint64(idx))
	return m
}
