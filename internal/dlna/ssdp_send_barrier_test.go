package dlna

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestSSDPSendBarrierJoinsRepliesAndLeavesByebyeAndMediaServer(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	reg := strictReg("uuid:barrier", 0, SSDPEndpoint{Interface: en0,
		Local: netip.MustParsePrefix("192.168.1.20/24"), Location: "http://192.168.1.20:7792/openhome/description.xml"})
	reg.SendBarrier = new(SSDPSendBarrier)
	h, _ := mustRegister(t, sub, reg)
	media, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	ep := h.endpoints[0]
	to := netip.MustParseAddrPort(searchSrc)
	entered, release, leftWrite := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	n.mu.Lock()
	n.onReplyWrite = func(r *fakeReply, _ int) {
		r.mu.Lock()
		deadline := r.deadline
		r.mu.Unlock()
		close(entered)
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-release:
		case <-timer.C:
		}
		close(leftWrite)
	}
	n.mu.Unlock()
	replied := make(chan struct{})
	go func() { defer close(replied); h.answer(ep, h.targets[:1], 0, to) }()
	t.Cleanup(func() { unpark(); <-replied })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reply did not enter its write")
	}
	// Keep the registration context live: this must be the barrier, not
	// context cancellation's asynchronous reply-socket Close, doing the join.
	reg.SendBarrier.Invalidate()
	after := len(n.snapshot())
	select {
	case <-leftWrite:
	default:
		t.Error("invalidation completed while a normal reply write was still pending")
	}
	unpark()
	<-replied
	if got := replies(n, after); len(got) != 0 {
		t.Errorf("reply recorded after invalidation completed: %+v", got)
	}
	n.mu.Lock()
	n.onReplyWrite = nil
	n.mu.Unlock()
	after = len(n.snapshot())
	if err := h.sendAliveAll(ep); !errors.Is(err, errSSDPSendCancelled) {
		t.Errorf("invalidated alive = %v, want skipped send", err)
	}
	h.answer(ep, h.targets, 0, to)
	if got := len(n.snapshot()) - after; got != 0 {
		t.Errorf("invalidated registration wrote %d normal packets", got)
	}
	if err := h.Withdraw(context.Background()); err != nil {
		t.Fatal(err)
	}
	bye := n.snapshot()[after:]
	if len(bye) != len(h.targets) {
		t.Fatalf("byebye count = %d, want %d", len(bye), len(h.targets))
	}
	for _, w := range bye {
		if header(w.pkt, "NTS") != "ssdp:byebye" || header(w.pkt, "LOCATION") != ep.spec.Location {
			t.Errorf("invalidated departure: %s", w.pkt)
		}
	}
	after = len(n.snapshot())
	if err := media.sendAliveAll(media.endpoints[0]); err != nil {
		t.Fatal(err)
	}
	media.answer(media.endpoints[0], media.targets, 0, to)
	if got := len(n.snapshot()) - after; got != 2*len(media.targets) {
		t.Errorf("MediaServer alive/reply packets = %d, want %d", got, 2*len(media.targets))
	}
}
