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
	buf := make([]byte, 2048)
	oob := make([]byte, datagramDstSpace)
	_ = l.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, oobn, _, _, err := l.ReadMsgUDP(buf, oob)
	if err != nil {
		return "", netip.Addr{}, false, err
	}
	dst, ok = parseDatagramDst(oob[:oobn])
	return string(buf[:n]), dst, ok, nil
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
// at offset 16) and as on 32-bit: found alone and after another message, and
// refused, never misread, when it is absent or cut short.
func TestParseWSAPktinfo(t *testing.T) {
	tailnet := [4]byte{100, 64, 0, 7}
	for _, ptrSize := range []int{8, 4} {
		pkt := wsaCmsg(ptrSize, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 12))
		other := wsaCmsg(ptrSize, wsaIPProtoIP, 4 /* IP_TTL */, []byte{64, 0, 0, 0})
		ipv6 := wsaCmsg(ptrSize, 41 /* IPPROTO_IPV6 */, wsaIPPktinfo, make([]byte, 20))
		cases := []struct {
			name string
			oob  []byte
			want netip.Addr
		}{
			{"alone", pkt, netip.AddrFrom4(tailnet)},
			{"after_another_message", append(append([]byte{}, other...), pkt...), netip.AddrFrom4(tailnet)},
			{"an_ipv6_message_of_the_same_type", ipv6, netip.Addr{}},
			{"none", nil, netip.Addr{}},
			{"a_header_alone", pkt[:ptrSize+8], netip.Addr{}},
			{"cut_inside_the_data", pkt[:len(pkt)-ptrSize], netip.Addr{}},
			{"a_length_past_the_buffer", func() []byte {
				b := append([]byte{}, pkt...)
				if ptrSize == 8 {
					binary.LittleEndian.PutUint64(b, 1<<40)
				} else {
					binary.LittleEndian.PutUint32(b, 1<<30)
				}
				return b
			}(), netip.Addr{}},
			{"a_length_shorter_than_its_header", func() []byte {
				b := append([]byte{}, pkt...)
				b[0] = 4
				return b
			}(), netip.Addr{}},
		}
		for _, tc := range cases {
			got, ok := parseWSAPktinfo(tc.oob, ptrSize)
			if ok != tc.want.IsValid() || got != tc.want {
				t.Errorf("%d-byte pointers, %s: got %v (%v), want %v", ptrSize, tc.name, got, ok, tc.want)
			}
		}
	}
	if n := len(wsaCmsg(8, wsaIPProtoIP, wsaIPPktinfo, inPktinfo(tailnet, 1))); n != 24 {
		t.Errorf("one IP_PKTINFO message is %d bytes on 64-bit Windows, want WSA_CMSG_SPACE(8) = 24", n)
	}
	if _, ok := parseWSAPktinfo(make([]byte, 64), 2); ok {
		t.Error("a pointer size that is neither 4 nor 8 was parsed")
	}
}
