package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// adapterFixture drives the per-track enqueuer the API handlers call.
// The pool is STOPPED on purpose: an enqueue that reaches it comes back
// as transcode.ErrPoolClosed, which is the proof that every gate before
// the pool — resolve, lookup, sox, eligibility, freshness, resumability —
// passed. A refusal surfaces as the typed api sentinel instead.
type adapterFixture struct {
	a      *upscaleEnqueuerAdapter
	store  *manifest.Store
	libDir string
}

func newAdapterFixture(t *testing.T) *adapterFixture {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "library")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pool := transcode.NewPool(store, 1, 4)
	pool.Stop()
	return &adapterFixture{
		store:  store,
		libDir: libDir,
		a: &upscaleEnqueuerAdapter{
			pool:      pool,
			store:     store,
			resolver:  bridgefs.New([]string{libDir}),
			cfg:       &config.Config{},
			outputDir: func() string { return filepath.Join(dir, "variants") },
			tempDir:   func() string { return "/scratch/render" },
		},
	}
}

func (f *adapterFixture) withCaps(caps transcode.DSDRenderCaps) *adapterFixture {
	f.a.dsdCaps = func() transcode.DSDRenderCaps { return caps }
	return f
}

// seed writes a real (sparse) file — FreshnessFromFile stats it — and the
// manifest row the adapter looks up. Returns the row.
func (f *adapterFixture) seed(t *testing.T, rel, codec string, rate float64, bits int, isDSD bool, compression string, durationSec float64, channels int) *manifest.Track {
	t.Helper()
	abs := filepath.Join(f.libDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(1 << 20); err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	tr := &manifest.Track{
		Path: rel, Size: 1 << 20, ModTime: time.Unix(1700000000, 0),
		Codec: codec, IsDSD: &isDSD, SampleRate: &rate, Compression: compression,
	}
	if bits > 0 {
		b := bits
		tr.BitsPerSample = &b
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
	return tr
}

var (
	capsDSD    = transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true}
	capsDSDDST = transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true, DecodeDST: true}
)

// TestBuildOptimizeSpec_DSDNeedsCaps: the compact tier for a DSD source
// exists only under caps, and the spec it builds carries the render
// facts from the row.
func TestBuildOptimizeSpec_DSDNeedsCaps(t *testing.T) {
	f := newAdapterFixture(t)
	tr := f.seed(t, "A/DSD/01.dsf", "DSF", 2822400, 1, true, "", 300, 2)
	abs := filepath.Join(f.libDir, tr.Path)

	if _, err := buildOptimizeSpec(tr, abs, "/variants", "/scratch", transcode.DSDRenderCaps{}); !errors.Is(err, api.ErrUpscaleIneligible) {
		t.Fatalf("no caps: err = %v, want ErrUpscaleIneligible", err)
	}
	if _, err := buildOptimizeSpec(tr, abs, "/variants", "/scratch", transcode.DSDRenderCaps{Enabled: true}); !errors.Is(err, api.ErrUpscaleIneligible) {
		t.Fatalf("enabled without decoders: err = %v, want ErrUpscaleIneligible (fail closed)", err)
	}
	spec, err := buildOptimizeSpec(tr, abs, "/variants", "/scratch", capsDSD)
	if err != nil {
		t.Fatalf("caps on: %v", err)
	}
	if got, want := spec.VariantID(), "optimized-dsd-v1-44100-16"; got != want {
		t.Errorf("VariantID = %q, want %q", got, want)
	}
	if !spec.SourceIsDSD || spec.Kind != transcode.JobKindOptimize || spec.TargetBits != 16 || spec.TargetSampleRate != 44100 {
		t.Errorf("spec = isDSD:%v kind:%q bits:%d rate:%d, want true/optimize/16/44100",
			spec.SourceIsDSD, spec.Kind, spec.TargetBits, spec.TargetSampleRate)
	}
	if spec.TempDir != "/scratch" || spec.SourceChannels != 2 || spec.SourceDurationSec != 300 || spec.SourceSampleRate != 2822400 {
		t.Errorf("render facts = tempDir:%q ch:%d dur:%v rate:%d, want /scratch 2 300 2822400",
			spec.TempDir, spec.SourceChannels, spec.SourceDurationSec, spec.SourceSampleRate)
	}
	if spec.SourceAbsPath != abs || spec.SourceLibraryRel != tr.Path || spec.OutputDir != "/variants" {
		t.Errorf("paths = %q / %q / %q", spec.SourceAbsPath, spec.SourceLibraryRel, spec.OutputDir)
	}
}

