package dlna

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// readArmed reads one datagram from l, armed by armDatagramDst, and returns
// its payload and the destination the kernel reported.
func readArmed(l *net.UDPConn) (payload string, dst netip.Addr, ok bool, err error) {
	payload, m, err := readArmedMeta(l)
	return payload, m.dst, m.dstOK, err
}

// readArmedMeta reads one datagram from l and returns its payload and all
// the kernel reported beside it (parseDatagramMeta).
func readArmedMeta(l *net.UDPConn) (payload string, m datagramMeta, err error) {
	buf := make([]byte, 2048)
	oob := make([]byte, datagramMetaSpace)
	_ = l.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, oobn, _, _, err := l.ReadMsgUDP(buf, oob)
	if err != nil {
		return "", datagramMeta{}, err
	}
	return string(buf[:n]), parseDatagramMeta(oob[:oobn]), nil
}

// TestTheSSDPListenerReadsEachDatagramsDestination pins the platform half of
// backlog B48's SSDP check on every platform CI runs, Windows' WSARecvMsg
// path included: an armed socket reports the address a datagram was sent to,
// a unicast to 127.0.0.1 as 127.0.0.1, and a group datagram as the group.
func TestTheSSDPListenerReadsEachDatagramsDestination(t *testing.T) {
	t.Run("unicast", func(t *testing.T) {
		l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("bind a loopback socket: %v", err)
		}
		defer l.Close()
		if err := armDatagramDst(l); err != nil {
			t.Fatalf("armDatagramDst: %v", err)
		}
		c, err := net.DialUDP("udp4", nil, l.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatalf("open a sender: %v", err)
		}
		defer c.Close()
		nonce := testNonce(t)
		if _, err := c.Write([]byte(nonce)); err != nil {
			t.Fatalf("send: %v", err)
		}
		payload, dst, ok, err := readArmed(l)
		if err != nil {
			t.Fatalf("read the datagram: %v", err)
		}
		if payload != nonce {
			t.Fatalf("read %q, want the datagram sent, %q", payload, nonce)
		}
		if want := netip.MustParseAddr("127.0.0.1"); !ok || dst != want {
			t.Errorf("destination %v (reported %v), want %v", dst, ok, want)
		}
	})
	t.Run("group", func(t *testing.T) {
		lo := loopbackInterface(t)
		group := testMulticastGroup(t)
		l, err := net.ListenMulticastUDP("udp4", lo, group)
		if err != nil {
			t.Skipf("join %v on %s: %v", group, lo.Name, err)
		}
		defer l.Close()
		// Windows applies IP_MULTICAST_LOOP to what a socket RECEIVES, and
		// ListenMulticastUDP turns it off on the listener, so nothing this
		// host sends would reach it (backlog B120); on Linux and macOS it
		// governs only what a socket sends.
		_ = ipv4.NewPacketConn(l).SetMulticastLoopback(true)
		if err := armDatagramDst(l); err != nil {
			t.Fatalf("armDatagramDst: %v", err)
		}
		nonce := testNonce(t)
		sendHostLocal(t, lo, group, nonce)
		payload, dst, ok, err := readArmed(l)
		var ne net.Error
		if err != nil && runtime.GOOS == "windows" && errors.As(err, &ne) && ne.Timeout() {
			t.Skipf("no looped-back multicast reached the listener on %s: %v", lo.Name, err)
		}
		if err != nil {
			t.Fatalf("read the datagram: %v", err)
		}
		if payload != nonce {
			t.Fatalf("read %q, want the datagram sent, %q", payload, nonce)
		}
		if want := netip.AddrFrom4([4]byte(group.IP.To4())); !ok || dst != want {
			t.Errorf("destination %v (reported %v), want the group %v", dst, ok, want)
		}
	})
}

