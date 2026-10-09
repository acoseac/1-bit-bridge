package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
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
// the two Stage A files it holds while it surveys, as the batch and the
// auto-optimize sweep refuse. It made no check: every request on a full
// volume queued a render that could only fail. Where the two directories
// share a volume the peak is those two files, or one of them beside the
// rendition, and that volume alone is probed.
func TestAnOnDemandRenditionIsQueuedOnlyWhereItHasRoom(t *testing.T) {
	const gib = int64(1) << 30
	one := transcode.TempBytesForRender(2, 44100, 300)
	margin := transcode.DefaultDiskSafetyMargin
	oneFree := transcode.RequiredBytesWithMargin(one, margin)
	twoFree := transcode.RequiredBytesWithMargin(one*2, margin)
	for _, tc := range []roomCase{
		{name: "PCM, room", variants: gib, scratch: gib},
		{name: "PCM, no room on the variants volume", variants: 100 << 10, scratch: gib, want: roomRefused},
		{name: "upscale, no room on the variants volume", upscale: true, variants: 100 << 10, scratch: gib, want: roomRefused},
		{name: "DSD, room on two volumes", dsd: true, variants: gib, scratch: gib, scratchProbed: true},
		{name: "DSD, no room for the scratch", dsd: true, variants: gib, scratch: 50 << 20, want: roomRefused, scratchProbed: true},
		{name: "DSD, room for one scratch file and not two", dsd: true, variants: gib, scratch: oneFree, want: roomRefused, scratchProbed: true},
		{name: "DSD, room for two scratch files", dsd: true, variants: gib, scratch: twoFree, scratchProbed: true},
		{name: "DSD, no room on the variants volume", dsd: true, variants: 10 << 10, scratch: gib, want: roomRefused},
		{name: "DSD, one volume with room for both", dsd: true, shared: true, variants: gib},
		{name: "DSD, one volume with room for one scratch and not two", dsd: true, shared: true, variants: oneFree, want: roomRefused},
		{name: "DSD, one volume with room for two scratches", dsd: true, shared: true, variants: twoFree},
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
	var variantsProbed, scratchProbed bool
	f.a.diskFree = tc.probe(t, f.a.outputDir(), transcode.RenderScratchDir(f.a.renderTempDir()), &variantsProbed, &scratchProbed)
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
	// An earlier gate can answer as the pool does (EnqueueOne reads a stopped
	// pool as a missing source), so an outcome the pre-flight decided needs
	// the variants volume probed.
	if (tc.want == roomQueued || tc.want == roomRefused) && !variantsProbed {
		t.Error("the variants volume was never probed")
	}
}

// probe is the free-space probe a row reports: its two volumes' free bytes,
// or its failure. It notes which volumes were asked.
func (tc roomCase) probe(t *testing.T, variantsDir, scratchDir string, variantsProbed, scratchProbed *bool) func(string) (int64, error) {
	return func(dir string) (int64, error) {
		switch {
		case tc.probeFails:
			return 0, errors.New("statfs: input/output error")
		case strings.HasPrefix(dir, variantsDir):
			*variantsProbed = true
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
// render holds two Stage A scratches while it surveys, and one of them
// beside the rendition at Stage C. Where the volumes are separate the
// scratch volume needs the two files. Where they are one volume the peak
// is the larger of those two moments, and room for that peak queues even
// when the rendition added on top of both scratches would not fit.
func TestADSDRenderOnOneVolumeNeedsRoomForItsScratchAndItsRendition(t *testing.T) {
	f := newAdapterFixture(t).withCaps(capsDSD)
	const rel = "A/DSD/01.dsf"
	tr := f.seed(t, rel, "DSF", 2822400, 1, true, "", 300, 2)
	spec, err := buildOptimizeSpec(tr, filepath.Join(f.libDir, rel), f.a.outputDir(), f.a.renderTempDir(), capsDSD)
	if err != nil {
		t.Fatal(err)
	}
	spec.SourceSize = tr.Size // finalizeAndEnqueue's stamp, from the row
	one := spec.RenderScratchBytes()
	two := spec.TempVolumeBytes()
	if one <= 0 || two != one*2 {
		t.Fatalf("temp volume = %d, want two scratch files of %d", two, one)
	}
	projected := transcode.ProjectedSize(spec.SourceSize, spec.SourceSampleRate, spec.SourceBits,
		spec.TargetSampleRate, spec.TargetBits, transcode.DefaultCompressionFactor(spec.TargetBits))
	peak := transcode.RenditionHoldOnOneVolume(projected, one, two)
	over := projected + two
	if projected <= 0 || peak != two || over <= peak {
		t.Fatalf("premise: projected %d, two scratches %d, peak %d, rendition on top of both %d", projected, two, peak, over)
	}
	margin := transcode.DefaultDiskSafetyMargin
	scratchDir := transcode.RenderScratchDir(f.a.renderTempDir())
	need := func(scratchFree, variantsFree int64, shared bool, want roomOutcome, wantProbes int) {
		t.Helper()
		var probes int
		f.a.diskFree = func(dir string) (int64, error) {
			probes++
			if dir == scratchDir {
				return scratchFree, nil
			}
			return variantsFree, nil
		}
		f.a.sameVolume = func(string, string) (bool, error) { return shared, nil }
		err := f.a.EnqueueOptimize(rel)
		if got := roomOutcomeOf(err); got != want || probes != wantProbes {
			t.Errorf("shared=%v scratch=%d variants=%d: enqueue = %v, outcome %d after %d probe(s); want %d after %d",
				shared, scratchFree, variantsFree, err, got, probes, want, wantProbes)
		}
	}
	rendition := transcode.RequiredBytesWithMargin(projected, margin)
	need(transcode.RequiredBytesWithMargin(two, margin), rendition, false, roomQueued, 2)
	need(transcode.RequiredBytesWithMargin(one, margin), rendition, false, roomRefused, 2)
	need(0, transcode.RequiredBytesWithMargin(two, margin), true, roomQueued, 1)
	need(0, transcode.RequiredBytesWithMargin(one, margin), true, roomRefused, 1)
}

// TestTheOnDemandPreFlightSaysNothingOfADirectoryNothingHasWrittenYet: the
// pre-flight runs per request, and before the first render makes them the
// variants directory and the render scratch are missing on every request,
// so it probes their closest existing ancestors without the warning the
// sweep's and the batch's probe logs once a pass.
func TestTheOnDemandPreFlightSaysNothingOfADirectoryNothingHasWrittenYet(t *testing.T) {
	logs := loggingtest.Record(t)
	f := newAdapterFixture(t).withCaps(capsDSD)
	const rel = "A/DSD/01.dsf"
	f.seed(t, rel, "DSF", 2822400, 1, true, "", 300, 2)
	f.a.tempDir = func() string { return filepath.Join(t.TempDir(), "not-yet") }
	f.a.sameVolume = func(string, string) (bool, error) { return false, nil } // probe both
	for range 3 {
		if err := f.a.EnqueueOptimize(rel); roomOutcomeOf(err) != roomQueued {
			t.Fatalf("enqueue = %v, want the job to reach the pool", err)
		}
	}
	if lines := logs.Lines("disk probe: directory missing; probing nearest existing ancestor"); len(lines) != 0 {
		t.Errorf("the per-request pre-flight warned about a missing directory:\n%s", strings.Join(lines, "\n"))
	}
}