// TestBuildOptimizeSpec_PCMUnchangedByCaps: caps never touch a PCM
// source — same id as before, no DSD facts, no scratch dir.
func TestBuildOptimizeSpec_PCMUnchangedByCaps(t *testing.T) {
	f := newAdapterFixture(t)
	tr := f.seed(t, "A/Album/01.flac", "FLAC", 96000, 24, false, "", 300, 2)
	abs := filepath.Join(f.libDir, tr.Path)
	for _, caps := range []transcode.DSDRenderCaps{{}, capsDSD, capsDSDDST} {
		spec, err := buildOptimizeSpec(tr, abs, "/variants", "/scratch", caps)
		if err != nil {
			t.Fatalf("caps %+v: %v", caps, err)
		}
		if got, want := spec.VariantID(), "optimized-v2-48000-16"; got != want {
			t.Errorf("caps %+v: VariantID = %q, want %q", caps, got, want)
		}
		if spec.SourceIsDSD || spec.TempDir != "" || spec.SourceDurationSec != 0 {
			t.Errorf("caps %+v: a PCM spec carries DSD facts: isDSD=%v tempDir=%q dur=%v",
				caps, spec.SourceIsDSD, spec.TempDir, spec.SourceDurationSec)
		}
	}
}

// TestBuildPCMRenderSpec pins the faithful tier's gate and shape: DSD
// only (a FLAC is ineligible whatever the caps), the family's 4× base
// rate, 24 bits, DST only with the dst decoder.
func TestBuildPCMRenderSpec(t *testing.T) {
	f := newAdapterFixture(t)
	dsf := f.seed(t, "A/DSD/01.dsf", "DSF", 2822400, 1, true, "", 300, 2)
	dsf48 := f.seed(t, "A/DSD/02.dff", "DFF", 3072000, 1, true, "", 300, 2)
	dst := f.seed(t, "A/DSD/03.dff", "DFF", 2822400, 1, true, "DST", 300, 2)
	flac := f.seed(t, "A/Album/01.flac", "FLAC", 96000, 24, false, "", 300, 2)
	abs := func(tr *manifest.Track) string { return filepath.Join(f.libDir, tr.Path) }

	spec, err := buildPCMRenderSpec(dsf, abs(dsf), "/variants", "/scratch", capsDSD)
	if err != nil {
		t.Fatalf("dsf: %v", err)
	}
	if got, want := spec.VariantID(), "pcm-v1-176400-24"; got != want {
		t.Errorf("dsf VariantID = %q, want %q", got, want)
	}
	if spec.Kind != transcode.JobKindPCMRender || spec.TargetBits != 24 || !spec.SourceIsDSD || spec.TempDir != "/scratch" {
		t.Errorf("dsf spec = kind:%q bits:%d isDSD:%v tempDir:%q", spec.Kind, spec.TargetBits, spec.SourceIsDSD, spec.TempDir)
	}
	spec48, err := buildPCMRenderSpec(dsf48, abs(dsf48), "/variants", "/scratch", capsDSD)
	if err != nil {
		t.Fatalf("48k family: %v", err)
	}
	if got, want := spec48.VariantID(), "pcm-v1-192000-24"; got != want {
		t.Errorf("48k family VariantID = %q, want %q", got, want)
	}
	if _, err := buildPCMRenderSpec(flac, abs(flac), "/variants", "/scratch", capsDSDDST); !errors.Is(err, api.ErrUpscaleIneligible) {
		t.Errorf("FLAC: err = %v, want ErrUpscaleIneligible (the pcm kind is DSD-only)", err)
	}
	if _, err := buildPCMRenderSpec(dsf, abs(dsf), "/variants", "/scratch", transcode.DSDRenderCaps{}); !errors.Is(err, api.ErrUpscaleIneligible) {
		t.Errorf("no caps: err = %v, want ErrUpscaleIneligible", err)
	}
	if _, err := buildPCMRenderSpec(dst, abs(dst), "/variants", "/scratch", capsDSD); !errors.Is(err, api.ErrUpscaleIneligible) {
		t.Errorf("DST without the dst decoder: err = %v, want ErrUpscaleIneligible", err)
	}
	specDST, err := buildPCMRenderSpec(dst, abs(dst), "/variants", "/scratch", capsDSDDST)
	if err != nil {
		t.Fatalf("DST with the dst decoder: %v", err)
	}
	if specDST.SourceCompression != "DST" {
		t.Errorf("SourceCompression = %q, want DST", specDST.SourceCompression)
	}
}

