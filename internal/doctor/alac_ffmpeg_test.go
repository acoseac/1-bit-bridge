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
	realFF, realSox := missingFFmpeg, probeSox
	t.Cleanup(func() { missingFFmpeg, probeSox = realFF, realSox })
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
		missing  []string
		probe    func(context.Context, string) (bool, error)
		wantWarn bool
		why      string
	}{
		{"ALAC present, no ffmpeg", true, []string{"ffmpeg", "ffprobe"}, yes, true,
			"the whole point: something to fix, and a one-line fix"},
		{"ALAC present, ffmpeg installed", true, nil, yes, false,
			"nothing to fix"},
		{"no ALAC in the library", true, []string{"ffmpeg", "ffprobe"}, no, false,
			"never nag about a binary this library does not need"},
		{"upscale disabled", false, []string{"ffmpeg", "ffprobe"}, yes, false,
			"analysis never touches ALAC, so only the upscale gate can want ffmpeg"},
		{"no probe wired", true, []string{"ffmpeg", "ffprobe"}, nil, false,
			"a fresh install with no scan yet must not be told to install ffmpeg " +
				"for a library it has not read"},
		{"probe errored", true, []string{"ffmpeg", "ffprobe"}, broken, false,
			"an error is `don't know`, never `no`"},
		// Some distros package the two separately, so this is a real state —
		// and telling this operator to "install ffmpeg" sends them to look at
		// a binary they already have.
		{"ffmpeg present but ffprobe missing", true, []string{"ffprobe"}, yes, true,
			"ffprobe supplies the pipe's geometry and the guard's duration; both are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missingFFmpeg = func() []string { return tc.missing }
			d := Deps{UpscaleEnabled: tc.upscale, LibraryHasCodec: tc.probe}
			got := checkAudioToolchain(context.Background(), d)
			warned := got.Status == Warn && strings.Contains(got.Summary, "ALAC")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v (status=%v summary=%q)\n%s",
					warned, tc.wantWarn, got.Status, got.Summary, tc.why)
			}
			if tc.wantWarn {
				if !strings.Contains(got.Hint, "ffmpeg") {
					t.Errorf("the hint must name the fix, got %q", got.Hint)
				}
				for _, bin := range tc.missing {
					if !strings.Contains(got.Summary, bin) {
						t.Errorf("summary must name the ABSENT binary %q, got %q\n"+
							"a wrong diagnosis sends the operator to a binary they already have",
							bin, got.Summary)
					}
				}
				if len(tc.missing) == 1 && strings.Contains(got.Summary, "ffmpeg + ffprobe") {
					t.Errorf("only %v is missing; the summary must not blame both: %q", tc.missing, got.Summary)
				}
			}
		})
	}
}
