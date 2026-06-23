package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPCMGeometryReachesFormatDistribution is the end-to-end capstone for
// the PCM-geometry backfill: each of WAV / AIFF / ALAC / MP3 — the
// formats that previously carried no sampleRate — must, after Extract +
// a store round-trip, surface a non-zero sample rate in
// FormatDistribution (the composition bar's data source). Pre-fix every
// one of them landed in the rate-0 "Unknown" bucket; this pins that they
// no longer do, and that BitsPerSample follows the where-applicable rule
// (set for the lossless three, nil for lossy MP3).
func TestPCMGeometryReachesFormatDistribution(t *testing.T) {
	dir := t.TempDir()

	wavPath := filepath.Join(dir, "track.wav")
	if err := os.WriteFile(wavPath, buildWAVWithID3(t, nil, buildWAVFmtChunk(1, 2, 96000, 24)), 0o600); err != nil {
		t.Fatal(err)
	}
	aiffPath := filepath.Join(dir, "track.aiff")
	if err := os.WriteFile(aiffPath, buildAIFFWithID3(t, nil, buildAIFFCOMMChunk(2, 44100*5, 16, 44100)), 0o600); err != nil {
		t.Fatal(err)
	}
	alacPath := filepath.Join(dir, "track.m4a")
	if err := os.WriteFile(alacPath, buildMP4WithALACConfigRate(24, 192000), 0o600); err != nil {
		t.Fatal(err)
	}
	mp3Path := filepath.Join(dir, "track.mp3")
	writeMinimalMP3(t, mp3Path, map[string]string{"title": "x"})

	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	cases := []struct {
		logical string
		abs     string
		codec   string
		rate    int
		bits    int // -1 = expect nil (lossy)
	}{
		{"WAV/01.wav", wavPath, "WAV", 96000, 24},
		{"AIFF/01.aiff", aiffPath, "AIFF", 44100, 16},
		{"ALAC/01.m4a", alacPath, "ALAC", 192000, 24},
		{"MP3/01.mp3", mp3Path, "MP3", 44100, -1},
	}
	for _, tc := range cases {
		tr := &Track{Path: tc.logical, Size: 1, ModTime: time.Now()}
		if err := Extract(tc.abs, tr); err != nil {
			t.Fatalf("Extract %s: %v", tc.logical, err)
		}
		if tr.Codec != tc.codec {
			t.Errorf("%s: Codec = %q, want %q", tc.logical, tr.Codec, tc.codec)
		}
		if tr.SampleRate == nil || int(*tr.SampleRate) != tc.rate {
			t.Errorf("%s: SampleRate = %v, want %d", tc.logical, tr.SampleRate, tc.rate)
		}
		if tc.bits < 0 {
			if tr.BitsPerSample != nil {
				t.Errorf("%s: BitsPerSample = %v, want nil (lossy)", tc.logical, *tr.BitsPerSample)
			}
		} else if tr.BitsPerSample == nil || *tr.BitsPerSample != tc.bits {
			t.Errorf("%s: BitsPerSample = %v, want %d", tc.logical, tr.BitsPerSample, tc.bits)
		}
		if err := s.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatalf("UpsertTrack %s: %v", tc.logical, err)
		}
	}

	groups, err := s.FormatDistribution(context.Background())
	if err != nil {
		t.Fatalf("FormatDistribution: %v", err)
	}
	total, unknownRate := 0, 0
	for _, g := range groups {
		total += g.Count
		if g.SampleRate <= 0 {
			unknownRate += g.Count
		}
	}
	if total != len(cases) {
		t.Fatalf("FormatDistribution total = %d, want %d", total, len(cases))
	}
	if unknownRate != 0 {
		t.Errorf(`%d track(s) still in the rate-0 "Unknown" bucket, want 0`, unknownRate)
	}
}
