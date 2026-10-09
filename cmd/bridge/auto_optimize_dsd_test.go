package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// seedDSDTrack is seedTrack's DSD twin: a sparse file plus a DSD row
// carrying the render facts the sweep must forward (nominal rate,
// channels, duration, DSDIFF compression).
func (f *autoOptimizeFixture) seedDSDTrack(t *testing.T, rel, codec string, rate float64, sizeBytes int, compression string, durationSec float64, channels int) {
	t.Helper()
	scanned := f.scannedSparseFile(t, rel, sizeBytes)
	isDSD, bits := true, 1
	tr := &manifest.Track{
		Path: rel, Size: int64(sizeBytes), ModTime: scanned,
		Codec: codec, IsDSD: &isDSD, SampleRate: &rate, BitsPerSample: &bits,
		Compression: compression,
	}
	if durationSec > 0 {
		d := durationSec
		tr.Duration = &d
	}
	if channels > 0 {
		c := channels
		tr.Channels = &c
	}
	if err := f.store.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", rel, err)
	}
}

// seedTimedPCMTrack is seedTrack with a duration and a channel count, the
// facts a PCM job's gain-guard budget is computed from. The row is a
// 96 kHz / 24-bit FLAC, which the optimize query admits.
func (f *autoOptimizeFixture) seedTimedPCMTrack(t *testing.T, rel string, sizeBytes int, durationSec float64, channels int) {
	t.Helper()
	scanned := f.scannedSparseFile(t, rel, sizeBytes)
	rate, bits, dsd := 96000.0, 24, false
	d, c := durationSec, channels
	if err := f.store.UpsertTrack(context.Background(), &manifest.Track{
		Path: rel, Size: int64(sizeBytes), ModTime: scanned,
		SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &dsd,
		Duration: &d, Channels: &c,
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", rel, err)
	}
}

func sweptJob(t *testing.T, f *autoOptimizeFixture, rel string) (transcode.JobSpec, bool) {
	t.Helper()
	for _, s := range f.submitted.snapshot() {
		if s.SourceLibraryRel == rel {
			return s, true
		}
	}
	return transcode.JobSpec{}, false
}

func sweptPaths(f *autoOptimizeFixture) []string {
	var out []string
	for _, s := range f.submitted.snapshot() {
		out = append(out, s.SourceLibraryRel)
	}
	return out
}

// seedMixedLibrary is the three-candidate fixture every caps test uses:
// one PCM, one plain DSF, one DST-compressed DFF.
func seedMixedLibrary(t *testing.T, f *autoOptimizeFixture) {
	t.Helper()
	f.seedTrack(t, "A/Album/01.flac", 4096)
	f.seedDSDTrack(t, "A/DSD/01.dsf", "DSF", 2822400, 1<<20, "", 300, 2)
	f.seedDSDTrack(t, "A/DSD/02.dff", "DFF", 2822400, 1<<20, "DST", 300, 2)
}

// TestAutoOptimizeSweepIncludesDSDOnlyWhenCapsOn: the sweep's candidate
// query admits DSD sources ONLY under live DSD-render caps — nil caps
// (the pre-rendition posture) sweep exactly the PCM library; caps
// without DST add plain DSF/DFF; caps with DST add the DST row too. The
// DSD job carries the render facts the two-stage chain reads.
func TestAutoOptimizeSweepIncludesDSDOnlyWhenCapsOn(t *testing.T) {
	t.Run("caps unwired sweeps PCM only", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		seedMixedLibrary(t, f)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if got := sweptPaths(f); len(got) != 1 || got[0] != "A/Album/01.flac" {
			t.Fatalf("swept %v, want only the FLAC with no DSD-render caps", got)
		}
	})
	t.Run("caps without DST add plain DSD", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		seedMixedLibrary(t, f)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps {
			return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true}
		}
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if counts.Enqueued != 2 {
			t.Fatalf("Enqueued = %d, want 2 (FLAC + plain DSF; the DST row needs the dst decoder) — swept %v",
				counts.Enqueued, sweptPaths(f))
		}
		if _, dst := sweptJob(t, f, "A/DSD/02.dff"); dst {
			t.Error("the DST-compressed DFF was swept without a dst decoder")
		}
		job, ok := sweptJob(t, f, "A/DSD/01.dsf")
		if !ok {
			t.Fatalf("the DSF was not swept; swept %v", sweptPaths(f))
		}
		if !job.SourceIsDSD {
			t.Error("SourceIsDSD = false on the DSF job; VariantID() would land in the PCM family")
		}
		if got, want := job.VariantID(), "optimized-dsd-v2-44100-16"; got != want {
			t.Errorf("VariantID = %q, want %q", got, want)
		}
		if job.Kind != transcode.JobKindOptimize || job.TargetBits != 16 || job.TargetSampleRate != 44100 {
			t.Errorf("kind/bits/rate = %q/%d/%d, want optimize/16/44100", job.Kind, job.TargetBits, job.TargetSampleRate)
		}
		if !job.Background {
			t.Error("a swept DSD render must ride the low-priority lane like every swept job")
		}
		if job.TempDir != "/scratch/render" {
			t.Errorf("TempDir = %q, want the sweeper's %q (Stage A scratch goes there)", job.TempDir, "/scratch/render")
		}
		if job.SourceChannels != 2 || job.SourceDurationSec != 300 {
			t.Errorf("channels/duration = %d/%v, want the row's 2/300", job.SourceChannels, job.SourceDurationSec)
		}
		if job.SourceSampleRate != 2822400 {
			t.Errorf("SourceSampleRate = %d, want the NOMINAL DSD rate 2822400", job.SourceSampleRate)
		}
		flac, ok := sweptJob(t, f, "A/Album/01.flac")
		if !ok {
			t.Fatal("the FLAC was not swept")
		}
		if flac.SourceIsDSD || flac.TempDir != "/scratch/render" {
			t.Errorf("the PCM job lands its gain-guard file in the scratch dir: SourceIsDSD=%v TempDir=%q", flac.SourceIsDSD, flac.TempDir)
		}
	})
	t.Run("caps with DST add the DST row", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		seedMixedLibrary(t, f)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps {
			return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}
		}
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if counts.Enqueued != 3 {
			t.Fatalf("Enqueued = %d, want 3 — swept %v", counts.Enqueued, sweptPaths(f))
		}
		dst, ok := sweptJob(t, f, "A/DSD/02.dff")
		if !ok {
			t.Fatal("the DST row was not swept with a dst decoder")
		}
		if dst.SourceCompression != "DST" {
			t.Errorf("SourceCompression = %q, want DST (the chain doubles its timeout on it)", dst.SourceCompression)
		}
	})
	t.Run("enabled caps without decoders sweep PCM only", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		seedMixedLibrary(t, f)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true} }
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if got := sweptPaths(f); len(got) != 1 || got[0] != "A/Album/01.flac" {
			t.Fatalf("swept %v, want only the FLAC when ffmpeg lacks the DSD decoders (fail closed)", got)
		}
	})
}

