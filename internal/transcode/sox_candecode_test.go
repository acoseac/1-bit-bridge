package transcode

import "testing"

// TestSoxInfoCanDecode pins the source-format guard.
//
// The gate and the decoder disagreed: ALAC clears every upstream check —
// manifest.IsLossyCodec doesn't list it (lossless), canSetBitsPerSample
// allowlists it, OptimizeEligible names "ALAC" outright, and since PR #440
// M4A carries PCM geometry too — so an .m4a reached a sox with no MP4
// demuxer and the job failed AFTER the client was told it was eligible.
//
// Both fail-open arms are load-bearing and easy to "tidy" away, so they
// are pinned explicitly rather than left implicit in the happy path.
func TestSoxInfoCanDecode(t *testing.T) {
	// A realistic stock build's format block (Homebrew sox 14.4.2,
	// trimmed): note it carries opus but has no m4a/mp4 token at all.
	stock := SoxInfo{
		FormatsKnown: true,
		Formats: []string{
			"8svx", "aif", "aifc", "aiff", "au", "caf", "flac", "gsm",
			"mp2", "mp3", "ogg", "opus", "raw", "sph", "voc", "vorbis",
			"w64", "wav", "wavpcm",
		},
	}
	// A minimal apt install without libsox-fmt-all: no flac.
	minimal := SoxInfo{
		FormatsKnown: true,
		Formats:      []string{"aiff", "au", "raw", "wav"},
	}

	cases := []struct {
		name string
		info SoxInfo
		path string
		want bool
	}{
		// Formats a stock build genuinely handles.
		{"stock/flac", stock, "/lib/Artist/Album/01.flac", true},
		{"stock/wav", stock, "/lib/a.wav", true},
		{"stock/aiff", stock, "/lib/a.aiff", true},
		{"stock/aif", stock, "/lib/a.aif", true},
		{"stock/aifc", stock, "/lib/a.aifc", true},
		{"stock/uppercase ext", stock, "/lib/A.FLAC", true},

		// The case this guard exists for.
		{"stock/m4a (ALAC) refused", stock, "/lib/a.m4a", false},
		{"stock/mp4 refused", stock, "/lib/a.mp4", false},

		// Per-source coverage of the minimal-install case ProbeSox's
		// HasFLAC field only answers globally.
		{"minimal build refuses flac", minimal, "/lib/a.flac", false},
		{"minimal build still allows wav", minimal, "/lib/a.wav", true},

		// Fail-open: an unparseable `sox --help` must never disable a
		// working install (ProbeSox's documented posture).
		{"formats unknown → allow flac", SoxInfo{FormatsKnown: false}, "/lib/a.flac", true},
		{"formats unknown → allow m4a", SoxInfo{FormatsKnown: false}, "/lib/a.m4a", true},

		// Fail-open: extensions outside the map are shapes this guard
		// wasn't written to judge. Refusing them here would silently
		// narrow the pipeline as a side effect of an unrelated change.
		{"unmapped ext (.dsf)", stock, "/lib/a.dsf", true},
		{"unmapped ext (.mp3)", stock, "/lib/a.mp3", true},
		{"no extension", stock, "/lib/bare", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.CanDecode(tc.path); got != tc.want {
				t.Errorf("CanDecode(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestSoxInfoCanDecodeMatchesProbeOutput closes the loop between the
// parser and the guard: a real `sox --help` block goes through
// parseSoxFileFormats and the resulting SoxInfo must refuse .m4a while
// accepting .flac. Pinning them together means a change to either the
// tokeniser or the extension map can't quietly break the pairing.
func TestSoxInfoCanDecodeMatchesProbeOutput(t *testing.T) {
	const help = `sox:      SoX v14.4.2

AUDIO FILE FORMATS: 8svx aif aifc aiff au caf cdda flac mp3 ogg opus raw wav
PLAYLIST FORMATS: m3u pls
AUDIO DEVICE DRIVERS: coreaudio
`
	formats, known := parseSoxFileFormats(help)
	if !known {
		t.Fatal("parseSoxFileFormats did not find the format block")
	}
	info := SoxInfo{Formats: formats, FormatsKnown: known}

	if !info.CanDecode("/lib/a.flac") {
		t.Error("a build advertising flac must accept .flac")
	}
	if info.CanDecode("/lib/a.m4a") {
		t.Error("a build with no m4a token must refuse .m4a — this is the ALAC case")
	}
}
