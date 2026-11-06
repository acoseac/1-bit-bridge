package dsdconv

import "errors"

// The chain's fixed-point scale: coefficients and the stage outputs carry
// 2^30 for 1.0, and stage 3's sum 2^60.
const scaleBits = 30

// ditherSeed seeds the 16-bit plans' xorshift64 at each occurrence's first
// frame.
const ditherSeed uint64 = 0x9E3779B97F4A7C15

// geometry is one DSD rate's chain under one plan.
type geometry struct {
	bitRate, d, t, m1, m3, c3, h, r int
	// g1 is stage 1 over its T-byte window's 8T slots, slot 0 zero and the
	// filter centred on slot 4T; g2 is the half-band, centred on 15; g3 is
	// stage 3, centred on c3. Each sums to exactly 2^30.
	g1, g2, g3 []int64
	// tab is stage 1's byte tables, tab[j*256+b] the sum of window byte j's
	// eight taps for the byte b, bit 7 earliest.
	tab []int32
}

func geometryFor(bitRateHz int, p Plan) (*geometry, error) {
	return nil, errors.New("dsdconv: no geometry yet")
}
