package dlna

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestCloseJoinsAWithdrawalAlreadySendingByeBye parks Withdraw after it has
// unpublished its endpoints. Close must still own its cleanup.
func TestCloseJoinsAWithdrawalAlreadySendingByeBye(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "deadline"}[expire], func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			n.mu.Lock()
			n.onSenderWrite = func(s *fakeSender, pkt string) {
				if header(pkt, "NTS") == "ssdp:byebye" {
					once.Do(func() {
						close(entered)
						select {
						case <-release:
						case <-s.closed:
						}
					})
				}
			}
			n.mu.Unlock()
			withdrawn := make(chan error, 1)
			go func() { withdrawn <- h.Withdraw(context.Background()) }()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			closed := make(chan error, 1)
			go func() { closed <- sub.Close(ctx) }()
			waitFor(t, "Close to begin", func() bool { sub.mu.Lock(); defer sub.mu.Unlock(); return sub.closed })
			// Wake an idle reader so the old Close can expose its premature
			// return without waiting for the listener's one-second idle tick.
			n.openListener(t, "en0").deliver("ignored", netip.MustParseAddrPort(searchSrc), groupMeta(2))
			var closeErr error
			returned := false
			select {
			case closeErr = <-closed:
				returned = true
				t.Error("Close returned while Withdraw's byebye and sockets were still pending")
			case <-time.After(40 * time.Millisecond):
			}
			if !expire || returned {
				close(release)
			}
			if !returned {
				select {
				case closeErr = <-closed:
				case <-time.After(350 * time.Millisecond):
					t.Error("Close exceeded its deadline while a withdrawal was sending")
					if expire {
						close(release)
					}
					closeErr = <-closed
				}
			}
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("Close left sockets open: %d/%d/%d", l, s, r)
			}
			<-withdrawn
			if expire && !returned && !errors.Is(closeErr, context.DeadlineExceeded) {
				t.Errorf("Close = %v, want deadline exceeded", closeErr)
			}
			if !expire && len(notifies(n, 0, "ssdp:byebye")) != len(h.targets) {
				t.Error("released withdrawal lost its byebye burst")
			}
		})
	}
}

// TestCloseBudgetIncludesInflightWrites parks each initial alive in the
// fake kernel; its ordinary one-second write budget exceeds Close's budget.
func TestCloseBudgetIncludesInflightWrites(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "Register", true: "Update"}[update], func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			var h *SSDPHandle
			if update {
				h, _ = mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			}
			entered := make(chan struct{})
			var once sync.Once
			n.mu.Lock()
			n.blockSends["en1"] = true
			n.onSenderWrite = func(s *fakeSender, pkt string) {
				if s.iface == "en1" {
					once.Do(func() { close(entered) })
				}
			}
			n.mu.Unlock()
			done := make(chan error, 1)
			results := make(chan SSDPResult, 1)
			go func() {
				ep := SSDPEndpoint{Interface: en1, Location: msOnEn1}
				if update {
					res, err := h.Update(context.Background(), []SSDPEndpoint{{Interface: en0, Location: msOnEn0}, ep})
					results <- res
					done <- err
				} else {
					_, res, err := sub.Register(context.Background(), legacyReg(msUDN, fakeServed(), ep))
					results <- res
					done <- err
				}
			}()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := sub.Close(ctx)
			if took := time.Since(start); took > 350*time.Millisecond {
				t.Errorf("Close took %v for a 100ms budget", took)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Close = %v, want deadline exceeded", err)
			}
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("Close left sockets open: %d/%d/%d", l, s, r)
			}
			if err := <-done; !errors.Is(err, ErrSSDPClosed) {
				t.Errorf("inflight operation = %v", err)
			}
			// What Close rolled back is reported failed, as Register reports it;
			// Update's kept endpoint (position 0) stays active until Close
			// withdraws the handle (CodeRabbit's review of #1329).
			res := <-results
			addition := 0
			if update {
				addition = 1
				if len(res.Active) != 1 || res.Active[0].Position != 0 || res.Active[0].Err != nil {
					t.Errorf("Active = %+v, want only the kept endpoint at position 0", res.Active)
				}
			} else if len(res.Active) != 0 {
				t.Errorf("Active = %+v, want none", res.Active)
			}
			if len(res.Failed) != 1 || res.Failed[0].Position != addition || !errors.Is(res.Failed[0].Err, ErrSSDPClosed) {
				t.Errorf("Failed = %+v, want the rolled-back addition at position %d with ErrSSDPClosed", res.Failed, addition)
			}
		})
	}
}

