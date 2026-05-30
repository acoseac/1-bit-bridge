package dlna

import (
	"context"
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
			t.Skipf("multicast unavailable in this environment: %v", err)
			return
		}
		// Let the periodic goroutine tick a few times.
		time.Sleep(10 * time.Millisecond)
		a.Stop()
	}
}
