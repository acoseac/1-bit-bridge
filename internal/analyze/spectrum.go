package analyze

import (
	"encoding/binary"
	"math"
)

// Whole-track, time-averaged frequency spectrum — the measurement behind
// "is this file genuinely hi-res, or an upsampled CD rip?".
//
// Rides the STFT `keyTempoAnalyzer` already runs: the magnitudes are in
// registers at the end of every `processWindow`, and folding them into 60
// bands is arithmetic on data that was going to be discarded. No new decode,
// no second FFT.
//
// # The 24 kHz ceiling, and why the bandwidth is often absent
//
// This package decodes at 48 kHz (`AnalysisSampleRate`, the one-decode
// invariant), so it can see to 24 kHz and no further. That is enough for the
// question the feature exists to answer — a CD-sourced upsample cliffs at
// 22.05 kHz, comfortably inside — and NOT enough to tell a 96 kHz-native file
// from a 48 kHz-native one.
//
// So a measurement that runs into the ceiling is reported as NO measurement
// rather than as "24 kHz". Reporting the number would be actively harmful:
// 24 kHz is exactly 48 kHz's Nyquist, so a genuinely 96 kHz-native file would
// be read as "consistent with a 48 kHz source" — a false accusation
// manufactured by our own analysis rate. See `bandwidthCeilingGuardHz`.

const (
	// SpectrumBandCount is the number of log-spaced display bands.
	//
	// **60 is a cross-repo contract, not a tunable.** iOS draws the curve
	// this produces on the same axis as its live meter's 60 bars
	// (`MeterAnalysis.bandCount`), positioned by band index. A different
	// count here misaligns the overlay with no error on either side —
	// the silent failure the plan calls out. `TestSpectrumBandMapMatchesIOSFixture`
	// pins the whole mapping, not just the count.
	SpectrumBandCount = 60

	// spectrumMinHz is the low edge of the band map, matching iOS's
	// `makeBandMap(minimumHz:)` default.
	spectrumMinHz = 20.0

	// SpectrumSchemaVersion versions the `1BSP` blob's layout.
	SpectrumSchemaVersion = 1

	// spectrumContentFloorDB is how far below the loudest bin a bin may sit
	// and still count as carrying content — the same rule iOS applies, and
	// deliberately relative to the PEAK rather than to full scale or to a
	// share of total energy (tape hiss, dither and DSD noise shaping all put
	// real energy in the top bands of files that are not hi-res at all).
	spectrumContentFloorDB = -60.0

	// bandwidthCeilingGuardHz is how far below the analysis Nyquist a
	// measured bandwidth must sit to be reported at all.
	//
	// At or above it the file's real ceiling is somewhere ≥ 24 kHz and this
	// package cannot say where — so it says nothing. 500 Hz of margin covers
	// the top band's own width plus resampler ringing at the decode's own
	// edge, without eating into the 22.05 kHz case that matters.
	bandwidthCeilingGuardHz = 500.0

	// spectrumFloorDB is the level a band or bin is clamped to, matching
	// iOS's `MeterAnalysis.meterFloorDB` so the two curves share a scale.
	spectrumFloorDB = -90.0

	// minSpectrumWindows is the fewest STFT windows worth averaging — about
	// a second of audio. Below it the top bands are empty because barely
	// anything was measured, which reads as "no content up there": the false
	// accusation this feature must never make.
	minSpectrumWindows = 20
)

// BandRange is one band's inclusive FFT-bin span.
type BandRange struct{ Lo, Hi int }