// TestStrictReplyKeepsItsSelectedEndpoint models an overlapping route: the
// fake network's default has no relationship to the selected endpoint.
func TestStrictReplyKeepsItsSelectedEndpoint(t *testing.T) {
	n := newFakeNet()
	// The same subnet also has a more specific route through en1.
	n.replyRoute = SSDPEndpointSnapshot{Interface: SSDPInterface{Index: 3, Name: "en1"}, Local: netip.MustParsePrefix("192.168.1.99/24")}
	sub := fakeSubsystem(t, n)
	ep := SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}
	mustRegister(t, sub, strictReg(srcUDN, 4, ep))
	mustRegister(t, sub, legacyReg(msUDN, fakeServed(), ep))
	for _, meta := range []datagramMeta{groupMeta(2), unicastMeta("192.168.1.20", 2)} {
		sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, meta)
		n.mu.Lock()
		if len(n.replies) == 0 {
			t.Error("strict reply was not created")
		} else {
			got := n.replies[len(n.replies)-1].from
			if got.Interface.Index != 2 || got.Local != ep.Local {
				t.Errorf("strict reply uses default/other route: %+v", got)
			}
		}
		n.mu.Unlock()
	}
	sendTo(t, sub, n, "en0", search(msUDN), searchSrc, groupMeta(2))
	n.mu.Lock()
	defer n.mu.Unlock()
	if got := n.replies[len(n.replies)-1].from; got != n.replyRoute {
		t.Errorf("MediaServer routing changed: %+v", got)
	}
}

// TestStrictReplyPinFailureSendsNothingAndLogsOnce exercises refusal at
// socket setup and at the per-packet send, without falling back to routing.
func TestStrictReplyPinFailureSendsNothingAndLogsOnce(t *testing.T) {
	for _, atWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup", true: "write"}[atWrite], func(t *testing.T) {
			n := newFakeNet()
			if atWrite {
				n.strictWriteErr = errInjected
			} else {
				n.strictReplyErr = errInjected
			}
			sub := fakeSubsystem(t, n)
			var log bytes.Buffer
			reg := strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")})
			reg.Logger = slog.New(slog.NewTextHandler(&log, nil))
			h, _ := mustRegister(t, sub, reg)
			for range 3 {
				if got := sendTo(t, sub, n, "en0", search("ssdp:all"), searchSrc, groupMeta(2)); len(got) != 0 {
					t.Error("unconfined strict reply sent")
				}
			}
			_ = h.Withdraw(context.Background()) // join every logger before reading
			if got := strings.Count(log.String(), "SSDP strict reply refused"); got != 1 {
				t.Errorf("refusal warnings = %d, want 1", got)
			}
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("refusal leaked sockets: %d/%d/%d", l, s, r)
			}
		})
	}
}

// round2Unicast records the host transport's bind/pin/write ordering. It
// implements UDP's operations without creating a socket or reading the host.
type round2Unicast struct {
	bound  netip.Addr
	pinned int
	closed bool
	writes int
	oob    []byte
	rawErr error
}

func (c *round2Unicast) Close() error                          { c.closed = true; return nil }
func (c *round2Unicast) SetWriteDeadline(time.Time) error      { return nil }
func (c *round2Unicast) SyscallConn() (syscall.RawConn, error) { return nil, c.rawErr }
func (c *round2Unicast) WriteToUDP(b []byte, _ *net.UDPAddr) (int, error) {
	c.writes++
	return len(b), nil
}
func (c *round2Unicast) WriteMsgUDP(b, oob []byte, _ *net.UDPAddr) (int, int, error) {
	c.writes++
	c.oob = append([]byte(nil), oob...)
	return len(b), len(oob), nil
}

