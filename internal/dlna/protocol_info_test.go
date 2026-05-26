package dlna

import (
	"strings"
	"testing"
)

func TestResolveMIMEType(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		extension string
		want      string
	}{
		// DSF — per-vendor matrix
		{"chord_dsf", "Chord 2go/1.5.7", ".dsf", "audio/x-dsf"},
		{"chord_2go_dsf", "2go-firmware/1.6", ".dsf", "audio/x-dsf"},
		{"chord_poly_dsf", "Chord Poly/2.1", ".dsf", "audio/x-dsf"},
		{"sony_dsf", "Sony SRS-HG1/3.2", ".dsf", "audio/dsd"},
		{"integra_dsf", "Integra DRX-3/1.0", ".dsf", "audio/x-dsf"},
		{"onkyo_dsf", "Onkyo TX-NR686/1.0", ".dsf", "audio/x-dsf"},
		{"unknown_dsf_default", "Generic-Renderer/1.0", ".dsf", "audio/x-dsf"},
		{"empty_ua_dsf_default", "", ".dsf", "audio/x-dsf"},

		// Phase 0 empirical findings (2026-05-26 spike against Chord 2go) —
		// the load-bearing surprise: the 2go's playback engine identifies as
		// MPD, not as Chord. A naive "Chord"-only matcher misses production
		// traffic entirely; the explicit MPD + Lavf branches surface in code
		// and tests so this lesson can't be lost.
		{"chord_2go_actual_mpd_ua", "Music Player Daemon 0.21.26", ".dsf", "audio/x-dsf"},
		{"libavformat_mconnect_probe", "Lavf/58.45.100", ".dsf", "audio/x-dsf"},
		{"sony_uses_mpd_still_returns_dsd", "Sony Music Player Daemon 0.21", ".dsf", "audio/dsd"}, // vendor-first branch wins
		{"sony_uses_libavformat_still_returns_dsd", "Sony Lavf/58", ".dsf", "audio/dsd"},          // vendor-first branch wins

		// DFF — per-vendor matrix
		{"sony_dff", "Sony SRS-HG1/3.2", ".dff", "audio/dsd"},
		{"chord_dff", "Chord 2go/1.5.7", ".dff", "audio/x-dff"},
		{"unknown_dff_default", "Generic/1.0", ".dff", "audio/x-dff"},

		// Other codecs — single canonical MIME per format
		{"flac", "any-ua", ".flac", "audio/x-flac"},
		{"alac_m4a", "any-ua", ".m4a", "audio/mp4"},
		{"alac_mp4", "any-ua", ".mp4", "audio/mp4"},
		{"wav", "any-ua", ".wav", "audio/wav"},
		{"aiff", "any-ua", ".aiff", "audio/aiff"},
		{"aif", "any-ua", ".aif", "audio/aiff"},
		{"mp3", "any-ua", ".mp3", "audio/mpeg"},
		{"ogg", "any-ua", ".ogg", "audio/ogg"},

		// Case folding on extension (lowercased input shouldn't change behavior;
		// uppercased input must collapse to the same MIME)
		{"dsf_uppercase_ext", "Chord 2go", ".DSF", "audio/x-dsf"},
		{"flac_uppercase_ext", "any-ua", ".FLAC", "audio/x-flac"},

		// Unknown extension falls back to octet-stream
		{"unknown_ext", "any", ".xyz", "application/octet-stream"},
		{"empty_ext", "any", "", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveMIMEType(tc.userAgent, tc.extension)
			if got != tc.want {
				t.Errorf("ResolveMIMEType(%q, %q) = %q, want %q",
					tc.userAgent, tc.extension, got, tc.want)
			}
		})
	}
}