// TestAutoOptimizeSweepProbesTheScratchVolumeOnEverySweep: the temp
// volume holds a PCM job's gain-guard file, so a PCM-only bridge probes
// it too. A probe failure skips the sweep with or without DSD caps.
func TestAutoOptimizeSweepProbesTheScratchVolumeOnEverySweep(t *testing.T) {
	scratchDir := transcode.RenderScratchDir("/scratch/render")
	t.Run("no caps still probes", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.seedTrack(t, "A/Album/01.flac", 4096)
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		probed := false
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				probed = true
			}
			return 1 << 50, nil
		}
		if counts := f.sweeper.sweepOnce(context.Background()); counts == nil || counts.Enqueued != 1 {
			t.Fatalf("sweepOnce = %+v, want one enqueued FLAC", counts)
		}
		if !probed {
			t.Errorf("diskFree was never asked about %q", scratchDir)
		}
	})
	t.Run("caps on probes the scratch dir", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.seedTrack(t, "A/Album/01.flac", 4096)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		probed := false
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				probed = true
			}
			return 1 << 50, nil
		}
		if counts := f.sweeper.sweepOnce(context.Background()); counts == nil || counts.Enqueued != 1 {
			t.Fatalf("sweepOnce = %+v, want one enqueued FLAC", counts)
		}
		if !probed {
			t.Errorf("diskFree was never asked about %q", scratchDir)
		}
	})
	t.Run("scratch probe error fails closed", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.seedTrack(t, "A/Album/01.flac", 4096)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				return 0, errors.New("statfs boom")
			}
			return 1 << 50, nil
		}
		if counts := f.sweeper.sweepOnce(context.Background()); counts != nil {
			t.Errorf("sweepOnce = %+v, want nil (fail closed on an unreadable scratch volume)", counts)
		}
		if f.submitted.count() != 0 {
			t.Errorf("enqueued %d jobs despite an unreadable scratch volume, want 0", f.submitted.count())
		}
	})
	t.Run("no caps, scratch probe error fails closed", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.seedTrack(t, "A/Album/01.flac", 4096)
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				return 0, errors.New("statfs boom")
			}
			return 1 << 50, nil
		}
		if counts := f.sweeper.sweepOnce(context.Background()); counts != nil {
			t.Errorf("sweepOnce = %+v, want nil (fail closed on an unreadable scratch volume)", counts)
		}
		if f.submitted.count() != 0 {
			t.Errorf("enqueued %d jobs despite an unreadable scratch volume, want 0", f.submitted.count())
		}
	})
}

