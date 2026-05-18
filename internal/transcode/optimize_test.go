package transcode

import (
	"strings"
	"testing"
)

// JobSpecVariantID_OptimizeKind locks the prefix-discriminated emit
// + the isolated optimize memo + the load-bearing invariant that
// the optimize-kind path NEVER consults the upscale memo (which is
// pre-seeded with `upscaled-v2-*` strings for (44100, 16) etc. —
// silent variant-routing corruption if the caches were shared).
func TestJobSpecVariantID_OptimizeKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec JobSpec
		want string
	}{
		{
			name: "optimize 44.1k/16 memo-hit",
			spec: JobSpec{Kind: JobKindOptimize, TargetSampleRate: 44100, TargetBits: 16},
			want: "optimized-" + VariantSchemaVersion + "-44100-16",
		},
		{
			name: "optimize 48k/16 memo-hit",
			spec: JobSpec{Kind: JobKindOptimize, TargetSampleRate: 48000, TargetBits: 16},
			want: "optimized-" + VariantSchemaVersion + "-48000-16",
		},
		{
			name: "optimize out-of-memo falls through to live sprintf",
			spec: JobSpec{Kind: JobKindOptimize, TargetSampleRate: 88200, TargetBits: 16},
			want: "optimized-" + VariantSchemaVersion + "-88200-16",
		},
		{
			name: "upscale zero-kind default preserves legacy prefix",
			spec: JobSpec{TargetSampleRate: 192000, TargetBits: 24},
			want: "upscaled-" + VariantSchemaVersion + "-192000-24",
		},
		{
			name: "upscale explicit kind preserves legacy prefix",
			spec: JobSpec{Kind: JobKindUpscale, TargetSampleRate: 96000, TargetBits: 24},
			want: "upscaled-" + VariantSchemaVersion + "-96000-24",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.VariantID()
			if got != tc.want {
				t.Errorf("VariantID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Regression: the optimize-kind path MUST NOT return an `upscaled-*`
// string for any (rate, bits) combination, even ones the upscale
// memo pre-seeds (44100, 16) / (48000, 16). Sharing the cache would
// silently corrupt variant routing at the SQL persistence boundary
// — the explicit, dedicated test.
func TestJobSpecVariantID_OptimizeKindNeverReturnsUpscalePrefix(t *testing.T) {
	t.Parallel()
	for _, rate := range []int{44100, 48000, 88200, 96000, 176400, 192000} {
		for _, bits := range []int{16, 24} {
			spec := JobSpec{Kind: JobKindOptimize, TargetSampleRate: rate, TargetBits: bits}
			got := spec.VariantID()
			if strings.HasPrefix(got, "upscaled-") {
				t.Errorf("optimize-kind (rate=%d, bits=%d) returned upscale prefix %q — "+
					"cache-collision regression: optimize must NEVER consult upscale memo",
					rate, bits, got)
			}
			if !strings.HasPrefix(got, "optimized-") {
				t.Errorf("optimize-kind (rate=%d, bits=%d) returned %q — expected optimize prefix",
					rate, bits, got)
			}
		}
	}
}

// TargetRateForOptimize: family-preserving downsample target.
func TestTargetRateForOptimize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sourceRate int
		want       int
		why        string
	}{
		{44100, 44100, "44.1 family canonical → 44.1"},
		{88200, 44100, "44.1 family /2 → 44.1"},
		{176400, 44100, "44.1 family /4 → 44.1"},
		{352800, 44100, "44.1 family /8 → 44.1"},
		{48000, 48000, "48 family canonical → 48"},
		{96000, 48000, "48 family /2 → 48"},
		{192000, 48000, "48 family /4 → 48"},
		{384000, 48000, "48 family /8 → 48"},
		{32000, 48000, "non-family rate falls back to 48 (broader compat)"},
		{22050, 48000, "non-multiple-of-44100 falls back to 48 (broader compat)"},
	}
	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			got := TargetRateForOptimize(tc.sourceRate)
			if got != tc.want {
				t.Errorf("TargetRateForOptimize(%d) = %d, want %d (%s)",
					tc.sourceRate, got, tc.want, tc.why)
			}
		})
	}
}

