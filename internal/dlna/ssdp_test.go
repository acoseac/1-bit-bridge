package dlna

import (
	"context"
	"net"
	"testing"
	"time"
)

// Test_NewSSDPAdvertiser_DefaultsAdvertiseInterval pins the contract
// that a zero / sub-minute AdvertiseInterval collapses to the
// `defaultAdvertiseInterval` (14 min). Defensive against test config
// mistakes that would otherwise spam NOTIFY traffic. Since B470 PR 5a1 the
// clamp is a registration's (advertiseIntervalOf, under the subsystem's
// one-minute floor), and the name is the advertiser's it replaced.
func Test_NewSSDPAdvertiser_DefaultsAdvertiseInterval(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero", 0, defaultAdvertiseInterval},
		{"sub_minute", 30 * time.Second, defaultAdvertiseInterval},
		{"exactly_one_minute", time.Minute, time.Minute}, // boundary — clamp is strictly less-than
		{"reasonable_value", 5 * time.Minute, 5 * time.Minute},
		{"larger_than_default", time.Hour, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := advertiseIntervalOf(tc.in, NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{}).intervalFloor); got != tc.want {
				t.Errorf("AdvertiseInterval=%v collapsed to %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Test_NewSSDPAdvertiser_PopulatesNotifyTargets pins that the MediaServer's
// registration carries the canonical 5-tuple from `NotifyTargetsFor(UDN)`,
// in order. A future refactor that drops the targets initialization would
// silently break SSDP advertisement.
func Test_NewSSDPAdvertiser_PopulatesNotifyTargets(t *testing.T) {
	const udn = "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c"
	s := &Server{cfg: ServerConfig{UDN: udn}, served: newServedAddrs(nil)}
	got := s.mediaServerRegistration(nil).Targets
	want := NotifyTargetsFor(udn)
	if len(got) != len(want) {
		t.Fatalf("targets length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("targets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Test_SSDPAdvertiser_StopBeforeStartIsSafe pins that tearing down what was
// never started is a no-op that doesn't panic: a nil registration handle
// withdraws, a subsystem with nothing registered closes, both twice.
// Defensive against shutdown-on-startup-failure paths in `cmd/bridge`
// (e.g., if Start() returns an error early, the shutdown sequence still
// stops what it holds).
func Test_SSDPAdvertiser_StopBeforeStartIsSafe(t *testing.T) {
	var h *SSDPHandle
	for range 2 {
		if err := h.Withdraw(context.Background()); err != nil {
			t.Errorf("a nil handle's Withdraw = %v, want nil", err)
		}
	}
	a := newTestAdvertiser(testAdvertiserConfig{UDN: "uuid:test", Location: "http://x", ServerToken: "test"})
	a.Stop()
	a.Stop()
	sub := NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{})
	for range 2 {
		if err := sub.Close(context.Background()); err != nil {
			t.Errorf("Close of a subsystem with nothing registered = %v, want nil", err)
		}
	}
}

// Test_SSDPAdvertiser_MSearchListenerWakesOnContextCancel pins B31: a
// listener's read loop must return promptly when the subsystem's context is
// cancelled WITHOUT anything closing its socket — the per-iteration read
// deadline is what lets a bare parent-ctx cancel unpark the goroutine (and
// free its WaitGroup slot). Pre-fix the goroutine parked in the read until a
// packet arrived, so the 3s bound below would fire and fail.
//
// Drives the subsystem's read loop against a plain loopback UDP socket (no
// multicast permissions needed → runs in sandboxed CI). The listener never
// receives a packet, so every read hits the deadline: exactly the "quiet
// network" case the fix targets.
func Test_SSDPAdvertiser_MSearchListenerWakesOnContextCancel(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind loopback UDP listener: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sub := NewSSDPSubsystem(ctx, SSDPSubsystemOptions{Logger: ssdpLogger})
	sub.transport = &socketTransport{socks: map[string]*net.UDPConn{"": listener}}
	sub.notifyTo = loopbackDiscard(t)
	h, _, err := sub.Register(ctx, testAdvertiserConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    "http://127.0.0.1:7790/dlna/description.xml",
		ServerToken: "test",
	}.registration())
	if err != nil {
		t.Fatalf("register on the loopback socket: %v", err)
	}
	t.Cleanup(func() {
		_ = h.Withdraw(context.Background())
		_ = sub.Close(context.Background())
	})
	sub.mu.Lock()
	reading := h.active[0].listener.reading
	sub.mu.Unlock()
	if !reading {
		t.Fatal("the registered endpoint's listener has no read loop running")
	}

	// Give the goroutine time to pass the top-of-loop context check and
	// PARK inside the read before cancelling. Without this, cancel() would
	// usually land before the first read, so the loop would exit via the
	// top-of-loop check — never exercising the read-deadline wake path this
	// test guards. 50ms >> the microseconds the goroutine needs to reach the
	// read, and << the 500ms deadline, so it's parked-in-read.
	time.Sleep(50 * time.Millisecond)

	// Cancel the context WITHOUT closing the listener — the whole point of
	// B31 is a parent-ctx cancel that never reaches a socket close. The
	// goroutine (parked in the read) can only observe it after the read
	// deadline fires and loops back to the top; pre-fix it never would.
	cancel()

	done := make(chan struct{})
	go func() {
		sub.readers.Wait()
		close(done)
	}()
	select {
	case <-done:
		// The loop returned — the read deadline let it observe the cancel.
	case <-time.After(3 * time.Second):
		t.Fatal("the listener's read loop did not return after ctx cancel — leaked, parked in its read")
	}
}

// loopbackInterface returns this host's loopback interface, which every test
// that starts a real SSDP advertiser binds it to: the group join, the NOTIFY
// sends (pinned there by SetMulticastInterface) and the M-SEARCH listener
// then stay on this host. Until 2026-09-28 two tests bound the OS default,
// the LAN, and one run of this package multicast about 310 NOTIFYs of a fake
// MediaServer with a loopback LOCATION to every device and bridge on it
// (backlog B38; measured with a listener joined on the LAN interface).
//
// It skips where the host has no loopback interface that is up, which a test
// that must not reach the LAN cannot replace with another interface, and
// where multicast cannot be pinned to it. A MediaServer endpoint only warns
// when that pin fails, and sends on the OS default interface instead, so a
// test whose Start succeeded would multicast onto the LAN there; the skip
// decides with the call the endpoint makes (pinMulticastInterface), on a
// socket opened the way its sender is (openNotifySender), before any
// advertiser starts (CodeRabbit on #1086).
func loopbackInterface(t *testing.T) *net.Interface {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 && ifaces[i].Flags&net.FlagUp != 0 {
			skipUnlessMulticastPins(t, &ifaces[i])
			return &ifaces[i]
		}
	}
	t.Skip("no loopback interface is up")
	return nil
}

// skipUnlessMulticastPins skips the test unless outgoing multicast can be
// pinned to iface.
func skipUnlessMulticastPins(t *testing.T, iface *net.Interface) {
	t.Helper()
	probe, err := openNotifySender()
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	defer probe.Close()
	if err := pinMulticastInterface(probe, iface); err != nil {
		t.Skipf("cannot pin multicast to %s, where Start would send on the OS default interface: %v", iface.Name, err)
	}
}

// Test_SSDPAdvertiser_StartStopRaceFree exercises the teardown race: with a
// short advertise interval the endpoint's timer fires `sendAliveAll` rapidly
// while the listener's read loop is parked in its read, and the withdrawal
// closes the sockets concurrently. Run under `-race`, a shape where a
// goroutine reads a socket field a withdrawal writes trips the race detector
// AND can nil-deref-panic; the subsystem's goroutines hold their own
// endpoint and listener, which only a withdrawal that has joined them
// releases.
//
// The advertiser runs on the loopback interface (loopbackInterface), so
// its NOTIFYs, one burst a millisecond, never leave this host. Skips when
// multicast binding is unavailable there (sandboxed CI): the structural fix
// stands on its own; this is the on-hardware proof.
func Test_SSDPAdvertiser_StartStopRaceFree(t *testing.T) {
	lo := loopbackInterface(t)
	for i := 0; i < 5; i++ {
		a := newTestAdvertiser(testAdvertiserConfig{
			UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
			Location:    "http://127.0.0.1:7790/dlna/description.xml",
			ServerToken: "test",
			Interface:   lo,
			// Past the one-minute floor, so the timer actually ticks during
			// the test window, putting `sendAliveAll` in flight against the
			// concurrent withdrawal.
			AdvertiseInterval: time.Millisecond,
		})

		if err := a.Start(context.Background()); err != nil {
			// Skip ONLY if the very first Start fails (environment lacks
			// multicast). A failure on a later iteration after a prior
			// success indicates a teardown regression (e.g. a socket /
			// port not released by Stop) and must FAIL, not skip.
			if i == 0 {
				t.Skipf("multicast unavailable in this environment: %v", err)
				return
			}
			t.Fatalf("Start failed on iteration %d after a prior success: %v", i, err)
		}
		// Let the periodic goroutine tick a few times.
		time.Sleep(10 * time.Millisecond)
		a.Stop()
	}
}
