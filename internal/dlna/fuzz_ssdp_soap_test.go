// Fuzz coverage for the LAN-facing, UNAUTHENTICATED parsers, and for the
// control data the SSDP listener reads beside each datagram.
//
// The datagram and SOAP parsers sit at the bridge's widest attack surface:
// SSDP arrives as UDP datagrams from anything on the subnet, and the SOAP
// action header comes off an HTTP request the DLNA server answers before any
// identity is established. There is no bearer token in front of either, so
// unlike the /v1 handlers a panic in one is reachable by any device on the
// LAN.
//
// The control data is not: the destination and the arrival index beside a
// datagram (FuzzParseDatagramMeta, FuzzParseWSAPktinfo) are ancillary bytes
// the kernel writes, and a network peer cannot supply them. Their parsers are
// fuzzed because they must hold whatever they are handed all the same
// (backlog B610).
//
// A panic on the SSDP read loop is worse than a handler panic, too: the loop
// runs on a goroutine of its own with no recovery (SSDPSubsystem.readLoop),
// not under net/http's per-request recovery, so a panic there ends the
// process, the whole of bridge serve, rather than one request.
package dlna

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

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

// FuzzParseDatagramMeta feeds this platform's control-message parser (x/net,
// under a recover of the parser's own, over Linux's and macOS's in_pktinfo,
// and parseWSAPktinfo on Windows) bytes no kernel wrote: the control data
// beside a datagram is the kernel's, but the parser must hold whatever it is
// handed. It must not panic, and what it calls reported must be a value the
// metadata can hold, an IPv4 destination and a positive index, with nothing
// left in a field it does not report. Its committed seeds are 64-bit Linux's
// control data, the nightly's crash of 2026-10-24 to 31 (B610) among them;
// TestTheDatagramMetaSeedsAreLinuxsControlData says what each decodes to.
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

// fuzzDispatchTable is a dispatch table FuzzSSDPDispatch plans over, built
// white-box with no socket and no goroutine: the MediaServer on en0 (index
// 2) and on en1 (index 3), whose listener cannot report destinations, and a
// strict device on en0 with two address aliases and on en2 (index 4), whose
// listener reports both and is not confined (which Register refuses; the
// dispatch's own check is what this exercises).
func fuzzDispatchTable() (*ssdpTable, []*ssdpListener) {
	l0 := &ssdpListener{key: ssdpListenerKey{index: 2, name: "en0"}, setup: fullSetup}
	l1 := &ssdpListener{key: ssdpListenerKey{index: 3, name: "en1"},
		setup: ssdpListenerSetup{caps: SSDPListenerCaps{ArrivalReporting: true, ReceiveConfined: true}}}
	l2 := &ssdpListener{key: ssdpListenerKey{index: 4, name: "en2"},
		setup: ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true}}}
	ms := &SSDPHandle{udn: msUDN, targets: NotifyTargetsFor(msUDN), admission: ssdpServedAdmission{served: fakeServed()}, log: discardLogger()}
	ms.active = []*ssdpEndpoint{
		{h: ms, key: l0.key, listener: l0, spec: SSDPEndpoint{Location: msOnEn0}},
		{h: ms, key: l1.key, listener: l1, spec: SSDPEndpoint{Location: msOnEn1}},
	}
	src := &SSDPHandle{udn: srcUDN, targets: sourceTargets(srcUDN), admission: SSDPStrictAdmission{Source: onLink}, log: discardLogger()}
	src.active = []*ssdpEndpoint{
		{h: src, key: l0.key, listener: l0, spec: SSDPEndpoint{Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}},
		{h: src, key: l0.key, listener: l0, spec: SSDPEndpoint{Location: "http://192.168.1.21:7791/openhome/description.xml",
			Local: netip.MustParsePrefix("192.168.1.21/24")}},
		{h: src, key: l0.key, listener: l0, spec: SSDPEndpoint{Location: "http://10.2.0.20:7792/openhome/description.xml",
			Local: netip.MustParsePrefix("10.2.0.20/24")}},
		{h: src, key: l2.key, listener: l2, spec: SSDPEndpoint{Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}},
	}
	return buildTable([]*SSDPHandle{ms, src}), []*ssdpListener{l0, l1, l2}
}

