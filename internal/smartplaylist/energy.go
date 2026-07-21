package smartplaylist

import (
	"hash/fnv"
	"math"
	"sort"
)

// Energy-envelope tuning. The envelope is a per-mix loudness contour (one bar
// per member track, downsampled to energyMaxBars) the iOS client renders as
// the "waveform-signed cover" halo. Loudness is mapped LINEARLY against a
// clamped dB window — NOT 10^(dB/20) — because replaygain dB is already
// perceptual; an amplitude conversion would collapse most of the range to
// near-zero and flatten the spline (Gemini consult on the Home-redesign plan).
const (
	energyFloorDB       = -24.0 // dB mapped to 0.0
	energyCeilingDB     = 0.0   // dB mapped to 1.0
	energyMidpoint      = 0.5   // value for a track with no loudness
	energyMaxBars       = 48
	energyVarianceFloor = 0.05 // below this, inject micro-noise so a brickwalled mix isn't a featureless ring
	energyNoiseAmp      = 0.04 // ±4% deterministic, seeded per (slug,index)
)

// LoudnessToEnergy maps a replaygain track-gain (dB, typically negative) to a
// 0..1 coefficient by LINEAR interpolation across [floor, ceiling].
func LoudnessToEnergy(db float64) float64 {
	v := (db - energyFloorDB) / (energyCeilingDB - energyFloorDB)
	return clamp01(v)
}

// EnergyEnvelope builds the normalized 0..1 contour from per-track loudness
// (nil = unknown → midpoint), downsampling to at most energyMaxBars and, when
// the result is near-flat, injecting deterministic per-index micro-noise so
// the cover's "crown" stays organic even for a brickwalled compilation. seed
// makes the noise stable per family.
//
// Returns nil when fewer than HALF the members carry loudness — the contour
// would be mostly midpoints (meaningless), so the client renders its richer
// seeded waveform fallback instead.
func EnergyEnvelope(loudness []*float64, seed uint64) []float64 {
	if len(loudness) == 0 {
		return nil
	}
	known := 0
	for _, db := range loudness {
		if db != nil {
			known++
		}
	}
	if known*2 < len(loudness) {
		return nil
	}
	raw := make([]float64, len(loudness))
	for i, db := range loudness {
		if db == nil {
			raw[i] = energyMidpoint
		} else {
			raw[i] = LoudnessToEnergy(*db)
		}
	}
	env := downsampleAverage(raw, energyMaxBars)
	if variance(env) < energyVarianceFloor {
		applySeededNoise(env, seed)
	}
	return env
}

// ModalRateHz returns the most-frequent sample rate, breaking ties toward the
// HIGHEST rate so a split mix biases the iOS glow toward the high-res
// (audiophile) color. Zero/negative rates are ignored; 0 when none are known.
func ModalRateHz(rates []int) int {
	counts := map[int]int{}
	for _, r := range rates {
		if r > 0 {
			counts[r]++
		}
	}
	if len(counts) == 0 {
		return 0
	}
	bestRate, bestCount := 0, -1
	// Iterate in descending rate order so the highest rate wins a tie.
	uniq := make([]int, 0, len(counts))
	for r := range counts {
		uniq = append(uniq, r)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(uniq)))
	for _, r := range uniq {
		if counts[r] > bestCount {
			bestRate, bestCount = r, counts[r]
		}
	}
	return bestRate
}

// --- helpers ---

func clamp01(v float64) float64 {
	// NaN fails BOTH comparisons below and would pass straight through
	// into the persisted Energy []float64, where json.Marshal rejects
	// it and aborts an entire Smart Mix regeneration. Map it to the
	// same midpoint an unknown-loudness track gets.
	if math.IsNaN(v) {
		return energyMidpoint
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// downsampleAverage reduces src to at most maxBars by averaging contiguous
// buckets (preserving the loudness contour's shape). src shorter than maxBars
// is returned as-is.
func downsampleAverage(src []float64, maxBars int) []float64 {
	if len(src) <= maxBars {
		out := make([]float64, len(src))
		copy(out, src)
		return out
	}
	out := make([]float64, maxBars)
	n := float64(len(src))
	for b := 0; b < maxBars; b++ {
		start := int(float64(b) * n / float64(maxBars))
		end := int(float64(b+1) * n / float64(maxBars))
		if end <= start {
			end = start + 1
		}
		var sum float64
		for i := start; i < end && i < len(src); i++ {
			sum += src[i]
		}
		out[b] = sum / float64(end-start)
	}
	return out
}

func variance(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var v float64
	for _, x := range xs {
		d := x - mean
		v += d * d
	}
	return v / float64(len(xs))
}

// applySeededNoise nudges each element by a deterministic ±energyNoiseAmp,
// derived from (seed, index), keeping values in [0,1].
func applySeededNoise(env []float64, seed uint64) {
	for i := range env {
		// Map the hash to [-1, 1], scale by the amplitude.
		frac := float64(fnv1a64(seed, uint64(i))%10001)/10000.0*2 - 1
		env[i] = clamp01(env[i] + frac*energyNoiseAmp)
	}
}

// fnv1a64 is an inlined, allocation-free FNV-1a hash over two big-endian uint64s.
// It avoids fnv.New64a()'s interface allocation and the buffer escape h.Write
// forces — mirroring hashSeedPath's inlined hash (PR #431). The byte order (seed
// MSB→LSB, then index) makes it bit-identical to hashing putUint64(seed) followed
// by putUint64(index) through fnv.New64a, so the noise pattern is unchanged.
func fnv1a64(a, b uint64) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	// Unrolled over a then b (no array, no outer loop) so the compiler can't
	// leave any allocation on the table (Gemini round-3). Pinned by
	// TestFnv1a64_ZeroAllocMatchesReference.
	hash := offset
	for s := 56; s >= 0; s -= 8 {
		hash ^= (a >> s) & 0xff
		hash *= prime
	}
	for s := 56; s >= 0; s -= 8 {
		hash ^= (b >> s) & 0xff
		hash *= prime
	}
	return hash
}

func putUint64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * (7 - i)))
	}
}

// SeedFromSlug is a stable per-family seed for the energy noise.
func SeedFromSlug(slug string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(slug))
	return h.Sum64()
}

// SeedFromISOWeek returns a deterministic seed for a UTC ISO year-week pair.
// The regenerator computes (year, week) via time.Unix(0, nowNS).UTC().ISOWeek()
// and passes them in — keeping the engine package clock-free (no `time`
// import). Stable for a 7-day window, rotates on Monday-UTC. Used by the
// deterministic weekly shuffle for Artist Deep Cuts + the two mood bands.
func SeedFromISOWeek(year, week int) uint64 {
	h := fnv.New64a()
	var buf [16]byte
	putUint64(buf[0:8], uint64(year))
	putUint64(buf[8:16], uint64(week))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}
