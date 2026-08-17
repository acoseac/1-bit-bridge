package admin

import (
	"strings"
	"testing"
)

func f64p(v float64) *float64 { return &v }
func intp(v int) *int         { return &v }

// TestFundamentalSkipReason pins the kind-agnostic per-track skip
// classification the inspector badges tiles with: precedence
// (DSD > lossy > unknown), case-insensitive lossy-codec matching, the
// nil/zero rate/bits → unknown_format gate, and "" for eligible tracks.
func TestFundamentalSkipReason(t *testing.T) {
	cases := []struct {
		name  string
		isDSD bool
		codec string
		rate  *float64
		bits  *int
		want  string
	}{
		{"DSD", true, "DSF", f64p(2822400), intp(1), "dsd_bitstream"},
		{"DSD beats lossy codec", true, "MP3", nil, nil, "dsd_bitstream"},
		{"lossy MP3", false, "MP3", nil, nil, "lossy_source"},
		{"lossy lowercase", false, "mp3", nil, nil, "lossy_source"},
		{"lossy AAC", false, "AAC", f64p(44100), intp(16), "lossy_source"},
		{"lossy OGG", false, "OGG", nil, nil, "lossy_source"},
		{"lossy beats unknown (no rate)", false, "AAC", nil, nil, "lossy_source"},
		{"unknown — no codec, no rate", false, "", nil, nil, "unknown_format"},
		{"unknown — FLAC but no rate", false, "FLAC", nil, intp(24), "unknown_format"},
		{"unknown — zero rate", false, "FLAC", f64p(0), intp(24), "unknown_format"},
		{"unknown — no bits", false, "WAV", f64p(48000), nil, "unknown_format"},
		{"unknown — zero bits", false, "WAV", f64p(48000), intp(0), "unknown_format"},
		{"eligible FLAC", false, "FLAC", f64p(96000), intp(24), ""},
		{"eligible ALAC (lossless)", false, "ALAC", f64p(44100), intp(16), ""},
		{"eligible WAV", false, "WAV", f64p(192000), intp(24), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// nil canDecode = probe unwired: fail open, so every one of
			// these expectations must be unchanged by the no_decoder work.
			got := fundamentalSkipReason(c.isDSD, c.codec, c.rate, c.bits, "a/b.flac", nil)
			if got != c.want {
				t.Errorf("fundamentalSkipReason(dsd=%v, codec=%q, rate=%v, bits=%v) = %q, want %q",
					c.isDSD, c.codec, c.rate, c.bits, got, c.want)
			}
		})
	}
}

// TestIsLossyCodecLabel pins the lossy denylist the badge AND the
// upscale gates share (it delegates to manifest.IsLossyCodec — the
// single source of truth also mirrored by upscaleEligibleSQL; the
// optimize gate remains transcode.OptimizeEligible's PCM allowlist).
// Lossless + DSD + empty codec are NOT lossy.
func TestIsLossyCodecLabel(t *testing.T) {
	lossy := []string{"MP3", "mp3", " AAC ", "OGG", "OPUS", "WMA"}
	for _, c := range lossy {
		if !isLossyCodecLabel(c) {
			t.Errorf("isLossyCodecLabel(%q) = false, want true", c)
		}
	}
	notLossy := []string{"FLAC", "ALAC", "WAV", "AIFF", "DSF", "DFF", ""}
	for _, c := range notLossy {
		if isLossyCodecLabel(c) {
			t.Errorf("isLossyCodecLabel(%q) = true, want false", c)
		}
	}
}

// TestFundamentalSkipReason_NoDecoder pins the toolchain-dependent branch.
//
// The motivating case, measured 2026-08-17 against the Docker image: ALAC is
// lossless (so lossy_source doesn't fire) and carries real PCM geometry since
// PR #440 (so unknown_format doesn't either), so it rendered with NO badge and
// was advertised as eligible — then every job failed with
// `sox FAIL formats: no handler for file extension 'm4a'`.
func TestFundamentalSkipReason_NoDecoder(t *testing.T) {
	rate, bits := f64p(44100), intp(16)
	// Stands in for SoxInfo.CanDecode on a build with no MP4 demuxer.
	noMP4 := func(p string) bool { return !strings.HasSuffix(strings.ToLower(p), ".m4a") }

	if got := fundamentalSkipReason(false, "ALAC", rate, bits, "A/B.m4a", noMP4); got != "no_decoder" {
		t.Errorf("undecodable ALAC = %q, want no_decoder — the tile would claim it is eligible", got)
	}
	if got := fundamentalSkipReason(false, "FLAC", rate, bits, "A/B.flac", noMP4); got != "" {
		t.Errorf("decodable FLAC = %q, want no badge", got)
	}
	// Nil-safe: an unwired probe must never invent a badge.
	if got := fundamentalSkipReason(false, "ALAC", rate, bits, "A/B.m4a", nil); got != "" {
		t.Errorf("unwired probe = %q, want no badge (fail open)", got)
	}

	// Ordering: file-intrinsic reasons outrank the toolchain-dependent one,
	// so a track is described by what it IS wherever that is knowable.
	if got := fundamentalSkipReason(true, "ALAC", rate, bits, "A/B.m4a", noMP4); got != "dsd_bitstream" {
		t.Errorf("DSD + undecodable = %q, want dsd_bitstream", got)
	}
	if got := fundamentalSkipReason(false, "AAC", rate, bits, "A/B.m4a", noMP4); got != "lossy_source" {
		t.Errorf("lossy + undecodable = %q, want lossy_source", got)
	}
	if got := fundamentalSkipReason(false, "ALAC", nil, nil, "A/B.m4a", noMP4); got != "unknown_format" {
		t.Errorf("no geometry + undecodable = %q, want unknown_format", got)
	}
}
