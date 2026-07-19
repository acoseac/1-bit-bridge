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
// mistakes that would otherwise spam NOTIFY traffic.
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
			a := NewSSDPAdvertiser(SSDPConfig{
				UDN:               "uuid:test",
				Location:          "http://x",
				ServerToken:       "test",
				AdvertiseInterval: tc.in,
			})
			if a.cfg.AdvertiseInterval != tc.want {
				t.Errorf("AdvertiseInterval=%v collapsed to %v, want %v",
					tc.in, a.cfg.AdvertiseInterval, tc.want)
			}
		})
	}
}

// Test_NewSSDPAdvertiser_PopulatesNotifyTargets pins that the
// advertiser's internal `targets` slice is the canonical 5-tuple from
// `NotifyTargetsFor(UDN)`. A future refactor that drops the targets
// initialization would silently break SSDP advertisement.
func Test_NewSSDPAdvertiser_PopulatesNotifyTargets(t *testing.T) {
	const udn = "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c"
	a := NewSSDPAdvertiser(SSDPConfig{UDN: udn, Location: "http://x", ServerToken: "test"})
	want := NotifyTargetsFor(udn)
	if len(a.targets) != len(want) {
		t.Fatalf("targets length = %d, want %d", len(a.targets), len(want))
	}
	for i := range want {
		if a.targets[i] != want[i] {
			t.Errorf("targets[%d] = %+v, want %+v", i, a.targets[i], want[i])
		}
	}
}

// Test_SSDPAdvertiser_StopBeforeStartIsSafe pins that calling Stop()
// on an advertiser that was never Start()-ed is a no-op (doesn't
// panic). Defensive against shutdown-on-startup-failure paths in
// `cmd/bridge/main.go` (e.g., if Start() returns an error early, the
// shutdown sequence might still call Stop() on the unstarted
// advertiser).
func Test_SSDPAdvertiser_StopBeforeStartIsSafe(t *testing.T) {
	a := NewSSDPAdvertiser(SSDPConfig{UDN: "uuid:test", Location: "http://x", ServerToken: "test"})
	// Should not panic
	a.Stop()
	// Calling twice should also be safe
	a.Stop()
}

// Test_SSDPAdvertiser_MSearchListenerWakesOnContextCancel pins B31: the
// M-SEARCH listener loop must return promptly when its context is
// cancelled WITHOUT Stop() closing the socket — the per-iteration read
// deadline is what lets a bare parent-ctx cancel unpark the goroutine (and
// free its WaitGroup slot). Pre-fix the goroutine parked in ReadFromUDP
// until a packet arrived, so the 3s bound below would fire and fail.
//
// Drives runMSearchListener directly against a plain loopback UDP socket
// (no multicast permissions needed → runs in sandboxed CI). The listener
// never receives a packet, so every read hits the deadline: exactly the
// "quiet network" case the fix targets.
func Test_SSDPAdvertiser_MSearchListenerWakesOnContextCancel(t *testing.T) {
	a := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    "http://127.0.0.1:7790/dlna/description.xml",
		ServerToken: "test",
	})
	// runMSearchListener reads s.log only on a NON-timeout read error; set
	// it so a stray error can't nil-deref. Start() would set this, but we
	// drive the listener directly (bypassing the multicast bind).
	a.log = ssdpLogger

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind loopback UDP listener: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Drive the listener exactly as Start() does: hold a wg slot for its
	// lifetime (the goroutine's `defer s.wg.Done()` releases it on return).
	a.wg.Add(1)
	go a.runMSearchListener(ctx, listener)

	// Give the goroutine time to pass the top-of-loop ctx.Done() check and
	// PARK inside ReadFromUDP before cancelling. Without this, cancel()
	// would usually land before the first read, so the loop would exit via
	// the top-of-loop select — never exercising the read-deadline wake path
	// this test guards. 50ms >> the microseconds the goroutine needs to
	// reach the read, and << the 500ms deadline, so it's parked-in-read.
	time.Sleep(50 * time.Millisecond)

	// Cancel the ctx WITHOUT closing the listener — the whole point of B31
	// is a parent-ctx cancel that never reaches Stop()'s socket-close. The
	// goroutine (parked in ReadFromUDP) can only observe it after the read
	// deadline fires and loops back to the top; pre-fix it never would.
	cancel()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Listener returned — the read deadline let it observe ctx.Done().
	case <-time.After(3 * time.Second):
		t.Fatal("runMSearchListener did not return after ctx cancel — leaked, parked in ReadFromUDP")
	}
}

// Test_SSDPAdvertiser_StartStopRaceFree exercises the teardown race the
// capture-local fix closes: with a short advertise interval the periodic
// NOTIFY goroutine fires `sendAliveAll` rapidly while the M-SEARCH
// goroutine is parked in `ReadFromUDP`, and `Stop()` closes + nils the
// socket fields concurrently. Run under `-race`, the OLD shape (helpers
// reading `s.sender` / `s.listener` from the struct) trips the race
// detector AND can nil-deref-panic; the fixed shape (goroutines hold
// captured local copies) is clean.
//
// Skips when multicast binding is unavailable (sandboxed CI) — the
// structural fix stands on its own; this is the on-hardware proof.
func Test_SSDPAdvertiser_StartStopRaceFree(t *testing.T) {
	for i := 0; i < 5; i++ {
		a := NewSSDPAdvertiser(SSDPConfig{
			UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
			Location:    "http://127.0.0.1:7790/dlna/description.xml",
			ServerToken: "test",
		})
		// White-box: bypass the >=1min constructor clamp so the periodic
		// NOTIFY goroutine actually ticks during the test window, putting
		// `sendAliveAll` in flight against the concurrent Stop.
		a.cfg.AdvertiseInterval = time.Millisecond

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
