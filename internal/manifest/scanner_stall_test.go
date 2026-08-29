package manifest

import (
	"testing"
	"time"
)

// The stall watchdog exists because a scan wedged in an uninterruptible
// read keeps `IsScanning` true forever, and clients defer every
// incremental sync while that is true — a silent, indefinite outage
// (2026-08-29, `fuse_open` on a network mount). These pin the two
// halves that matter: a healthy scan is never called stalled, and a
// scan that stops committing eventually is.

func TestScanStalledFor_ZeroWhenNoScanRunning(t *testing.T) {
	s := &Scanner{}
	if got := s.ScanStalledFor(time.Now()); got != 0 {
		t.Fatalf("no scan running should report 0, got %v", got)
	}
	if s.IsScanStalled(time.Now()) {
		t.Fatal("no scan running must never report stalled")
	}
}

func TestScanStalledFor_ZeroBeforeTheClockIsSeeded(t *testing.T) {
	// scanning=true but lastProgressAt unset: the window between the
	// flag going up and the seed landing must not read as a stall.
	s := &Scanner{}
	s.scanning.Store(true)
	if got := s.ScanStalledFor(time.Now()); got != 0 {
		t.Fatalf("unseeded clock should report 0, got %v", got)
	}
}

func TestIsScanStalled_HealthyScanIsNotStalled(t *testing.T) {
	s := &Scanner{}
	s.scanning.Store(true)
	now := time.Now()
	s.noteScanProgress(now)

	// Just under the threshold — still healthy.
	if s.IsScanStalled(now.Add(scanStallThreshold - time.Second)) {
		t.Fatal("a scan under the threshold must not be called stalled")
	}
}

func TestIsScanStalled_TripsPastTheThreshold(t *testing.T) {
	s := &Scanner{}
	s.scanning.Store(true)
	now := time.Now()
	s.noteScanProgress(now)

	if !s.IsScanStalled(now.Add(scanStallThreshold + time.Second)) {
		t.Fatal("a scan past the threshold with no commits must report stalled")
	}
}

func TestIsScanStalled_ProgressResetsTheClockAndTheLogLatch(t *testing.T) {
	s := &Scanner{}
	s.scanning.Store(true)
	start := time.Now()
	s.noteScanProgress(start)

	stalledAt := start.Add(scanStallThreshold + time.Second)
	if !s.IsScanStalled(stalledAt) {
		t.Fatal("precondition: should be stalled")
	}
	if !s.stallLogged.Load() {
		t.Fatal("the first stall observation should latch the log")
	}

	// The scan gets somewhere again: no longer stalled, and the latch
	// clears so a SECOND stall episode is logged rather than swallowed.
	s.noteScanProgress(stalledAt)
	if s.IsScanStalled(stalledAt.Add(time.Minute)) {
		t.Fatal("progress must clear the stall")
	}
	if s.stallLogged.Load() {
		t.Fatal("progress must clear the log latch so a second episode still logs")
	}
	if !s.IsScanStalled(stalledAt.Add(scanStallThreshold + time.Second)) {
		t.Fatal("a second stall episode must trip again")
	}
}

func TestScanStalledFor_BackwardClockDoesNotManufactureAStall(t *testing.T) {
	// NTP correction / a clock step must not invent a stall — the
	// failure direction here is "clients stop syncing", so the guard
	// fails toward healthy.
	s := &Scanner{}
	s.scanning.Store(true)
	now := time.Now()
	s.noteScanProgress(now)

	if got := s.ScanStalledFor(now.Add(-time.Hour)); got != 0 {
		t.Fatalf("a backward clock should report 0, got %v", got)
	}
	if s.IsScanStalled(now.Add(-time.Hour)) {
		t.Fatal("a backward clock must not report stalled")
	}
}

func TestScanStallThreshold_IsGenerous(t *testing.T) {
	// Pinned deliberately: a healthy scan can legitimately go minutes
	// without committing (walking a large tree before the first batch
	// fills). Shortening this is a behaviour change that needs the
	// docblock's asymmetry argument re-read, not a casual tweak.
	if scanStallThreshold < 10*time.Minute {
		t.Fatalf("threshold %v is too aggressive for a healthy slow scan", scanStallThreshold)
	}
}
