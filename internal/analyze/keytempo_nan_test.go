package analyze

import (
	"math"
	"testing"
)

// TestKeyTempo_NonFiniteInputYieldsNoBogusKey is the B2 regression guard: a
// corrupt decode feeding NaN/Inf must NOT commit a confident key. Without the
// input sanitize, a NaN chroma slips estimateKey's `sum <= 0` gate and returns
// a bogus (C major, ok=true); sanitized to 0, the chroma stays empty → no key.
func TestKeyTempo_NonFiniteInputYieldsNoBogusKey(t *testing.T) {
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		kt := newKeyTempoAnalyzer()
		for i := 0; i < (minWindowsForKey+50)*stftWindow; i++ {
			kt.add(bad)
		}
		if kt.windows < minWindowsForKey {
			t.Fatalf("bad=%v: processed %d windows, need >= %d for a meaningful test", bad, kt.windows, minWindowsForKey)
		}
		if root, mode, ok := kt.estimateKey(); ok {
			t.Errorf("bad=%v: estimateKey returned a confident key (%d, %q) from non-finite input — the guard regressed", bad, root, mode)
		}
	}
}
