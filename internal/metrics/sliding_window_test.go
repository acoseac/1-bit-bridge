package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestSlidingHistogram_EmptyReturnsZero(t *testing.T) {
	h := NewSlidingHistogram()
	p50, p99 := h.Snapshot()
	if p50 != 0 || p99 != 0 {
		t.Fatalf("empty Snapshot: want (0, 0), got (%v, %v)", p50, p99)
	}
}

func TestSlidingHistogram_QuantilesAcrossDistribution(t *testing.T) {
	h := NewSlidingHistogram()
	// 100 observations 0.001..0.100s. Spread is exact: each
	// value appears once.
	for i := 1; i <= 100; i++ {
		h.Observe(float64(i) / 1000.0)
	}
	p50, p99 := h.Snapshot()
	// p50 should land near 0.050; p99 near 0.099. Exact indices
	// depend on integer math; allow ±5 ms tolerance.
	if p50 < 0.045 || p50 > 0.055 {
		t.Errorf("p50: want ~0.050, got %v", p50)
	}
	if p99 < 0.094 || p99 > 0.100 {
		t.Errorf("p99: want ~0.099, got %v", p99)
	}
}

func TestSlidingHistogram_DropsObservationsPastFreshnessWindow(t *testing.T) {
	h := NewSlidingHistogram()
	// Seed an "old" observation by writing a Sample directly with
	// a long-past timestamp. Production observers stamp Date()
	// inside Observe; this is the only way to simulate aged data
	// in a fast unit test.
	h.mu.Lock()
	h.samples[0] = Sample{Value: 999_999, Timestamp: time.Now().Add(-5 * time.Minute)}
	h.cursor = 1
	h.mu.Unlock()
	// Add a fresh observation at 0.010s.
	h.Observe(0.010)
	p50, p99 := h.Snapshot()
	// Only the fresh observation survives the 60-s window — both
	// quantiles should reflect 0.010s, NOT the stale 999_999µs.
	if p50 > 0.020 || p99 > 0.020 {
		t.Fatalf("stale observation polluted quantiles: p50=%v p99=%v (expected ~0.010 for both)", p50, p99)
	}
}

func TestSlidingHistogram_ConcurrentObserveDoesNotTear(t *testing.T) {
	h := NewSlidingHistogram()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				h.Observe(0.001)
			}
		}()
	}
	wg.Wait()
	// 8 × 500 = 4000 observations into a 1024-slot ring; under
	// `go test -race` the absence of a torn write is enforced
	// by the race detector. The Snapshot() call additionally
	// proves the lock semantics hold.
	p50, _ := h.Snapshot()
	if p50 == 0 {
		t.Fatalf("Snapshot returned 0 after 4000 observations — concurrency issue")
	}
}

func TestSlidingHistogram_NegativeObservationClampedToZero(t *testing.T) {
	h := NewSlidingHistogram()
	h.Observe(-1.0)
	// Should not produce a wrapped uint64; quantile of [0] is 0.
	p50, _ := h.Snapshot()
	if p50 != 0 {
		t.Fatalf("negative observation produced non-zero p50: %v", p50)
	}
}
