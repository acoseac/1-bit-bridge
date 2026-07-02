package analyze

import (
	"math"
	"strings"
	"testing"
)

// TestLoudnessMeterRecoversFromNonFiniteSample pins the fix for finding C
// (bridge02-03 review): a non-finite sample (corrupt decode) must not
// poison the whole track's loudness. Pre-fix, a NaN propagated through the
// biquad feedback so every later sample went NaN; an Inf produced
// `-Inf + Inf = NaN` in the same feedback. Either way the block energies
// all failed the absolute gate and integratedLUFS collapsed to -Inf (the
// "silence" sentinel). With the sanitize, each non-finite sample is treated
// as silence and the meter recovers to a finite, sane loudness.
func TestLoudnessMeterRecoversFromNonFiniteSample(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spike float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLoudnessMeter(2)
			frame := make([]float64, 2)
			n := 2 * AnalysisSampleRate
			for i := 0; i < n; i++ {
				s := 0.5 * math.Sin(2*math.Pi*1000*float64(i)/float64(AnalysisSampleRate))
				// Spikes before AND across a block boundary — proves
				// recovery, not just that the first block predates the poison.
				if i == 4800 || i == 30000 {
					s = tc.spike
				}
				frame[0], frame[1] = s, s
				m.addFrame(frame)
			}
			lufs := m.integratedLUFS()
			if math.IsNaN(lufs) || math.IsInf(lufs, 0) {
				t.Fatalf("integratedLUFS non-finite (%v) — a %s sample poisoned the meter", lufs, tc.name)
			}
			if lufs < -70 || lufs > 0 {
				t.Fatalf("integratedLUFS out of sane range: %v", lufs)
			}
		})
	}
}

// TestSafeAnalysisFilenameOddBudgetUsesFullWidth pins the fix for finding
// I: on an odd middle-truncate budget the head must get the leftover byte
// so the full basename budget is used (matching the transcode
// safeVariantFilename twin). The natural suffix length makes the budget
// 229 (odd): 251 cap − 22 (`~<sha8>.waveform.bin`). Pre-fix the result was
// one byte short of the cap.
func TestSafeAnalysisFilenameOddBudgetUsesFullWidth(t *testing.T) {
	const fsBasenameCap = 255 - len(analysisTmpSuffix)
	long := strings.Repeat("a", 300) + ".flac" // pure ASCII → sanitized == raw, forces the hash+truncate path
	got := safeAnalysisFilename(long)
	if !strings.HasSuffix(got, waveformExt) {
		t.Fatalf("missing suffix: %q", got)
	}
	if len(got) != fsBasenameCap {
		t.Fatalf("odd-budget truncation should use the full budget: len(got)=%d, want %d", len(got), fsBasenameCap)
	}
}
