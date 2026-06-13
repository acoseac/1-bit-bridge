package analyze

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// peaker reduces a streaming PCM signal to a min/max peak envelope over
// fixed-width sample buckets. It never holds the whole decoded signal —
// only the running bucket extents plus the int8-quantised result
// slices — so a long track stays flat in memory.
type peaker struct {
	bucketSamples int
	curMin        float32
	curMax        float32
	n             int
	mins          []int8
	maxs          []int8
}

func newPeaker(bucketSamples int) *peaker {
	if bucketSamples < 1 {
		bucketSamples = 1
	}
	return &peaker{bucketSamples: bucketSamples}
}

// add folds one sample into the current bucket, flushing when the
// bucket fills.
func (p *peaker) add(s float32) {
	// A NaN sample (corrupt decode) would poison the whole bucket: it
	// propagates through min/max and `int(NaN)` in quantizeSample is
	// undefined. Treat it as silence. Gemini on #395.
	if math.IsNaN(float64(s)) {
		s = 0
	}
	if p.n == 0 {
		p.curMin, p.curMax = s, s
	} else {
		if s < p.curMin {
			p.curMin = s
		}
		if s > p.curMax {
			p.curMax = s
		}
	}
	p.n++
	if p.n >= p.bucketSamples {
		p.flush()
	}
}

func (p *peaker) flush() {
	if p.n == 0 {
		return
	}
	// Bound memory against a pathologically long input; beyond the cap
	// we keep consuming samples (so totalSamples / duration stays
	// correct) but stop growing the envelope.
	if len(p.mins) < maxWaveformBuckets {
		p.mins = append(p.mins, quantizeSample(p.curMin))
		p.maxs = append(p.maxs, quantizeSample(p.curMax))
	}
	p.n = 0
}

// finish flushes the trailing partial bucket. Call once after the last
// add().
func (p *peaker) finish() { p.flush() }

func (p *peaker) count() int { return len(p.mins) }

// quantizeSample maps a float sample in [-1, 1] to a symmetric int8
// peak in [-127, 127]. Out-of-range inputs (intersample peaks above
// 0 dBFS, or a decoder emitting slightly >1.0) clamp rather than wrap.
func quantizeSample(v float32) int8 {
	// Belt-and-braces NaN guard: `int(math.Round(NaN))` is undefined in
	// Go. The peaker already maps NaN→0 on input, but a direct caller
	// shouldn't hit UB either.
	if math.IsNaN(float64(v)) {
		return 0
	}
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	q := int(math.Round(float64(v) * 127))
	if q > 127 {
		q = 127
	} else if q < -127 {
		q = -127
	}
	return int8(q)
}

// encodeWaveform serialises the peaker envelope into the compact binary
// sidecar format:
//
//	offset  size  field
//	0       4     magic "1BWF"
//	4       1     format version
//	5       1     flags (0)
//	6       4     uint32 LE  sample rate (Hz)
//	10      4     uint32 LE  samples per bucket
//	14      4     uint32 LE  bucket count
//	18      4     uint32 LE  duration (ms)
//	22      2*N   int8 pairs (min, max) per bucket
//
// iOS maps bucket i → (i / count) * duration for time alignment.
func encodeWaveform(p *peaker, rateHz, bucketSamples int, totalSamples int64) []byte {
	count := p.count()
	out := make([]byte, waveformHeaderLen, waveformHeaderLen+count*2)
	copy(out[0:4], waveformMagic)
	out[4] = waveformFormatVersion
	out[5] = 0 // flags
	binary.LittleEndian.PutUint32(out[6:10], uint32(rateHz))
	binary.LittleEndian.PutUint32(out[10:14], uint32(bucketSamples))
	binary.LittleEndian.PutUint32(out[14:18], uint32(count))
	var durationMS uint32
	if rateHz > 0 {
		durationMS = uint32(totalSamples * 1000 / int64(rateHz))
	}
	binary.LittleEndian.PutUint32(out[18:22], durationMS)
	for i := 0; i < count; i++ {
		out = append(out, byte(p.mins[i]), byte(p.maxs[i]))
	}
	return out
}

// waveformTag is the 8-hex content tag (first 4 bytes of SHA-256) of
// the sidecar bytes. Used by iOS as an immutable-cache key — a
// regenerated waveform (source edited) yields a new tag so the client
// re-fetches.
func waveformTag(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
}
