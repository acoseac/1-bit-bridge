package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The ALAC warning exists because an operator with ALAC and no ffmpeg gets a
// pipeline that refuses those tracks with nothing telling them one binary
// would fix it. These pin when it fires — and, more importantly, when it
// must NOT.
func TestAudioToolchainALACWarning(t *testing.T) {
	realFF, realSox := ffmpegAvailable, probeSox
	t.Cleanup(func() { ffmpegAvailable, probeSox = realFF, realSox })
	// A healthy sox, so the branches under test are reachable on a runner
	// that has no audio toolchain at all.
	probeSox = func(context.Context) (transcode.SoxInfo, error) {
		return transcode.SoxInfo{Path: "/usr/bin/sox", Version: "v14.4.2", FormatsKnown: true, HasFLAC: true}, nil
	}

	yes := func(context.Context, string) (bool, error) { return true, nil }
	no := func(context.Context, string) (bool, error) { return false, nil }
	broken := func(context.Context, string) (bool, error) { return false, errors.New("db locked") }

	cases := []struct {
		name     string
		upscale  bool
		ffmpeg   bool
		probe    func(context.Context, string) (bool, error)
		wantWarn bool
		why      string
	}{
		{"ALAC present, no ffmpeg", true, false, yes, true,
			"the whole point: something to fix, and a one-line fix"},
		{"ALAC present, ffmpeg installed", true, true, yes, false,
			"nothing to fix"},
		{"no ALAC in the library", true, false, no, false,
			"never nag about a binary this library does not need"},
		{"upscale disabled", false, false, yes, false,
			"analysis never touches ALAC, so only the upscale gate can want ffmpeg"},
		{"no probe wired", true, false, nil, false,
			"a fresh install with no scan yet must not be told to install ffmpeg " +
				"for a library it has not read"},
		{"probe errored", true, false, broken, false,
			"an error is `don't know`, never `no`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ffmpegAvailable = func() bool { return tc.ffmpeg }
			d := Deps{UpscaleEnabled: tc.upscale, LibraryHasCodec: tc.probe}
			got := checkAudioToolchain(context.Background(), d)
			warned := got.Status == Warn && strings.Contains(got.Summary, "ALAC")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v (status=%v summary=%q)\n%s",
					warned, tc.wantWarn, got.Status, got.Summary, tc.why)
			}
			if tc.wantWarn && !strings.Contains(got.Hint, "ffmpeg") {
				t.Errorf("the hint must name the fix, got %q", got.Hint)
			}
		})
	}
}