// TestAutoOptimizeSweepSubmitsAPCMJobWhenTheTempVolumeHasRoom pins the
// PCM half of the scratch budget. A candidate with a known duration has
// a positive TempVolumeBytes, so the sweep has to have read the temp
// volume's free space or the floor check stops before any job is
// submitted. The tight-space case is the control: a sweep that skips
// the floor check once the probe runs would submit it.
func TestAutoOptimizeSweepSubmitsAPCMJobWhenTheTempVolumeHasRoom(t *testing.T) {
	const floor = 100 << 20
	const durationSec = 300.0
	scratchDir := transcode.RenderScratchDir("/scratch/render")
	target := transcode.TargetRateForOptimize(96000)
	perJob := transcode.JobSpec{
		SourceChannels: 2, SourceDurationSec: durationSec, TargetSampleRate: target,
	}.GuardTempBytes()
	if perJob <= 0 {
		t.Fatalf("fixture: a 300 s stereo PCM job budgets %d bytes of guard file", perJob)
	}
	seed := func(t *testing.T, scratchFree int64, probeErr error) *autoOptimizeFixture {
		t.Helper()
		f := newAutoOptimizeFixture(t)
		f.seedTimedPCMTrack(t, "A/Album/01.flac", 4096, durationSec, 2)
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		f.sweeper.minFreeBytes = func() int64 { return floor }
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				return scratchFree, probeErr
			}
			return 1 << 50, nil
		}
		return f
	}

	t.Run("room on the temp volume submits the job", func(t *testing.T) {
		f := seed(t, floor+perJob, nil)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if counts.DiskFloorReached {
			t.Error("DiskFloorReached = true, want false (the guard file fits beside the floor)")
		}
		if counts.Enqueued != 1 || f.submitted.count() != 1 {
			t.Errorf("Enqueued = %d (submitted %d), want 1 — swept %v",
				counts.Enqueued, f.submitted.count(), strings.Join(sweptPaths(f), ", "))
		}
	})
	t.Run("below the floor submits nothing", func(t *testing.T) {
		f := seed(t, floor+perJob-1, nil)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if !counts.DiskFloorReached {
			t.Error("DiskFloorReached = false, want true (the guard file would breach the floor)")
		}
		if counts.Enqueued != 0 || f.submitted.count() != 0 {
			t.Errorf("Enqueued = %d (submitted %d), want 0", counts.Enqueued, f.submitted.count())
		}
	})
	t.Run("a probe failure skips the sweep", func(t *testing.T) {
		f := seed(t, floor+perJob, errors.New("statfs boom"))
		if counts := f.sweeper.sweepOnce(context.Background()); counts != nil {
			t.Errorf("sweepOnce = %+v, want nil (an unreadable temp volume skips the sweep)", counts)
		}
		if f.submitted.count() != 0 {
			t.Errorf("enqueued %d jobs despite an unreadable temp volume, want 0", f.submitted.count())
		}
	})
}

