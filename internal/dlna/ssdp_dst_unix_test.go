//go:build linux || darwin

package dlna

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

// unixCmsg lays one control message out as this platform's kernel writes
// it: a cmsghdr (cmsg_len, a size_t on Linux and a socklen_t on macOS, then
// cmsg_level and cmsg_type), the data at CMSG_LEN(0), and the message padded
// to CMSG_SPACE. msgLen overrides cmsg_len when it is not negative.
func unixCmsg(level, typ int, data []byte, msgLen int) []byte {
	b := make([]byte, unix.CmsgSpace(len(data)))
	if msgLen < 0 {
		msgLen = unix.CmsgLen(len(data))
	}
	lenSize := unix.SizeofCmsghdr - 8
	if lenSize == 8 {
		binary.NativeEndian.PutUint64(b, uint64(msgLen))
	} else {
		binary.NativeEndian.PutUint32(b, uint32(msgLen))
	}
	binary.NativeEndian.PutUint32(b[lenSize:], uint32(level))
	binary.NativeEndian.PutUint32(b[lenSize+4:], uint32(typ))
	copy(b[unix.CmsgLen(0):], data)
	return b
}

// inetPktinfo is a struct in_pktinfo as Linux and macOS lay it out:
// ipi_ifindex, ipi_spec_dst (the local address a reply would route from),
// ipi_addr (the destination the datagram's header carries).
func inetPktinfo(ifIndex uint32, specDst, addr [4]byte) []byte {
	b := make([]byte, unix.SizeofInet4Pktinfo)
	binary.NativeEndian.PutUint32(b, ifIndex)
	copy(b[4:8], specDst[:])
	copy(b[8:12], addr[:])
	return b
}

// TestParseDatagramMetaReadsThisPlatformsPktinfo pins the Linux and macOS
// parser over control data laid out as the kernel writes it: the destination
// is in_pktinfo's ipi_addr, never ipi_spec_dst, the index is ipi_ifindex,
// found alone and after another message; an index of 0 leaves the
// destination valid and the index not; and a message that is absent, cut
// short or malformed reports neither, never a guess.
func TestParseDatagramMetaReadsThisPlatformsPktinfo(t *testing.T) {
	group := [4]byte{239, 255, 255, 250}
	local := [4]byte{192, 168, 1, 20}
	pkt := unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(7, local, group), -1)
	ttl := unixCmsg(unix.IPPROTO_IP, unix.IP_TTL, []byte{64, 0, 0, 0}, -1)
	for _, tc := range []struct {
		name    string
		oob     []byte
		wantDst netip.Addr
		wantIdx int
	}{
		{"alone", pkt, netip.AddrFrom4(group), 7},
		{"after_another_message", append(append([]byte{}, ttl...), pkt...), netip.AddrFrom4(group), 7},
		{"an_index_of_zero", unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(0, local, group), -1), netip.AddrFrom4(group), 0},
		{"none", nil, netip.Addr{}, 0},
		{"another_message_alone", ttl, netip.Addr{}, 0},
		{"an_ipv6_message", unixCmsg(unix.IPPROTO_IPV6, unix.IPV6_PKTINFO, make([]byte, 20), -1), netip.Addr{}, 0},
		{"a_header_alone", pkt[:unix.CmsgLen(0)], netip.Addr{}, 0},
		{"cut_short_by_the_kernel", unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(7, local, group), unix.CmsgLen(8)), netip.Addr{}, 0},
		{"a_length_past_the_buffer", unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(7, local, group), 1<<20), netip.Addr{}, 0},
		{"a_zero_length", unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(7, local, group), 0), netip.Addr{}, 0},
		{"zeros", make([]byte, 64), netip.Addr{}, 0},
	} {
		m := parseDatagramMeta(tc.oob)
		if m.dstOK != tc.wantDst.IsValid() || m.dst != tc.wantDst {
			t.Errorf("%s: destination %v (%v), want %v", tc.name, m.dst, m.dstOK, tc.wantDst)
		}
		if m.ifOK != (tc.wantIdx != 0) || m.ifIndex != tc.wantIdx {
			t.Errorf("%s: index %d (%v), want %d", tc.name, m.ifIndex, m.ifOK, tc.wantIdx)
		}
	}
	if datagramMetaSpace < unix.CmsgSpace(unix.SizeofInet4Pktinfo) {
		t.Errorf("datagramMetaSpace is %d bytes, less than one IP_PKTINFO message's %d",
			datagramMetaSpace, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
	}
}

// TestParseDatagramMetaHoldsAnIPMessageOfTypeZero pins the nightly fuzz
// crash of 2026-10-24 to 31 (backlog B610): an IPPROTO_IP control message of
// type 0, which x/net's parser on Linux hands to a parse function its table
// leaves unset, reports neither a destination nor an arrival index, and the
// parser returns.
func TestParseDatagramMetaHoldsAnIPMessageOfTypeZero(t *testing.T) {
	oob := unixCmsg(unix.IPPROTO_IP, 0, []byte{192, 168, 1, 20, 7, 0, 0, 0}, -1)
	if m := parseDatagramMeta(oob); m != (datagramMeta{}) {
		t.Errorf("an IPPROTO_IP message of type 0 reported %+v, want neither", m)
	}
}
