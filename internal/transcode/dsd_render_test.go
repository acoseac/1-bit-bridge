package transcode

import (
	"math"
	"strings"
	"testing"
)

// The DSD families' identity + eligibility, pinned separately from the
// PCM optimize suite so a change to either family's recipe shows up under
// its own name.

func TestJobSpecVariantID_DSDFamilies(t *testing.T) {
	cases := []struct {
		name string
		spec JobSpec
		want string
	}{
		{"optimize on a DSD source is the compact DSD family",
			JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, TargetSampleRate: 44100, TargetBits: 16},
			"optimized-dsd-v1-44100-16"},
		{"compact 48k family",
			JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, TargetSampleRate: 48000, TargetBits: 16},
			"optimized-dsd-v1-48000-16"},
		{"pcm 44.1k family",
			JobSpec{Kind: JobKindPCMRender, SourceIsDSD: true, TargetSampleRate: 176400, TargetBits: 24},
			"pcm-v1-176400-24"},
		{"pcm 48k family",
			JobSpec{Kind: JobKindPCMRender, SourceIsDSD: true, TargetSampleRate: 192000, TargetBits: 24},
			"pcm-v1-192000-24"},
		// Off-memo inputs fall through to the live format, same family.
		{"pcm off-memo rate still names the family",
			JobSpec{Kind: JobKindPCMRender, SourceIsDSD: true, TargetSampleRate: 88200, TargetBits: 24},
			"pcm-v1-88200-24"},
		{"compact off-memo bits still names the family",
			JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, TargetSampleRate: 44100, TargetBits: 24},
			"optimized-dsd-v1-44100-24"},
		// The PCM optimize family is untouched — this is the pin that keeps
		// 8,356 existing optimize rows resolving to the same ids.
		{"optimize on a PCM source stays the PCM family",
			JobSpec{Kind: JobKindOptimize, TargetSampleRate: 44100, TargetBits: 16},
			"optimized-" + VariantSchemaVersion + "-44100-16"},
		{"upscale ignores SourceIsDSD",
			JobSpec{Kind: JobKindUpscale, SourceIsDSD: true, TargetSampleRate: 176400, TargetBits: 24},
			"upscaled-" + VariantSchemaVersion + "-176400-24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.VariantID(); got != tc.want {
				t.Errorf("VariantID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDSDFamilyPrefixes(t *testing.T) {
	if !strings.HasPrefix(VariantPrefixOptimizedDSD+"-", VariantPrefixOptimized+"-") {
		t.Errorf("the compact DSD family %q must sit under the optimize prefix %q so every "+
			"HasPrefix(\"optimized-\") / LIKE 'optimized-%%' site admits it", VariantPrefixOptimizedDSD, VariantPrefixOptimized)
	}
	if VariantPrefixPCM != "pcm" || VariantPrefixOptimizedDSD != "optimized-dsd" {
		t.Errorf("prefixes are wire-stable: pcm=%q optimizedDSD=%q", VariantPrefixPCM, VariantPrefixOptimizedDSD)
	}
	if DSDRenditionSchemaVersion != "v1" {
		t.Errorf("DSDRenditionSchemaVersion = %q; bumping it re-renders every DSD sidecar", DSDRenditionSchemaVersion)
	}
	// A DSD id is never mistaken for the PCM optimize schema: the compact
	// family's version segment is the DSD schema, not VariantSchemaVersion.
	id := JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, TargetSampleRate: 44100, TargetBits: 16}.VariantID()
	if strings.Contains(id, "-"+VariantSchemaVersion+"-") {
		t.Errorf("%q carries the PCM schema version; the DSD recipe must version independently", id)
	}
}

func TestTargetRateForPCMRender(t *testing.T) {
	cases := []struct {
		rate int
		want int
	}{
		{2822400, 176400},  // DSD64
		{5644800, 176400},  // DSD128
		{11289600, 176400}, // DSD256
		{22579200, 176400}, // DSD512
		{3072000, 192000},  // DSD64 @ 48k family
		{6144000, 192000},  // DSD128 @ 48k
		{12288000, 192000}, // DSD256 @ 48k
		{0, 0},
		{-1, 0},
		{44100, 176400}, // in-family, whatever the multiple
		{2822401, 0},    // off by one: a header we do not trust
		{1000000, 0},
	}
	for _, tc := range cases {
		if got := TargetRateForPCMRender(tc.rate); got != tc.want {
			t.Errorf("TargetRateForPCMRender(%d) = %d, want %d", tc.rate, got, tc.want)
		}
	}
	if _, err := ResolveTargetRateForPCMRender(0); err == nil {
		t.Error("a non-positive rate must error")
	}
	if _, err := ResolveTargetRateForPCMRender(2822401); err == nil {
		t.Error("an off-family rate must error, never render at a guessed rate")
	}
	if got, err := ResolveTargetRateForPCMRender(11289600); err != nil || got != 176400 {
		t.Errorf("DSD256 → (%d, %v), want 176400", got, err)
	}
}

