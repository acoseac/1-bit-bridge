package dlna

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWithdrawCancelsAnAnswerWaitingOutItsDelay pins that a withdrawal wakes
// an answer parked in its MX delay at once: it is never sent, its socket is
// never dialled, and its slot is free when Withdraw returns.
func TestWithdrawCancelsAnAnswerWaitingOutItsDelay(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	sub.replyDelay = func(time.Duration) time.Duration { return time.Hour }
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	ep := h.endpoints[0]
	n.openListener(t, "en0").deliver(search("upnp:rootdevice"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	waitFor(t, "the answer to take its slot", func() bool { return len(ep.pool) == 1 })
	start := time.Now()
	if err := h.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Withdraw took %v with an answer in its delay", took)
	}
	if len(ep.pool) != 0 {
		t.Error("the delayed answer still holds its slot after Withdraw returned")
	}
	if got := replies(n, 0); len(got) != 0 {
		t.Errorf("the cancelled answer was sent%s", describe(got))
	}
	n.mu.Lock()
	dialled := len(n.replies)
	n.mu.Unlock()
	if dialled != 0 {
		t.Errorf("the cancelled answer dialled %d sockets", dialled)
	}
}

// TestWithdrawCutsAnAnswerMidBurstAndByebyeFollowsIt pins the in-flight send
// race (B470 PR 5a1 §7): an answer burst between two writes when the
// withdrawal starts has its socket closed by the cancellation, writes
// nothing more, and is joined before the first byebye, so every byebye comes
// after every answer.
func TestWithdrawCutsAnAnswerMidBurstAndByebyeFollowsIt(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	parked := make(chan *fakeReply, 1)
	n.onReplyWrite = func(r *fakeReply, i int) {
		if i == 1 {
			parked <- r
			<-r.closed
		}
	}
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	n.openListener(t, "en0").deliver(search("ssdp:all"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	var r *fakeReply
	select {
	case r = <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the burst never reached its second write")
	}
	if err := h.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if !r.isClosed() {
		t.Error("the answer's socket is open after Withdraw returned")
	}
	var lastReply, firstByebye int
	byebyes := 0
	for _, w := range n.snapshot() {
		switch {
		case w.kind == "reply":
			lastReply = w.seq
		case header(w.pkt, "NTS") == "ssdp:byebye":
			byebyes++
			if firstByebye == 0 {
				firstByebye = w.seq
			}
		}
	}
	if got := replies(n, 0); len(got) != 1 {
		t.Errorf("the cut burst wrote %d answers, want its first alone:%s", len(got), describe(got))
	}
	if byebyes != len(NotifyTargetsFor(msUDN)) || firstByebye < lastReply {
		t.Errorf("%d byebyes, the first at %d, after the last answer at %d: want every byebye after every answer", byebyes, firstByebye, lastReply)
	}
}

// TestABurstsWritesAreBoundedFromItsFirstWrite pins ssdpBurstDeadline: a
// burst whose writes the kernel will not take ends a second after its first
// write; and the second runs from that first write, after the MX delay,
// never from the search's arrival, so an answer delayed four seconds by MX 5
// still writes every target (the review's finding on the deadline).
func TestABurstsWritesAreBoundedFromItsFirstWrite(t *testing.T) {
	t.Run("a_blocked_write", func(t *testing.T) {
		n := newFakeNet()
		n.blockReplies = true
		sub := fakeSubsystem(t, n)
		h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
		ep := h.endpoints[0]
		start := time.Now()
		n.openListener(t, "en0").deliver(search("ssdp:all"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
		waitFor(t, "the answer to start", func() bool {
			n.mu.Lock()
			defer n.mu.Unlock()
			return len(n.replies) == 1
		})
		waitFor(t, "the blocked burst to end", func() bool { return len(ep.pool) == 0 })
		if took := time.Since(start); took < 900*time.Millisecond || took > 3*time.Second {
			t.Errorf("a burst whose every write blocks took %v, want about %v", took, ssdpBurstDeadline)
		}
	})
	t.Run("an_mx_5_delay_of_4s", func(t *testing.T) {
		n := newFakeNet()
		sub := fakeSubsystem(t, n)
		var window atomic.Int64
		sub.replyDelay = func(w time.Duration) time.Duration {
			window.Store(int64(w))
			return 4 * time.Second
		}
		mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
		pkt := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 5\r\nST: ssdp:all\r\n\r\n"
		got := sendTo(t, sub, n, "en0", pkt, searchSrc, groupMeta(2))
		if time.Duration(window.Load()) != 5*time.Second {
			t.Errorf("the delay was drawn in a window of %v, want MX's 5 s", time.Duration(window.Load()))
		}
		if len(got) != len(NotifyTargetsFor(msUDN)) {
			t.Errorf("an answer delayed 4 s wrote %d of its %d targets%s", len(got), len(NotifyTargetsFor(msUDN)), describe(got))
		}
	})
}

// TestNoSendFollowsAByebyeOrWithdraw pins the withdrawal's guarantee under
// load: with alive bursts every millisecond and searches arriving
// throughout, no alive or answer is written after the first byebye, and
// nothing at all once Withdraw has returned.
func TestNoSendFollowsAByebyeOrWithdraw(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	sub.intervalFloor = 0
	sub.replyDelay = func(w time.Duration) time.Duration {
		return time.Duration(time.Now().UnixNano() % int64(2*time.Millisecond))
	}
	reg := legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0})
	reg.AdvertiseInterval = time.Millisecond
	h, _ := mustRegister(t, sub, reg)
	l := n.openListener(t, "en0")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
				l.deliver(search("ssdp:all"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if err := h.Withdraw(context.Background()); err != nil {
		t.Errorf("Withdraw: %v", err)
	}
	after := len(n.snapshot())
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	writes := n.snapshot()
	if len(writes) != after {
		t.Errorf("%d datagrams were written after Withdraw returned", len(writes)-after)
	}
	firstByebye := 0
	for _, w := range writes {
		if header(w.pkt, "NTS") == "ssdp:byebye" {
			firstByebye = w.seq
			break
		}
	}
	if firstByebye == 0 {
		t.Fatal("no byebye was sent")
	}
	for _, w := range writes {
		if w.seq > firstByebye && (w.kind == "reply" || header(w.pkt, "NTS") == "ssdp:alive") {
			t.Errorf("a %s was written after the first byebye: %q", w.kind, w.pkt)
			break
		}
	}
}

// TestNoAliveFollowsAByebye pins the order B470 PR 5a1 makes deliberate,
// deterministically: an alive burst caught between two writes when the
// withdrawal starts finishes the write it is in, sends nothing more, and is
// joined before the byebye begins, where the advertiser it replaced sent its
// byebye first and cancelled after, so a burst in flight could follow it.
func TestNoAliveFollowsAByebye(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	ep := h.endpoints[0]
	parked, release := make(chan struct{}), make(chan struct{})
	alives := 0
	n.mu.Lock()
	n.onSenderWrite = func(_ *fakeSender, pkt string) {
		if header(pkt, "NTS") != "ssdp:alive" {
			return
		}
		alives++ // one goroutine writes alives: the endpoint's timer
		if alives == 2 {
			close(parked)
			<-release
		}
	}
	n.mu.Unlock()
	from := len(n.snapshot())
	ep.kick <- struct{}{}
	<-parked
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- h.Withdraw(context.Background()) }()
	waitFor(t, "the withdrawal to cancel the endpoint", func() bool { return ep.ctx.Err() != nil })
	time.Sleep(20 * time.Millisecond) // a byebye sent ahead of the join would be written by now
	close(release)
	if err := <-withdrawn; err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	firstByebye, lastAlive := 0, 0
	for _, w := range n.snapshot()[from:] {
		switch header(w.pkt, "NTS") {
		case "ssdp:byebye":
			if firstByebye == 0 {
				firstByebye = w.seq
			}
		case "ssdp:alive":
			lastAlive = w.seq
		}
	}
	if firstByebye == 0 || lastAlive > firstByebye {
		t.Errorf("the last alive at %d, the first byebye at %d: an alive followed the byebye", lastAlive, firstByebye)
	}
	if got := len(notifies(n, from, "ssdp:alive")); got != 2 {
		t.Errorf("the cut burst wrote %d alives, want the two it had begun", got)
	}
}

// TestASearchArrivingBeforeTheFirstAliveIsAnsweredAfterIt pins when a
// listener starts reading: once its first endpoint is published, after that
// endpoint's start burst. A search that reaches the socket earlier waits in
// it, as it waited in an advertiser's socket until the advertiser's first
// alive had gone out, and is answered then, not read and dropped by a
// dispatch that does not list the endpoint yet.
func TestASearchArrivingBeforeTheFirstAliveIsAnsweredAfterIt(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	var once sync.Once
	n.onSenderWrite = func(_ *fakeSender, pkt string) {
		if header(pkt, "NTS") == "ssdp:alive" {
			once.Do(func() {
				n.openListener(t, "en0").deliver(search("upnp:rootdevice"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
			})
		}
	}
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	settle(t, sub, n)
	lastAlive, reply := 0, 0
	for _, w := range n.snapshot() {
		switch {
		case w.kind == "reply":
			reply = w.seq
		case header(w.pkt, "NTS") == "ssdp:alive":
			lastAlive = w.seq
		}
	}
	if reply == 0 || reply < lastAlive {
		t.Errorf("the search that arrived during the start burst was answered at %d, the burst ended at %d: want it answered, after", reply, lastAlive)
	}
}

// TestAByebyeThatFailsStillReleasesTheEndpoint pins that a byebye is
// best-effort: its failure is returned, joined, and the endpoint is released
// and the registration withdrawn all the same; a second Withdraw returns
// nil, and an Update after it is refused.
func TestAByebyeThatFailsStillReleasesTheEndpoint(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	n.mu.Lock()
	n.writeErr["en0"] = errInjected
	n.mu.Unlock()
	mustErr(t, "Withdraw with byebyes failing", h.Withdraw(context.Background()), errInjected)
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("%d listeners, %d senders and %d answer sockets left open", l, s, r)
	}
	if err := h.Withdraw(context.Background()); err != nil {
		t.Errorf("a second Withdraw = %v, want nil", err)
	}
	_, err := h.Update(context.Background(), []SSDPEndpoint{{Interface: en0, Location: msOnEn0}})
	mustErr(t, "Update after Withdraw", err, ErrSSDPWithdrawn)
}

// TestTheLastWithdrawalClosesASharedListener pins the listener's reference
// count: two devices share en0's listener, the first's withdrawal leaves it
// open and reading for the second, and the second's closes it and joins its
// reader.
func TestTheLastWithdrawalClosesASharedListener(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	ms, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	src, _ := mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
	fl := n.openListener(t, "en0")
	l := ms.endpoints[0].listener
	if err := ms.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw the MediaServer: %v", err)
	}
	if fl.isClosed() {
		t.Fatal("the first withdrawal closed the listener the second device still uses")
	}
	if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 1 {
		t.Errorf("the remaining device was answered%s, want once", describe(got))
	}
	if err := src.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw the test device: %v", err)
	}
	if !fl.isClosed() {
		t.Error("the last withdrawal left the listener open")
	}
	select {
	case <-l.readerDone:
	default:
		t.Error("the listener's reader was not joined")
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.listeners) != 0 {
		t.Errorf("the subsystem still lists %d listeners", len(sub.listeners))
	}
}

// TestCloseWithdrawsEverythingAndRefusesWhatComesAfter pins Close: every
// registration withdrawn with its byebye, every socket closed, every reader
// joined, and a Register or Update after it refused.
func TestCloseWithdrawsEverythingAndRefusesWhatComesAfter(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	src, _ := mustRegister(t, sub, strictReg(srcUDN, 4, SSDPEndpoint{Interface: en2, Location: srcOnEn2, Local: netip.MustParsePrefix("172.20.0.9/16")}))
	if err := sub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(notifies(n, 0, "ssdp:byebye")); got != len(NotifyTargetsFor(msUDN))+len(sourceTargets(srcUDN)) {
		t.Errorf("Close sent %d byebyes, want each device's", got)
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("%d listeners, %d senders and %d answer sockets left open", l, s, r)
	}
	if !waitUntil(time.Now().Add(time.Second), sub.readers.Wait) {
		t.Error("a reader was still running after Close returned")
	}
	_, _, err := sub.Register(context.Background(), legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	mustErr(t, "Register after Close", err, ErrSSDPClosed)
	_, err = src.Update(context.Background(), nil)
	mustErr(t, "Update after Close", err, ErrSSDPClosed)
	if err := sub.Close(context.Background()); err != nil {
		t.Errorf("a second Close = %v, want nil", err)
	}
}

// TestAWithdrawalPastItsBudgetCutsTheSendsAndStillJoins pins the budget's
// expiry: a withdrawal whose deadline passes while an alive burst is blocked
// in a write closes the sender under it, sends no byebye, releases and joins
// everything, and returns context.DeadlineExceeded.
func TestAWithdrawalPastItsBudgetCutsTheSendsAndStillJoins(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	ep := h.endpoints[0]
	inWrite := make(chan struct{})
	var once sync.Once
	n.mu.Lock()
	n.blockSends["en0"] = true
	n.onSenderWrite = func(*fakeSender, string) { once.Do(func() { close(inWrite) }) }
	n.mu.Unlock()
	ep.kick <- struct{}{}
	select {
	case <-inWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("the kicked burst never wrote")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	from := len(n.snapshot())
	start := time.Now()
	err := h.Withdraw(ctx)
	mustErr(t, "a withdrawal past its budget", err, context.DeadlineExceeded)
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("the withdrawal took %v, past its 100 ms budget and up to the burst's own deadline", took)
	}
	if got := notifies(n, from, "ssdp:byebye"); len(got) != 0 {
		t.Errorf("byebyes went out past the budget: %q", got)
	}
	select {
	case <-ep.aliveDone:
	default:
		t.Error("the endpoint's timer goroutine was not joined")
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("%d listeners, %d senders and %d answer sockets left open", l, s, r)
	}
}

// TestAListenerClosingIsNeverOpenedTwice pins the review's first race: a
// withdrawal drops the last reference to en0's listener and closes it
// (parked here inside the close), and a Register on en0 meanwhile waits for
// that close to finish and then opens a fresh listener: never a second one
// while the first is still open, and never the closing one.
func TestAListenerClosingIsNeverOpenedTwice(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	ms, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	first := n.openListener(t, "en0")
	parked, release := make(chan struct{}), make(chan struct{})
	first.onClose = func() {
		close(parked)
		<-release
	}
	var overlap atomic.Bool
	n.mu.Lock()
	n.onListen = func(iface string) {
		if iface == "en0" && !first.isClosed() {
			overlap.Store(true)
		}
	}
	n.mu.Unlock()
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- ms.Withdraw(context.Background()) }()
	<-parked
	registered := make(chan error, 1)
	go func() {
		_, _, err := sub.Register(context.Background(), strictReg(srcUDN, 4,
			SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}))
		registered <- err
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-registered:
		t.Fatalf("Register returned (%v) while the listener it needs was still closing", err)
	default:
	}
	close(release)
	if err := <-withdrawn; err != nil {
		t.Errorf("Withdraw: %v", err)
	}
	if err := <-registered; err != nil {
		t.Fatalf("Register once the listener had closed: %v", err)
	}
	n.mu.Lock()
	listens := n.listens
	n.mu.Unlock()
	if overlap.Load() || listens != 2 || n.openListener(t, "en0") == first {
		t.Errorf("listens %d, overlap %v: want a fresh listener opened once the first was closed", listens, overlap.Load())
	}
}

// TestCloseWaitsForARegisterInFlight pins the review's second race: a Close
// that starts while a Register is opening a listener waits for it, the
// Register is refused with ErrSSDPClosed and holds nothing, and nothing is
// left open.
func TestCloseWaitsForARegisterInFlight(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n.onListen = func(string) {
		once.Do(func() { close(parked) })
		<-release
	}
	registered := make(chan error, 1)
	go func() {
		_, _, err := sub.Register(context.Background(), legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
		registered <- err
	}()
	<-parked
	closed := make(chan error, 1)
	go func() { closed <- sub.Close(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a Register was in flight", err)
	default:
	}
	close(release)
	mustErr(t, "the Register Close overtook", <-registered, ErrSSDPClosed)
	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("%d listeners, %d senders and %d answer sockets left open", l, s, r)
	}
	if got := n.snapshot(); len(got) != 0 {
		t.Errorf("the refused Register sent %d datagrams", len(got))
	}
}

// TestADialThatCompletesDuringWithdrawalSendsNothing pins the dial race of
// B470 PR 5a1 §7: an answer whose socket is dialled while the withdrawal
// runs has it closed as soon as it exists, and writes nothing.
func TestADialThatCompletesDuringWithdrawalSendsNothing(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n.onDial = func(netip.AddrPort) {
		once.Do(func() { close(parked) })
		<-release
	}
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	ep := h.endpoints[0]
	n.openListener(t, "en0").deliver(search("ssdp:all"), netip.MustParseAddrPort(searchSrc), groupMeta(2))
	<-parked
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- h.Withdraw(context.Background()) }()
	waitFor(t, "the withdrawal to cancel the endpoint", func() bool { return ep.ctx.Err() != nil })
	close(release)
	if err := <-withdrawn; err != nil {
		t.Errorf("Withdraw: %v", err)
	}
	if got := replies(n, 0); len(got) != 0 {
		t.Errorf("an answer whose dial completed during the withdrawal was sent%s", describe(got))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.replies) != 1 || !n.replies[0].isClosed() {
		t.Errorf("%d answer sockets, want the one dialled, closed", len(n.replies))
	}
}