// TestResolveMIMEType_caseSensitiveUA pins the deliberate decision to
// NOT lowercase the User-Agent in the matcher. Vendor strings are
// case-stable in the wild; case-insensitive matching would broaden the
// match surface and risk false positives from unrelated UA substrings
// (e.g. a "chord" appearing in a non-Chord renderer's model description).
func TestResolveMIMEType_caseSensitiveUA(t *testing.T) {
	// Sony's branch is the canary for case-sensitivity because its
	// MIME ("audio/dsd") is DISTINCT from the default ("audio/x-dsf").
	// The original Chord-vs-default formulation of this test asserted
	// the same MIME on both branches and so couldn't catch a
	// regression that flipped the matcher to case-insensitive. Per
	// CodeRabbit Minor on PR #303.
	//
	// Canonical case "Sony" → vendor MIME.
	got := ResolveMIMEType("Sony SRS-HG1/3.2", ".dsf")
	if got != "audio/dsd" {
		t.Errorf("expected Sony vendor MIME (audio/dsd) for canonical case, got %q", got)
	}
	// Lower-case "sony" must NOT match the Sony branch — falls
	// through to default "audio/x-dsf". If this assertion ever
	// flips to audio/dsd, the matcher became case-insensitive and
	// vendor classification is unreliable.
	got = ResolveMIMEType("sony-clone/1.0", ".dsf")
	if got != "audio/x-dsf" {
		t.Errorf("expected default branch (audio/x-dsf) for lowercase sony, got %q (matcher went case-insensitive?)", got)
	}
}

func TestProtocolInfoFor(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		extension string
		wantMIME  string
	}{
		{"chord_dsf", "Chord 2go", ".dsf", "audio/x-dsf"},
		{"sony_dsf", "Sony SRS-HG1", ".dsf", "audio/dsd"},
		{"flac", "any-ua", ".flac", "audio/x-flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProtocolInfoFor(tc.userAgent, tc.extension)
			// Shape: "http-get:*:<mime>:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=<flags>"
			wantPrefix := "http-get:*:" + tc.wantMIME + ":"
			if !strings.HasPrefix(got, wantPrefix) {
				t.Fatalf("ProtocolInfoFor(%q,%q) = %q, missing prefix %q",
					tc.userAgent, tc.extension, got, wantPrefix)
			}
			if !strings.Contains(got, "DLNA.ORG_OP=01") {
				t.Errorf("missing DLNA.ORG_OP=01: %q", got)
			}
			if !strings.Contains(got, "DLNA.ORG_CI=0") {
				t.Errorf("missing DLNA.ORG_CI=0 (transcoding flag): %q", got)
			}
			if !strings.Contains(got, "DLNA.ORG_FLAGS="+DLNAFlags) {
				t.Errorf("missing canonical flag string: %q", got)
			}
		})
	}
}

// TestDLNAFlags_canonicalShape locks the 32-char hex shape — a typo
// here would silently disable streaming-mode or background-transfer
// bits on every served track.
func TestDLNAFlags_canonicalShape(t *testing.T) {
	if len(DLNAFlags) != 32 {
		t.Fatalf("DLNAFlags must be 32 hex chars, got %d: %q", len(DLNAFlags), DLNAFlags)
	}
	for i, r := range DLNAFlags {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			t.Errorf("DLNAFlags[%d] = %q is not hex", i, r)
		}
	}
	// Pin the exact value — anyone changing this needs to know they're
	// changing the streaming + background-transfer + DLNA v1.5
	// advertisement for every served track.
	const expected = "01700000000000000000000000000000"
	if DLNAFlags != expected {
		t.Fatalf("DLNAFlags drift: got %q, want %q (change is deliberate? update the test + plan doc)", DLNAFlags, expected)
	}
}

// TestProtocolInfoFor_transcodingFlagIsZero is the bit-exact-contract
// regression guard. CI=1 would tell renderers the stream IS transcoded;
// any future code that flips this is a structural violation of the
// project mission and must be caught here.
func TestProtocolInfoFor_transcodingFlagIsZero(t *testing.T) {
	for _, ext := range []string{".dsf", ".dff", ".flac", ".wav", ".m4a", ".mp3"} {
		got := ProtocolInfoFor("any-ua", ext)
		if !strings.Contains(got, "DLNA.ORG_CI=0") {
			t.Errorf("BIT-EXACT CONTRACT VIOLATION: %s carries non-zero CI flag: %q", ext, got)
		}
		if strings.Contains(got, "DLNA.ORG_CI=1") {
			t.Errorf("BIT-EXACT CONTRACT VIOLATION: %s explicitly transcoded: %q", ext, got)
		}
	}
}
