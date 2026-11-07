package dsdconv

import (
	"fmt"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The chain's fixed-point scale: coefficients and the stage outputs carry
// 2^30 for 1.0, and stage 3's sum 2^60.
const scaleBits = 30

// ditherSeed seeds the 16-bit plans' xorshift64 at each occurrence's first
// frame.
const ditherSeed uint64 = 0x9E3779B97F4A7C15

// attenuationDB is every stage's stopband by Kaiser's window and estimate:
// 110 dB, 10 dB under the mask's 100 (B728 §4.2), so the starting sizes of
// §4.2 come from one formula; TestTheResponseMeetsItsMask measures what the
// coefficients give.
const attenuationDB = 110

// The rates of the chain's stages: stage 1 decimates to 352.8 kHz, stage 2
// to 176.4 kHz, stage 3 to the plan's rate.
const (
	stage1RateHz = 352800
	stage2RateHz = 176400
)

// keepHz is the band stages 1 and 2 hold clean: the widest plan's stopband
// start, since stage 3 removes everything above it.
func keepHz() int { return Plans()[0].StopbandHz() }

// geometry is one DSD rate's chain under one plan.
type geometry struct {
	bitRate, d, t, m1, m3, c3, h, r int
	// c2 is stage 2's centre.
	c2 int
	// g1 is stage 1 over its T-byte window's 8T slots, slot 0 zero and the
	// filter centred on slot 4T; g2 is the half-band, centred on c2; g3 is
	// stage 3, centred on c3. Each sums to exactly 2^30.
	g1, g2, g3 []int64
	// tabs is stage 1's byte tables: tabs[j][b] is window byte j's eight
	// taps summed for the byte b, bit 7 earliest, a 1 bit +1 and a 0 bit -1.
	tabs [][256]int32
	// pairs2 are the offsets k > 0 from stage 2's centre whose taps are not
	// zero (a half-band's are odd), and h2 their taps.
	pairs2 []int
	h2     []int64
	// h3 is stage 3 from its centre on: h3[k] is the tap k from it.
	h3 []int64
}

var cache struct {
	sync.Mutex
	stage1 map[int]*geometry // by bit rate: g1, tabs, t, m1
	stage2 []int64
	stage3 map[[2]int][]int64 // by passband and stopband
	geo    map[[2]int]*geometry
}

// takesRate reports whether the converter takes a DSD rate: DSD64, DSD128
// and DSD256.
func takesRate(bitRateHz int) bool {
	switch bitRateHz {
	case format.DSD64RateHz, 2 * format.DSD64RateHz, 4 * format.DSD64RateHz:
		return true
	}
	return false
}

// geometryFor is the chain at a DSD rate under a plan, designed once and
// kept for the process.
func geometryFor(bitRateHz int, p Plan) (*geometry, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if !takesRate(bitRateHz) {
		return nil, &RateError{BitRateHz: bitRateHz}
	}
	cache.Lock()
	defer cache.Unlock()
	key := [2]int{bitRateHz, p.RateHz}
	if g := cache.geo[key]; g != nil {
		return g, nil
	}
	if cache.geo == nil {
		cache.geo = map[[2]int]*geometry{}
		cache.stage1 = map[int]*geometry{}
		cache.stage3 = map[[2]int][]int64{}
	}
	s1 := cache.stage1[bitRateHz]
	if s1 == nil {
		s1 = designStage1(bitRateHz)
		cache.stage1[bitRateHz] = s1
	}
	if cache.stage2 == nil {
		cache.stage2 = designStage2()
	}
	band := [2]int{p.PassbandHz(), p.StopbandHz()}
	g3 := cache.stage3[band]
	if g3 == nil {
		g3 = designStage3(band[0], band[1])
		cache.stage3[band] = g3
	}
	g := &geometry{
		bitRate: bitRateHz, d: bitRateHz / p.RateHz, t: s1.t, m1: s1.m1, m3: stage2RateHz / p.RateHz,
		g1: s1.g1, tabs: s1.tabs, g2: cache.stage2, g3: g3,
		c2: (len(cache.stage2) - 1) / 2, c3: (len(g3) - 1) / 2,
	}
	for k := 1; k <= g.c2; k++ {
		if v := g.g2[g.c2+k]; v != 0 {
			g.pairs2 = append(g.pairs2, k)
			g.h2 = append(g.h2, v)
		}
	}
	g.h3 = g.g3[g.c3:]
	// The right reach in bits of output frame k's centre, k·D: stage 3's
	// half-length in stage-2 samples, stage 2's in stage-1 samples and
	// stage 1's own; H is the frames whose reach passes a join.
	g.r = g.c3*2*g.m1 + g.c2*g.m1 + 4*g.t - 1
	g.h = g.r / g.d
	cache.geo[key] = g
	return g, nil
}

// designStage1 is stage 1 at a DSD rate: a low-pass to 352.8 kHz holding
// the bands that fold into the kept band, k·352.8 kHz ± keepHz, so a
// transition from keepHz to 352.8 kHz - keepHz (272.8 kHz), centred on
// 176.4 kHz. Its taps are Kaiser's estimate rounded up to T tables of eight,
// T even, less one: an odd count, so the window's centre is a slot (4T),
// which is what centres output frame k on bit k·D, and slot 0 is zero. 79,
// 159 and 303 taps at DSD64, DSD128 and DSD256.
func designStage1(bitRateHz int) *geometry {
	min := kaiserTaps(attenuationDB, stage1RateHz-2*keepHz(), bitRateHz)
	t := (min + 1 + 7) / 8
	if t%2 != 0 {
		t++
	}
	taps := lowpass(8*t-1, stage1RateHz/2, bitRateHz, attenuationDB)
	g := &geometry{t: t, m1: bitRateHz / stage1RateHz, g1: make([]int64, 8*t), tabs: make([][256]int32, t)}
	copy(g.g1[1:], taps)
	for j := 0; j < t; j++ {
		for b := 0; b < 256; b++ {
			var v int64
			for i := 0; i < 8; i++ {
				if b&(0x80>>i) != 0 {
					v += g.g1[8*j+i]
				} else {
					v -= g.g1[8*j+i]
				}
			}
			if v != int64(int32(v)) {
				panic(fmt.Sprintf("dsdconv: stage 1's table %d at %d Hz sums %d, past an int32", j, bitRateHz, v))
			}
			g.tabs[j][b] = int32(v)
		}
	}
	return g
}

// designStage2 is the half-band from 352.8 to 176.4 kHz, cutoff a quarter
// of its rate: it passes the kept band and holds 176.4 kHz - keepHz on
// (136.4 kHz), which is what folds into the kept band at its decimation.
// Its taps are Kaiser's estimate rounded up to 4L - 1, a half-band's: 31.
func designStage2() []int64 {
	min := kaiserTaps(attenuationDB, stage1RateHz/2-2*keepHz(), stage1RateHz)
	taps := 3
	for taps < min {
		taps += 4
	}
	return lowpass(taps, 1, 4, attenuationDB)
}

// designStage3 is the plan's low-pass at 176.4 kHz, from its passband to
// its stopband, cut at their centre. Its taps are Kaiser's estimate, made
// odd: 127 for 30 kHz and 40 kHz, and 613 for 20 kHz and 22.05 kHz, whose
// transition is a fifth as wide.
func designStage3(passHz, stopHz int) []int64 {
	taps := kaiserTaps(attenuationDB, stopHz-passHz, stage2RateHz)
	if taps%2 == 0 {
		taps++
	}
	return lowpass(taps, passHz+stopHz, 2*stage2RateHz, attenuationDB)
}
