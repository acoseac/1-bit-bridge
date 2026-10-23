package dlna

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// The update tests' endpoints: the test device on en0, en1 and en2.
var (
	upEn0 = SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}
	upEn1 = SSDPEndpoint{Interface: en1, Location: "http://10.0.0.5:7791/openhome/description.xml", Local: netip.MustParsePrefix("10.0.0.5/8")}
	upEn2 = SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}
)

// firstSeq is the sequence number of the first notify of kind nts naming
// location written since from, or 0.
func firstSeq(n *fakeNet, from int, nts, location string) int {
	for _, w := range n.snapshot()[from:] {
		if w.kind == "notify" && header(w.pkt, "NTS") == nts && header(w.pkt, "LOCATION") == location {
			return w.seq
		}
	}
	return 0
}

// lastSeq is the sequence number of the last notify of kind nts naming
// location written since from, or 0.
func lastSeq(n *fakeNet, from int, nts, location string) int {
	last := 0
	for _, w := range n.snapshot()[from:] {
		if w.kind == "notify" && header(w.pkt, "NTS") == nts && header(w.pkt, "LOCATION") == location {
			last = w.seq
		}
	}
	return last
}

// TestAnUpdateKeepsWhatItKeeps pins Update's diff: an endpoint equal to an
// active one keeps its sender, timer, responder slots and listener (no
// listen, no byebye, no start burst); a removed one says byebye; an added
// one announces after the removal's byebye; and when anything was removed,
// every remaining endpoint repeats its alive at once after it (the review's
// finding: a byebye names the device, so a control point that heard it and
// hears another interface would drop it until the next repeat).
func TestAnUpdateKeepsWhatItKeeps(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0, upEn1))
	kept := h.endpoints[1]
	n.mu.Lock()
	listensBefore := n.listens
	n.mu.Unlock()
	from := len(n.snapshot())
	res, err := h.Update(context.Background(), []SSDPEndpoint{upEn1, upEn2})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(res.Active) != 2 || res.Active[0].Position != 0 || res.Active[1].Position != 1 || len(res.Failed) != 0 {
		t.Fatalf("result %+v, want both active in the order given", res)
	}
	if h.endpoints[0] != kept {
		t.Error("the kept endpoint was replaced, not kept")
	}
	n.mu.Lock()
	listens := n.listens - listensBefore
	n.mu.Unlock()
	if listens != 1 {
		t.Errorf("the update opened %d listeners, want en2's alone", listens)
	}
	if got := lastSeq(n, from, "ssdp:byebye", upEn1.Location); got != 0 {
		t.Error("the kept endpoint said byebye")
	}
	byebye := lastSeq(n, from, "ssdp:byebye", upEn0.Location)
	added := firstSeq(n, from, "ssdp:alive", upEn2.Location)
	if byebye == 0 || added == 0 || added < byebye {
		t.Errorf("the removed endpoint's last byebye at %d, the added one's first alive at %d: want both, the byebye first", byebye, added)
	}
	waitFor(t, "the remaining endpoint's repeat", func() bool { return firstSeq(n, from, "ssdp:alive", upEn1.Location) != 0 })
	if repeat := firstSeq(n, from, "ssdp:alive", upEn1.Location); repeat < byebye {
		t.Errorf("the remaining endpoint repeated its alive at %d, before the byebye at %d", repeat, byebye)
	}

	// An update that removes nothing repeats nothing.
	from = len(n.snapshot())
	if _, err := h.Update(context.Background(), []SSDPEndpoint{upEn1, upEn2, upEn0}); err != nil {
		t.Fatalf("the second Update: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, loc := range []string{upEn1.Location, upEn2.Location} {
		if got := firstSeq(n, from, "ssdp:alive", loc); got != 0 {
			t.Errorf("an update that removed nothing repeated %s's alive", loc)
		}
	}
	if got := firstSeq(n, from, "ssdp:alive", upEn0.Location); got == 0 {
		t.Error("the endpoint added back did not announce")
	}
}

