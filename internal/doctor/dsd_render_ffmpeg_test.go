package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The DSD-render check exists because an operator who flips
// `upscale.dsdRender.enabled` on a host whose ffmpeg lacks the dsd_*
// decoders gets a sweep that renders nothing, with nothing saying why.
// These pin each verdict — and that the check spawns NO ffmpeg while the
// feature is off.
func TestDSDRenderToolchainCheck(t *testing.T) {
	real := probeFFmpeg
	t.Cleanup(func() { probeFFmpeg = real })

	full := transcode.FFmpegInfo{Path: "/usr/bin/ffmpeg", DecodersKnown: true, HasDSD: true, HasDST: true}
	noDST := transcode.FFmpegInfo{Path: "/usr/bin/ffmpeg", DecodersKnown: true, HasDSD: true}
	noDSD := transcode.FFmpegInfo{Path: "/usr/bin/ffmpeg", DecodersKnown: true}
	missing := transcode.FFmpegInfo{MissingBinaries: []string{"ffmpeg", "ffprobe"}}

	probeOK := func(info transcode.FFmpegInfo) func(context.Context) (transcode.FFmpegInfo, error) {
		return func(context.Context) (transcode.FFmpegInfo, error) { return info, nil }
	}
	probeMissing := func(context.Context) (transcode.FFmpegInfo, error) {
		return missing, fmt.Errorf("probe: %w", transcode.ErrFFmpegMissing)
	}
	probeBroken := func(context.Context) (transcode.FFmpegInfo, error) {
		return transcode.FFmpegInfo{Path: "/usr/bin/ffmpeg"}, errors.New("exit status 127")
	}

	hasDFF := func(_ context.Context, codec string) (bool, error) { return codec == "DFF", nil }
	noDSDInLibrary := func(context.Context, string) (bool, error) { return false, nil }
	brokenLibrary := func(context.Context, string) (bool, error) { return false, errors.New("db locked") }

	cases := []struct {
		name        string
		enabled     bool
		probe       func(context.Context) (transcode.FFmpegInfo, error)
		library     func(context.Context, string) (bool, error)
		wantStatus  Status
		wantSummary string
		wantHint    []string
	}{
		{"enabled, DSD and DST decoders", true, probeOK(full), hasDFF, OK, "DSD and DST", nil},
		{"enabled, no DST decoder", true, probeOK(noDST), hasDFF, Warn, "not DST", []string{"dst", "DSF / DFF renders"}},
		{"enabled, no DSD decoders", true, probeOK(noDSD), hasDFF, Fail, "lacks the DSD decoders",
			[]string{"dsd_lsbf_planar", "dsd_msbf_planar", "disable"}},
		{"enabled, ffmpeg missing", true, probeMissing, hasDFF, Fail, "not found",
			[]string{"ffmpeg + ffprobe", "brew install ffmpeg", "apt install ffmpeg", "choco install ffmpeg"}},
		{"enabled, ffmpeg present but not runnable", true, probeBroken, hasDFF, Fail, "not runnable", []string{"exit status 127"}},
		{"disabled, library holds DFF", false, nil, hasDFF, Warn, "not enabled", []string{"dsdRender.enabled", "CarPlay"}},
		{"disabled, no DSD in the library", false, nil, noDSDInLibrary, OK, "not enabled", nil},
		{"disabled, no library probe wired", false, nil, nil, OK, "not enabled", nil},
		{"disabled, library probe errored", false, nil, brokenLibrary, OK, "not enabled", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawned := false
			probeFFmpeg = func(ctx context.Context) (transcode.FFmpegInfo, error) {
				spawned = true
				if tc.probe == nil {
					t.Errorf("probeFFmpeg was called with the feature OFF — the check must not spawn ffmpeg for a feature nobody enabled")
					return transcode.FFmpegInfo{}, errors.New("unexpected probe")
				}
				return tc.probe(ctx)
			}
			got := checkDSDRenderToolchain(context.Background(),
				Deps{DSDRenderEnabled: tc.enabled, LibraryHasCodec: tc.library})
			if got.Name != checkNameDSDRenderToolchain {
				t.Errorf("Name = %q, want %q", got.Name, checkNameDSDRenderToolchain)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v (summary=%q hint=%q)", got.Status, tc.wantStatus, got.Summary, got.Hint)
			}
			if !strings.Contains(got.Summary, tc.wantSummary) {
				t.Errorf("Summary = %q, want it to contain %q", got.Summary, tc.wantSummary)
			}
			for _, h := range tc.wantHint {
				if !strings.Contains(got.Hint, h) {
					t.Errorf("Hint = %q, want it to contain %q", got.Hint, h)
				}
			}
			if tc.enabled && !spawned {
				t.Error("the enabled arm must consult the ffmpeg probe")
			}
		})
	}
}

// TestDSDRenderToolchainIsRegistered pins that the check is in the
// registry a `bridge doctor` run walks — a check that exists but is never
// run is the "contract with no observer" shape.
func TestDSDRenderToolchainIsRegistered(t *testing.T) {
	real := probeFFmpeg
	t.Cleanup(func() { probeFFmpeg = real })
	probeFFmpeg = func(context.Context) (transcode.FFmpegInfo, error) {
		return transcode.FFmpegInfo{Path: "/usr/bin/ffmpeg", DecodersKnown: true, HasDSD: true, HasDST: true}, nil
	}
	found := false
	for _, c := range Run(context.Background(), Deps{DSDRenderEnabled: true}).Checks {
		if c.Name == checkNameDSDRenderToolchain {
			found = true
		}
	}
	if !found {
		t.Fatalf("no registered check reports as %q", checkNameDSDRenderToolchain)
	}
}
