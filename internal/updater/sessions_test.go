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
