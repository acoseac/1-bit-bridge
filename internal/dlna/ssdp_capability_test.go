package dlna

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEachListenerCapabilityFailsOnItsOwn pins B470 PR 5a1's capability
// contract, one capability at a time: a listener that cannot report
// destinations, arrivals, or be confined still opens; the MediaServer runs on
// it with the problem reported, as before, and keeps its destination guard
// whenever destinations are reported (an arrival report that fails cannot
// disable it); a strict endpoint on it is refused, and the refusal leaves
// the listener, which the MediaServer shares, open.
func TestEachListenerCapabilityFailsOnItsOwn(t *testing.T) {
	src := SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}
	for _, tc := range []struct {
		name  string
		setup ssdpListenerSetup
		// tailnetAnswered is whether a unicast to the tailnet address is
		// answered: only on a listener that cannot report destinations,
		// as before backlog B48.
		tailnetAnswered bool
	}{
		{"no_destination_reporting", ssdpListenerSetup{caps: SSDPListenerCaps{ArrivalReporting: true, ReceiveConfined: true}, dstErr: errInjected}, true},
		{"no_arrival_reporting", ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ReceiveConfined: true}, arrivalErr: errInjected}, false},
		{"no_confinement", ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true}, confineErr: errInjected}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet()
			n.setups["en0"] = tc.setup
			sub := fakeSubsystem(t, n)
			_, res := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			if r := res.Active[0]; r.Listener != tc.setup.caps || !slices.ContainsFunc(r.Problems, func(e error) bool { return errors.Is(e, errInjected) }) {
				t.Errorf("the MediaServer's endpoint reports %+v with problems %v, want %+v and the failure", r.Listener, r.Problems, tc.setup.caps)
			}
			_, sres, err := sub.Register(context.Background(), strictReg(srcUDN, 4, src))
			mustErr(t, "a strict registration on that listener", err, ErrSSDPNoEndpoint)
			if len(sres.Failed) != 1 || !errors.Is(sres.Failed[0].Err, ErrSSDPStrictCapability) {
				t.Errorf("the strict endpoint failed with %+v, want ErrSSDPStrictCapability", sres.Failed)
			}
			if l, _, _ := n.openSockets(); l != 1 {
				t.Errorf("%d listeners open after the strict refusal, want the MediaServer's one", l)
			}
			if got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, unicastMeta("192.168.1.20", 2)); len(got) != 1 {
				t.Errorf("a unicast to the MediaServer's address was answered%s, want once", describe(got))
			}
			got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "100.64.0.9:50000", unicastMeta("100.64.0.7", 9))
			if answered := len(got) > 0; answered != tc.tailnetAnswered {
				t.Errorf("a unicast to the tailnet address was answered%s, want answered %v", describe(got), tc.tailnetAnswered)
			}
		})
	}
}

// TestAStrictUnicastNeedsReportingNotConfinementOfTheListenerThatReadIt pins
// that which wildcard socket a unicast lands on is the kernel's choice: a
// strict registration answers one read by another registration's listener
// when that listener reports the destination and the arrival, confined or
// not (confinement is a matter of group datagrams), and refuses one read by a
// listener that cannot report the arrival.
func TestAStrictUnicastNeedsReportingNotConfinementOfTheListenerThatReadIt(t *testing.T) {
	src := SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}
	for _, tc := range []struct {
		name     string
		setup    ssdpListenerSetup
		answered bool
	}{
		{"unconfined", ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ArrivalReporting: true}, confineErr: errInjected}, true},
		{"no_arrival_reporting", ssdpListenerSetup{caps: SSDPListenerCaps{DestinationReporting: true, ReceiveConfined: true}, arrivalErr: errInjected}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet()
			n.setups["en0"] = tc.setup
			sub := fakeSubsystem(t, n)
			mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			mustRegister(t, sub, strictReg(srcUDN, 4, src))
			got := sendTo(t, sub, n, "en0", search(srcUDN), "172.20.0.50:50000", unicastMeta("172.20.0.9", 4))
			if answered := len(got) == 1 && got[0].location == srcOnEn2; answered != tc.answered || len(got) > 1 {
				t.Errorf("a unicast to the test device read by en0's listener was answered%s, want answered %v", describe(got), tc.answered)
			}
		})
	}
}

