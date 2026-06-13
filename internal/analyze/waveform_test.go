package analyze

import (
	"encoding/binary"
	"testing"
)

func TestPeakerBucketingDeterministic(t *testing.T) {
	// 10 samples, bucket=4 → buckets [0..3], [4..7], [8..9] = 3 buckets.
	p := newPeaker(4)
	vals := []float32{0.1, -0.2, 0.3, -0.4, 0.5, -0.6, 0.7, -0.8, 0.9, -1.0}
	for _, v := range vals {
		p.add(v)
	}
	p.finish()
	if p.count() != 3 {
		t.Fatalf("count = %d, want 3", p.count())
	}
	if p.mins[0] != quantizeSample(-0.4) || p.maxs[0] != quantizeSample(0.3) {
		t.Fatalf("bucket 0: min=%d max=%d", p.mins[0], p.maxs[0])
	}
	if p.mins[2] != quantizeSample(-1.0) || p.maxs[2] != quantizeSample(0.9) {
		t.Fatalf("bucket 2 (partial): min=%d max=%d", p.mins[2], p.maxs[2])
	}
}

func TestQuantizeSampleClamps(t *testing.T) {
	cases := []struct {
		in   float32
		want int8
	}{
		{0, 0},
		{1.0, 127},
		{-1.0, -127},
		{2.0, 127},   // clamp, not wrap
		{-2.0, -127}, // clamp, not wrap
		{0.5, 64},    // round(0.5*127)=round(63.5)=64
	}
	for _, c := range cases {
		if got := quantizeSample(c.in); got != c.want {
			t.Errorf("quantizeSample(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEncodeWaveformHeader(t *testing.T) {
	p := newPeaker(2)
	for _, v := range []float32{0.5, -0.5, 0.25, -0.25} {
		p.add(v)
	}
	p.finish()
	totalSamples := int64(48000) // exactly 1s → 1000 ms
	b := encodeWaveform(p, AnalysisSampleRate, 2, totalSamples)

	if len(b) != waveformHeaderLen+2*2 {
		t.Fatalf("len = %d, want %d", len(b), waveformHeaderLen+4)
	}
	if string(b[0:4]) != waveformMagic {
		t.Fatalf("magic = %q", b[0:4])
	}
	if b[4] != waveformFormatVersion {
		t.Fatalf("version = %d", b[4])
	}
	if got := binary.LittleEndian.Uint32(b[6:10]); got != AnalysisSampleRate {
		t.Fatalf("rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[10:14]); got != 2 {
		t.Fatalf("bucketSamples = %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[14:18]); got != 2 {
		t.Fatalf("count = %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[18:22]); got != 1000 {
		t.Fatalf("durationMS = %d, want 1000", got)
	}
}

func TestWaveformTagStableAndContentSensitive(t *testing.T) {
	a := []byte("alpha")
	if waveformTag(a) != waveformTag(a) {
		t.Fatal("tag not stable for identical bytes")
	}
	if len(waveformTag(a)) != 8 {
		t.Fatalf("tag length = %d, want 8", len(waveformTag(a)))
	}
	if waveformTag(a) == waveformTag([]byte("beta")) {
		t.Fatal("tag collision across different content")
	}
}
