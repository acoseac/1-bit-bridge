// Fuzz coverage for the LAN-facing, UNAUTHENTICATED parsers.
//
// These sit at the bridge's widest attack surface: SSDP arrives as UDP
// datagrams from anything on the subnet, and the SOAP action header comes off
// an HTTP request the DLNA server answers before any identity is established.
// There is no bearer token in front of either, so unlike the /v1 handlers a
// panic here is reachable by any device on the LAN.
//
// A panic in the SSDP listener is worse than a normal handler panic, too: it
// runs on the read loop rather than under net/http's per-request recovery, so
// the blast radius is the discovery loop rather than one request.
package dlna

import "testing"

func FuzzParseMSearchST(f *testing.F) {
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nST: ssdp:all\r\nMX: 3\r\n\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { _ = ParseMSearchST(b) })
}

func FuzzParseMSearchMX(f *testing.F) {
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nMX: 3\r\n\r\n"))
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nMX: 99999999999999999999\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) { _ = ParseMSearchMX(b) })
}

// FuzzParseDatagramMeta feeds this platform's control-message parser (x/net
// over Linux's and macOS's in_pktinfo, parseWSAPktinfo on Windows) bytes no
// kernel wrote: the control data beside a datagram is the kernel's, but the
// parser must hold whatever it is handed. It must not panic, and what it
// calls reported must be a value the metadata can hold, an IPv4 destination
// and a positive index, with nothing left in a field it does not report.
func FuzzParseDatagramMeta(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 64))
	f.Add(wsaCmsg(8, wsaIPProtoIP, wsaIPPktinfo, inPktinfo([4]byte{192, 168, 1, 20}, 7)))
	f.Fuzz(func(t *testing.T, b []byte) {
		checkDatagramMeta(t, parseDatagramMeta(b))
	})
}

// FuzzParseWSAPktinfo feeds the Windows parser, which runs on every platform,
// bytes no Winsock wrote, at both pointer sizes, under the same invariants.
func FuzzParseWSAPktinfo(f *testing.F) {
	f.Add([]byte{}, true)
	f.Add(wsaCmsg(8, wsaIPProtoIP, wsaIPPktinfo, inPktinfo([4]byte{100, 64, 0, 7}, 12)), true)
	f.Add(wsaCmsg(4, wsaIPProtoIP, wsaIPPktinfo, inPktinfo([4]byte{100, 64, 0, 7}, 0)), false)
	f.Fuzz(func(t *testing.T, b []byte, wide bool) {
		ptrSize := 4
		if wide {
			ptrSize = 8
		}
		dst, _, ok := parseWSAPktinfo(b, ptrSize)
		if ok != dst.IsValid() || (ok && !dst.Is4()) {
			t.Fatalf("parseWSAPktinfo answered %v (%v)", dst, ok)
		}
		checkDatagramMeta(t, wsaDatagramMeta(b, ptrSize))
	})
}

// checkDatagramMeta fails a fuzz case whose metadata holds what no datagram
// can carry.
func checkDatagramMeta(t *testing.T, m datagramMeta) {
	t.Helper()
	if m.dstOK != m.dst.IsValid() || (m.dstOK && !m.dst.Is4()) {
		t.Fatalf("destination %v reported %v", m.dst, m.dstOK)
	}
	if m.ifOK != (m.ifIndex > 0) {
		t.Fatalf("index %d reported %v", m.ifIndex, m.ifOK)
	}
}

func FuzzParseSOAPAction(f *testing.F) {
	f.Add(`"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, s string) { _, _ = ParseSOAPAction(s) })
}
