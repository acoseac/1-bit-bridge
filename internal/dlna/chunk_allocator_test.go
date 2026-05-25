package dlna

import (
	"math"
	"testing"
)

func TestChunkSizeFor(t *testing.T) {
	cases := []struct {
		name    string
		rtt     float64
		jitter  float64
		loss    float64
		wantMin int // inclusive lower bound on result
		wantMax int // inclusive upper bound on result
	}{
		// Nominal LAN — minimal jitter, no loss, low RTT
		{"nominal_lan", 1.0, 0.5, 0.0, MinChunkSize, MinChunkSize},
		{"zero_everything", 0.0, 0.0, 0.0, MinChunkSize, MinChunkSize},

		// Moderate jitter, no loss — multiplier slightly above 1
		{"moderate_jitter", 5.0, 5.0, 0.0, MinChunkSize, MinChunkSize + 32*1024},

		// High jitter dominates (quadratic)
		{"high_jitter", 5.0, 30.0, 0.0, MinChunkSize + 500*1024, MaxChunkSize},

		// Modest loss tightens
		{"1pct_loss", 5.0, 2.0, 1.0, MinChunkSize + 16*1024, MinChunkSize + 64*1024},
		{"5pct_loss", 5.0, 2.0, 5.0, MinChunkSize * 2, MaxChunkSize},

		// High RTT (e.g. tailnet across continents)
		{"high_rtt", 200.0, 1.0, 0.0, MinChunkSize + 32*1024, MinChunkSize + 96*1024},

		// Pathological — extreme everything clamps to ceiling
		{"all_extreme_clamps_to_max", 1000.0, 100.0, 50.0, MaxChunkSize, MaxChunkSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ChunkSizeFor(tc.rtt, tc.jitter, tc.loss)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("ChunkSizeFor(rtt=%.1f jitter=%.1f loss=%.1f) = %d, want [%d, %d]",
					tc.rtt, tc.jitter, tc.loss, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestChunkSizeFor_alwaysBounded(t *testing.T) {
	// Sweep a wide range — output must always be in [Min, Max]
	for rtt := -10.0; rtt <= 2000; rtt += 50 {
		for jitter := -1.0; jitter <= 200; jitter += 10 {
			for loss := -1.0; loss <= 100; loss += 5 {
				got := ChunkSizeFor(rtt, jitter, loss)
				if got < MinChunkSize || got > MaxChunkSize {
					t.Fatalf("out-of-bounds: ChunkSizeFor(%.1f, %.1f, %.1f) = %d, must be in [%d, %d]",
						rtt, jitter, loss, got, MinChunkSize, MaxChunkSize)
				}
			}
		}
	}
}

func TestChunkSizeFor_alwaysAligned(t *testing.T) {
	// Output must be 4KB-aligned for memory-allocator friendliness.
	const align = 4 * 1024
	for rtt := 0.0; rtt <= 500; rtt += 17 {
		for jitter := 0.0; jitter <= 50; jitter += 7 {
			for loss := 0.0; loss <= 20; loss += 1.5 {
				got := ChunkSizeFor(rtt, jitter, loss)
				if got%align != 0 {
					t.Errorf("misaligned: ChunkSizeFor(%.1f, %.1f, %.1f) = %d, not 4KB-aligned",
						rtt, jitter, loss, got)
				}
			}
		}
	}
}

// TestChunkSizeFor_defensiveInputsCollapseToDefault — bad telemetry
// inputs (NaN, Inf, negative) must NEVER produce a too-small chunk that
// would promote underrun. Default-on-failure is the safe direction.
func TestChunkSizeFor_defensiveInputsCollapseToDefault(t *testing.T) {
	bads := []struct{ rtt, jitter, loss float64 }{
		{math.NaN(), 0, 0},
		{0, math.NaN(), 0},
		{0, 0, math.NaN()},
		{math.Inf(1), 0, 0},
		{math.Inf(-1), 0, 0},
		{-1, 0, 0},
		{0, -1, 0},
		{0, 0, -1},
	}
	for _, tc := range bads {
		got := ChunkSizeFor(tc.rtt, tc.jitter, tc.loss)
		if got != DefaultChunkSize {
			t.Errorf("ChunkSizeFor(%.1f, %.1f, %.1f) = %d, want DefaultChunkSize=%d (bad input must collapse safely)",
				tc.rtt, tc.jitter, tc.loss, got, DefaultChunkSize)
		}
	}
}

// TestChunkSizeFor_monotonicInJitter — increasing jitter (holding rtt
// and loss constant) must never DECREASE the chunk size. The model's
// purpose is to expand the chunk under stress; a non-monotonic curve
// would be a sign of coefficient drift.
func TestChunkSizeFor_monotonicInJitter(t *testing.T) {
	prev := ChunkSizeFor(5.0, 0.0, 0.0)
	for j := 1.0; j <= 50; j += 1.0 {
		cur := ChunkSizeFor(5.0, j, 0.0)
		if cur < prev {
			t.Fatalf("non-monotonic in jitter: at jitter=%.1f, ChunkSizeFor=%d < prev=%d", j, cur, prev)
		}
		prev = cur
	}
}
