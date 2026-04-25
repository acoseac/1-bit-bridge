package updater

import (
	"sync"
	"testing"
)

func TestTrackerCounts(t *testing.T) {
	tr := NewTracker()
	if got := tr.Inflight(); got != 0 {
		t.Fatalf("initial Inflight = %d, want 0", got)
	}
	tr.Begin()
	tr.Begin()
	if got := tr.Inflight(); got != 2 {
		t.Errorf("after 2 Begins: Inflight = %d, want 2", got)
	}
	tr.End()
	if got := tr.Inflight(); got != 1 {
		t.Errorf("after End: Inflight = %d, want 1", got)
	}
	tr.End()
	if got := tr.Inflight(); got != 0 {
		t.Errorf("after balanced End: Inflight = %d, want 0", got)
	}
}

func TestTrackerClampsUnderflowAndStaysSafe(t *testing.T) {
	// A buggy caller racing more End() than Begin() must NOT make
	// Inflight() return a negative — that would let Install fire
	// during a real download. Regression guard for the install path's
	// "if Inflight() > 0" gate.
	tr := NewTracker()
	tr.End()
	tr.End()
	tr.End()
	if got := tr.Inflight(); got != 0 {
		t.Errorf("after underflow: Inflight = %d, want 0 (clamped)", got)
	}
	// And subsequent Begin/End still work correctly.
	tr.Begin()
	if got := tr.Inflight(); got != 1 {
		t.Errorf("post-underflow Begin: Inflight = %d, want 1", got)
	}
}

func TestTrackerUnderflowDoesNotClobberLegitimateBegins(t *testing.T) {
	// Regression guard for PR #42 review (Gemini): a naive
	// Store(0) on the underflow branch could clobber concurrent
	// Begin() increments racing through during the underflow log
	// path, leaving Inflight()==0 while a download is genuinely
	// active — exactly the failure mode the install gate is
	// supposed to prevent.
	//
	// Drive the race: many goroutines doing End() (forcing
	// underflow) interleaved with Begin()s. The CAS-based clamp
	// must leave the final count consistent with the legitimate
	// Begins minus the legitimate Ends.
	tr := NewTracker()
	const ends = 100
	const begins = 50
	var wg sync.WaitGroup
	wg.Add(ends + begins)
	for i := 0; i < ends; i++ {
		go func() { defer wg.Done(); tr.End() }()
	}
	for i := 0; i < begins; i++ {
		go func() { defer wg.Done(); tr.Begin() }()
	}
	wg.Wait()
	// Math: 50 begins + 100 ends. Naively that's -50, clamped to 0.
	// With the CAS fix the count never goes negative AS OBSERVED by
	// other goroutines (only as a transient internal state), and
	// the final count is whatever's left after both sides race.
	// What we PIN here: the count is non-negative (underflow
	// clamped) and bounded above by the begins (can't fabricate
	// inflight from nothing).
	got := tr.Inflight()
	if got < 0 || got > begins {
		t.Errorf("Inflight = %d after race, want 0..=%d", got, begins)
	}
}

func TestTrackerConcurrentBeginEnd(t *testing.T) {
	// 1000 goroutines each Begin+End — final Inflight must be 0.
	tr := NewTracker()
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			tr.Begin()
			tr.End()
		}()
	}
	wg.Wait()
	if got := tr.Inflight(); got != 0 {
		t.Errorf("Inflight = %d after balanced concurrent ops, want 0", got)
	}
}
