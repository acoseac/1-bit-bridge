package dlna

import (
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// The synthetic host the dispatch tests share: en0 (index 2) holds
// 192.168.1.20/24, en1 (index 3) 10.0.0.5/8, and en2 (index 4)
// 172.20.0.9/16, where only the test device advertises. The MediaServer's
// served addresses are fakeServed's (en0, en1, loopback).
var (
	en0 = synthIface(2, "en0")
	en1 = synthIface(3, "en1")
	en2 = synthIface(4, "en2")
)

const (
	msUDN     = "uuid:00000000-0000-4000-8000-0000000000aa"
	srcUDN    = "uuid:00000000-0000-4000-8000-0000000000bb"
	msOnEn0   = "http://192.168.1.20:7790/dlna/description.xml"
	msOnEn1   = "http://10.0.0.5:7790/dlna/description.xml"
	srcOnEn0  = "http://192.168.1.20:7791/openhome/description.xml"
	srcOnEn2  = "http://172.20.0.9:7791/openhome/description.xml"
	searchSrc = "192.168.1.30:50000"
)

// sendTo delivers datagram pkt with meta to iface's open listener, from src,
// and waits until it has been dispatched and answered.
func sendTo(t *testing.T, sub *SSDPSubsystem, n *fakeNet, iface, pkt, src string, meta datagramMeta) []answer {
	t.Helper()
	from := len(n.snapshot())
	n.openListener(t, iface).deliver(pkt, netip.MustParseAddrPort(src), meta)
	settle(t, sub, n)
	return replies(n, from)
}

// sortedUSNs is each answer as "LOCATION USN", sorted: two registrations'
// answers are written by goroutines of their own, in no fixed order.
func sortedUSNs(as []answer) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.location+" "+a.usn)
	}
	slices.Sort(out)
	return out
}

func want(lines ...string) []string {
	out := append([]string(nil), lines...)
	slices.Sort(out)
	return out
}

// twoDevices registers the MediaServer on en0 and the test device on en0
// (192.168.1.20/24) over a fake network.
func twoDevices(t *testing.T) (*SSDPSubsystem, *fakeNet) {
	t.Helper()
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	return sub, n
}

// TestTwoDevicesAnswerEachSearchOnceEach pins B470 PR 5a1's dispatch: one
// datagram, parsed once, is matched by each registration on its own, and
// each admitted registration answers it with one matched set: a rootdevice
// search gets two rootdevice answers, ssdp:all every target of each, a UUID
// only its device, and a type both advertise one answer from each, never a
// first-match return.
func TestTwoDevicesAnswerEachSearchOnceEach(t *testing.T) {
	sub, n := twoDevices(t)
	n.mu.Lock()
	listens := n.listens
	n.mu.Unlock()
	if listens != 1 {
		t.Fatalf("two devices on en0 opened %d listeners, want the one they share", listens)
	}
	ms, src := NotifyTargetsFor(msUDN), sourceTargets(srcUDN)
	all := []string{}
	for _, tg := range ms {
		all = append(all, msOnEn0+" "+tg.USN)
	}
	for _, tg := range src {
		all = append(all, srcOnEn0+" "+tg.USN)
	}
	for _, tc := range []struct {
		st   string
		want []string
	}{
		{"upnp:rootdevice", want(msOnEn0+" "+ms[0].USN, srcOnEn0+" "+src[0].USN)},
		{"ssdp:all", want(all...)},
		{msUDN, want(msOnEn0 + " " + msUDN)},
		{srcUDN, want(srcOnEn0 + " " + srcUDN)},
		{"urn:schemas-upnp-org:device:MediaServer:1", want(msOnEn0 + " " + ms[2].USN)},
		{"urn:av-openhome-org:device:Source:1", want(srcOnEn0 + " " + src[2].USN)},
		{"urn:schemas-upnp-org:service:ConnectionManager:1", want(msOnEn0+" "+ms[4].USN, srcOnEn0+" "+src[3].USN)},
		{"urn:schemas-upnp-org:device:MediaRenderer:1", nil},
	} {
		got := sendTo(t, sub, n, "en0", search(tc.st), searchSrc, groupMeta(2))
		if g := sortedUSNs(got); !slices.Equal(g, tc.want) {
			t.Errorf("a group search for %s was answered%s\nwant %q", tc.st, describe(got), tc.want)
		}
		for _, a := range got {
			if a.to != searchSrc {
				t.Errorf("an answer went to %s, not the searcher %s", a.to, searchSrc)
			}
		}
	}
}