// MakeBandMap returns the log-spaced band → FFT-bin ranges for one sample
// rate.
//
// **A verbatim port of iOS's `MeterAnalysis.makeBandMap`, and it must stay
// one.** The two sides' bands are drawn on the same axis, so any divergence —
// a different rounding, a different clamp, a different first edge — moves the
// curve relative to the bars with nothing to catch it. Ported line for line
// rather than rewritten idiomatically for exactly that reason; the fixture
// test compares against values captured from the Swift implementation.
//
// Bands run minimumHz → Nyquist. Bin 0 (DC) is always skipped; the last band
// always ends at the last real bin. Ranges are monotonic and non-overlapping:
// a low band whose ideal range collapses below one bin is clamped to the
// single next unclaimed bin, which pushes its neighbours up.
func MakeBandMap(sampleRate float64, fftSize, bandCount int, minimumHz float64) []BandRange {
	lastBin := fftSize/2 - 1
	// minimumHz is validated too: this is exported, and a non-positive or
	// non-finite floor makes `fMin` non-positive, which makes `logStep` NaN
	// or Inf and silently degrades every band into a single-bin range. A
	// degenerate map is worse than none — it would draw a curve that looks
	// plausible and means nothing.
	if !(sampleRate > 0) || math.IsInf(sampleRate, 0) || math.IsNaN(sampleRate) ||
		lastBin < bandCount || bandCount <= 0 ||
		!(minimumHz > 0) || math.IsInf(minimumHz, 0) {
		return nil
	}
	binHz := sampleRate / float64(fftSize)
	nyquist := sampleRate / 2
	fMin := math.Min(minimumHz, nyquist/4)
	logStep := math.Pow(nyquist/fMin, 1.0/float64(bandCount))
	ranges := make([]BandRange, 0, bandCount)
	previousEnd := 0 // bin 0 = DC, permanently claimed
	edge := fMin
	for band := 0; band < bandCount; band++ {
		nextEdge := edge * logStep
		remainingBands := bandCount - band
		// Leave at least one bin for every band still to come.
		maxStart := lastBin - remainingBands + 1
		start := min(max(previousEnd+1, int(math.Floor(edge/binHz))), maxStart)
		idealEnd := int(math.Floor(nextEdge/binHz)) - 1
		end := lastBin
		if band != bandCount-1 {
			end = min(max(start, idealEnd), lastBin-remainingBands+1)
		}
		ranges = append(ranges, BandRange{Lo: start, Hi: end})
		previousEnd = end
		edge = nextEdge
	}
	return ranges
}

// SpectrumResult is the finished measurement.
type SpectrumResult struct {
	// Bands is the time-averaged level per band in dBFS, floored at
	// spectrumFloorDB. Always SpectrumBandCount entries.
	//
	// Averaged as linear POWER and converted once at the end, not averaged
	// in the log domain: a geometric mean weights a band by how OFTEN it
	// carries content, so a cymbal sounding in 5% of frames sinks toward the
	// floor — indistinguishable from a band that is empty because the file
	// was upsampled.
	Bands []float64

	// BandwidthHz is the highest frequency carrying content, or nil when the
	// measurement ran into this package's own 24 kHz ceiling (see the file
	// docblock) or there was nothing to measure.
	BandwidthHz *int

	// CliffDepthDB is how steeply the spectrum falls across BandwidthHz.
	// nil whenever BandwidthHz is.
	CliffDepthDB *float64

	// Windows is how many STFT frames were folded in.
	Windows int
}

// spectrumAccumulator folds STFT magnitudes into the band + bin averages.
// Owned by keyTempoAnalyzer; never used standalone.
type spectrumAccumulator struct {
	bandMap []BandRange
	// bandPower / binPower are running sums of linear POWER.
	bandPower []float64
	binPower  []float64
	// scale converts a raw gonum magnitude to a dBFS-calibrated amplitude:
	// a full-scale bin-centred sine puts A·Σw/2 in its peak bin, so 2/Σw
	// lands unity. Matches iOS's calibration (whose zrop transform returns
	// twice the true DFT, hence its 1/Σw for the same result).
	scale   float64
	windows int
}

func newSpectrumAccumulator(windowSum float64, bins int) *spectrumAccumulator {
	bandMap := MakeBandMap(float64(AnalysisSampleRate), stftWindow, SpectrumBandCount, spectrumMinHz)
	if len(bandMap) != SpectrumBandCount {
		return nil
	}
	scale := 0.0
	if windowSum > 0 {
		scale = 2 / windowSum
	}
	return &spectrumAccumulator{
		bandMap:   bandMap,
		bandPower: make([]float64, SpectrumBandCount),
		binPower:  make([]float64, bins),
		scale:     scale,
	}
}

