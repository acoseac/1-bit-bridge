package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// roomOutcome is what an on-demand enqueue answered about the pre-flight.
type roomOutcome int

const (
	// roomQueued: every gate passed, and the fixture's stopped pool answered.
	roomQueued roomOutcome = iota
	// roomRefused: api.ErrUpscaleNoRoom, carrying the numbers.
	roomRefused
	// roomUnchecked: the probe's own failure, before the pool.
	roomUnchecked
	// roomUnexpected: anything else.
	roomUnexpected
)

// roomOutcomeOf reads an enqueue's answer. Reaching the fixture's stopped
// pool is the proof that every gate before it passed.
func roomOutcomeOf(err error) roomOutcome {
	switch {
	case errors.Is(err, transcode.ErrPoolClosed), errors.Is(err, api.ErrUpscaleSourceMissing):
		return roomQueued
	case errors.Is(err, api.ErrUpscaleNoRoom) && errors.Is(err, transcode.ErrInsufficientDiskSpace):
		return roomRefused
	case err != nil && !errors.Is(err, api.ErrUpscaleNoRoom) &&
		strings.Contains(err.Error(), "check free space for the rendition"):
		return roomUnchecked
	}
	return roomUnexpected
}

// roomCase is one row of TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom.
type roomCase struct {
	name                     string
	dsd, upscale             bool
	shared                   bool  // the variants directory and the render scratch share a volume
	variants, scratch        int64 // free bytes the probe reports for each
	probeFails, compareFails bool
	scratchProbed            bool // whether the render scratch is probed on its own
	want                     roomOutcome
}

// TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom: a client's request for
// a rendition is refused before it is queued when the variants volume has no
// room for the projected rendition, or a DSD render's scratch volume none for
// its Stage A intermediate, as the batch and the auto-optimize sweep refuse
// (backlog B264). It made no check: every request on a full volume queued a
// render that could only fail. Where the two directories share a volume the
// render needs room for both there, and that volume alone is probed.
func TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []roomCase{
		{name: "PCM, room", variants: gib, scratch: gib},
		{name: "PCM, no room on the variants volume", variants: 100 << 10, scratch: gib, want: roomRefused},
		{name: "upscale, no room on the variants volume", upscale: true, variants: 100 << 10, scratch: gib, want: roomRefused},
		{name: "DSD, room on two volumes", dsd: true, variants: gib, scratch: gib, scratchProbed: true},
		{name: "DSD, no room for the scratch", dsd: true, variants: gib, scratch: 50 << 20, want: roomRefused, scratchProbed: true},
		{name: "DSD, no room on the variants volume", dsd: true, variants: 10 << 10, scratch: gib, want: roomRefused},
		{name: "DSD, one volume with room for both", dsd: true, shared: true, variants: gib},
		{name: "a volume the probe cannot read", variants: gib, scratch: gib, probeFails: true, want: roomUnchecked},
		{name: "volumes that cannot be compared", dsd: true, variants: gib, scratch: gib, compareFails: true, want: roomUnchecked},
	} {
		t.Run(tc.name, tc.run)
	}
}

func (tc roomCase) run(t *testing.T) {
	f := newAdapterFixture(t).withCaps(capsDSD)
	rel := "A/Album/01.flac"
	if tc.dsd {
		rel = "A/DSD/01.dsf"
		f.seed(t, rel, "DSF", 2822400, 1, true, "", 300, 2)
	} else {
		f.seed(t, rel, "FLAC", 96000, 24, false, "", 0, 0)
	}
	var scratchProbed bool
	f.a.diskFree = tc.probe(t, f.a.outputDir(), transcode.RenderScratchDir(f.a.renderTempDir()), &scratchProbed)
	f.a.sameVolume = func(string, string) (bool, error) {
		if tc.compareFails {
			return false, errors.New("stat: input/output error")
		}
		return tc.shared, nil
	}
	enqueue := f.a.EnqueueOptimize
	if tc.upscale {
		enqueue = f.a.EnqueueOne
	}
	if err := enqueue(rel); roomOutcomeOf(err) != tc.want {
		t.Errorf("enqueue = %v, outcome %d; want %d", err, roomOutcomeOf(err), tc.want)
	}
	if scratchProbed != tc.scratchProbed {
		t.Errorf("the render scratch was probed = %v, want %v", scratchProbed, tc.scratchProbed)
	}
}

// probe is the free-space probe a row reports: its two volumes' free bytes,
// or its failure.
func (tc roomCase) probe(t *testing.T, variantsDir, scratchDir string, scratchProbed *bool) func(string) (int64, error) {
	return func(dir string) (int64, error) {
		switch {
		case tc.probeFails:
			return 0, errors.New("statfs: input/output error")
		case strings.HasPrefix(dir, variantsDir):
			return tc.variants, nil
		case dir == scratchDir:
			*scratchProbed = true
			return tc.scratch, nil
		}
		t.Errorf("probed %q, neither the variants directory nor the render scratch", dir)
		return 0, errors.New("an unexpected probe")
	}
}

// TestADSDRenderOnOneVolumeNeedsRoomForItsScratchAndItsRendition: a DSD
// render holds its Stage A scratch while Stage C writes the rendition, so
// where the variants directory and the render scratch share a volume the
// pre-flight wants room for both there. Free space that holds each need
// alone and not the two together is refused on one volume, and passes on
// two (CodeRabbit on #1145).
func TestADSDRenderOnOneVolumeNeedsRoomForItsScratchAndItsRendition(t *testing.T) {
	f := newAdapterFixture(t).withCaps(capsDSD)
	const rel = "A/DSD/01.dsf"
	tr := f.seed(t, rel, "DSF", 2822400, 1, true, "", 300, 2)
	spec, err := buildOptimizeSpec(tr, filepath.Join(f.libDir, rel), f.a.outputDir(), f.a.renderTempDir(), capsDSD)
	if err != nil {
		t.Fatal(err)
	}
	spec.SourceSize = tr.Size // finalizeAndEnqueue's stamp, from the row
	projected := transcode.ProjectedSize(spec.SourceSize, spec.SourceSampleRate, spec.SourceBits,
		spec.TargetSampleRate, spec.TargetBits, transcode.DefaultCompressionFactor(spec.TargetBits))
	scratch := spec.RenderScratchBytes()
	margin := transcode.DefaultDiskSafetyMargin
	each := max(transcode.RequiredBytesWithMargin(projected, margin), transcode.RequiredBytesWithMargin(scratch, margin))
	if both := transcode.RequiredBytesWithMargin(projected+scratch, margin); projected <= 0 || both <= each {
		t.Fatalf("premise: projected %d and scratch %d need %d together, more than the %d each needs alone", projected, scratch, both, each)
	}
	f.a.diskFree = func(string) (int64, error) { return each, nil }
	for _, shared := range []bool{false, true} {
		f.a.sameVolume = func(string, string) (bool, error) { return shared, nil }
		err := f.a.EnqueueOptimize(rel)
		want := roomQueued
		if shared {
			want = roomRefused
		}
		if got := roomOutcomeOf(err); got != want {
			t.Errorf("one volume = %v: enqueue = %v, outcome %d; want %d", shared, err, got, want)
		}
	}
}