// TestDisjointInterfacesAnswerOnlyTheirOwnGroupSearches pins that a group
// search is dispatched to the registrations with an endpoint on the
// listener that read it, and to no other: the MediaServer on en0 and the
// test device on en2 each answer their own interface's searches.
func TestDisjointInterfacesAnswerOnlyTheirOwnGroupSearches(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}))
	if got := sortedUSNs(sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, groupMeta(2))); !slices.Equal(got, want(msOnEn0+" "+msUDN+"::upnp:rootdevice")) {
		t.Errorf("en0's group search was answered %q, want the MediaServer alone", got)
	}
	if got := sortedUSNs(sendTo(t, sub, n, "en2", search("upnp:rootdevice"), "172.20.0.50:50000", groupMeta(4))); !slices.Equal(got, want(srcOnEn2+" "+srcUDN+"::upnp:rootdevice")) {
		t.Errorf("en2's group search was answered %q, want the test device alone", got)
	}
}

// TestAUnicastIsDispatchedToEveryRegistrationWhicheverListenerReadIt pins
// that a unicast whose destination is reported goes to every registration,
// whichever listener the kernel handed it to: the MediaServer, with no
// endpoint on en2's listener, answers from its first endpoint, and the test
// device answers one sent to its own address on its own interface, read by
// the MediaServer's listener.
func TestAUnicastIsDispatchedToEveryRegistrationWhicheverListenerReadIt(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}))
	got := sendTo(t, sub, n, "en2", search("upnp:rootdevice"), searchSrc, unicastMeta("192.168.1.20", 2))
	if g := sortedUSNs(got); !slices.Equal(g, want(msOnEn0+" "+msUDN+"::upnp:rootdevice")) {
		t.Errorf("a unicast to the MediaServer's address read on en2 was answered%s", describe(got))
	}
	got = sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "172.20.0.50:50000", unicastMeta("172.20.0.9", 4))
	if g := sortedUSNs(got); !slices.Equal(g, want(srcOnEn2+" "+srcUDN+"::upnp:rootdevice")) {
		t.Errorf("a unicast to the test device's address read on en0 was answered%s", describe(got))
	}
}

// TestTheMediaServerAnswersAUnicastFromTheListenerThatReadIt pins backlog
// B119 as B470 PR 5a1 keeps it: with the MediaServer alone, a unicast is
// answered with the LOCATION of the endpoint on the listener the kernel
// handed it to, whichever served address it was sent to, never chosen by
// the destination.
func TestTheMediaServerAnswersAUnicastFromTheListenerThatReadIt(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en0, Location: msOnEn0}, SSDPEndpoint{Interface: en1, Location: msOnEn1}))
	for _, tc := range []struct{ listener, dst, want string }{
		{"en0", "10.0.0.5", msOnEn0},
		{"en1", "192.168.1.20", msOnEn1},
		{"en0", "192.168.1.20", msOnEn0},
	} {
		got := sendTo(t, sub, n, tc.listener, search("upnp:rootdevice"), searchSrc, unicastMeta(tc.dst, 0))
		if len(got) != 1 || got[0].location != tc.want {
			t.Errorf("a unicast to %s read by %s's listener was answered%s, want once from %s", tc.dst, tc.listener, describe(got), tc.want)
		}
	}
}

