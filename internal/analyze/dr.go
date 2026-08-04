package analyze

import (
	"math"
	"sort"
)

// drMeter computes the community DR score (the Pleasurize Music
// Foundation / TT DR Offline Meter algorithm, the "DR12" numbers audio
// enthusiasts trade): per channel, the program is cut into 3-second
// blocks; each block records its RMS (with the meter's characteristic
// sqrt(2) factor) and its sample peak; the DR value is the dB distance
// between the SECOND-highest block peak and the energy-average RMS of the
// loudest 20% of blocks, averaged across channels and rounded to an
// integer.
//
// Second-highest peak and top-20% RMS are both the published algorithm's
// own outlier defenses — a single digital over or one hot transient must
// not define the score. Rides the existing 48 kHz analysis decode (block
// boundaries at 48 kHz land within one sample of the reference meter's).
type drMeter struct {
	channels     int
	blockSamples int
	// Per-channel accumulators for the block in progress.
	n     int
	sumSq []float64
	curPk []float64
	// Per-channel per-block records: linear RMS (already x sqrt2) and peak.
	rms   [][]float64
	peaks [][]float64
}

const (
	drBlockSeconds = 3
	// drMinBlocks gates the score: below ~9 s of program the top-20%
	// selection is a single block and the statistic is meaningless.
	drMinBlocks = 3
	// drPartialBlockMinFraction: the final partial block still counts when
	// it holds at least a third of a block (1 s) — matching the reference
	// meter's tolerance for track tails without letting a 50 ms stub
	// contribute a garbage RMS.
	drPartialBlockMinFraction = 3
)

func newDRMeter(channels int) *drMeter {
	if channels < 1 {
		channels = 1
	}
	m := &drMeter{
		channels:     channels,
		blockSamples: drBlockSeconds * AnalysisSampleRate,
		sumSq:        make([]float64, channels),
		curPk:        make([]float64, channels),
		rms:          make([][]float64, channels),
		peaks:        make([][]float64, channels),
	}
	return m
}

func (m *drMeter) addFrame(frame []float64) {
	if len(frame) != m.channels {
		return
	}
	for c, x := range frame {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			x = 0
		}
		m.sumSq[c] += x * x
		if a := math.Abs(x); a > m.curPk[c] {
			m.curPk[c] = a
		}
	}
	m.n++
	if m.n >= m.blockSamples {
		m.flushBlock()
	}
}

func (m *drMeter) flushBlock() {
	if m.n == 0 {
		return
	}
	for c := 0; c < m.channels; c++ {
		// The TT meter's RMS carries a sqrt(2) factor: rms = sqrt(2*sum/n).
		m.rms[c] = append(m.rms[c], math.Sqrt(2*m.sumSq[c]/float64(m.n)))
		m.peaks[c] = append(m.peaks[c], m.curPk[c])
		m.sumSq[c] = 0
		m.curPk[c] = 0
	}
	m.n = 0
}

// finish folds the trailing partial block (when long enough to be
// meaningful) into the records.
func (m *drMeter) finish() {
	if m.n >= m.blockSamples/drPartialBlockMinFraction {
		m.flushBlock()
	}
	m.n = 0
}

// score returns the rounded DR value, or ok=false when the program is too
// short (fewer than drMinBlocks blocks) or every channel is silent.
//
// A silent channel is SKIPPED, not fatal. `flushBlock` appends to every
// channel on the same tick, so block COUNT is a whole-program property
// and `blocks < drMinBlocks` rightly returns for the track — but "this
// channel carries no signal" is per-channel, and one such channel used
// to return from the function and suppress the score for the whole
// track. A 5.1 rip with an unused LFE, a one-dead-channel LP transfer,
// or a mono master laid into a stereo container all landed there: a
// DRScore of nil, committed with the schema stamp, so the scan-skip
// gate never looked again.
//
// The `counted` accumulator below is the tell that this was an
// oversight rather than a decision — it and its `counted == 0` guard
// only mean anything if channels can drop out individually. Until now
// `counted` was necessarily equal to m.channels and the guard was
// unreachable.
func (m *drMeter) score() (int, bool) {
	total := 0.0
	counted := 0
	for c := 0; c < m.channels; c++ {
		blocks := len(m.rms[c])
		if blocks < drMinBlocks {
			// Whole-track: every channel has the same block count.
			return 0, false
		}
		// Loudest 20% of blocks by RMS, energy-averaged.
		sorted := append([]float64(nil), m.rms[c]...)
		sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
		n20 := blocks / 5
		if n20 < 1 {
			n20 = 1
		}
		energy := 0.0
		for i := 0; i < n20; i++ {
			energy += sorted[i] * sorted[i]
		}
		rms20 := math.Sqrt(energy / float64(n20))
		// Second-highest block peak (the algorithm's over-tolerance).
		pks := append([]float64(nil), m.peaks[c]...)
		sort.Sort(sort.Reverse(sort.Float64Slice(pks)))
		peak := pks[0]
		if len(pks) >= 2 {
			peak = pks[1]
		}
		if peak <= 0 || rms20 <= 0 {
			// This channel is digitally silent — no dynamic range to
			// measure. Skip it rather than the track, and do NOT fold a
			// zero into the average: a silent channel has no DR, which
			// is not the same as a DR of zero (zero means maximally
			// compressed, the loudness-war reading).
			continue
		}
		total += 20 * math.Log10(peak/rms20)
		counted++
	}
	if counted == 0 {
		// Every channel silent — now reachable, and the correct answer:
		// there is no dynamic range to report, so report nothing rather
		// than a fabricated 0.
		return 0, false
	}
	dr := int(math.Round(total / float64(counted)))
	if dr < 0 {
		dr = 0
	}
	if dr > 30 {
		dr = 30
	}
	return dr, true
}