// TestTheSSDPListenerReadsEachDatagramsArrivalInterface pins the arrival half
// of the metadata (B470 PR 5a1) on every platform CI runs, Windows'
// WSARecvMsg path included: a socket armed for the destination and the
// arrival reports, beside each datagram, the index of the interface it
// arrived on, the loopback interface's for a unicast to 127.0.0.1 and for a
// group datagram sent out of the loopback interface, with the destination as
// before.
func TestTheSSDPListenerReadsEachDatagramsArrivalInterface(t *testing.T) {
	lo := loopbackInterface(t)
	arm := func(t *testing.T, l *net.UDPConn) {
		t.Helper()
		if err := armDatagramDst(l); err != nil {
			t.Fatalf("armDatagramDst: %v", err)
		}
		if err := armDatagramArrival(l); err != nil {
			t.Fatalf("armDatagramArrival: %v", err)
		}
	}
	t.Run("unicast", func(t *testing.T) {
		l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("bind a loopback socket: %v", err)
		}
		defer l.Close()
		arm(t, l)
		c, err := net.DialUDP("udp4", nil, l.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatalf("open a sender: %v", err)
		}
		defer c.Close()
		nonce := testNonce(t)
		if _, err := c.Write([]byte(nonce)); err != nil {
			t.Fatalf("send: %v", err)
		}
		payload, m, err := readArmedMeta(l)
		if err != nil {
			t.Fatalf("read the datagram: %v", err)
		}
		if payload != nonce {
			t.Fatalf("read %q, want the datagram sent, %q", payload, nonce)
		}
		if want := netip.MustParseAddr("127.0.0.1"); !m.dstOK || m.dst != want {
			t.Errorf("destination %v (reported %v), want %v", m.dst, m.dstOK, want)
		}
		if !m.ifOK || m.ifIndex != lo.Index {
			t.Errorf("arrival index %d (reported %v), want %s's, %d", m.ifIndex, m.ifOK, lo.Name, lo.Index)
		}
	})
	t.Run("group", func(t *testing.T) {
		group := testMulticastGroup(t)
		l, err := net.ListenMulticastUDP("udp4", lo, group)
		if err != nil {
			t.Skipf("join %v on %s: %v", group, lo.Name, err)
		}
		defer l.Close()
		_ = ipv4.NewPacketConn(l).SetMulticastLoopback(true) // Windows: the receive side (B120)
		arm(t, l)
		nonce := testNonce(t)
		sendHostLocal(t, lo, group, nonce)
		payload, m, err := readArmedMeta(l)
		var ne net.Error
		if err != nil && runtime.GOOS == "windows" && errors.As(err, &ne) && ne.Timeout() {
			t.Skipf("no looped-back multicast reached the listener on %s: %v", lo.Name, err)
		}
		if err != nil {
			t.Fatalf("read the datagram: %v", err)
		}
		if payload != nonce {
			t.Fatalf("read %q, want the datagram sent, %q", payload, nonce)
		}
		if want := netip.AddrFrom4([4]byte(group.IP.To4())); !m.dstOK || m.dst != want {
			t.Errorf("destination %v (reported %v), want the group %v", m.dst, m.dstOK, want)
		}
		if !m.ifOK || m.ifIndex != lo.Index {
			t.Errorf("arrival index %d (reported %v), want %s's, %d", m.ifIndex, m.ifOK, lo.Name, lo.Index)
		}
	})
}

// wsaCmsg lays one control message out as Winsock's WSA_CMSG macros do on a
// platform whose pointers are ptrSize bytes: cmsg_len (a SIZE_T), cmsg_level,
// cmsg_type, the data at the header rounded up to ptrSize, and the message
// padded to ptrSize.
func wsaCmsg(ptrSize int, level, typ int32, data []byte) []byte {
	align := func(n int) int { return (n + ptrSize - 1) &^ (ptrSize - 1) }
	dataOff := align(ptrSize + 8)
	msgLen := dataOff + len(data)
	b := make([]byte, align(msgLen))
	if ptrSize == 8 {
		binary.LittleEndian.PutUint64(b, uint64(msgLen))
	} else {
		binary.LittleEndian.PutUint32(b, uint32(msgLen))
	}
	binary.LittleEndian.PutUint32(b[ptrSize:], uint32(level))
	binary.LittleEndian.PutUint32(b[ptrSize+4:], uint32(typ))
	copy(b[dataOff:], data)
	return b
}

// inPktinfo is an IN_PKTINFO: the destination, then the interface index.
func inPktinfo(dst [4]byte, ifIndex uint32) []byte {
	b := make([]byte, wsaInPktinfoSize)
	copy(b, dst[:])
	binary.LittleEndian.PutUint32(b[4:], ifIndex)
	return b
}