// TestTheMediaServerKeepsItsUnconfinedFallbackButNotForAnotherDevicesInterface
// pins the one refusal B470 PR 5a1 adds to the MediaServer, which no
// MediaServer alone can meet (the review's finding on the confinement
// fallback). A listener that could not be confined hears the group
// datagrams that arrive on other interfaces where something on the host
// joined, and the MediaServer answers them with its own LOCATION, as it
// always has; but not one whose arrival index names an interface where only
// another registration of this subsystem has an endpoint.
func TestTheMediaServerKeepsItsUnconfinedFallbackButNotForAnotherDevicesInterface(t *testing.T) {
	n := newFakeNet()
	n.setups["en0"] = ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true}, confineErr: errInjected}
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	foreign := func() []answer {
		return sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "172.20.0.50:50000", groupMeta(4))
	}
	if got := foreign(); len(got) != 1 || got[0].location != msOnEn0 {
		t.Fatalf("with the MediaServer alone, en2's group search on en0's unconfined listener was answered%s, "+
			"want the fallback's one answer from en0", describe(got))
	}
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}))
	if got := foreign(); len(got) != 0 {
		t.Errorf("en2's group search on en0's unconfined listener, en2 joined only by the test device, was answered%s", describe(got))
	}
	if got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "172.21.0.50:50000", groupMeta(9)); len(got) != 1 || got[0].location != msOnEn0 {
		t.Errorf("a group search from an interface no registration joined was answered%s, want the fallback's", describe(got))
	}
}

// TestOneAnswerSetPerRegistrationPerDispatch pins that address aliases do
// not multiply answers: a registration with two endpoints on one interface
// answers a group search once, from the first, and a unicast from the one
// whose address it was sent to.
func TestOneAnswerSetPerRegistrationPerDispatch(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	const second = "http://192.168.1.21:7791/openhome/description.xml"
	mustRegister(t, sub, strictReg(srcUDN, 4,
		SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")},
		SSDPEndpoint{Interface: en0, Location: second, Local: netip.MustParsePrefix("192.168.1.21/24")}))
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en0, Location: msOnEn0}, SSDPEndpoint{Interface: en0, Location: "http://192.168.1.20:7792/dlna/description.xml"}))
	got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, groupMeta(2))
	if g := sortedUSNs(got); !slices.Equal(g, want(srcOnEn0+" "+srcUDN+"::upnp:rootdevice", msOnEn0+" "+msUDN+"::upnp:rootdevice")) {
		t.Errorf("a group search was answered%s, want one answer per device, each from its first endpoint", describe(got))
	}
	got = sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, unicastMeta("192.168.1.21", 2))
	if len(got) != 1 || got[0].location != second {
		t.Errorf("a unicast to the second alias was answered%s, want once from %s", describe(got), second)
	}
}

// TestTheMediaServerAdmitsByDestinationAlone pins that B470 PR 5a1 adds no
// arrival test and no source test to the MediaServer: a unicast to a served
// address is answered whatever interface it arrived on (one it does not
// advertise on included) and whatever its source, loopback is served, and
// the tailnet, an unreported destination and an address nothing serves are
// refused.
func TestTheMediaServerAdmitsByDestinationAlone(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	for _, tc := range []struct {
		name, src string
		meta      datagramMeta
		answered  bool
	}{
		{"served_off_its_interface", searchSrc, unicastMeta("10.0.0.5", 3), true},
		{"loopback", "127.0.0.1:50000", unicastMeta("127.0.0.1", 1), true},
		{"a_public_source", "8.8.8.8:50000", unicastMeta("192.168.1.20", 2), true},
		{"no_arrival_reported", searchSrc, unicastMeta("192.168.1.20", 0), true},
		{"the_tailnet", "100.64.0.9:50000", unicastMeta("100.64.0.7", 9), false},
		{"unserved", searchSrc, unicastMeta("192.168.9.9", 2), false},
		{"destination_unreported", searchSrc, datagramMeta{ifIndex: 2, ifOK: true}, false},
	} {
		got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), tc.src, tc.meta)
		if answered := len(got) == 1 && got[0].location == msOnEn0; answered != tc.answered || len(got) > 1 {
			t.Errorf("%s: answered%s, want answered %v", tc.name, describe(got), tc.answered)
		}
	}
}

