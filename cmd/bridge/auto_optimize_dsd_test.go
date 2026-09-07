package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// seedDSDTrack is seedTrack's DSD twin: a sparse file plus a DSD row
// carrying the render facts the sweep must forward (nominal rate,
// channels, duration, DSDIFF compression).
func (f *autoOptimizeFixture) seedDSDTrack(t *testing.T, rel, codec string, rate float64, sizeBytes int, compression string, durationSec float64, channels int) {
	t.Helper()
	abs := filepath.Join(f.libDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	fh, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create %q: %v", rel, err)
	}
	if err := fh.Truncate(int64(sizeBytes)); err != nil {
		_ = fh.Close()
		t.Fatalf("truncate %q: %v", rel, err)
	}
	if err := fh.Close(); err != nil {
		t.Fatalf("close %q: %v", rel, err)
	}
	isDSD, bits := true, 1
	tr := &manifest.Track{
		Path: rel, Size: int64(sizeBytes), ModTime: time.Unix(1700000000, 0),
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
		if got, want := job.VariantID(), "optimized-dsd-v1-44100-16"; got != want {
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
		if flac.SourceIsDSD || flac.TempDir != "" {
			t.Errorf("the PCM job must not carry DSD facts: SourceIsDSD=%v TempDir=%q", flac.SourceIsDSD, flac.TempDir)
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

// TestAutoOptimizeSweepProbesScratchVolumeOnlyWithCaps: a PCM-only
// bridge never spends a statfs on a directory it never writes; with caps
// on, the scratch dir is probed and a probe failure fails CLOSED.
func TestAutoOptimizeSweepProbesScratchVolumeOnlyWithCaps(t *testing.T) {
	scratchDir := transcode.RenderScratchDir("/scratch/render")
	t.Run("no caps, no probe", func(t *testing.T) {
		f := newAutoOptimizeFixture(t)
		f.seedTrack(t, "A/Album/01.flac", 4096)
		f.sweeper.tempDir = func() string { return "/scratch/render" }
		f.sweeper.diskFree = func(dir string) (int64, error) {
			if dir == scratchDir {
				t.Errorf("the scratch volume was probed with DSD renditions off")
			}
			return 1 << 50, nil
		}
		if counts := f.sweeper.sweepOnce(context.Background()); counts == nil || counts.Enqueued != 1 {
			t.Fatalf("sweepOnce = %+v, want one enqueued FLAC", counts)
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
}

// TestAutoOptimizeSweepStopsWhenScratchDoesNotFit pins the second disk
// budget: a DSD job whose Stage A scratch would push the scratch volume
// under the floor STOPS the sweep (DiskFloorReached), and the same job
// enqueues when the scratch fits. The check is a point check — scratch is
// freed per job, so the sweep's TOTAL is never held at once — sized for
// the number of lanes the pool can run CONCURRENTLY, because that peak
// is. At one lane two identical jobs both fit when one does; at two they
// do not, which is the last pair of subtests.
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
	t.Run("scratch fits both", func(t *testing.T) {
		f := seed(t, floor+perJob+perJob/2)
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
	// both would hold Stage A scratch simultaneously, so the point check
	// has to be sized for the lane count or two individually-fitting jobs
	// together breach the floor and a job that was already admitted fails
	// mid-render (CodeRabbit on PR #863).
	t.Run("does not fit once two lanes can hold scratch at once", func(t *testing.T) {
		f := seed(t, floor+perJob+perJob/2)
		f.sweeper.lanes = func() int { return 2 }
		counts := f.sweeper.sweepOnce(context.Background())
		if counts == nil {
			t.Fatal("sweepOnce returned nil")
		}
		if !counts.DiskFloorReached {
			t.Error("DiskFloorReached = false, want true (two concurrent renders need 2× the scratch)")
		}
		if counts.Enqueued != 0 {
			t.Errorf("Enqueued = %d, want 0 — swept %v", counts.Enqueued, strings.Join(sweptPaths(f), ", "))
		}
	})
	// A lane count of one (or an unwired probe) is the pre-#863 shape.
	t.Run("one lane is the unwired shape", func(t *testing.T) {
		f := seed(t, floor+perJob+perJob/2)
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