// add folds one window's magnitudes. `mag` is the analyzer's live buffer —
// read, never retained.
func (s *spectrumAccumulator) add(mag []float64) {
	if s == nil {
		return
	}
	// Bands: the band's value is its LOUDEST bin, matching how iOS's live
	// meter reduces a band, so the two curves mean the same thing.
	for b, r := range s.bandMap {
		peak := 0.0
		hi := min(r.Hi, len(mag)-1)
		for bin := r.Lo; bin <= hi; bin++ {
			if m := mag[bin]; m > peak {
				peak = m
			}
		}
		a := peak * s.scale
		s.bandPower[b] += a * a
	}
	// Bins, at full resolution, for the bandwidth measurement — 60 log
	// bands are ~14% wide at the top and cannot resolve a 22.05 kHz wall
	// from a 20 kHz one (measured on iOS: 22031 vs 25078 for two files
	// whose real ceilings are 20000 and 22050).
	n := min(len(mag), len(s.binPower))
	for bin := 0; bin < n; bin++ {
		a := mag[bin] * s.scale
		s.binPower[bin] += a * a
	}
	s.windows++
}

// finish returns the averaged spectrum, or nil when too little was measured.
func (s *spectrumAccumulator) finish() *SpectrumResult {
	if s == nil || s.windows < minSpectrumWindows {
		return nil
	}
	div := float64(s.windows)
	bands := make([]float64, len(s.bandPower))
	for i, sum := range s.bandPower {
		bands[i] = powerToDB(sum / div)
	}
	res := &SpectrumResult{Bands: bands, Windows: s.windows}
	if hz, cliff, ok := s.measureCeiling(div); ok {
		res.BandwidthHz = &hz
		res.CliffDepthDB = &cliff
	}
	return res
}

func powerToDB(power float64) float64 {
	if power <= 0 {
		return spectrumFloorDB
	}
	return math.Max(spectrumFloorDB, 10*math.Log10(power))
}

// measureCeiling finds the highest bin carrying content and how steeply the
// spectrum falls across it. ok is false when nothing was measurable OR the
// answer would be the analysis ceiling rather than the file's (see the file
// docblock — that case must report nothing, not 24 kHz).
func (s *spectrumAccumulator) measureCeiling(div float64) (hz int, cliffDB float64, ok bool) {
	binHz := float64(AnalysisSampleRate) / float64(stftWindow)
	peakDB := math.Inf(-1)
	for bin := 1; bin < len(s.binPower); bin++ {
		if db := powerToDB(s.binPower[bin] / div); db > peakDB {
			peakDB = db
		}
	}
	if math.IsInf(peakDB, -1) {
		return 0, 0, false
	}
	floorDB := peakDB + spectrumContentFloorDB

	topBin := 0
	for bin := len(s.binPower) - 1; bin >= 1; bin-- {
		if powerToDB(s.binPower[bin]/div) >= floorDB {
			topBin = bin
			break
		}
	}
	if topBin == 0 {
		return 0, 0, false
	}
	bandwidth := math.Min(float64(topBin+1)*binHz, float64(AnalysisSampleRate)/2)

	// The ceiling guard. A file whose content reaches our own Nyquist has a
	// real bandwidth somewhere at or above 24 kHz and this package cannot
	// say where — reporting 24 kHz would read as 48 kHz's Nyquist and
	// accuse a 96 kHz-native master of being an upsample.
	if bandwidth >= float64(AnalysisSampleRate)/2-bandwidthCeilingGuardHz {
		return 0, 0, false
	}

	below, okBelow := s.meanBinDB(bandwidth*0.8, bandwidth, binHz, div)
	above, okAbove := s.meanBinDB(bandwidth, bandwidth*1.25, binHz, div)
	if !okBelow || !okAbove {
		return int(math.Round(bandwidth)), 0, false
	}
	return int(math.Round(bandwidth)), below - above, true
}

