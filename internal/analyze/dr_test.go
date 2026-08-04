package analyze

import (
	"math"
	"testing"
)

func feedSine(m *drMeter, seconds int, amplitude float64) {
	frame := make([]float64, m.channels)
	for n := 0; n < seconds*AnalysisSampleRate; n++ {
		v := amplitude * math.Sin(2*math.Pi*440*float64(n)/AnalysisSampleRate)
		for c := range frame {
			frame[c] = v
		}
		m.addFrame(frame)
	}
}

// A steady full-scale sine is the maximally-compressed program: with the
// TT meter's sqrt(2) RMS factor its block RMS equals its peak, so DR ~ 0.
// This is WHY the factor exists — the meter is calibrated so "no
// dynamics" reads zero.
func TestDRSteadySineReadsNearZero(t *testing.T) {
	m := newDRMeter(2)
	feedSine(m, 15, 0.9)
	m.finish()
	dr, ok := m.score()
	if !ok {
		t.Fatal("expected a score")
	}
	if dr > 1 {
		t.Fatalf("steady sine DR = %d, want ~0", dr)
	}
}

// A quiet bed with brief loud transients is the high-dynamics case:
// block RMS stays low while block peaks are high.
func TestDRTransientProgramReadsHigh(t *testing.T) {
	m := newDRMeter(1)
	frame := make([]float64, 1)
	for n := 0; n < 15*AnalysisSampleRate; n++ {
		v := 0.05 * math.Sin(2*math.Pi*440*float64(n)/AnalysisSampleRate)
		// One 5 ms 0.9 burst per second — a drum-hit shape.
		if n%AnalysisSampleRate < AnalysisSampleRate/200 {
			v = 0.9 * math.Sin(2*math.Pi*2000*float64(n)/AnalysisSampleRate)
		}
		frame[0] = v
		m.addFrame(frame)
	}
	m.finish()
	dr, ok := m.score()
	if !ok {
		t.Fatal("expected a score")
	}
	if dr < 12 {
		t.Fatalf("transient program DR = %d, want clearly high (>= 12)", dr)
	}
}

// The algorithm's own outlier defense: the SECOND-highest block peak is
// used, so one lone digital over must not inflate the score.
func TestDRSecondHighestPeakIgnoresALoneOver(t *testing.T) {
	base := newDRMeter(1)
	spiked := newDRMeter(1)
	frame := make([]float64, 1)
	for n := 0; n < 15*AnalysisSampleRate; n++ {
		v := 0.3 * math.Sin(2*math.Pi*440*float64(n)/AnalysisSampleRate)
		frame[0] = v
		base.addFrame(frame)
		// Identical program plus a single full-scale sample in block 0.
		if n == 100 {
			frame[0] = 1.0
		}
		spiked.addFrame(frame)
		frame[0] = v
	}
	base.finish()
	spiked.finish()
	drBase, ok1 := base.score()
	drSpiked, ok2 := spiked.score()
	if !ok1 || !ok2 {
		t.Fatal("expected scores")
	}
	if drSpiked != drBase {
		t.Fatalf("lone over changed DR: %d -> %d (second-highest-peak defense failed)", drBase, drSpiked)
	}
}

func TestDRTooShortIsNotOK(t *testing.T) {
	m := newDRMeter(2)
	feedSine(m, 5, 0.5) // < drMinBlocks * 3 s
	m.finish()
	if _, ok := m.score(); ok {
		t.Fatal("a <9 s program must not produce a DR score")
	}
}

func TestDRSilenceIsNotOK(t *testing.T) {
	m := newDRMeter(2)
	frame := make([]float64, 2)
	for n := 0; n < 12*AnalysisSampleRate; n++ {
		m.addFrame(frame)
	}
	m.finish()
	if _, ok := m.score(); ok {
		t.Fatal("silence must not produce a DR score")
	}
}

