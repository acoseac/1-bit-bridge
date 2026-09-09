package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// specsFromBatch runs the two DSD-admitting submits against a pool whose
// runner parks, and returns the specs the workers received keyed by
// `<rel>|<variantID>`.
func specsFromBatch(t *testing.T, s *manifest.Store, want int) map[string]JobSpec {
	t.Helper()
	p := NewPool(s, 8, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	specs := make(chan JobSpec, 16)
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		specs <- spec
		<-ctx.Done()
		return RunResult{SizeBytes: spec.SourceSize}, nil
	}
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatal(err)
	}
	c.WithDSDRender(capsFn(dsdCapsWithDST)).WithRenderTempDir("/scratch/render")
	if _, err := c.SubmitPCMRender(context.Background(), "DSD", t.TempDir()); err != nil {
		t.Fatalf("SubmitPCMRender: %v", err)
	}
	if _, err := c.SubmitOptimize(context.Background(), "DSD", t.TempDir()); err != nil {
		t.Fatalf("SubmitOptimize: %v", err)
	}
	got := map[string]JobSpec{}
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case sp := <-specs:
			got[sp.SourceLibraryRel+"|"+sp.VariantID()] = sp
		case <-deadline:
			t.Fatalf("only %d of %d specs reached the workers: %v", len(got), want, got)
		}
	}
	return got
}

// A batch is an operator bulk action, and a DSD render is minutes where a
// PCM optimize is seconds — so batch DSD work rides the BACKGROUND lane
// and cannot park the phone's own on-demand request behind up to
// maxJobTimeout the pool has no way to preempt. PCM-source batches keep
// exactly the laning they had before the DSD tiers existed, which is what
// makes this change invisible to every pre-#863 deployment.
func TestBatchDSDJobsRideTheBackgroundLane(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	got := specsFromBatch(t, s, 7)

	for _, key := range []string{
		"DSD/01.dsf|pcm-v1-176400-24",
		"DSD/02.dff|pcm-v1-176400-24",
		"DSD/03.dff|pcm-v1-192000-24",
		"DSD/01.dsf|optimized-dsd-v1-44100-16",
		"DSD/02.dff|optimized-dsd-v1-44100-16",
		"DSD/03.dff|optimized-dsd-v1-48000-16",
	} {
		sp, ok := got[key]
		if !ok {
			t.Errorf("no spec for %s", key)
			continue
		}
		if !sp.Background {
			t.Errorf("%s: Background = false, want true (a bulk DSD render is not latency-sensitive)", key)
		}
		if routesToForegroundLane(sp.Kind, sp.Background) {
			t.Errorf("%s: routes to the foreground lane", key)
		}
	}
	// The PCM source in the same batch keeps its pre-#863 laning.
	if sp, ok := got["DSD/04.flac|optimized-v2-48000-16"]; !ok {
		t.Error("no spec for the FLAC row")
	} else if sp.Background {
		t.Error("a PCM-source optimize batch moved to the background lane — laning must be byte-identical for pre-#863 shapes")
	}
}

// The batch path carries the source's real geometry. Without it
// RenderScratchBytes assumes stereo (under-reserving for a multichannel
// source) and the decode-completeness guard loses its manifest fallback
// for a source whose ffprobe duration reads 0 — a truncated render would
// publish silently. The sweeper and the single-file enqueuers always
// filled these; the coordinator's batch path did not (CodeRabbit on
// PR #863).
func TestBatchDSDJobsCarryChannelsAndDuration(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedDSDBatchFixture(t, s)
	got := specsFromBatch(t, s, 7)

	sp, ok := got["DSD/01.dsf|pcm-v1-176400-24"]
	if !ok {
		t.Fatal("no spec for DSD/01.dsf")
	}
	if sp.SourceChannels != 6 || sp.SourceDurationSec != 30.55 {
		t.Errorf("geometry = %d ch / %.1f s, want the seeded 6 / 30.55", sp.SourceChannels, sp.SourceDurationSec)
	}
	// And the scratch estimate follows the real channel count rather than
	// the stereo fallback — the number the sweep's disk budget grades.
	if got, stereo := sp.RenderScratchBytes(), TempBytesForRender(2, 176400, 30.55); got <= stereo {
		t.Errorf("RenderScratchBytes = %d, want > the stereo estimate %d for a 6-channel source", got, stereo)
	}
	// A row with no duration/channels in the manifest reports zero —
	// "unknown", which the consumers fall back from. Pinned so the
	// json_extract is not silently returning a wrong non-zero value.
	if sp := got["DSD/03.dff|pcm-v1-192000-24"]; sp.SourceChannels != 0 || sp.SourceDurationSec != 0 {
		t.Errorf("unseeded geometry = %d ch / %.1f s, want 0 / 0", sp.SourceChannels, sp.SourceDurationSec)
	}
	// A PCM row never carries them either — the projection reads both for
	// DSD rows only, exactly like AutoOptimizeCandidate.
	if sp := got["DSD/04.flac|optimized-v2-48000-16"]; sp.SourceChannels != 0 || sp.SourceDurationSec != 0 {
		t.Errorf("PCM row geometry = %d ch / %.1f s, want 0 / 0", sp.SourceChannels, sp.SourceDurationSec)
	}
}
