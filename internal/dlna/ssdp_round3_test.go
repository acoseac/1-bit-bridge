package dlna

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestStrictRegisterRefusesAnUnusableReplyEndpointBeforeOpening(t *testing.T) {
	testStrictReplyEndpointValidation(t, false)
}

func TestStrictUpdateRefusesAnUnusableReplyEndpointBeforeOpening(t *testing.T) {
	testStrictReplyEndpointValidation(t, true)
}

func testStrictReplyEndpointValidation(t *testing.T, update bool) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		local netip.Prefix
		iface *net.Interface
	}{
		{"missing_local", netip.Prefix{}, en1},
		{"unspecified", netip.MustParsePrefix("0.0.0.0/24"), en1},
		{"ipv6", netip.MustParsePrefix("fd00::20/64"), en1},
		{"mapped_ipv4", netip.MustParsePrefix("::ffff:192.168.1.20/120"), en1},
		{"missing_interface", netip.MustParsePrefix("192.168.1.20/24"), nil},
		{"zero_index", netip.MustParsePrefix("192.168.1.20/24"), &net.Interface{Name: "en1"}},
		{"negative_index", netip.MustParsePrefix("192.168.1.20/24"), &net.Interface{Index: -1, Name: "en1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			bad := SSDPEndpoint{Interface: tc.iface, Location: "http://192.168.2.20/source.xml", Local: tc.local}
			var res SSDPResult
			if update {
				good := SSDPEndpoint{Interface: en0, Location: srcOnEn0, Local: netip.MustParsePrefix("192.168.1.20/24")}
				h, _ := mustRegister(t, sub, strictReg(srcUDN, 4, good))
				var err error
				res, err = h.Update(context.Background(), []SSDPEndpoint{good, bad})
				if err != nil {
					t.Fatal(err)
				}
				if len(res.Active) != 1 || res.Active[0].Endpoint.Location != good.Location {
					t.Errorf("update active = %+v, want retained endpoint alone", res.Active)
				}
				if got := sendTo(t, sub, n, "en0", search(srcUDN), searchSrc, groupMeta(2)); len(got) != 1 {
					t.Errorf("retained endpoint answered %d times", len(got))
				}
			} else {
				h, r, err := sub.Register(context.Background(), strictReg(srcUDN, 4, bad))
				res = r
				if h != nil || !errors.Is(err, ErrSSDPNoEndpoint) || len(res.Active) != 0 {
					t.Errorf("Register returned handle=%t, %+v, %v; want no endpoint", h != nil, res, err)
				}
			}
			if len(res.Failed) != 1 || !errors.Is(res.Failed[0].Err, ErrSSDPStrictCapability) {
				t.Errorf("failed = %+v, want strict capability refusal", res.Failed)
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			want := 0
			if update {
				want = 1
			}
			if n.listens != want || len(n.senders) != want {
				t.Errorf("opened listeners/senders = %d/%d, want %d/%d", n.listens, len(n.senders), want, want)
			}
		})
	}
}

// TestWithdrawalReportsAnEarlierCloseDeadline starts retirement before Close
// so the withdrawal initially has its own five-second budget.
func TestWithdrawalReportsAnEarlierCloseDeadline(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	entered := make(chan struct{})
	var once sync.Once
	n.mu.Lock()
	n.onSenderWrite = func(s *fakeSender, pkt string) {
		if header(pkt, "NTS") == "ssdp:byebye" {
			once.Do(func() { close(entered); <-s.closed })
		}
	}
	n.mu.Unlock()
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- h.Withdraw(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sub.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want deadline exceeded", err)
	}
	if err := <-withdrawn; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Withdraw = %v, want Close's deadline exceeded", err)
	}
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("Close left sockets open: %d/%d/%d", l, s, r)
	}
}

// TestByebyeRefreshesTheCloseDeadlineBetweenWrites models Close's deadline
// publication at a write boundary, before its asynchronous watcher runs.
func TestByebyeRefreshesTheCloseDeadlineBetweenWrites(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "shortened", true: "expired"}[expired], func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			deadline := time.Now().Add(500 * time.Millisecond)
			if expired {
				deadline = time.Now().Add(-time.Second)
			}
			// Publish exactly the state Close stores, without its watcher. This
			// makes a late watcher deterministic and observes the per-write guard.
			writes := 0
			n.mu.Lock()
			n.onSenderWrite = func(s *fakeSender, pkt string) {
				if header(pkt, "NTS") != "ssdp:byebye" {
					return
				}
				writes++
				if writes == 1 {
					sub.mu.Lock()
					sub.closed, sub.closeDeadline = true, deadline
					sub.mu.Unlock()
				} else {
					s.mu.Lock()
					got := s.deadline
					s.mu.Unlock()
					if expired {
						t.Error("byebye write attempted after Close's deadline")
					}
					if !got.Equal(deadline) {
						t.Errorf("write deadline = %v, want %v", got, deadline)
					}
				}
			}
			n.mu.Unlock()
			err := h.sendByebyeAll(h.endpoints[0], time.Now().Add(5*time.Second))
			sub.mu.Lock()
			sub.closed = false // restore before fakeSubsystem's real Close cleanup
			sub.mu.Unlock()
			n.mu.Lock()
			n.onSenderWrite = nil
			n.mu.Unlock()
			if expired && (!errors.Is(err, context.DeadlineExceeded) || writes != 1) {
				t.Errorf("expired burst = %v, %d writes; want deadline exceeded, one write", err, writes)
			}
			if !expired && (err != nil || writes != len(h.targets)) {
				t.Errorf("shortened burst = %v, %d writes", err, writes)
			}
		})
	}
}
