package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
)

// TestMDNSLifecycleSetIsIdempotentOnRepeatedEnable: Set(true)
// twice in a row must NOT spawn a second advertiser (would
// double-register the Bonjour record + leak the first
// advertiser's goroutines).
func TestMDNSLifecycleSetIsIdempotentOnRepeatedEnable(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	// Port 0 is reserved + cheap — mDNS Advertise binds a UDP
	// listener on this for the rebind goroutine. Real
	// production has the bridge's listen port here.
	cfg := bridgemdns.Config{
		InstanceName:    "Test Lifecycle",
		Port:            17788,
		ProtocolVersion: 1,
		LibraryName:     "Test Library",
	}
	m := newMDNSLifecycle(cfg, stdout, stderr)
	defer m.Close()

	m.Set(true)
	first := m.advertiser
	m.Set(true) // second Set with the same state — should be a no-op
	if m.advertiser != first {
		t.Error("second Set(true) replaced the advertiser; want idempotent no-op")
	}
	m.Set(false)
	if m.advertiser != nil {
		t.Error("Set(false) didn't clear advertiser")
	}
	m.Set(false) // second Set(false) — no-op
	// (no further state to assert; reaching here without panic is the pin)
}

// TestMDNSLifecycleCloseIsSafeFromMultipleGoroutines: Close +
// Set racing must not deadlock or double-close.
func TestMDNSLifecycleCloseIsSafeFromMultipleGoroutines(t *testing.T) {
	cfg := bridgemdns.Config{
		InstanceName:    "Test",
		Port:            17789,
		ProtocolVersion: 1,
		LibraryName:     "Test",
	}
	m := newMDNSLifecycle(cfg, &bytes.Buffer{}, &bytes.Buffer{})
	m.Set(true)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				m.Set(false)
			} else {
				m.Close()
			}
		}(i)
	}
	wg.Wait()
	if m.advertiser != nil {
		t.Error("advertiser should be nil after the race")
	}
}

// TestMDNSLifecycleSetReportsViaStdout pins the operator-facing
// log line so a panic-clicker in the admin UI sees the runtime
// state confirming their action landed.
func TestMDNSLifecycleSetReportsViaStdout(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cfg := bridgemdns.Config{
		InstanceName:    "Logged Lib",
		Port:            17790,
		ProtocolVersion: 1,
		LibraryName:     "Logged Lib",
	}
	m := newMDNSLifecycle(cfg, stdout, stderr)
	defer m.Close()
	m.Set(true)
	if !strings.Contains(stdout.String(), "advertising") {
		t.Errorf("Set(true) didn't log; stdout=%q", stdout.String())
	}
	m.Set(false)
	if !strings.Contains(stdout.String(), "stopped") {
		t.Errorf("Set(false) didn't log; stdout=%q", stdout.String())
	}
}
