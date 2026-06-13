package analyze

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// sineMeter feeds an n-channel sine (identical on every channel) of the
// given amplitude + duration into a fresh meter and returns its
// integrated loudness.
func sineMeter(channels int, amp float64, seconds int) (*loudnessMeter, []float64) {
	m := newLoudnessMeter(channels)
	n := seconds * AnalysisSampleRate
	interleaved := make([]float64, 0, n*channels)
	frame := make([]float64, channels)
	for i := 0; i < n; i++ {
		s := amp * math.Sin(2*math.Pi*1000*float64(i)/float64(AnalysisSampleRate))
		for ch := range frame {
			frame[ch] = s
		}
		m.addFrame(frame)
		interleaved = append(interleaved, frame...)
	}
	return m, interleaved
}

// ffmpegLUFS runs ffmpeg's ebur128 filter over raw f32le interleaved
// samples and returns the integrated loudness. Returns (_, false) when
// ffmpeg isn't installed (the cross-check test skips).
var ebur128Re = regexp.MustCompile(`I:\s+(-?\d+(?:\.\d+)?)\s+LUFS`)

func ffmpegLUFS(t *testing.T, interleaved []float64, channels int) (float64, bool) {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, false
	}
	buf := new(bytes.Buffer)
	var b [4]byte
	for _, s := range interleaved {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(s)))
		buf.Write(b[:])
	}
	raw := filepath.Join(t.TempDir(), "a.pcm")
	if err := os.WriteFile(raw, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write raw pcm: %v", err)
	}
	out, _ := exec.Command(ff, "-hide_banner", "-nostats",
		"-f", "f32le", "-ar", strconv.Itoa(AnalysisSampleRate), "-ac", strconv.Itoa(channels),
		"-i", raw, "-af", "ebur128", "-f", "null", "-").CombinedOutput()
	matches := ebur128Re.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		t.Fatalf("could not parse ebur128 output:\n%s", out)
	}
	v, err := strconv.ParseFloat(matches[len(matches)-1][1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// TestLoudnessMatchesFFmpegMono is the absolute-correctness gate: my
// integrated loudness must agree with ffmpeg's reference ebur128 on the
// exact same samples (the "wrong is worse than none" bar). Skipped
// without ffmpeg.
func TestLoudnessMatchesFFmpegMono(t *testing.T) {
	m, interleaved := sineMeter(1, 0.5, 8)
	my := m.integratedLUFS()
	ref, ok := ffmpegLUFS(t, interleaved, 1)
	if !ok {
		t.Skip("ffmpeg not installed; skipping R128 reference cross-check")
	}
	t.Logf("mono 1kHz @0.5: mine=%.2f ffmpeg=%.2f", my, ref)
	if math.Abs(my-ref) > 0.6 {
		t.Fatalf("mono LUFS mine=%.2f vs ffmpeg=%.2f (diff %.2f > 0.6 LU)", my, ref, my-ref)
	}
}

func TestLoudnessMatchesFFmpegStereo(t *testing.T) {
	m, interleaved := sineMeter(2, 0.4, 8)
	my := m.integratedLUFS()
	ref, ok := ffmpegLUFS(t, interleaved, 2)
	if !ok {
		t.Skip("ffmpeg not installed; skipping R128 reference cross-check")
	}
	t.Logf("stereo 1kHz @0.4: mine=%.2f ffmpeg=%.2f", my, ref)
	if math.Abs(my-ref) > 0.6 {
		t.Fatalf("stereo LUFS mine=%.2f vs ffmpeg=%.2f (diff %.2f > 0.6 LU)", my, ref, my-ref)
	}
}

// TestLoudnessLinearWithAmplitude: halving the amplitude drops loudness
// by ~6 LU. Pure (no ffmpeg) — locks the meter's relative behaviour.
func TestLoudnessLinearWithAmplitude(t *testing.T) {
	full, _ := sineMeter(1, 0.5, 5)
	half, _ := sineMeter(1, 0.25, 5)
	diff := full.integratedLUFS() - half.integratedLUFS()
	if math.Abs(diff-6.02) > 0.3 {
		t.Fatalf("amplitude halving should drop ~6.02 LU, got %.2f", diff)
	}
}

// TestLoudnessStereoIdenticalIsPlus3: a centred (identical L/R) signal
// reads ~3 LU louder in stereo than mono (two channels summed at G=1.0).
func TestLoudnessStereoIdenticalIsPlus3(t *testing.T) {
	mono, _ := sineMeter(1, 0.4, 5)
	stereo, _ := sineMeter(2, 0.4, 5)
	diff := stereo.integratedLUFS() - mono.integratedLUFS()
	if math.Abs(diff-3.01) > 0.2 {
		t.Fatalf("stereo-identical should be ~+3.01 LU vs mono, got %.2f", diff)
	}
}

func TestLoudnessSilenceIsNegInf(t *testing.T) {
	m := newLoudnessMeter(2)
	for i := 0; i < 5*AnalysisSampleRate; i++ {
		m.addFrame([]float64{0, 0})
	}
	if !math.IsInf(m.integratedLUFS(), -1) {
		t.Fatalf("silence should be -Inf, got %.2f", m.integratedLUFS())
	}
}

func TestReplayGainFromLUFS(t *testing.T) {
	rg, ok := replayGainFromLUFS(-23.0)
	if !ok || math.Abs(rg-5.0) > 1e-9 {
		t.Fatalf("RG(-23 LUFS) = %.3f ok=%v, want 5.0", rg, ok)
	}
	rg, ok = replayGainFromLUFS(-10.0)
	if !ok || math.Abs(rg-(-8.0)) > 1e-9 {
		t.Fatalf("RG(-10 LUFS) = %.3f, want -8.0", rg)
	}
	if _, ok := replayGainFromLUFS(math.Inf(-1)); ok {
		t.Fatal("RG of silence (-Inf) should report !ok")
	}
}