// ResolveTargetRateForOptimize: unconditionally returns the
// family-preserving target rate. Eligibility (PCM-only, hi-res
// gate) is handled separately by `OptimizeEligible`. A previous
// version of this function returned 0 for `sourceRate <= target`,
// which silently skipped bit-only optimize candidates (44.1/24 →
// 44.1/16 never ran). Gemini bot review on PR #270.
func TestResolveTargetRateForOptimize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sourceRate int
		wantRate   int
		wantErr    bool
		why        string
	}{
		{192000, 48000, false, "192k → 48k (hi-res 48 family)"},
		{96000, 48000, false, "96k → 48k (hi-res 48 family)"},
		{176400, 44100, false, "176.4k → 44.1k (hi-res 44.1 family)"},
		{88200, 44100, false, "88.2k → 44.1k (hi-res 44.1 family)"},
		{44100, 44100, false, "44.1k → 44.1k (bit-only candidate eligibility flows through)"},
		{48000, 48000, false, "48k → 48k (bit-only candidate eligibility flows through)"},
		{0, 0, true, "source rate 0 → error"},
		{-1, 0, true, "source rate negative → error"},
	}
	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			got, err := ResolveTargetRateForOptimize(tc.sourceRate)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveTargetRateForOptimize(%d) err = %v, wantErr = %v",
					tc.sourceRate, err, tc.wantErr)
			}
			if got != tc.wantRate {
				t.Errorf("ResolveTargetRateForOptimize(%d) = %d, want %d (%s)",
					tc.sourceRate, got, tc.wantRate, tc.why)
			}
		})
	}
}

// OptimizeEligible: PCM hi-res qualifies; DSD/lossy/at-floor reject;
// legacy codec="" falls back to filename extension.
func TestOptimizeEligible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		codec      string
		sourceRate int
		sourceBits int
		want       bool
	}{
		// Hi-res PCM happy paths.
		{"96/24 FLAC qualifies", "x.flac", "FLAC", 96000, 24, true},
		{"192/24 FLAC qualifies", "x.flac", "FLAC", 192000, 24, true},
		{"44.1/24 FLAC qualifies (bit depth)", "x.flac", "FLAC", 44100, 24, true},
		{"88.2/16 FLAC qualifies (rate)", "x.flac", "FLAC", 88200, 16, true},
		{"96/24 ALAC qualifies", "x.m4a", "ALAC", 96000, 24, true},
		{"96/24 WAV qualifies", "x.wav", "WAV", 96000, 24, true},
		{"96/24 AIFF qualifies", "x.aiff", "AIFF", 96000, 24, true},
		// At-floor PCM rejects (nothing to do).
		{"44.1/16 FLAC rejects (at floor)", "x.flac", "FLAC", 44100, 16, false},
		{"48/16 FLAC rejects (at floor)", "x.flac", "FLAC", 48000, 16, false},
		// Non-PCM rejects.
		{"DSF rejects (non-PCM)", "x.dsf", "DSF", 2822400, 1, false},
		{"DFF rejects (non-PCM)", "x.dff", "DFF", 2822400, 1, false},
		{"AAC rejects (lossy)", "x.aac", "AAC", 44100, 16, false},
		{"MP3 rejects (lossy)", "x.mp3", "MP3", 44100, 16, false},
		// Legacy-row codec="" fallback to extension.
		{"legacy codec=\"\" + .flac + 96/24 qualifies", "Artist/Track.flac", "", 96000, 24, true},
		{"legacy codec=\"\" + .m4a + 96/24 qualifies", "Artist/Track.m4a", "", 96000, 24, true},
		{"legacy codec=\"\" + .dsf rejects", "Artist/Track.dsf", "", 2822400, 1, false},
		{"legacy codec=\"\" + .mp3 rejects (extension AND rate gate)", "Artist/Track.mp3", "", 44100, 16, false},
		{"legacy codec=\"\" + unknown ext rejects", "Artist/Track.wma", "", 44100, 16, false},
		// Mixed-case codec normalises.
		{"lowercase codec normalises (flac)", "x.flac", "flac", 96000, 24, true},
		{"whitespace codec trims (flac)", "x.flac", "  FLAC  ", 96000, 24, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := OptimizeEligible(tc.path, tc.codec, tc.sourceRate, tc.sourceBits)
			if got != tc.want {
				t.Errorf("OptimizeEligible(%q, %q, %d, %d) = %v, want %v",
					tc.path, tc.codec, tc.sourceRate, tc.sourceBits, got, tc.want)
			}
		})
	}
}

// JobKind string constants match the wire shape (matches `kind` field
// values in UpscaleRequest). Locked against silent renames.
func TestJobKindWireConstants(t *testing.T) {
	t.Parallel()
	if string(JobKindUpscale) != "upscale" {
		t.Errorf("JobKindUpscale = %q, want %q", JobKindUpscale, "upscale")
	}
	if string(JobKindOptimize) != "optimize" {
		t.Errorf("JobKindOptimize = %q, want %q", JobKindOptimize, "optimize")
	}
}
