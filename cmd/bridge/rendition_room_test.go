package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom: a client's request for
// a rendition is refused before it is queued when the variants volume has no
// room for the projected rendition, or a DSD render's scratch volume none for
// its Stage A intermediate, as the batch and the auto-optimize sweep refuse
// (backlog B264). It made no check: every request on a full volume queued a
// render that could only fail. Reaching the fixture's stopped pool is the
// proof that every gate passed.
func TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		name                string
		dsd                 bool
		variants, scratch   int64 // free bytes the probe reports
		probeFails, refused bool
		wantScratchProbed   bool
		enqueue             func(*upscaleEnqueuerAdapter, string) error
	}{
		{name: "PCM, room", variants: gib, scratch: gib},
		{name: "PCM, no room on the variants volume", variants: 100 << 10, scratch: gib, refused: true},
		{name: "upscale, no room on the variants volume", variants: 100 << 10, scratch: gib, refused: true,
			enqueue: func(a *upscaleEnqueuerAdapter, rel string) error { return a.EnqueueOne(rel) }},
		{name: "DSD, room", dsd: true, variants: gib, scratch: gib, wantScratchProbed: true},
		{name: "DSD, no room for the scratch", dsd: true, variants: gib, scratch: 50 << 20, refused: true, wantScratchProbed: true},
		{name: "DSD, no room on the variants volume", dsd: true, variants: 10 << 10, scratch: gib, refused: true},
		{name: "a volume the probe cannot read", variants: gib, scratch: gib, probeFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdapterFixture(t).withCaps(capsDSD)
			rel := "A/Album/01.flac"
			if tc.dsd {
				rel = "A/DSD/01.dsf"
				f.seed(t, rel, "DSF", 2822400, 1, true, "", 300, 2)
			} else {
				f.seed(t, rel, "FLAC", 96000, 24, false, "", 0, 0)
			}
			variantsDir := f.a.outputDir()
			var scratchProbed bool
			f.a.diskFree = func(dir string) (int64, error) {
				if tc.probeFails {
					return 0, errors.New("statfs: input/output error")
				}
				if strings.HasPrefix(dir, variantsDir) {
					return tc.variants, nil
				}
				if dir != transcode.RenderScratchDir(f.a.renderTempDir()) {
					t.Errorf("probed %q, neither the variants directory nor the render scratch", dir)
				}
				scratchProbed = true
				return tc.scratch, nil
			}
			enqueue := tc.enqueue
			if enqueue == nil {
				enqueue = func(a *upscaleEnqueuerAdapter, rel string) error { return a.EnqueueOptimize(rel) }
			}
			err := enqueue(f.a, rel)
			reachedPool := errors.Is(err, transcode.ErrPoolClosed) || errors.Is(err, api.ErrUpscaleSourceMissing)
			switch {
			case tc.refused:
				if !errors.Is(err, api.ErrUpscaleNoRoom) || !errors.Is(err, transcode.ErrInsufficientDiskSpace) {
					t.Errorf("enqueue = %v, want ErrUpscaleNoRoom with the numbers", err)
				}
			case tc.probeFails:
				if err == nil || reachedPool || errors.Is(err, api.ErrUpscaleNoRoom) {
					t.Errorf("enqueue = %v, want the probe's failure, before the pool", err)
				}
			default:
				if !reachedPool {
					t.Errorf("enqueue = %v, want the job to reach the pool", err)
				}
			}
			if scratchProbed != tc.wantScratchProbed {
				t.Errorf("the render scratch was probed = %v, want %v", scratchProbed, tc.wantScratchProbed)
			}
		})
	}
}
