//go:build linux || darwin

package dlna

import (
	"encoding/binary"
	"math"
	"net/netip"
	"slices"
	"testing"

	"golang.org/x/net/ipv4"
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

// referenceDatagramMeta is parseDatagramMeta as it was before B610, x/net's
// parse with nothing around it, run under a recover of the test's own: m is
// what that parser reported, or neither where x/net's parse panicked (and
// that parser with it), which panicked says.
func referenceDatagramMeta(oob []byte) (m datagramMeta, panicked bool) {
	defer func() {
		if recover() != nil {
			m, panicked = datagramMeta{}, true
		}
	}()
	var cm ipv4.ControlMessage
	if err := cm.Parse(oob); err != nil {
		return datagramMeta{}, false
	}
	if cm.Dst != nil {
		if a, ok := netip.AddrFromSlice(cm.Dst); ok {
			m.dst, m.dstOK = a.Unmap(), true
		}
	}
	if cm.IfIndex > 0 {
		m.ifIndex, m.ifOK = metaIndex(uint64(cm.IfIndex))
	}
	return m, false
}

// controlDataCase is one shape of control data, laid out for this platform
// (unixCmsg), with the metadata parseDatagramMeta must report for it and
// whether x/net's own parse panics on it here.
type controlDataCase struct {
	name   string
	oob    []byte
	want   datagramMeta
	panics bool
}

// ssdpGroup4 and peer4 are the destinations pktinfoAt's messages carry: the
// SSDP group, and an address of another host on the LAN.
var (
	ssdpGroup4 = [4]byte{239, 255, 255, 250}
	peer4      = [4]byte{192, 168, 1, 30}
)

// pktinfoAt is an IP_PKTINFO message laid out for this platform, sent to dst
// (from 192.168.1.20, the local address a reply would route from) and
// arriving on index ifIndex.
func pktinfoAt(ifIndex uint32, dst [4]byte) []byte {
	return unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(ifIndex, [4]byte{192, 168, 1, 20}, dst), -1)
}

// pktinfoMeta is the metadata a datagram sent to dst reports: the
// destination, and the arrival index idx when it is not 0.
func pktinfoMeta(dst [4]byte, idx int) datagramMeta {
	return datagramMeta{dst: netip.AddrFrom4(dst), dstOK: true, ifIndex: idx, ifOK: idx > 0}
}

