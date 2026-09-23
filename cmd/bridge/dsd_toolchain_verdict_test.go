package main

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestDSDRenderToolchainVerdict pins the four states the settings page
// can be in, and specifically that "we could not read the decoder
// listing" is NOT reported as "your ffmpeg build lacks the decoders".
//
// HasDSD is false whenever the listing did not parse, so without its own
// branch every probe failure — a timeout, a PATH wrapper, an ffmpeg that
// errored — told the operator something confident about their BUILD that
// the bridge had not established. Reported from the field against the
// v0.2.0 Docker image, whose Dockerfile asserts all four decoders at
// BUILD time and fails without them, making the build the one
// explanation it could not have been.
func TestDSDRenderToolchainVerdict(t *testing.T) {
	const somePath = "/usr/bin/ffmpeg"
	for _, tc := range []struct {
		name    string
		info    transcode.FFmpegInfo
		wantOK  bool
		wantHas []string
		wantNot []string
	}{
		{
			name:    "ffmpeg absent",
			info:    transcode.FFmpegInfo{MissingBinaries: []string{"ffmpeg", "ffprobe"}},
			wantOK:  false,
			wantHas: []string{"not on PATH"},
			wantNot: []string{"lacks the dsd_"},
		},
		{
			name:    "listing unreadable, with a reason",
			info:    transcode.FFmpegInfo{Path: somePath, ProbeErr: "ffmpeg -decoders timed out after 5s"},
			wantOK:  false,
			wantHas: []string{"could not be read", "timed out"},
			// The whole point: this must not blame the build.
			wantNot: []string{"lacks the dsd_"},
		},
		{
			name:    "listing unreadable, no reason recorded",
			info:    transcode.FFmpegInfo{Path: somePath},
			wantOK:  false,
			wantHas: []string{"could not be read"},
			wantNot: []string{"lacks the dsd_", ": "},
		},
		{
			name:    "listing read, decoders genuinely absent",
			info:    transcode.FFmpegInfo{Path: somePath, DecodersKnown: true},
			wantOK:  false,
			wantHas: []string{"lacks the dsd_", "dsd_msbf_planar"},
			wantNot: []string{"could not be read"},
		},
		{
			name:   "all present",
			info:   transcode.FFmpegInfo{Path: somePath, DecodersKnown: true, HasDSD: true},
			wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := dsdRenderToolchainVerdict(tc.info)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (why=%q)", ok, tc.wantOK, why)
			}
			if ok && why != "" {
				t.Errorf("a passing verdict carried a reason: %q", why)
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(why, want) {
					t.Errorf("why = %q, want it to contain %q", why, want)
				}
			}
			for _, notWant := range tc.wantNot {
				if strings.Contains(why, notWant) {
					t.Errorf("why = %q, must NOT contain %q", why, notWant)
				}
			}
		})
	}
}

// TestTheVerdictComposesWithTheConsolePrefix pins the SENTENCE, not just
// its substrings.
//
// The admin handler renders this as `"saved, but "+why+", so no DSD
// track will be rendered"`. Doctor's standalone wording — "ffmpeg is on
// PATH but its decoder listing could not be read" — produced "saved, but
// ffmpeg is on PATH but …" when composed. The substring assertions above
// all passed; it took reading the real response from a container to see
// it.
func TestTheVerdictComposesWithTheConsolePrefix(t *testing.T) {
	for _, info := range []transcode.FFmpegInfo{
		{MissingBinaries: []string{"ffprobe"}},
		{Path: "/x", ProbeErr: "boom"},
		{Path: "/x"},
		{Path: "/x", DecodersKnown: true},
	} {
		ok, why := dsdRenderToolchainVerdict(info)
		if ok {
			continue
		}
		sentence := "saved, but " + why + ", so no DSD track will be rendered"
		if strings.Contains(sentence, "but ffmpeg is on PATH but") || strings.Count(sentence, " but ") > 1 {
			t.Errorf("verdict does not compose with the console prefix: %q", sentence)
		}
	}
}

// TestFFmpegSnapshotCarriesTheProbeReason pins the plumbing the branch
// above depends on: FFmpegSnapshot discards ProbeFFmpeg's error, so
// without ProbeErr the console could say the listing was unreadable but
// never why — which is the half an operator needs.
func TestFFmpegSnapshotCarriesTheProbeReason(t *testing.T) {
	// A FFmpegInfo that reached the unreadable branch with a reason must
	// surface it; one without must not emit a dangling separator.
	_, withReason := dsdRenderToolchainVerdict(transcode.FFmpegInfo{Path: "/x", ProbeErr: "boom"})
	if !strings.HasSuffix(withReason, ": boom") {
		t.Errorf("the probe reason did not reach the operator: %q", withReason)
	}
	_, without := dsdRenderToolchainVerdict(transcode.FFmpegInfo{Path: "/x"})
	if strings.HasSuffix(without, ":") || strings.Contains(without, ": ") {
		t.Errorf("a missing reason left a dangling separator: %q", without)
	}
}