// TestASenderThatCannotBePinned pins the outgoing half, independent of the
// listener's: the MediaServer's endpoint starts with a Warn and announces on
// the OS default, as it always has; a strict one does not start, announces
// nothing, and answers nothing.
func TestASenderThatCannotBePinned(t *testing.T) {
	n := newFakeNet()
	n.pinErr["en0"] = errInjected
	sub := fakeSubsystem(t, n)
	var log syncBuffer
	reg := legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0})
	reg.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, res := mustRegister(t, sub, reg)
	if res.Active[0].SenderPinned {
		t.Error("the MediaServer's endpoint reports its sender pinned")
	}
	if !strings.Contains(log.String(), "SSDP multicast interface bind failed (falling back to OS default)") {
		t.Errorf("no Warn for the pin:\n%s", log.String())
	}
	if got := notifies(n, 0, "ssdp:alive"); len(got) != len(NotifyTargetsFor(msUDN)) {
		t.Errorf("the MediaServer announced %q, want its start burst", got)
	}
	from := len(n.snapshot())
	_, sres, err := sub.Register(context.Background(), strictReg(srcUDN, 4,
		SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	mustErr(t, "a strict registration whose sender cannot be pinned", err, ErrSSDPNoEndpoint)
	if !errors.Is(sres.Failed[0].Err, ErrSSDPStrictCapability) || !errors.Is(sres.Failed[0].Err, errInjected) {
		t.Errorf("the strict endpoint failed with %v, want the capability refusal naming the pin's error", sres.Failed[0].Err)
	}
	for _, w := range n.snapshot()[from:] {
		if strings.Contains(w.pkt, srcUDN) {
			t.Errorf("the refused strict endpoint sent %q", w.pkt)
		}
	}
	if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 0 {
		t.Errorf("the refused strict device was answered for%s", describe(got))
	}
	if _, s, _ := n.openSockets(); s != 1 {
		t.Errorf("%d senders open, want the MediaServer's one", s)
	}
}

// TestTheDestinationGuardHoldsWhenArrivalReportingFails pins on the host's
// own sockets, armed by the host transport's own arm, what
// TestEachListenerCapabilityFailsOnItsOwn pins on the fake: a listener whose
// arrival report the kernel refuses is still armed for the destination, and
// the MediaServer's B48 guard on it still refuses a search sent to the
// tailnet and answers one sent to the LAN.
func TestTheDestinationGuardHoldsWhenArrivalReportingFails(t *testing.T) {
	var judged judgedAs
	sink := newLogSink()
	const location = "http://192.168.1.20:7790/dlna/description.xml"
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind a loopback listener: %v", err)
	}
	refuseArrival := hostSSDPTransport{armArrival: func(*net.UDPConn) error { return errInjected }}
	_, _, res := registerOnTransport(t, &socketTransport{hostSSDPTransport: refuseArrival}, l, testAdvertiserConfig{
		UDN:         "uuid:b48b48b4-8b48-4b48-8b48-b48b48b48b48",
		Location:    location,
		ServerToken: "test",
		Logger:      sink.logger(),
		served:      macServedJudgedBy(&judged),
	}.registration())
	if caps := res.Active[0].Listener; !caps.DestinationReporting || caps.ArrivalReporting {
		t.Fatalf("the listener reports %+v, want destinations and no arrivals", caps)
	}
	addr := l.LocalAddr().(*net.UDPAddr)
	judged.set("100.64.0.7")
	refused := rootDeviceSearch(t, addr)
	if !sink.waitFor("refused an M-SEARCH to an address it does not serve", 5*time.Second) {
		t.Fatalf("no refusal in the log:\n%s", sink.String())
	}
	if got := firstAnswer(refused, 5*defaultMSearchJitter); got != "" {
		t.Errorf("a search judged as sent to the tailnet was answered %q", got)
	}
	judged.set("192.168.1.20")
	if got := firstAnswer(rootDeviceSearch(t, addr), 5*time.Second); got != location {
		t.Errorf("a search judged as sent to the LAN was answered %q, want %s", got, location)
	}
}

// TestRegisterBesideAnExistingDeviceSharesItsListener pins that a second
// device on an interface takes a reference to the listener the first one
// opened, never a second listener, and leaves the first one's endpoint as it
// was, announcing its own start burst alone.
func TestRegisterBesideAnExistingDeviceSharesItsListener(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	ms, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	before := ms.endpoints[0]
	from := len(n.snapshot())
	src, _ := mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	n.mu.Lock()
	listens := n.listens
	n.mu.Unlock()
	if listens != 1 || src.endpoints[0].listener != before.listener {
		t.Errorf("the second device opened %d listeners in all, want it to share the first's", listens)
	}
	if ms.endpoints[0] != before {
		t.Error("registering beside the MediaServer replaced its endpoint")
	}
	if got := notifies(n, from, "ssdp:alive"); len(got) != len(sourceTargets(srcUDN)) || !strings.HasPrefix(got[0], srcOnEn0) {
		t.Errorf("the second registration announced %q, want its own start burst alone", got)
	}
}