// TestTargetRateForOptimize_DSDRates pins that the compact tier's existing
// family-modulo resolver already lands every DSD rate on its family base —
// so an equality-style rewrite can never sneak in.
func TestTargetRateForOptimize_DSDRates(t *testing.T) {
	for rate, want := range map[int]int{
		2822400: 44100, 5644800: 44100, 11289600: 44100, 22579200: 44100,
		3072000: 48000, 6144000: 48000, 12288000: 48000,
	} {
		if got := TargetRateForOptimize(rate); got != want {
			t.Errorf("TargetRateForOptimize(%d) = %d, want %d", rate, got, want)
		}
	}
}

func TestIsDSDSource(t *testing.T) {
	cases := []struct {
		codec, path string
		want        bool
	}{
		{"DSF", "/l/a.dsf", true},
		{"DFF", "/l/a.dff", true},
		{"dsf", "/l/a.flac", true}, // codec wins, case-folded
		{" DFF ", "/l/a.wav", true},
		{"", "/l/a.dsf", true}, // legacy row: extension fallback
		{"", "/l/A.DFF", true},
		{"", "/l/a.flac", false},
		{"FLAC", "/l/a.dsf", false}, // a stamped codec is trusted over the extension
		{"ALAC", "/l/a.m4a", false},
	}
	for _, tc := range cases {
		if got := IsDSDSource(tc.codec, tc.path); got != tc.want {
			t.Errorf("IsDSDSource(%q, %q) = %v, want %v", tc.codec, tc.path, got, tc.want)
		}
	}
}

func TestDSDRenderEligible(t *testing.T) {
	on := DSDRenderCaps{Enabled: true, DecodeDSD: true}
	onDST := DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}
	cases := []struct {
		name        string
		path, codec string
		isDSD       bool
		rate        int
		compression string
		caps        DSDRenderCaps
		want        bool
	}{
		{"DSD64 DSF with caps on", "/l/a.dsf", "DSF", true, 2822400, "", on, true},
		{"DSD256 DFF with caps on", "/l/a.dff", "DFF", true, 11289600, "", on, true},
		{"DSD64 @48k family", "/l/a.dff", "DFF", true, 3072000, "", on, true},
		{"legacy row: no codec, dsf extension", "/l/a.dsf", "", true, 2822400, "", on, true},

		{"flag off refuses", "/l/a.dsf", "DSF", true, 2822400, "", DSDRenderCaps{DecodeDSD: true}, false},
		{"no DSD decoders refuses", "/l/a.dsf", "DSF", true, 2822400, "", DSDRenderCaps{Enabled: true}, false},
		{"zero caps refuse", "/l/a.dsf", "DSF", true, 2822400, "", DSDRenderCaps{}, false},

		{"isDSD false refuses even with a DSF codec", "/l/a.dsf", "DSF", false, 2822400, "", on, false},
		{"forged isDSD on an MP3 refuses", "/l/a.mp3", "MP3", true, 2822400, "", on, false},
		{"forged isDSD on a FLAC refuses", "/l/a.flac", "FLAC", true, 2822400, "", on, false},

		{"off-family rate refuses", "/l/a.dsf", "DSF", true, 2822401, "", on, false},
		{"zero rate refuses", "/l/a.dsf", "DSF", true, 0, "", on, false},

		{"DST without the dst decoder refuses", "/l/a.dff", "DFF", true, 2822400, "DST", on, false},
		{"DST with the dst decoder is eligible", "/l/a.dff", "DFF", true, 2822400, "DST", onDST, true},
		{"dst lowercase still counts as DST", "/l/a.dff", "DFF", true, 2822400, " dst ", on, false},

		{"SACD virtual track refuses", "/l/Album.iso/st/03.dff", "DFF", true, 2822400, "DST", onDST, false},
		{"SACD virtual track refuses (uppercase)", "/l/ALBUM.ISO/st/12.dff", "DFF", true, 2822400, "", on, false},
		{"a dff under a folder merely NAMED iso is fine", "/l/iso-collection/a.dff", "DFF", true, 2822400, "", on, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DSDRenderEligible(tc.path, tc.codec, tc.isDSD, tc.rate, tc.compression, tc.caps)
			if got != tc.want {
				t.Errorf("DSDRenderEligible = %v, want %v", got, tc.want)
			}
			if pcm := PCMRenderEligible(tc.path, tc.codec, tc.isDSD, tc.rate, tc.compression, tc.caps); pcm != got {
				t.Errorf("PCMRenderEligible = %v disagrees with DSDRenderEligible = %v", pcm, got)
			}
		})
	}
}

