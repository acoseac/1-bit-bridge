package analyze

import "math"

// EBU R128 / ITU-R BS.1770-4 integrated loudness, computed from streaming
// 48 kHz PCM. The result drives a signal-derived ReplayGain value the
// bridge surfaces only when the source carries no ReplayGain tag.
//
// **48 kHz is load-bearing**: the K-weighting biquad coefficients below
// are the spec's 48 kHz values. The analyze decode path forces 48 kHz, so
// they apply directly (no per-rate bilinear-transform recompute).
//
// Mono and stereo (the overwhelming majority of music) are measured
// exactly — every channel weight is 1.0, matching the spec. True
// multichannel (5.1+) loudness is a documented approximation here: we do
// NOT apply the surround +1.5 dB weighting or exclude the LFE channel
// (channel-order assumptions are risky and 5.1 music is vanishingly rare
// in these libraries). See channelWeights.

const (
	r128BlockSamples = AnalysisSampleRate * 400 / 1000 // 19200 — 400 ms momentary block
	r128HopSamples   = AnalysisSampleRate * 100 / 1000 // 4800  — 100 ms hop (75% overlap)

	absGateLUFS = -70.0 // absolute gate
	relGateLU   = -10.0 // relative gate offset below the gated mean

	// replayGainRefLUFS is the ReplayGain 2.0 reference loudness. The
	// track gain that brings a measured program to this reference is
	// `ref - measured`.
	replayGainRefLUFS = -18.0
)

// biquad is a transposed-direct-form-II second-order section. `a1`/`a2`
// are the denominator coefficients in `y = b0·x + b1·x₋₁ + b2·x₋₂ −
// a1·y₋₁ − a2·y₋₂` form (a0 normalised to 1).
type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.z1
	f.z1 = f.b1*x - f.a1*y + f.z2
	f.z2 = f.b2*x - f.a2*y
	return y
}

// kWeightStage1 is the BS.1770 "pre-filter" high-shelf (48 kHz).
func kWeightStage1() biquad {
	return biquad{
		b0: 1.53512485958697, b1: -2.69169618940638, b2: 1.19839281085285,
		a1: -1.69065929318241, a2: 0.73248077421585,
	}
}

// kWeightStage2 is the BS.1770 "RLB" high-pass (48 kHz).
func kWeightStage2() biquad {
	return biquad{
		b0: 1.0, b1: -2.0, b2: 1.0,
		a1: -1.99004745483398, a2: 0.99007225036621,
	}
}

// channelWeights returns the per-channel G weight. 1.0 for every channel
// — exact for mono/stereo; approximate for multichannel (see the package
// docblock).
func channelWeights(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 1.0
	}
	return w
}

// loudnessMeter accumulates EBU R128 momentary-block energies from
// streaming interleaved n-channel 48 kHz frames, then gates them into an
// integrated loudness. It holds only the per-channel K-weight filter
// state + a sliding-window square-sum + the block-energy slice — never
// the whole signal.
type loudnessMeter struct {
	channels int
	weights  []float64
	stage1   []biquad
	stage2   []biquad

	// Sliding 400 ms window of K-filtered sample squares, per channel,
	// with a running sum so each block is O(1) to read.
	ringSq [][]float64
	sumSq  []float64
	pos    int
	total  int64

	blockEnergies []float64 // channel-weighted mean-square per 400 ms block
}

func newLoudnessMeter(channels int) *loudnessMeter {
	if channels < 1 {
		channels = 1
	}
	m := &loudnessMeter{
		channels: channels,
		weights:  channelWeights(channels),
		stage1:   make([]biquad, channels),
		stage2:   make([]biquad, channels),
		ringSq:   make([][]float64, channels),
		sumSq:    make([]float64, channels),
	}
	for ch := 0; ch < channels; ch++ {
		m.stage1[ch] = kWeightStage1()
		m.stage2[ch] = kWeightStage2()
		m.ringSq[ch] = make([]float64, r128BlockSamples)
	}
	return m
}

// addFrame folds one interleaved frame (one sample per channel) into the
// meter. Extra samples beyond `channels` are ignored; missing ones are
// treated as 0.
func (m *loudnessMeter) addFrame(frame []float64) {
	for ch := 0; ch < m.channels; ch++ {
		var x float64
		if ch < len(frame) {
			x = frame[ch]
		}
		f := m.stage2[ch].process(m.stage1[ch].process(x))
		sq := f * f
		m.sumSq[ch] += sq - m.ringSq[ch][m.pos]
		m.ringSq[ch][m.pos] = sq
	}
	m.pos++
	if m.pos == r128BlockSamples {
		m.pos = 0
	}
	m.total++
	// Emit a block once the window is full, then every hop.
	if m.total >= r128BlockSamples && (m.total-r128BlockSamples)%r128HopSamples == 0 {
		var energy float64
		for ch := 0; ch < m.channels; ch++ {
			energy += m.weights[ch] * (m.sumSq[ch] / float64(r128BlockSamples))
		}
		m.blockEnergies = append(m.blockEnergies, energy)
	}
}

// energyToLUFS converts a channel-weighted mean-square to loudness.
func energyToLUFS(e float64) float64 {
	if e <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(e)
}

// integratedLUFS applies the BS.1770 two-stage gating (absolute −70 LUFS,
// then relative −10 LU below the gated mean) and returns the integrated
// loudness, or −Inf when nothing survives (silence / too-short input).
func (m *loudnessMeter) integratedLUFS() float64 {
	if len(m.blockEnergies) == 0 {
		return math.Inf(-1)
	}
	// Absolute gate.
	absKept := make([]float64, 0, len(m.blockEnergies))
	for _, e := range m.blockEnergies {
		if energyToLUFS(e) >= absGateLUFS {
			absKept = append(absKept, e)
		}
	}
	if len(absKept) == 0 {
		return math.Inf(-1)
	}
	// Relative gate: threshold = loudness of the abs-gated mean energy − 10 LU.
	var sum float64
	for _, e := range absKept {
		sum += e
	}
	relThresh := energyToLUFS(sum/float64(len(absKept))) + relGateLU
	var relSum float64
	var relN int
	for _, e := range absKept {
		if energyToLUFS(e) >= relThresh {
			relSum += e
			relN++
		}
	}
	if relN == 0 {
		return math.Inf(-1)
	}
	return energyToLUFS(relSum / float64(relN))
}

// replayGainFromLUFS converts an integrated loudness to the ReplayGain
// 2.0 track gain (dB) — the gain that brings the program to the −18 LUFS
// reference. Returns (0, false) for a non-finite loudness (silence).
func replayGainFromLUFS(lufs float64) (float64, bool) {
	if math.IsInf(lufs, 0) || math.IsNaN(lufs) {
		return 0, false
	}
	return replayGainRefLUFS - lufs, true
}