// TestParseWSAPktinfo pins the Windows parser on every platform, over control
// data laid out as on 64-bit Windows (24 bytes for one IP_PKTINFO, the data
// at offset 16) and as on 32-bit: found alone and after another message,
// with the destination and the arrival index each read from its own place
// in IN_PKTINFO, and refused, never misread, when it is absent or cut short.
// An index of 0 names no interface: the destination stays valid beside it
// and the index does not (wsaDatagramMeta).
func TestParseWSAPktinfo(t *testing.T) {
	tailnet := [4]byte{100, 64, 0, 7}
	for _, ptrSize := range []int{8, 4} {
		pkt := wsaCmsg(ptrSize, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 12))
		noIndex := wsaCmsg(ptrSize, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 0))
		bigIndex := wsaCmsg(ptrSize, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 0x0102_0304))
		other := wsaCmsg(ptrSize, wsaIPProtoIP, 4 /* IP_TTL */, []byte{64, 0, 0, 0})
		ipv6 := wsaCmsg(ptrSize, 41 /* IPPROTO_IPV6 */, wsaIPPktinfo, make([]byte, 20))
		cases := []struct {
			name    string
			oob     []byte
			want    netip.Addr
			wantIdx uint32
		}{
			{"alone", pkt, netip.AddrFrom4(tailnet), 12},
			{"after_another_message", append(append([]byte{}, other...), pkt...), netip.AddrFrom4(tailnet), 12},
			{"an_index_of_zero", noIndex, netip.AddrFrom4(tailnet), 0},
			{"an_index_in_every_byte", bigIndex, netip.AddrFrom4(tailnet), 0x0102_0304},
			{"an_ipv6_message_of_the_same_type", ipv6, netip.Addr{}, 0},
			{"none", nil, netip.Addr{}, 0},
			{"a_header_alone", pkt[:ptrSize+8], netip.Addr{}, 0},
			{"cut_inside_the_data", pkt[:len(pkt)-ptrSize], netip.Addr{}, 0},
			{"a_length_past_the_buffer", func() []byte {
				b := append([]byte{}, pkt...)
				if ptrSize == 8 {
					binary.LittleEndian.PutUint64(b, 1<<40)
				} else {
					binary.LittleEndian.PutUint32(b, 1<<30)
				}
				return b
			}(), netip.Addr{}, 0},
			{"a_length_shorter_than_its_header", func() []byte {
				b := append([]byte{}, pkt...)
				b[0] = 4
				return b
			}(), netip.Addr{}, 0},
		}
		for _, tc := range cases {
			got, idx, ok := parseWSAPktinfo(tc.oob, ptrSize)
			if ok != tc.want.IsValid() || got != tc.want || idx != tc.wantIdx {
				t.Errorf("%d-byte pointers, %s: got %v index %d (%v), want %v index %d",
					ptrSize, tc.name, got, idx, ok, tc.want, tc.wantIdx)
			}
			m := wsaDatagramMeta(tc.oob, ptrSize)
			if m.dstOK != tc.want.IsValid() || m.dst != tc.want {
				t.Errorf("%d-byte pointers, %s: the metadata's destination is %v (%v), want %v", ptrSize, tc.name, m.dst, m.dstOK, tc.want)
			}
			if wantOK := tc.wantIdx != 0; m.ifOK != wantOK || int64(m.ifIndex) != int64(tc.wantIdx) {
				t.Errorf("%d-byte pointers, %s: the metadata's index is %d (%v), want %d (%v)",
					ptrSize, tc.name, m.ifIndex, m.ifOK, tc.wantIdx, wantOK)
			}
		}
	}
	if n := len(wsaCmsg(8, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 1))); n != 24 {
		t.Errorf("one IP_PKTINFO message is %d bytes on 64-bit Windows, want WSA_CMSG_SPACE(8) = 24", n)
	}
	if _, _, ok := parseWSAPktinfo(make([]byte, 64), 2); ok {
		t.Error("a pointer size that is neither 4 nor 8 was parsed")
	}
}

// TestADatagramIndexPastAnIntIsNoInterface pins metaIndex: an index is valid
// only when it is positive and fits an int32, so a reported value no
// interface can have is not wrapped into one that might.
func TestADatagramIndexPastAnIntIsNoInterface(t *testing.T) {
	for _, tc := range []struct {
		idx  uint64
		want int
		ok   bool
	}{
		{0, 0, false},
		{1, 1, true},
		{1<<31 - 1, 1<<31 - 1, true},
		{1 << 31, 0, false},
		{1<<32 - 1, 0, false},
	} {
		if got, ok := metaIndex(tc.idx); got != tc.want || ok != tc.ok {
			t.Errorf("metaIndex(%d) = %d, %v, want %d, %v", tc.idx, got, ok, tc.want, tc.ok)
		}
	}
}