// portableControlData is every shape of control data whose metadata and
// whose x/net verdict are the same on Linux and macOS: B610's policy, on
// each shape its review named. x/net's answer stands wherever its parse
// returns. A message at another level, or of an IPPROTO_IP type x/net does
// not read here, is passed over, before or after an IP_PKTINFO; so is an
// IP_PKTINFO shorter than its struct, and one before it stands. A header
// whose length runs past the buffer or stops short of a header makes the
// whole buffer report neither, before or after an IP_PKTINFO. Of two
// IP_PKTINFO messages the later wins, its index of 0 included. A tail
// shorter than a header is ignored. And the index is in_pktinfo's 32 bits,
// reported only from 1 to MaxInt32 (Linux reads it signed, macOS unsigned,
// and metaIndex refuses the rest). The IPPROTO_IP messages of type 0 are
// platformControlData's, since x/net panics on them on Linux and not on
// macOS.
func portableControlData() []controlDataCase {
	pkt := pktinfoAt(7, ssdpGroup4)
	tos := unixCmsg(unix.IPPROTO_IP, unix.IP_TOS, []byte{0x10, 0, 0, 0}, -1)
	sock := unixCmsg(unix.SOL_SOCKET, 0, make([]byte, 8), -1)
	short := unixCmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, inetPktinfo(9, [4]byte{192, 168, 1, 20}, peer4)[:8], -1)
	past := unixCmsg(unix.IPPROTO_IP, unix.IP_TTL, []byte{64, 0, 0, 0}, 1<<20)
	below := unixCmsg(unix.IPPROTO_IP, unix.IP_TTL, []byte{64, 0, 0, 0}, 4)
	return []controlDataCase{
		{"a_type_x_net_does_not_read_alone", tos, datagramMeta{}, false},
		{"a_type_x_net_does_not_read_before_a_pktinfo", slices.Concat(tos, pkt), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_type_x_net_does_not_read_after_a_pktinfo", slices.Concat(pkt, tos), pktinfoMeta(ssdpGroup4, 7), false},
		{"another_levels_type_0_before_a_pktinfo", slices.Concat(sock, pkt), pktinfoMeta(ssdpGroup4, 7), false},
		{"another_levels_type_0_after_a_pktinfo", slices.Concat(pkt, sock), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_short_pktinfo_alone", short, datagramMeta{}, false},
		{"a_short_pktinfo_before_a_whole_one", slices.Concat(short, pkt), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_short_pktinfo_after_a_whole_one", slices.Concat(pkt, short), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_length_past_the_buffer_before_a_pktinfo", slices.Concat(past, pkt), datagramMeta{}, false},
		{"a_length_past_the_buffer_after_a_pktinfo", slices.Concat(pkt, past), datagramMeta{}, false},
		{"a_length_below_the_header_before_a_pktinfo", slices.Concat(below, pkt), datagramMeta{}, false},
		{"a_length_below_the_header_after_a_pktinfo", slices.Concat(pkt, below), datagramMeta{}, false},
		{"two_pktinfos_that_disagree", slices.Concat(pkt, pktinfoAt(9, peer4)), pktinfoMeta(peer4, 9), false},
		{"the_same_two_the_other_way_round", slices.Concat(pktinfoAt(9, peer4), pkt), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_later_pktinfo_with_an_index_of_0", slices.Concat(pkt, pktinfoAt(0, peer4)), pktinfoMeta(peer4, 0), false},
		{"a_tail_shorter_than_a_header_after_a_pktinfo", slices.Concat(pkt, []byte{1, 2, 3, 4}), pktinfoMeta(ssdpGroup4, 7), false},
		{"an_index_of_0", pktinfoAt(0, ssdpGroup4), pktinfoMeta(ssdpGroup4, 0), false},
		{"an_index_of_maxint32", pktinfoAt(math.MaxInt32, ssdpGroup4), pktinfoMeta(ssdpGroup4, math.MaxInt32), false},
		{"an_index_of_maxint32_plus_1", pktinfoAt(math.MaxInt32+1, ssdpGroup4), pktinfoMeta(ssdpGroup4, 0), false},
		{"an_index_of_maxuint32", pktinfoAt(math.MaxUint32, ssdpGroup4), pktinfoMeta(ssdpGroup4, 0), false},
	}
}

// TestParseDatagramMetaAnswersEveryShapeOfControlData pins B610's policy on
// each shape portableControlData and this platform's platformControlData
// lay out: the metadata each must report, whether x/net's own parse panics
// on it here, and that the metadata is x/net's answer, or neither where that
// parse panics (referenceDatagramMeta).
func TestParseDatagramMetaAnswersEveryShapeOfControlData(t *testing.T) {
	for _, tc := range slices.Concat(portableControlData(), platformControlData()) {
		got := parseDatagramMeta(tc.oob)
		if got != tc.want {
			t.Errorf("%s: reported %+v, want %+v", tc.name, got, tc.want)
		}
		ref, panicked := referenceDatagramMeta(tc.oob)
		if panicked != tc.panics {
			// A release of x/net that stops panicking on this shape makes
			// its answer the policy's, and this case's metadata with it.
			t.Errorf("%s: x/net's parse panicked %v, want %v", tc.name, panicked, tc.panics)
		}
		if got != ref {
			t.Errorf("%s: reported %+v, where x/net's parse reports %+v (panicked %v)", tc.name, got, ref, panicked)
		}
	}
}

// FuzzParseDatagramMetaIsXNetsParse holds the parser to B610's policy on any
// bytes: what it reports is x/net's answer wherever x/net's parse returns,
// and neither wherever that parse panics (referenceDatagramMeta), with the
// metadata always a value a datagram can carry.
func FuzzParseDatagramMetaIsXNetsParse(f *testing.F) {
	for _, tc := range slices.Concat(portableControlData(), platformControlData()) {
		f.Add(tc.oob)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := parseDatagramMeta(b)
		if want, panicked := referenceDatagramMeta(b); got != want {
			t.Fatalf("reported %+v, where x/net's parse reports %+v (panicked %v)", got, want, panicked)
		}
		checkDatagramMeta(t, got)
	})
}