// TestAdapterEnqueueKinds_DSDGates walks the three enqueue entry points
// end to end against the stopped pool: the upscale kind refuses DSD
// regardless of caps; optimize and pcm refuse DSD without caps and reach
// the pool with them; pcm refuses a PCM source even with caps.
func TestAdapterEnqueueKinds_DSDGates(t *testing.T) {
	reachedPool := func(err error) bool { return errors.Is(err, transcode.ErrPoolClosed) }
	ineligible := func(err error) bool { return errors.Is(err, api.ErrUpscaleIneligible) }

	t.Run("upscale refuses DSD even with caps", func(t *testing.T) {
		f := newAdapterFixture(t).withCaps(capsDSDDST)
		f.seed(t, "A/DSD/01.dsf", "DSF", 2822400, 1, true, "", 300, 2)
		if err := f.a.EnqueueOne("A/DSD/01.dsf"); !ineligible(err) {
			t.Errorf("EnqueueOne(dsf) = %v, want ErrUpscaleIneligible (DSD is never upscaled)", err)
		}
	})
	t.Run("optimize gates DSD on caps", func(t *testing.T) {
		f := newAdapterFixture(t)
		f.seed(t, "A/DSD/01.dsf", "DSF", 2822400, 1, true, "", 300, 2)
		if err := f.a.EnqueueOptimize("A/DSD/01.dsf"); !ineligible(err) {
			t.Errorf("no caps: EnqueueOptimize(dsf) = %v, want ErrUpscaleIneligible", err)
		}
		f.withCaps(capsDSD)
		if err := f.a.EnqueueOptimize("A/DSD/01.dsf"); !reachedPool(err) {
			t.Errorf("caps on: EnqueueOptimize(dsf) = %v, want the stopped pool's ErrPoolClosed (every gate passed)", err)
		}
	})
	t.Run("pcm gates DSD on caps and refuses PCM", func(t *testing.T) {
		f := newAdapterFixture(t)
		f.seed(t, "A/DSD/01.dsf", "DSF", 2822400, 1, true, "", 300, 2)
		f.seed(t, "A/Album/01.flac", "FLAC", 96000, 24, false, "", 300, 2)
		if err := f.a.EnqueuePCMRender("A/DSD/01.dsf"); !ineligible(err) {
			t.Errorf("no caps: EnqueuePCMRender(dsf) = %v, want ErrUpscaleIneligible", err)
		}
		f.withCaps(capsDSD)
		if err := f.a.EnqueuePCMRender("A/DSD/01.dsf"); !reachedPool(err) {
			t.Errorf("caps on: EnqueuePCMRender(dsf) = %v, want the stopped pool's ErrPoolClosed", err)
		}
		if err := f.a.EnqueuePCMRender("A/Album/01.flac"); !ineligible(err) {
			t.Errorf("caps on: EnqueuePCMRender(flac) = %v, want ErrUpscaleIneligible (the single-file refusal)", err)
		}
		// A path with no manifest row is the adapter's documented silent
		// reject (the scanner has not reached it) — ineligible, not
		// missing; ErrUpscaleSourceMissing is reserved for a path the
		// resolver refuses.
		if err := f.a.EnqueuePCMRender("A/Unindexed/01.dsf"); !ineligible(err) {
			t.Errorf("unindexed path: EnqueuePCMRender = %v, want ErrUpscaleIneligible", err)
		}
	})
}

// There is deliberately NO test that the live-sox decodability check is
// skipped for a DSD row. decodeRouteFor never consults sox for a DSD
// path — it answers from the ffmpeg snapshot alone, the SAME snapshot
// the caps fold — so the verdict is identical with or without the guard
// on every host, and a first draft of such a test passed its own
// negative control on a machine with ffmpeg (the guard only becomes
// observable when ffmpeg is absent, i.e. the test's verdict would depend
// on the host's toolchain). Pinning it would need a transcode-level
// snapshot seam; the guard is layering, documented at the site.