// TestHostStrictReplyBindsAndPinsBeforeSending drives the real host factory
// through socket seams, including failure to bind and failure to pin.
func TestHostStrictReplyBindsAndPinsBeforeSending(t *testing.T) {
	from := SSDPEndpointSnapshot{Interface: SSDPInterface{Index: 2, Name: "en0"}, Local: netip.MustParsePrefix("192.168.1.20/24")}
	for _, failure := range []string{"", "bind", "pin"} {
		t.Run(failure, func(t *testing.T) {
			c := &round2Unicast{}
			host := hostSSDPTransport{
				openReply: func(addr *net.UDPAddr) (ssdpUnicastSocket, error) {
					if failure == "bind" {
						return nil, errInjected
					}
					c.bound = addr.AddrPort().Addr().Unmap()
					if addr.Port != 0 {
						t.Error("reply reused the listener port")
					}
					return c, nil
				},
				pinReply: func(_ ssdpUnicastSocket, index int) error {
					if c.bound != from.Local.Addr() || c.writes != 0 {
						t.Error("pin preceded bind or followed a send")
					}
					if failure == "pin" {
						return errInjected
					}
					c.pinned = index
					return nil
				},
			}
			conn, err := host.dialReply(netip.MustParseAddrPort(searchSrc), &from)
			if failure != "" {
				if !errors.Is(err, errInjected) || conn != nil || c.writes != 0 {
					t.Errorf("failed pin/bind fell back: %v", err)
				}
				if failure == "pin" && !c.closed {
					t.Error("pin refusal leaked socket")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.pinned != 2 {
				t.Error("strict reply did not pin the selected interface")
			}
			_, err = conn.Write([]byte("reply"))
			_ = conn.Close()
			switch runtime.GOOS {
			case "linux", "darwin", "windows":
				if err != nil || c.writes != 1 {
					t.Errorf("supported reply = %v, %d writes; want one send", err, c.writes)
				}
			default:
				// The pin seam above checks ordering on every platform; the
				// real unsupported-platform write still must fail closed.
				if !errors.Is(err, ErrSSDPStrictCapability) || c.writes != 0 {
					t.Errorf("unsupported reply = %v, %d writes; want refusal", err, c.writes)
				}
			}
			if !c.closed {
				t.Error("reply did not close")
			}
		})
	}
}

type unknownAdmission struct{ SSDPAdmission }

// TestAdmissionFormsAreValidatedBeforeOpening includes an embedded unknown
// implementation, which satisfies the sealed interface through promotion.
func TestAdmissionFormsAreValidatedBeforeOpening(t *testing.T) {
	for _, tc := range []struct {
		name      string
		admission SSDPAdmission
		valid     bool
	}{
		{"strict_pointer", &SSDPStrictAdmission{Source: onLink}, true},
		{"served_pointer", &ssdpServedAdmission{served: fakeServed()}, true},
		{"strict_nil", (*SSDPStrictAdmission)(nil), false},
		{"served_nil", (*ssdpServedAdmission)(nil), false},
		{"nil_hook", &SSDPStrictAdmission{}, false},
		{"unknown", unknownAdmission{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			reg := strictReg(srcUDN, 4, SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")})
			reg.Admission = tc.admission
			h, _, err := sub.Register(context.Background(), reg)
			if !tc.valid {
				if err == nil || h != nil {
					t.Error("invalid admission accepted")
				}
				if n.listens != 0 || len(n.senders) != 0 {
					t.Error("invalid admission opened sockets")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 1 {
				t.Errorf("pointer admission answered %d times", len(got))
			}
			if tc.name == "strict_pointer" {
				_ = h.Withdraw(context.Background())
				n.mu.Lock()
				n.pinErr["en0"] = errInjected
				n.mu.Unlock()
				if h, _, err := sub.Register(context.Background(), reg); h != nil || !errors.Is(err, ErrSSDPNoEndpoint) {
					t.Error("strict pointer bypassed sender pin refusal")
				}
			}
		})
	}
}

// TestMediaServerPacketsMatchTheOriginalGoldensWithoutSockets reuses the
// unchanged pre-extraction expected bytes. The real Server's registration
// builder feeds the fake network; the loopback goldens remain separate.
func TestMediaServerPacketsMatchTheOriginalGoldensWithoutSockets(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	s, err := NewServer(ServerConfig{
		Library: newTestLib(testTrack("t1", "track")), UDN: msUDN,
		ModelNumber: goldenModelNumber, FriendlyName: "Golden",
		ListenAddress: "127.0.0.1:7790", ServerURL: "http://127.0.0.1:7790",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.served = fakeServed()
	endpoints := []AdvertiseEndpoint{
		{Interface: en0, ServerURL: "http://192.168.1.20:7790"},
		{Interface: en1, ServerURL: "http://[2001:db8::1]:7790"},
		{Interface: en2, ServerURL: "http://bridge.example:7790"},
	}
	h, _ := mustRegister(t, sub, s.mediaServerRegistration(endpoints))
	checkBurst := func(start int, nts string) {
		t.Helper()
		var got, want []string
		for _, w := range n.snapshot()[start:] {
			if w.kind == "notify" {
				got = append(got, w.pkt)
			}
		}
		for _, ep := range endpoints {
			want = append(want, goldenBurst(nts, ep.ServerURL+"/dlna/description.xml", msUDN)...)
		}
		sameDatagrams(t, nts, got, want)
	}
	checkBurst(0, "ssdp:alive")
	// Drive each endpoint's existing repeat worker one at a time, retaining
	// the server's target order and checking the same bytes as startup.
	start := len(n.snapshot())
	for _, ep := range h.endpoints {
		before := len(n.snapshot())
		ep.kick <- struct{}{}
		waitFor(t, "the repeat burst", func() bool { return len(n.snapshot()) == before+5 })
	}
	checkBurst(start, "ssdp:alive")
	for _, ep := range endpoints {
		location := ep.ServerURL + "/dlna/description.xml"
		targets := goldenTargets(msUDN)
		searches := []string{"ssdp:all", "uuid:someone-else", "urn:schemas-upnp-org:device:MediaRenderer:1"}
		for _, tg := range targets {
			searches = append(searches, tg[0])
		}
		for _, st := range searches {
			start := len(n.snapshot())
			sendTo(t, sub, n, ep.Interface.Name, search(st), searchSrc, groupMeta(ep.Interface.Index))
			var got, want []string
			for _, w := range n.snapshot()[start:] {
				if w.kind == "reply" {
					got = append(got, normalizeDate(t, w.pkt))
				}
			}
			for _, tg := range targets {
				if st == "ssdp:all" || st == tg[0] {
					want = append(want, goldenResponse(location, tg[0], tg[1]))
				}
			}
			sameDatagrams(t, st, got, want)
		}
	}
	start = len(n.snapshot())
	if err := h.Withdraw(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkBurst(start, "ssdp:byebye")
}