// meanBinDB averages bin levels over [fromHz, toHz).
func (s *spectrumAccumulator) meanBinDB(fromHz, toHz, binHz, div float64) (float64, bool) {
	lo := int(math.Round(fromHz / binHz))
	hi := int(math.Round(toHz / binHz))
	lo = max(lo, 1)
	hi = min(hi, len(s.binPower))
	if lo >= hi {
		return 0, false
	}
	total := 0.0
	for bin := lo; bin < hi; bin++ {
		total += powerToDB(s.binPower[bin] / div)
	}
	return total / float64(hi-lo), true
}

// EncodeSpectrum serialises to the `1BSP` wire form — the SAME bytes iOS's
// `SpectrumProfile(data:)` reads, so there is one parser rather than two.
//
// Layout: magic(4) "1BSP" + version(1) + reserved(1) + sampleRate u32 +
// windows u32 + bandCount u32 + bandwidthHz u32 + cliff u16 (tenths of a dB,
// 0xFFFF = absent) + one quantised band per byte. 80 bytes at 60 bands.
func EncodeSpectrum(r *SpectrumResult) []byte {
	if r == nil || len(r.Bands) != SpectrumBandCount {
		return nil
	}
	out := make([]byte, 0, spectrumHeaderLen+len(r.Bands))
	out = append(out, '1', 'B', 'S', 'P', SpectrumSchemaVersion, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(AnalysisSampleRate))
	out = binary.LittleEndian.AppendUint32(out, clampU32(r.Windows))
	out = binary.LittleEndian.AppendUint32(out, uint32(SpectrumBandCount))
	bw := uint32(0)
	if r.BandwidthHz != nil && *r.BandwidthHz > 0 {
		bw = clampU32(*r.BandwidthHz)
	}
	out = binary.LittleEndian.AppendUint32(out, bw)
	out = binary.LittleEndian.AppendUint16(out, encodeCliff(r.CliffDepthDB))
	for _, db := range r.Bands {
		out = append(out, quantiseBandDB(db))
	}
	return out
}

const spectrumHeaderLen = 4 + 1 + 1 + 4 + 4 + 4 + 4 + 2

// spectrumCliffAbsent is the "not measured" sentinel for the cliff field.
const spectrumCliffAbsent uint16 = 0xFFFF

// encodeCliff stores tenths of a dB, clamping in the FLOAT domain first —
// converting a non-finite or huge value to an integer first is a panic in Go
// just as it traps in Swift.
func encodeCliff(db *float64) uint16 {
	if db == nil || math.IsNaN(*db) || math.IsInf(*db, 0) || *db < 0 {
		return spectrumCliffAbsent
	}
	tenths := math.Round(*db * 10)
	// One below the sentinel is still ~6553 dB, past anything measurable.
	return uint16(math.Min(tenths, float64(spectrumCliffAbsent-1)))
}

// quantiseBandDB stores dB below full scale, one byte per band (1 dB steps —
// far finer than the -60 dB content rule or the slope comparison can resolve).
func quantiseBandDB(db float64) byte {
	if math.IsNaN(db) || math.IsInf(db, 0) {
		return byte(-spectrumFloorDB)
	}
	clamped := math.Min(math.Max(db, spectrumFloorDB), 0)
	return byte(math.Min(255, math.Max(0, math.Round(-clamped))))
}

// clampU32 narrows an int to the wire's uint32.
//
// The comparison goes through uint64 because `v > math.MaxUint32` does not
// COMPILE on a 32-bit platform — the untyped constant overflows `int` — and
// the package genuinely failed `GOARCH=386` before this. Nothing ships 32-bit
// today (all six cross-compile targets are amd64/arm64), which is exactly why
// it went unnoticed; a Pi-class 32-bit host is not far-fetched for this
// codebase.
func clampU32(v int) uint32 {
	if v < 0 {
		return 0
	}
	if uint64(v) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

// spectrumBandwidthForLog renders the measured bandwidth for the analysis log
// line: -1 for "no spectrum", 0 for "measured, but past our own ceiling so
// deliberately unreported" (see the file docblock), otherwise the value.
// Distinguishing the two zeros in the log is what makes a field report about
// a missing bandwidth diagnosable.
func spectrumBandwidthForLog(r *SpectrumResult) int {
	if r == nil {
		return -1
	}
	if r.BandwidthHz == nil {
		return 0
	}
	return *r.BandwidthHz
}