func TestOptimizeEligibleFor(t *testing.T) {
	on := DSDRenderCaps{Enabled: true, DecodeDSD: true}
	cases := []struct {
		name        string
		path, codec string
		rate, bits  int
		isDSD       bool
		caps        DSDRenderCaps
		want        bool
	}{
		// The PCM rule is unchanged and needs no caps.
		{"hi-res FLAC is eligible without caps", "/l/a.flac", "FLAC", 96000, 24, false, DSDRenderCaps{}, true},
		{"44.1/16 FLAC stays ineligible", "/l/a.flac", "FLAC", 44100, 16, false, on, false},
		{"MP3 stays ineligible", "/l/a.mp3", "MP3", 44100, 16, false, on, false},
		// DSD joins only through the DSD rule.
		{"DSD with caps on is eligible", "/l/a.dsf", "DSF", 2822400, 1, true, on, true},
		{"DSD with caps off is ineligible", "/l/a.dsf", "DSF", 2822400, 1, true, DSDRenderCaps{}, false},
		{"DSD off-family is ineligible even with caps", "/l/a.dsf", "DSF", 2822401, 1, true, on, false},
		// bits=1 must never reach the PCM rule's `bits > 16` arm as a
		// reason — DSD is judged by the DSD rule only.
		{"DSD with the flag off is not rescued by the PCM rule", "/l/a.dsf", "DSF", 2822400, 1, true, DSDRenderCaps{DecodeDSD: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OptimizeEligibleFor(tc.path, tc.codec, tc.rate, tc.bits, tc.isDSD, "", tc.caps)
			if got != tc.want {
				t.Errorf("OptimizeEligibleFor = %v, want %v", got, tc.want)
			}
		})
	}
	// The PCM-only rule stays exported and DSD-blind: the three gates and the
	// lockstep test call it by name.
	if OptimizeEligible("/l/a.dsf", "DSF", 2822400, 1) {
		t.Error("OptimizeEligible must stay PCM-only; DSD eligibility lives in DSDRenderEligible")
	}
}

func TestDSDRenderCaps_Active(t *testing.T) {
	if (DSDRenderCaps{}).Active() || (DSDRenderCaps{Enabled: true}).Active() || (DSDRenderCaps{DecodeDSD: true}).Active() {
		t.Error("Active needs the flag AND the decoders")
	}
	if !(DSDRenderCaps{Enabled: true, DecodeDSD: true}).Active() {
		t.Error("flag + decoders is active")
	}
}

// TestProjectedSize_DSDSourcePin records what the shared PCM projection
// says about a DSD source, so nobody "fixes" it into DSD byte-math and
// silently changes every pre-flight. With sourceBits = 1 the formula is
// (targetRate / dsdRate) × targetBits × factor: a 1-second DSD64 stereo
// source (705,600 bytes) projects to ≈0.98× itself for the faithful tier
// and ≈0.14× for the compact tier at the shipped compression factors —
// both CONSERVATIVE against the measured renditions (a decimated,
// sinc-filtered 24-bit FLAC compresses better than the factor assumes),
// which is the direction a disk pre-flight must err in.
func TestProjectedSize_DSDSourcePin(t *testing.T) {
	const dsd64Stereo1s = 2822400 * 2 / 8 // 705,600 bytes
	src := float64(dsd64Stereo1s)
	cases := []struct {
		name         string
		rate, bits   int
		want         int64
		wantSourceFr float64
	}{
		{"pcm 176.4/24", 176400, 24, int64(math.Round(src * (176400.0 / 2822400.0) * 24 * FLACCompressionFactor24Bit)), 0.975},
		{"optimized 44.1/16", 44100, 16, int64(math.Round(src * (44100.0 / 2822400.0) * 16 * FLACCompressionFactor16Bit)), 0.1375},
	}
	for _, c := range cases {
		got := ProjectedSize(dsd64Stereo1s, 2822400, 1, c.rate, c.bits, DefaultCompressionFactor(c.bits))
		if got != c.want {
			t.Errorf("%s: ProjectedSize = %d, want %d", c.name, got, c.want)
		}
		if frac := float64(got) / src; frac < c.wantSourceFr-0.001 || frac > c.wantSourceFr+0.001 {
			t.Errorf("%s: projected/source = %.4f, want ≈%.4f", c.name, frac, c.wantSourceFr)
		}
	}
}