// The trailing partial block counts when it holds at least a second of
// audio — a track tail must not silently vanish from the statistic.
func TestDRPartialTailBlockCounts(t *testing.T) {
	m := newDRMeter(1)
	// 3 full blocks + a 2 s tail.
	feedSine(m, 11, 0.5)
	m.finish()
	if got := len(m.rms[0]); got != 4 {
		t.Fatalf("blocks = %d, want 4 (3 full + counted tail)", got)
	}
}

// feedAsymmetric drives channel 0 with a sine and leaves every other
// channel digitally silent — the shape no existing case covered, since
// they all write the same value to every channel.
func feedAsymmetric(m *drMeter, seconds int, amplitude float64) {
	frame := make([]float64, m.channels)
	for n := 0; n < seconds*AnalysisSampleRate; n++ {
		for c := range frame {
			frame[c] = 0
		}
		frame[0] = amplitude * math.Sin(2*math.Pi*440*float64(n)/AnalysisSampleRate)
		m.addFrame(frame)
	}
}

// One digitally-silent channel must not suppress the whole track's
// score. Real sources that hit this: a 5.1 rip with an unused LFE, a
// one-dead-channel LP transfer, a mono master laid into a stereo
// container.
//
// It mattered more than a missing number because the row still commits
// with the schema stamp, so the scan-skip gate never re-analyses it —
// the track is permanently marked "we looked, there is no DR", which is
// indistinguishable from a genuinely un-scoreable one.
func TestDRSilentChannelDoesNotSuppressTheTrack(t *testing.T) {
	m := newDRMeter(2)
	feedAsymmetric(m, 15, 0.9)
	m.finish()

	dr, ok := m.score()
	if !ok {
		t.Fatal("a track with one signal-carrying channel and one silent " +
			"channel must still score; the silent channel is skipped, not fatal")
	}
	if dr > 1 {
		t.Errorf("DR = %d, want ~0 — the surviving channel is a steady "+
			"full-scale sine, and the silent channel must be excluded from "+
			"the average rather than folded in as a zero", dr)
	}
}

// The score must be the surviving channels' average, not diluted by the
// silent ones. A high-dynamics channel beside a silent channel reads the
// same as that channel alone — averaging in a 0 would halve it.
func TestDRSilentChannelIsExcludedFromTheAverageNotZeroed(t *testing.T) {
	mono := newDRMeter(1)
	stereo := newDRMeter(2)

	monoFrame := make([]float64, 1)
	stereoFrame := make([]float64, 2)
	for n := 0; n < 15*AnalysisSampleRate; n++ {
		v := 0.05 * math.Sin(2*math.Pi*440*float64(n)/AnalysisSampleRate)
		if n%AnalysisSampleRate < AnalysisSampleRate/200 {
			v = 0.9 * math.Sin(2*math.Pi*2000*float64(n)/AnalysisSampleRate)
		}
		monoFrame[0] = v
		mono.addFrame(monoFrame)
		stereoFrame[0], stereoFrame[1] = v, 0 // right channel dead
		stereo.addFrame(stereoFrame)
	}
	mono.finish()
	stereo.finish()

	want, ok := mono.score()
	if !ok {
		t.Fatal("mono control failed to score")
	}
	got, ok := stereo.score()
	if !ok {
		t.Fatal("stereo-with-one-dead-channel failed to score")
	}
	if got != want {
		t.Errorf("dead-channel stereo DR = %d, mono control = %d — a skipped "+
			"channel must not enter the average; folding in a zero would "+
			"roughly halve the score", got, want)
	}
}

// All channels silent stays unscored. This is the `counted == 0` branch,
// which was unreachable before the per-channel skip existed. Reporting 0
// here would be wrong in a specific way: on this meter 0 means maximally
// compressed — the loudness-war reading — not "no signal".
func TestDRAllSilentChannelsStayUnscored(t *testing.T) {
	m := newDRMeter(2)
	frame := make([]float64, 2)
	for n := 0; n < 15*AnalysisSampleRate; n++ {
		m.addFrame(frame) // all zeros
	}
	m.finish()

	if dr, ok := m.score(); ok {
		t.Errorf("fully silent program scored DR = %d, want no score — 0 on "+
			"this meter means maximally compressed, not absent", dr)
	}
}