// FuzzSSDPDispatch feeds the dispatch's plan (B470 PR 5a1) a datagram and
// metadata of any shape, read by any of three listeners, and holds every
// answer it decides on to the registration rules: at most one per
// registration, from one of its own active endpoints, with targets of its
// own that the ST matched (only its UDN's, for a UUID); a strict one only
// with a reported destination and arrival, on its own interface, from an
// on-link source, and for a group search only on a listener that reports
// both and is confined, from an endpoint on it; and a MediaServer group
// answer only from its endpoint on the listener that read it.
func FuzzSSDPDispatch(f *testing.F) {
	f.Add([]byte(search("ssdp:all")), []byte{239, 255, 255, 250}, true, uint8(2), true, []byte{10, 2, 0, 30}, uint8(0))
	f.Add([]byte(search("ssdp:all")), []byte{239, 255, 255, 250}, true, uint8(2), true, []byte{192, 168, 1, 30}, uint8(128))
	f.Add([]byte(search("ssdp:all")), []byte{239, 255, 255, 250}, true, uint8(2), true, []byte{192, 168, 1, 30}, uint8(0))
	f.Add([]byte(search(srcUDN)), []byte{192, 168, 1, 21}, true, uint8(2), true, []byte{192, 168, 1, 30}, uint8(2))
	f.Add([]byte(search("upnp:rootdevice")), []byte{100, 64, 0, 7}, true, uint8(9), true, []byte{100, 64, 0, 9}, uint8(1))
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nMX: 3\r\nST: "+msUDN+"\r\n\r\n"), []byte{}, false, uint8(0), false, []byte{1, 2, 3, 4}, uint8(0))
	f.Fuzz(func(t *testing.T, pkt, dst []byte, dstOK bool, idx uint8, idxOK bool, src []byte, which uint8) {
		table, listeners := fuzzDispatchTable()
		if which&128 != 0 {
			slices.Reverse(table.entries[1].eps)
		}
		l := listeners[int(which&127)%len(listeners)]
		var m datagramMeta
		if a, ok := netip.AddrFromSlice(dst); ok && dstOK {
			m.dst, m.dstOK = a.Unmap(), true
		}
		if idxOK && idx > 0 {
			m.ifIndex, m.ifOK = int(idx), true
		}
		from := netip.AddrPortFrom(netip.IPv4Unspecified(), 50000)
		if a, ok := netip.AddrFromSlice(src); ok {
			from = netip.AddrPortFrom(a.Unmap(), 50000)
		}
		mx, plan := table.plan(l, pkt, from, m)
		if mx < 0 || mx > mxResponseCeilingSeconds {
			t.Fatalf("MX %d out of [0, %d]", mx, mxResponseCeilingSeconds)
		}
		st := ParseMSearchST(pkt)
		seen := map[*SSDPHandle]bool{}
		for _, p := range plan {
			checkPlanned(t, table, l, st, m, from, p, seen)
		}
		// Completeness as well as refusal: a later disjoint alias cannot
		// disappear behind the first, and overlap still selects only one.
		if st != "" && m.dstOK && m.dst == ssdpGroupAddr && m.ifOK && m.ifIndex == l.key.index &&
			l.setup.caps == fullSetup.caps {
			e := table.entries[1]
			matched := false
			for _, tg := range e.h.targets {
				if st == "ssdp:all" || tg.NT == st {
					matched = true
				}
			}
			var expected *ssdpEndpoint
			if matched {
				for _, ep := range e.eps {
					if ep.listener == l && ep.spec.Local.Contains(from.Addr()) {
						expected = ep
						break
					}
				}
			}
			var got *ssdpEndpoint
			for _, p := range plan {
				if p.h == e.h {
					got = p.ep
				}
			}
			if got != expected {
				t.Fatalf("strict alias selection got %v want %v", got, expected)
			}
		}
	})
}

// checkPlanned fails a fuzz case whose answer p breaks a registration rule
// (FuzzSSDPDispatch).
func checkPlanned(t *testing.T, table *ssdpTable, l *ssdpListener, st string, m datagramMeta, from netip.AddrPort,
	p ssdpPlanned, seen map[*SSDPHandle]bool) {
	t.Helper()
	if seen[p.h] {
		t.Fatalf("%s answers twice in one dispatch", p.h.udn)
	}
	seen[p.h] = true
	i := slices.IndexFunc(table.entries, func(e ssdpTableEntry) bool { return e.h == p.h })
	if i < 0 || !slices.Contains(table.entries[i].eps, p.ep) {
		t.Fatalf("%s answers from an endpoint that is not its own", p.h.udn)
	}
	if len(p.targets) == 0 {
		t.Fatalf("%s answers with no target", p.h.udn)
	}
	for _, tg := range p.targets {
		if !slices.Contains(p.h.targets, tg) || (st != "ssdp:all" && tg.NT != st) {
			t.Fatalf("%s answers %s with target %+v", p.h.udn, st, tg)
		}
	}
	if strings.HasPrefix(st, "uuid:") && st != p.h.udn {
		t.Fatalf("a search for %s is answered by %s", st, p.h.udn)
	}
	toGroup := m.dstOK && m.dst == ssdpGroupAddr
	if _, strict := p.h.admission.(SSDPStrictAdmission); strict {
		caps := l.setup.caps
		switch {
		case !m.dstOK || !m.ifOK || !caps.DestinationReporting || !caps.ArrivalReporting:
			t.Fatalf("a strict answer without a reported destination and arrival on a listener reporting both: %+v %+v", m, caps)
		case !p.ep.spec.Local.Contains(from.Addr()):
			t.Fatalf("a strict answer to an off-link source %v", from)
		case toGroup && (!caps.ReceiveConfined || m.ifIndex != l.key.index || p.ep.listener != l):
			t.Fatalf("a strict group answer from %s for a search that arrived on %d, read by %s", p.ep.key.name, m.ifIndex, l.key.name)
		case !toGroup && (p.ep.spec.Local.Addr() != m.dst || p.ep.key.index != m.ifIndex):
			t.Fatalf("a strict unicast answer from %s for a search sent to %v on %d", p.ep.spec.Location, m.dst, m.ifIndex)
		}
		return
	}
	if toGroup && l.setup.caps.DestinationReporting && p.ep.listener != l {
		t.Fatalf("a MediaServer group answer from %s for a search read by %s", p.ep.key.name, l.key.name)
	}
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