// TestAutoOptimizeSweepStopsWhenScratchDoesNotFit pins the second disk
// budget: a DSD job whose Stage A scratch would push the scratch volume
// under the floor STOPS the sweep (DiskFloorReached), and the same job
// enqueues when the scratch fits. A render holds TWO of those files while
// it surveys its album. The check is a point check — scratch is freed per
// job, so the sweep's TOTAL is never held at once — sized for the number
// of lanes the pool can run CONCURRENTLY, because that peak is. At one
// lane two identical jobs both fit when one does; at two they do not.
func TestAutoOptimizeSweepStopsWhenScratchDoesNotFit(t *testing.T) {
	const floor = 100 << 20
	const durationSec = 300.0
	scratchDir := transcode.RenderScratchDir("/scratch/render")
	perJob := transcode.TempBytesForRender(2, 44100, durationSec)
	if perJob <= 0 || perJob > 200<<20 {
		t.Fatalf("fixture: per-job scratch %d bytes is not in the ~106 MB range the test assumes", perJob)
	}
	seed := func(t *testing.T, scratchFree int64) *autoOptimizeFixture {
		f := newAutoOptimizeFixture(t)
		f.seedDSDTrack(t, "A/DSD/01.dsf", "DSF", 2822400, 1<<20, "", durationSec, 2)
		f.seedDSDTrack(t, "A/DSD/02.dsf", "DSF", 2822400, 1<<20, "", durationSec, 2)
		f.sweeper.dsdCaps = func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		f.sweeper.minFreeBytes = func() int64 { return floor }
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				return scratchFree, nil
			}
			return 1 << 50, nil
		}
		return f
	}

	t.Run("scratch does not fit", func(t *testing.T) {
		f := seed(t, floor+perJob/2)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if !counts.DiskFloorReached {
			t.Error("DiskFloorReached = false, want true (the render's scratch would breach the floor)")
		}
		if counts.Enqueued != 0 || f.submitted.count() != 0 {
			t.Errorf("Enqueued = %d (submitted %d), want 0", counts.Enqueued, f.submitted.count())
		}
	})
	// Room for one Stage A file and not the second the survey writes
	// beside it. One lane, so a budget of a single file would enqueue.
	t.Run("room for one scratch file and not two", func(t *testing.T) {
		f := seed(t, floor+perJob+perJob/2)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if !counts.DiskFloorReached || counts.Enqueued != 0 {
			t.Errorf("DiskFloorReached=%v Enqueued=%d, want true / 0 (one file fits, two do not)", counts.DiskFloorReached, counts.Enqueued)
		}
	})
	t.Run("two scratch files fit one lane", func(t *testing.T) {
		f := seed(t, floor+2*perJob+perJob/2)
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if counts.DiskFloorReached {
			t.Error("DiskFloorReached = true, want false (scratch is a point check per job, not a running sum)")
		}
		if counts.Enqueued != 2 {
			t.Errorf("Enqueued = %d, want both DSD jobs — swept %v", counts.Enqueued, strings.Join(sweptPaths(f), ", "))
		}
	})
	// The same free space, with the pool able to run TWO renders at once:
	// each holds two Stage A files, so the point check has to be sized for
	// the lane count or two individually-fitting jobs together breach the
	// floor and a job that was already admitted fails mid-render.
	t.Run("does not fit once two lanes can hold scratch at once", func(t *testing.T) {
		f := seed(t, floor+2*perJob+perJob/2)
		f.sweeper.lanes = func() int { return 2 }
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if !counts.DiskFloorReached {
			t.Error("DiskFloorReached = false, want true (two lanes each hold two scratch files)")
		}
		if counts.Enqueued != 0 {
			t.Errorf("Enqueued = %d, want 0 — swept %v", counts.Enqueued, strings.Join(sweptPaths(f), ", "))
		}
	})
	t.Run("one lane is the unwired shape", func(t *testing.T) {
		f := seed(t, floor+2*perJob+perJob/2)
		f.sweeper.lanes = func() int { return 1 }
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if counts.DiskFloorReached || counts.Enqueued != 2 {
			t.Errorf("one lane: DiskFloorReached=%v Enqueued=%d, want false / 2", counts.DiskFloorReached, counts.Enqueued)
		}
	})
}