// TestRegisterStartsWhatItCanAndReportsTheRest pins partial success: the
// endpoints that open are active, in order, the others reported with their
// error, and only the active ones announce.
func TestRegisterStartsWhatItCanAndReportsTheRest(t *testing.T) {
	n := newFakeNet()
	n.listenErr["en1"] = errInjected
	sub := fakeSubsystem(t, n)
	_, res := mustRegister(t, sub, legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en1, Location: msOnEn1}, SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	if len(res.Active) != 1 || res.Active[0].Position != 1 || len(res.Failed) != 1 || res.Failed[0].Position != 0 ||
		!errors.Is(res.Failed[0].Err, errInjected) {
		t.Errorf("result %+v, want en0 active and en1 failed with its error", res)
	}
	for _, line := range notifies(n, 0, "ssdp:alive") {
		if !strings.HasPrefix(line, msOnEn0) {
			t.Errorf("the failed endpoint announced %q", line)
		}
	}
}

// TestRegisterWithNothingStartedHoldsNothing pins the all-failed start: an
// error, no handle, and every socket it opened on the way closed again.
func TestRegisterWithNothingStartedHoldsNothing(t *testing.T) {
	n := newFakeNet()
	n.listenErr["en1"] = errInjected
	n.senderErr["en0"] = errInjected
	sub := fakeSubsystem(t, n)
	h, res, err := sub.Register(context.Background(), legacyReg(msUDN, fakeServed(),
		SSDPEndpoint{Interface: en1, Location: msOnEn1}, SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustErr(t, "Register with no endpoint that starts", err, ErrSSDPNoEndpoint)
	if h != nil || len(res.Active) != 0 || len(res.Failed) != 2 {
		t.Errorf("handle %v and result %+v, want none and both failed", h, res)
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("%d listeners, %d senders and %d answer sockets left open", l, s, r)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.listeners) != 0 || len(sub.handles) != 0 {
		t.Errorf("the subsystem holds %d listeners and %d registrations", len(sub.listeners), len(sub.handles))
	}
}

// TestAnInitialAliveThatFailsIsLoggedNotFatal pins that a start burst whose
// writes fail is the NOTIFY failure log's (a Warn), never a failed start.
func TestAnInitialAliveThatFailsIsLoggedNotFatal(t *testing.T) {
	n := newFakeNet()
	n.writeErr["en0"] = errInjected
	sub := fakeSubsystem(t, n)
	var log syncBuffer
	reg := legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0})
	reg.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, res := mustRegister(t, sub, reg); len(res.Active) != 1 {
		t.Fatalf("result %+v, want the endpoint active", res)
	}
	if !strings.Contains(log.String(), "NOTIFY send failed") {
		t.Errorf("no NOTIFY Warn for the failed start burst:\n%s", log.String())
	}
}

// TestTheMediaServerFailingBesideAnotherDeviceLeavesItAlone pins Start's
// rollback on a subsystem the wiring passed in: a MediaServer none of whose
// endpoints starts fails, as before, and the subsystem, the other device's
// registration, its listener and its answers stay as they were.
func TestTheMediaServerFailingBesideAnotherDeviceLeavesItAlone(t *testing.T) {
	n := newFakeNet()
	n.listenErr["en1"] = errInjected
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	addr := findFreePort(t)
	s, err := NewServer(ServerConfig{
		Library: newTestLib(), UDN: msUDN, ListenAddress: addr, ServerURL: "http://" + addr,
		AdvertiseEndpoints: []AdvertiseEndpoint{{Interface: en1, ServerURL: "http://10.0.0.5:7790"}},
		SSDP:               sub,
		Logger:             discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Start(context.Background())
	if err == nil || err.Error() != "dlna: SSDP start: no advertiser could bind (tried 1 endpoint(s))" {
		t.Fatalf("Start = %v, want the all-failed error", err)
	}
	_ = s.Stop(context.Background())
	sub.mu.Lock()
	closed, handles := sub.closed, len(sub.handles)
	sub.mu.Unlock()
	if closed || handles != 1 {
		t.Errorf("after the MediaServer's failed start the subsystem is closed=%v with %d registrations, want open with the other's", closed, handles)
	}
	if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 1 {
		t.Errorf("the other device was answered%s, want once", describe(got))
	}
}