// TestAFailedAdditionIsReportedAndNothingStandsIn pins that an addition that
// cannot open is reported in the result and replaced by nothing, while the
// rest of the update, a removal included, takes effect.
func TestAFailedAdditionIsReportedAndNothingStandsIn(t *testing.T) {
	n := newFakeNet()
	n.listenErr["en2"] = errInjected
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0, upEn1))
	res, err := h.Update(context.Background(), []SSDPEndpoint{upEn1, upEn2})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Position != 1 || !errors.Is(res.Failed[0].Err, errInjected) || len(res.Active) != 1 {
		t.Errorf("result %+v, want en1 active and en2 failed with its error", res)
	}
	if len(h.endpoints) != 1 || h.endpoints[0].spec.Location != upEn1.Location {
		t.Errorf("active after the update: %d endpoints, want en1's alone", len(h.endpoints))
	}
	if l, _, _ := n.openSockets(); l != 1 {
		t.Errorf("%d listeners open, want en1's alone: the removed en0's is closed, and en2's never opened", l)
	}
}

// TestAFailedReplacementStillRemovesTheOld pins that a LOCATION changed on
// one interface whose new endpoint cannot open leaves nothing on that
// interface: the old one, which the caller removed, is withdrawn with its
// byebye and not kept in its place.
func TestAFailedReplacementStillRemovesTheOld(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0))
	n.mu.Lock()
	n.senderErr["en0"] = errInjected
	n.mu.Unlock()
	moved := upEn0
	moved.Location = "http://192.168.1.20:7792/openhome/description.xml"
	from := len(n.snapshot())
	res, err := h.Update(context.Background(), []SSDPEndpoint{moved})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(res.Active) != 0 || len(res.Failed) != 1 {
		t.Errorf("result %+v, want the replacement failed and nothing active", res)
	}
	if got := len(notifies(n, from, "ssdp:byebye")); got != len(sourceTargets(srcUDN)) {
		t.Errorf("the replaced endpoint sent %d byebyes, want its burst", got)
	}
	if l, s, _ := n.openSockets(); l+s != 0 {
		t.Errorf("%d listeners and %d senders open, want none", l, s)
	}
}

// TestAnUpdateToNothingParksAndALaterOneRecovers pins the parked state: an
// update that leaves nothing active keeps the registration with no socket,
// answering nothing, until a later update brings it back.
func TestAnUpdateToNothingParksAndALaterOneRecovers(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0))
	if _, err := h.Update(context.Background(), nil); err != nil {
		t.Fatalf("Update to nothing: %v", err)
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("parked with %d listeners, %d senders and %d answer sockets open", l, s, r)
	}
	sub.mu.Lock()
	listeners, handles := len(sub.listeners), len(sub.handles)
	sub.mu.Unlock()
	if listeners != 0 || handles != 1 {
		t.Errorf("parked: the subsystem holds %d listeners and %d registrations, want none and the parked one", listeners, handles)
	}
	from := len(n.snapshot())
	res, err := h.Update(context.Background(), []SSDPEndpoint{upEn0})
	if err != nil || len(res.Active) != 1 {
		t.Fatalf("Update back: %v, %+v", err, res)
	}
	if got := notifies(n, from, "ssdp:alive"); len(got) != len(sourceTargets(srcUDN)) {
		t.Errorf("the recovered endpoint announced %q, want its start burst", got)
	}
	if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 1 {
		t.Errorf("the recovered registration was answered%s, want once", describe(got))
	}
}

// TestANewIncarnationGetsAListenerOfItsOwn pins the listener key: an
// interface recreated under the same index and name is a new identity when
// the caller says so (Incarnation), with a listener and a membership of its
// own, beside the old one the MediaServer, which never updates, still holds.
func TestANewIncarnationGetsAListenerOfItsOwn(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en1, Location: msOnEn1}))
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn1))
	old := n.openListener(t, "en1")
	reborn := upEn1
	reborn.Incarnation = 1
	if _, err := h.Update(context.Background(), []SSDPEndpoint{reborn}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fresh := n.openListener(t, "en1")
	if fresh == old || old.isClosed() {
		t.Fatalf("after the new incarnation: the old listener closed=%v, a fresh one=%v; want both open", old.isClosed(), fresh != old)
	}
	from := len(n.snapshot())
	fresh.deliver(search("upnp:rootdevice"), netip.MustParseAddrPort("10.0.0.50:50000"), groupMeta(3))
	old.deliver(search("upnp:rootdevice"), netip.MustParseAddrPort("10.0.0.50:50000"), groupMeta(3))
	settle(t, sub, n)
	got := sortedUSNs(replies(n, from))
	if want := want(upEn1.Location+" "+srcUDN+"::upnp:rootdevice", msOnEn1+" "+msUDN+"::upnp:rootdevice"); !slices.Equal(got, want) {
		t.Errorf("answered %q, want the test device from the new incarnation's listener and the MediaServer from the old", got)
	}
}