// seedRendition records a rendition row for rel, fresh against the track
// row.
func (f *autoOptimizeFixture) seedRendition(t *testing.T, rel, variantID string, rate, bits int) {
	t.Helper()
	st, err := f.store.GetTrackStat(context.Background(), rel)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat(%q) = %v, %v", rel, st, err)
	}
	gain := 4.0
	if err := f.store.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: rel, VariantID: variantID, SidecarPath: "/variants/" + rel + "." + variantID + ".flac",
		Format: "flac", SampleRate: rate, BitsPerSample: bits, SizeBytes: 1000,
		SourceMTimeNS: st.MTimeNS, SourceSize: st.Size, AppliedGainDB: &gain,
		SoxSettings: `{"rateFlag":"-v"}`, CreatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("UpsertVariant(%q, %q): %v", rel, variantID, err)
	}
}

// TestAutoOptimizeSweepMovesFaithfulRenditionsToTheCurrentSchema: the
// faithful tier is rendered on request only, and a phone that holds a `pcm-`
// rendition never asks for another, so after a DSD schema bump the sweeper
// re-renders exactly the tracks that already have one — a faithful job on
// the background lane with the same facts an on-request render takes. A
// track with no faithful rendition never gains one, a sweep whose cap the
// compact pass spent adds nothing, and the backlog shows in "remaining".
func TestAutoOptimizeSweepMovesFaithfulRenditionsToTheCurrentSchema(t *testing.T) {
	caps := func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
	v := transcode.DSDRenditionSchemaVersion

	t.Run("both passes in one sweep", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.sweeper.dsdCaps = caps
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		seedSchemaMoveLibrary(t, f)
		counts := sweepOnceOrFail(t, f)
		got := sweptJobsByID(f)
		checkSweptJobIDs(t, f, got, []string{
			"A/DSD/01.dsf pcm-" + v + "-176400-24",
			"A/DSD/03.dsf optimized-dsd-" + v + "-48000-16",
			"A/DSD/03.dsf pcm-" + v + "-192000-24",
		})
		if counts.Enqueued != 3 || counts.Regenerated != 3 {
			t.Errorf("Enqueued/Regenerated = %d/%d, want 3/3 (every one replaces an older rendition)", counts.Enqueued, counts.Regenerated)
		}
		checkFaithfulMoveJob(t, f, got["A/DSD/01.dsf pcm-"+v+"-176400-24"], "A/DSD/01.dsf")
		// Nothing was written (the enqueuer only records), so the whole
		// backlog is still there: one compact candidate, two faithful.
		if counts.Remaining != 3 {
			t.Errorf("Remaining = %d, want 3 (the compact candidate plus the two faithful renditions)", counts.Remaining)
		}
	})
	t.Run("a spent cap leaves the faithful pass nothing", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.sweeper.dsdCaps = caps
		f.sweeper.maxPerSweep = func() int { return 1 }
		seedSchemaMoveLibrary(t, f)
		counts := sweepOnceOrFail(t, f)
		if counts.Enqueued != 1 {
			t.Errorf("Enqueued %d, want the one job the cap allows", counts.Enqueued)
		}
		checkSweptJobIDs(t, f, sweptJobsByID(f), []string{"A/DSD/03.dsf optimized-dsd-" + v + "-48000-16"})
	})
	t.Run("DSD renditions off move nothing", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		seedSchemaMoveLibrary(t, f)
		counts := sweepOnceOrFail(t, f)
		if got := sweptPaths(f); len(got) != 0 || counts.Remaining != 0 {
			t.Errorf("swept %v, Remaining %d: with the caps off neither tier moves", got, counts.Remaining)
		}
	})
}