// TestTheMediaServerServesAnAddressThatAppearsAfterItStarted pins that the
// registration admits by the server's own servedAddrs, which refreshes: an
// address that comes up on a member after the start is answered once the
// snapshot has been rebuilt (a second after its first refusal), with no
// re-registration.
func TestTheMediaServerServesAnAddressThatAppearsAfterItStarted(t *testing.T) {
	clock := newTestClock()
	host := newTestHost(lan("en0", "192.168.1.20"))
	served := host.served(clock.now)
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, served, SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	if got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, unicastMeta("192.168.1.77", 2)); len(got) != 0 {
		t.Fatalf("an address the host does not hold yet was answered%s", describe(got))
	}
	host.set(lan("en0", "192.168.1.20", "192.168.1.77"))
	clock.advance(2 * servedMissRebuild)
	if got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, unicastMeta("192.168.1.77", 2)); len(got) != 1 {
		t.Errorf("the address, once the host holds it, was answered%s, want once", describe(got))
	}
}

// TestAStrictRegistrationAnswersOnlyWhatItCanProve pins SSDPStrictAdmission:
// a group search that arrived, by its reported index, on its listener's
// interface; a unicast sent to an endpoint's Local address on the interface
// it arrived on; and either one only when the source hook admits the
// searcher. The hook is handed the endpoint it would be answered from and
// the datagram as received.
func TestAStrictRegistrationAnswersOnlyWhatItCanProve(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	var mu sync.Mutex
	var seen []SSDPSearch
	reg := strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")})
	reg.Admission = SSDPStrictAdmission{Source: func(s SSDPSearch) bool {
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
		return onLink(s)
	}}
	mustRegister(t, sub, reg)
	for _, tc := range []struct {
		name, src string
		meta      datagramMeta
		answered  bool
	}{
		{"group_on_link", searchSrc, groupMeta(2), true},
		{"group_off_link", "10.9.9.9:50000", groupMeta(2), false},
		{"group_arrived_elsewhere", searchSrc, groupMeta(3), false},
		{"group_arrival_unreported", searchSrc, datagramMeta{dst: ssdpGroupAddr, dstOK: true}, false},
		{"destination_unreported", searchSrc, datagramMeta{ifIndex: 2, ifOK: true}, false},
		{"unicast_to_its_address", searchSrc, unicastMeta("192.168.1.20", 2), true},
		{"unicast_arrived_elsewhere", searchSrc, unicastMeta("192.168.1.20", 3), false},
		{"unicast_to_another_address", searchSrc, unicastMeta("192.168.1.21", 2), false},
		{"unicast_arrival_unreported", searchSrc, unicastMeta("192.168.1.20", 0), false},
	} {
		got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), tc.src, tc.meta)
		if answered := len(got) == 1; answered != tc.answered || len(got) > 1 {
			t.Errorf("%s: answered%s, want answered %v", tc.name, describe(got), tc.answered)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the source hook was never asked")
	}
	first := seen[0]
	wantEndpoint := SSDPEndpointSnapshot{Interface: SSDPInterface{Index: 2, Name: "en0"}, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}
	if first.Endpoint != wantEndpoint {
		t.Errorf("the hook was handed endpoint %+v, want %+v", first.Endpoint, wantEndpoint)
	}
	wantDatagram := SSDPDatagram{Source: netip.MustParseAddrPort(searchSrc), Destination: ssdpGroupAddr, DestinationValid: true,
		ArrivalIndex: 2, ArrivalValid: true, Listener: SSDPInterface{Index: 2, Name: "en0"}, ListenerCaps: fullSetup.caps}
	if first.Datagram != wantDatagram {
		t.Errorf("the hook was handed datagram %+v, want %+v", first.Datagram, wantDatagram)
	}
}

// TestAStrictRegistrationLeavesOutAnInterfaceTheMediaServerServes pins that a
// strict registration answers only on its own endpoints' interfaces: en1,
// DLNA-eligible and served by the MediaServer, is not the test device's.
func TestAStrictRegistrationLeavesOutAnInterfaceTheMediaServerServes(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en0, Location: msOnEn0}, SSDPEndpoint{Interface: en1, Location: msOnEn1}))
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	for _, tc := range []struct {
		name string
		meta datagramMeta
	}{
		{"group", groupMeta(3)},
		{"unicast", unicastMeta("10.0.0.5", 3)},
	} {
		got := sendTo(t, sub, n, "en1", search("upnp:rootdevice"), "10.0.0.50:50000", tc.meta)
		if len(got) != 1 || got[0].location != msOnEn1 {
			t.Errorf("%s on en1: answered%s, want the MediaServer alone, from en1", tc.name, describe(got))
		}
	}
}

