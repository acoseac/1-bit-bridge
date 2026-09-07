package transcode

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A job the caller marked DSD must never run down the ordinary sox path.
// Run picks its route from the source's EXTENSION plus the decoder probe,
// while the eligibility gates admit a row on its CODEC — so the two views
// can disagree, and the disagreement used to fall through to sox-direct
// (no clip guard, no measured true peak, no appliedGainDB, under a DSD
// variant id). Both reachable shapes are pinned here.
func TestRun_DSDJobRefusesANonDSDRoute(t *testing.T) {
	// Case 1: a row the scanner stamped DSF whose filename is not. The
	// extension short-circuits needsDecodeRouting, so the route is
	// sox-direct whatever the toolchain says — no probe involved, and no
	// filesystem access before the refusal.
	t.Run("codec says DSD, extension does not", func(t *testing.T) {
		_, err := Run(context.Background(), JobSpec{
			SourceAbsPath:    filepath.Join(t.TempDir(), "01.flac"),
			SourceLibraryRel: "A/01.flac",
			SourceIsDSD:      true,
			SourceSampleRate: 2822400,
			SourceBits:       1,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Kind:             JobKindPCMRender,
			OutputDir:        t.TempDir(),
		})
		if !errors.Is(err, ErrDSDDecodeUnavailable) {
			t.Errorf("err = %v, want ErrDSDDecodeUnavailable", err)
		}
	})

	// Case 2: a real .dsf on a host whose ffmpeg is missing. decodeRouteFor
	// answers routeNone, which used to fall through to sox — which cannot
	// read DSD, so the operator saw sox's unrelated diagnostic instead of
	// the real cause.
	t.Run("dsf with no ffmpeg", func(t *testing.T) {
		origFF, origProbe := ffmpegLookPath, ffprobeLookPath
		t.Cleanup(func() { ffmpegLookPath, ffprobeLookPath = origFF, origProbe })
		ffmpegLookPath = func() (string, error) { return "", errors.New("not found") }
		ffprobeLookPath = func() (string, error) { return "", errors.New("not found") }

		_, err := Run(context.Background(), JobSpec{
			SourceAbsPath:    filepath.Join(t.TempDir(), "01.dsf"),
			SourceLibraryRel: "A/01.dsf",
			SourceIsDSD:      true,
			SourceSampleRate: 2822400,
			SourceBits:       1,
			TargetSampleRate: 44100,
			TargetBits:       16,
			Kind:             JobKindOptimize,
			OutputDir:        t.TempDir(),
		})
		if !errors.Is(err, ErrDSDDecodeUnavailable) {
			t.Errorf("err = %v, want ErrDSDDecodeUnavailable", err)
		}
	})

	// Vacuity guard: the refusal is keyed on SourceIsDSD, not on the
	// extension — the same .flac spec WITHOUT the flag must get past this
	// guard and fail somewhere else entirely (sox, which is not wired
	// here). If this ever returns ErrDSDDecodeUnavailable the guard has
	// widened to every job and the two cases above prove nothing.
	t.Run("a PCM job is untouched by the guard", func(t *testing.T) {
		_, err := Run(context.Background(), JobSpec{
			SourceAbsPath:    filepath.Join(t.TempDir(), "01.flac"),
			SourceLibraryRel: "A/01.flac",
			SourceSampleRate: 44100,
			SourceBits:       16,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Kind:             JobKindUpscale,
			OutputDir:        t.TempDir(),
		})
		if errors.Is(err, ErrDSDDecodeUnavailable) {
			t.Errorf("a non-DSD job hit the DSD route guard: %v", err)
		}
	})
}
