package admin

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestBuildComposition pins the bucketing: PCM sampling-density tiers,
// DSD-rate tiers, codec aggregation + count-desc ordering, the
// PCM+DSD-partition-sums-to-total reconciliation, and (load-bearing for
// SSE diff-suppression) deterministic, byte-stable output.
func TestBuildComposition(t *testing.T) {
	groups := []manifest.FormatGroup{
		{Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, Count: 10},
		{Codec: "FLAC", SampleRate: 96000, BitsPerSample: 24, Count: 5},
		{Codec: "FLAC", SampleRate: 192000, BitsPerSample: 24, Count: 2},
		{Codec: "FLAC", SampleRate: 352800, BitsPerSample: 24, Count: 1},
		{Codec: "WAV", SampleRate: 352800, BitsPerSample: 32, Count: 1},
		{Codec: "ALAC", SampleRate: 44100, BitsPerSample: 16, Count: 3},
		{Codec: "DSF", SampleRate: 2822400, BitsPerSample: 1, IsDSD: true, Count: 2},
		{Codec: "DFF", SampleRate: 11289600, BitsPerSample: 1, IsDSD: true, Count: 1},
		{Codec: "", SampleRate: 0, BitsPerSample: 0, Count: 4}, // unknown
	}

	got := buildComposition(groups)

	if got.Total != 29 {
		t.Errorf("Total = %d, want 29", got.Total)
	}

	wantPCM := []compositionBar{
		{"44.1–48 kHz", 13}, // 10 FLAC + 3 ALAC
		{"88.2–96 kHz", 5},
		{"176.4–192 kHz", 2},
		{"≥352.8 kHz (DXD)", 1},
		{"32-bit PCM", 1},
		{"Unknown", 4},
	}
	if !reflect.DeepEqual(got.PCM, wantPCM) {
		t.Errorf("PCM = %+v\nwant %+v", got.PCM, wantPCM)
	}

	wantDSD := []compositionBar{{"DSD64", 2}, {"DSD256", 1}}
	if !reflect.DeepEqual(got.DSD, wantDSD) {
		t.Errorf("DSD = %+v\nwant %+v", got.DSD, wantDSD)
	}

	// Codecs: count desc, then label asc for the 1-count DFF/WAV tie.
	wantCodecs := []compositionBar{
		{"FLAC", 18}, {"Unknown", 4}, {"ALAC", 3}, {"DSF", 2}, {"DFF", 1}, {"WAV", 1},
	}
	if !reflect.DeepEqual(got.Codecs, wantCodecs) {
		t.Errorf("Codecs = %+v\nwant %+v", got.Codecs, wantCodecs)
	}

	// PCM + DSD partition the library → their segment counts must sum to
	// Total (Codecs is an orthogonal view that also sums to Total).
	sum := 0
	for _, b := range got.PCM {
		sum += b.Count
	}
	for _, b := range got.DSD {
		sum += b.Count
	}
	if sum != got.Total {
		t.Errorf("PCM+DSD segments sum to %d, want Total %d", sum, got.Total)
	}
	codecSum := 0
	for _, b := range got.Codecs {
		codecSum += b.Count
	}
	if codecSum != got.Total {
		t.Errorf("codec segments sum to %d, want Total %d", codecSum, got.Total)
	}

	// Determinism: identical input must marshal byte-identically, or the
	// SSE diff-suppression would publish a frame every tick.
	a, _ := json.Marshal(buildComposition(groups))
	b, _ := json.Marshal(buildComposition(groups))
	if string(a) != string(b) {
		t.Errorf("buildComposition not deterministic:\n a=%s\n b=%s", a, b)
	}
}

// TestBuildCompositionEmpty asserts a zero-track library yields a zero
// Total and empty (non-nil) bars — applyComposition then hides the block.
func TestBuildCompositionEmpty(t *testing.T) {
	got := buildComposition(nil)
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0", got.Total)
	}
	if len(got.PCM) != 0 || len(got.DSD) != 0 || len(got.Codecs) != 0 {
		t.Errorf("want all-empty bars, got %+v", got)
	}
}