// TestAnAnswerQueuedForARemovedEndpointIsNeverSent pins the endpoint's
// generation: an answer waiting out its delay when its endpoint is removed
// is never sent with the withdrawn LOCATION.
func TestAnAnswerQueuedForARemovedEndpointIsNeverSent(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	sub.replyDelay = func(time.Duration) time.Duration { return 200 * time.Millisecond }
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0, upEn1))
	removed := h.endpoints[0]
	n.openListener(t, "en0").deliver(search(srcUDN), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	waitFor(t, "the answer to wait out its delay", func() bool { return len(removed.pool) == 1 })
	if _, err := h.Update(context.Background(), []SSDPEndpoint{upEn1}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	for _, a := range replies(n, 0) {
		if a.location == upEn0.Location {
			t.Errorf("an answer queued before the removal was sent from the removed LOCATION to %s", a.to)
		}
	}
}

// TestARemovedEndpointWhoseAddressIsGoneIsRemovedAnyway pins that a removal's
// byebye is best-effort: an address already gone fails it, the failure is
// returned, and the endpoint is removed and released all the same.
func TestARemovedEndpointWhoseAddressIsGoneIsRemovedAnyway(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0, upEn1))
	n.mu.Lock()
	n.writeErr["en0"] = errInjected
	n.mu.Unlock()
	res, err := h.Update(context.Background(), []SSDPEndpoint{upEn1})
	mustErr(t, "Update whose removal's byebye fails", err, errInjected)
	if len(res.Active) != 1 || len(h.endpoints) != 1 || h.endpoints[0].spec.Location != upEn1.Location {
		t.Errorf("result %+v with %d endpoints active, want en1's alone", res, len(h.endpoints))
	}
	n.mu.Lock()
	var en0Open bool
	for _, l := range n.listeners {
		if l.iface == "en0" && !l.isClosed() {
			en0Open = true
		}
	}
	n.mu.Unlock()
	if en0Open {
		t.Error("the removed endpoint's listener is still open")
	}
}

// TestRemovingASharedInterfaceKeepsItForTheOther pins the membership's
// ownership: the test device's update that leaves en0 leaves en0's listener,
// which the MediaServer still uses, open and answering.
func TestRemovingASharedInterfaceKeepsItForTheOther(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0))
	shared := n.openListener(t, "en0")
	if _, err := h.Update(context.Background(), []SSDPEndpoint{upEn2}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if shared.isClosed() {
		t.Fatal("the update closed the listener the MediaServer still uses")
	}
	if got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), searchSrc, groupMeta(2)); len(got) != 1 || got[0].location != msOnEn0 {
		t.Errorf("en0's group search was answered%s, want the MediaServer alone", describe(got))
	}
}

// TestALocationChangedOnOneInterfaceSaysByebyeBeforeAlive pins the order of a
// LOCATION change: the old LOCATION's whole byebye burst, then the new one's
// alive burst.
func TestALocationChangedOnOneInterfaceSaysByebyeBeforeAlive(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, upEn0))
	moved := upEn0
	moved.Location = "http://192.168.1.20:7792/openhome/description.xml"
	from := len(n.snapshot())
	if _, err := h.Update(context.Background(), []SSDPEndpoint{moved}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var kinds []string
	for _, w := range n.snapshot()[from:] {
		kinds = append(kinds, header(w.pkt, "NTS")+" "+header(w.pkt, "LOCATION"))
	}
	var wantKinds []string
	for range sourceTargets(srcUDN) {
		wantKinds = append(wantKinds, "ssdp:byebye "+upEn0.Location)
	}
	for range sourceTargets(srcUDN) {
		wantKinds = append(wantKinds, "ssdp:alive "+moved.Location)
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("a LOCATION change wrote\n%s\nwant\n%s", strings.Join(kinds, "\n"), strings.Join(wantKinds, "\n"))
	}
}