// TestNothingButAMatchedSearchTakesAResponderSlot pins that a slot is taken
// only after the datagram is parsed, matched, admitted and its one endpoint
// chosen: a NOTIFY, a search for another device's UUID, a type nobody
// advertises, garbage, and a search the admission refuses take none, and a
// matched one takes one, of its own registration's endpoint.
func TestNothingButAMatchedSearchTakesAResponderSlot(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	var mu sync.Mutex
	var taken []string
	sub.slotTaken = func(ep *ssdpEndpoint) {
		mu.Lock()
		taken = append(taken, ep.h.udn)
		mu.Unlock()
	}
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustRegister(t, sub, strictReg(srcUDN, 1, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	for _, pkt := range []string{
		"NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\n\r\n",
		search("urn:schemas-upnp-org:device:MediaRenderer:1"),
		"\x00\x01garbage",
		"M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n\r\n",
	} {
		sendTo(t, sub, n, "en0", pkt, searchSrc, groupMeta(2))
	}
	sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "100.64.0.9:50000", unicastMeta("100.64.0.7", 2))
	mu.Lock()
	if len(taken) != 0 {
		t.Errorf("datagrams nobody answers took slots of %q", taken)
	}
	mu.Unlock()
	sendTo(t, sub, n, "en0", search(msUDN), searchSrc, groupMeta(2))
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(taken, []string{msUDN}) {
		t.Errorf("a search for the MediaServer's UUID took slots of %q, want its own one", taken)
	}
}

// TestAFullPoolDropsOnlyItsOwnEndpointsAnswer pins the responder budget: 16
// slots per MediaServer endpoint, taken across an answer's delay; a 17th
// search on that endpoint is dropped while one on the MediaServer's other
// endpoint, and one for another registration on the full endpoint's own
// listener, are answered.
func TestAFullPoolDropsOnlyItsOwnEndpointsAnswer(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var mu sync.Mutex
	hold := map[string]bool{}
	sub.replyDelay = func(time.Duration) time.Duration { return 0 }
	n.onDial = func(to netip.AddrPort) {
		mu.Lock()
		h := hold[to.String()]
		mu.Unlock()
		if h {
			<-release
		}
	}
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en0, Location: msOnEn0}, SSDPEndpoint{Interface: en1, Location: msOnEn1}))
	mustRegister(t, sub, strictReg(srcUDN, 2, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	const holder = "192.168.1.40:40000"
	mu.Lock()
	hold[holder] = true
	mu.Unlock()
	l := n.openListener(t, "en0")
	for range maxConcurrentMSearchResponders {
		l.deliver(search(msUDN), netip.MustParseAddrPort(holder), groupMeta(2))
	}
	waitFor(t, "the en0 endpoint's slots to fill", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		return len(sub.handles[0].active[0].pool) == maxConcurrentMSearchResponders
	})
	from := len(n.snapshot())
	l.deliver(search(msUDN), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	waitFor(t, "en0 to dispatch", l.idle)
	n.openListener(t, "en1").deliver(search(msUDN), netip.MustParseAddrPort("10.0.0.50:50000"), groupMeta(3))
	l.deliver(search(srcUDN), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	waitFor(t, "the two other answers", func() bool { return len(replies(n, from)) >= 2 })
	time.Sleep(50 * time.Millisecond)
	got := sortedUSNs(replies(n, from))
	if !slices.Equal(got, want(msOnEn1+" "+msUDN, srcOnEn0+" "+srcUDN)) {
		t.Errorf("with en0's MediaServer slots full, answered %q: want en1's and the test device's, and en0's search dropped", got)
	}
	close(release)
	settle(t, sub, n)
	var held, dropped int
	for _, a := range replies(n, from) {
		switch {
		case a.to == holder:
			held++
		case a.to == searchSrc && a.location == msOnEn0:
			dropped++
		}
	}
	if held != maxConcurrentMSearchResponders || dropped != 0 {
		t.Errorf("once released, the held answers wrote %d replies (want %d, one each) and the dropped search got %d (want 0)",
			held, maxConcurrentMSearchResponders, dropped)
	}
}