// seedSchemaMoveLibrary is the DSD v1 → v2 sweep fixture:
//   - 01: a current compact rendition and a v1 faithful one, so only the
//     faithful pass has work;
//   - 02: a current compact rendition and no faithful one, so nothing;
//   - 03 (the 48k family): both tiers on v1, so both passes pick it up.
func seedSchemaMoveLibrary(t *testing.T, f *autoOptimizeFixture) {
	t.Helper()
	compactV2 := "optimized-dsd-" + transcode.DSDRenditionSchemaVersion + "-44100-16"
	f.seedDSDTrack(t, "A/DSD/01.dsf", "DSF", 2822400, 1<<20, "", 300, 2)
	f.seedRendition(t, "A/DSD/01.dsf", compactV2, 44100, 16)
	f.seedRendition(t, "A/DSD/01.dsf", "pcm-v1-176400-24", 176400, 24)
	f.seedDSDTrack(t, "A/DSD/02.dsf", "DSF", 2822400, 1<<20, "", 300, 2)
	f.seedRendition(t, "A/DSD/02.dsf", compactV2, 44100, 16)
	f.seedDSDTrack(t, "A/DSD/03.dsf", "DSF", 3072000, 1<<20, "", 200, 2)
	f.seedRendition(t, "A/DSD/03.dsf", "optimized-dsd-v1-48000-16", 48000, 16)
	f.seedRendition(t, "A/DSD/03.dsf", "pcm-v1-192000-24", 192000, 24)
}

// sweepOnceOrFail runs one sweep and fails the test when it reports nothing.
func sweepOnceOrFail(t *testing.T, f *autoOptimizeFixture) *admin.AutoOptimizeSweepCounts {
	t.Helper()
	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	return counts
}

// sweptJobsByID keys the recorded jobs by "<path> <variant id>".
func sweptJobsByID(f *autoOptimizeFixture) map[string]transcode.JobSpec {
	out := map[string]transcode.JobSpec{}
	for _, s := range f.submitted.snapshot() {
		out[s.SourceLibraryRel+" "+s.VariantID()] = s
	}
	return out
}

// checkSweptJobIDs requires exactly the want jobs, in any order.
func checkSweptJobIDs(t *testing.T, f *autoOptimizeFixture, got map[string]transcode.JobSpec, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("swept %d jobs %v, want %v", len(got), sweptPaths(f), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing job %q; swept %v", k, sweptPaths(f))
		}
	}
}

// checkFaithfulMoveJob asserts the faithful job the sweep built for rel: the
// pcm kind at the family's 4x rate and 24 bits, on the background lane with
// the sweeper's scratch dir, at the on-request quality, with the render facts
// and source facts of the track row (the latter are what freshness compares).
func checkFaithfulMoveJob(t *testing.T, f *autoOptimizeFixture, job transcode.JobSpec, rel string) {
	t.Helper()
	if job.Kind != transcode.JobKindPCMRender || job.TargetBits != 24 || job.TargetSampleRate != 176400 {
		t.Errorf("kind/bits/rate = %q/%d/%d, want pcm/24/176400", job.Kind, job.TargetBits, job.TargetSampleRate)
	}
	if !job.Background || !job.SourceIsDSD || job.TempDir != "/scratch/render" {
		t.Errorf("Background/SourceIsDSD/TempDir = %v/%v/%q, want a background DSD render with the sweeper's scratch dir",
			job.Background, job.SourceIsDSD, job.TempDir)
	}
	if job.SourceChannels != 2 || job.SourceDurationSec != 300 || job.SourceSampleRate != 2822400 || job.Quality != transcode.QualityVeryHigh {
		t.Errorf("channels/duration/rate/quality = %d/%v/%d/%v, want the row's 2/300/2822400 and the on-request quality",
			job.SourceChannels, job.SourceDurationSec, job.SourceSampleRate, job.Quality)
	}
	st, err := f.store.GetTrackStat(context.Background(), rel)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat(%q) = %v, %v", rel, st, err)
	}
	if job.SourceMTimeNS != st.MTimeNS || job.SourceSize != st.Size {
		t.Errorf("source facts %d/%d, want the track row's %d/%d (what freshness compares)",
			job.SourceMTimeNS, job.SourceSize, st.MTimeNS, st.Size)
	}
}
