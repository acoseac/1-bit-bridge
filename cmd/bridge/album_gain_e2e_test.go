package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The album-level gain end to end, on the real toolchain, through both
// front doors: the serve pool and `bridge optimize`. The fixture is one
// album of two DSF tones, −3 and −12 dBFS. Both renditions must carry the one
// boost the loud track allows, so the quiet file sits 9 dB below the loud one
// as it does in the source; its own guard would have lifted it 4 dB more.

// toneAlbum is the fixture: a store, a library holding the two tones, and
// their rows as the scanner writes them, by title.
type toneAlbum struct {
	store  *manifest.Store
	libDir string
	dir    string
	tracks map[string]*manifest.Track
}

func newToneAlbum(t *testing.T) *toneAlbum {
	t.Helper()
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("sox is not on PATH")
	}
	if !transcode.FFmpegSnapshot().HasDSD {
		t.Skip("no ffmpeg with the DSD decoders on PATH")
	}
	dir := t.TempDir()
	a := &toneAlbum{dir: dir, libDir: filepath.Join(dir, "library"), tracks: map[string]*manifest.Track{}}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a.store = store
	for i, tone := range []struct {
		rel, title string
		dbfs       float64
	}{
		{"Fixture/Tones/01.dsf", "Loud", -3},
		{"Fixture/Tones/02.dsf", "Quiet", -12},
	} {
		abs := filepath.Join(a.libDir, tone.rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := dsdtone.MintDSF(abs, dsdtone.Tone{RateHz: 2822400, Seconds: 2, AmplitudeDBFS: tone.dbfs}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		isDSD, rate, bits, ch, dur, year, n, disc := true, 2822400.0, 1, 2, 2.0, 2026, i+1, 1
		tr := &manifest.Track{
			Path: tone.rel, Size: fi.Size(), ModTime: fi.ModTime(), Codec: "DSF",
			IsDSD: &isDSD, SampleRate: &rate, BitsPerSample: &bits, Channels: &ch, Duration: &dur,
			Title: tone.title, Artist: "Fixture", AlbumArtist: "Fixture", Album: "Tones",
			Year: &year, TrackNumber: &n, DiscNumber: &disc,
		}
		if err := store.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatalf("UpsertTrack(%q): %v", tone.rel, err)
		}
		a.tracks[tone.title] = tr
	}
	return a
}

// rendition is what one rendered tone ended up with.
type rendition struct {
	settings transcode.SoxSettingsView
	rms      float64
}

// rendered reads back the compact rendition of the titled tone: the row, the
// settings it records and the level of the file it points at.
func (a *toneAlbum) rendered(t *testing.T, title string) rendition {
	t.Helper()
	id := "optimized-dsd-" + transcode.DSDRenditionSchemaVersion + "-44100-16"
	row, err := a.store.GetVariant(context.Background(), a.tracks[title].Path, id)
	if err != nil || row == nil {
		t.Fatalf("no %s row for %s (err %v)", id, title, err)
	}
	v, ok := transcode.ParseSoxSettings(row.SoxSettings)
	if !ok || v.AppliedGainDB == nil || v.TrackGainDB == nil || row.AppliedGainDB == nil {
		t.Fatalf("%s settings %q do not carry the gains", title, row.SoxSettings)
	}
	if *row.AppliedGainDB != *v.AppliedGainDB {
		t.Errorf("%s: row gain %.1f, settings %.1f", title, *row.AppliedGainDB, *v.AppliedGainDB)
	}
	return rendition{settings: v, rms: soxRMS(t, row.SidecarPath)}
}

// assertAlbumBalanced is the property both front doors must deliver.
func (a *toneAlbum) assertAlbumBalanced(t *testing.T) {
	t.Helper()
	loudR, quietR := a.rendered(t, "Loud"), a.rendered(t, "Quiet")
	loud, quiet := loudR.settings, quietR.settings
	t.Logf("loud: guard %.1f, applied %.1f, RMS %.2f dB; quiet: guard %.1f, applied %.1f, RMS %.2f dB",
		*loud.TrackGainDB, *loud.AppliedGainDB, loudR.rms, *quiet.TrackGainDB, *quiet.AppliedGainDB, quietR.rms)
	if *quiet.TrackGainDB != 6.0 || *loud.TrackGainDB >= 3.0 {
		t.Fatalf("own guards %.1f (loud) / %.1f (quiet), want about 2 and 6 — the fixture is not what it claims",
			*loud.TrackGainDB, *quiet.TrackGainDB)
	}
	if *loud.AppliedGainDB != *loud.TrackGainDB || *quiet.AppliedGainDB != *loud.AppliedGainDB {
		t.Errorf("applied %.1f (loud) / %.1f (quiet), want both at the loud track's guard %.1f",
			*loud.AppliedGainDB, *quiet.AppliedGainDB, *loud.TrackGainDB)
	}
	if loud.GainScope != transcode.GainScopeAlbum || quiet.GainScope != transcode.GainScopeAlbum {
		t.Errorf("scopes %q / %q, want %q", loud.GainScope, quiet.GainScope, transcode.GainScopeAlbum)
	}
	if d := loudR.rms - quietR.rms; math.Abs(d-9) > 0.1 {
		t.Errorf("the quiet rendition sits %.2f dB below the loud one, want the source's 9", d)
	}
	profile := transcode.DSDPeakProfileFor(transcode.JobKindOptimize, 44100, "-v")
	paths := []string{a.tracks["Loud"].Path, a.tracks["Quiet"].Path}
	if peaks, err := a.store.FreshDSDPeaks(context.Background(), profile, paths); err != nil || len(peaks) != 2 {
		t.Errorf("fresh peaks on %q: %v (err %v), want both tracks on record", profile, peaks, err)
	}
}

// TestAlbumGainEndToEnd_RealToolchain: the serve side, wired by
// wireAlbumGain exactly as runServe wires it. The quiet track renders first
// on a single worker, so its render measures the loud mate through
// albumMateSpec before it applies a boost.
func TestAlbumGainEndToEnd_RealToolchain(t *testing.T) {
	a := newToneAlbum(t)
	caps := transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true}
	outDir, tempDir := filepath.Join(a.dir, "variants"), filepath.Join(a.dir, "scratch")
	pool := transcode.NewPool(a.store, 1, 4)
	t.Cleanup(pool.Stop)
	adapter := &upscaleEnqueuerAdapter{
		pool: pool, store: a.store, resolver: bridgefs.New([]string{a.libDir}), cfg: &config.Config{},
		outputDir: func() string { return outDir },
		tempDir:   func() string { return tempDir },
		dsdCaps:   func() transcode.DSDRenderCaps { return caps },
	}
	if _, err := wireAlbumGain(pool, a.store, adapter); err != nil {
		t.Fatal(err)
	}
	type end struct{ path, errMsg string }
	ends := make(chan end, 4)
	pool.SetOnJobComplete(func(path, _ string, _, _ int, _ float64, _ uuid.UUID, _ time.Time) {
		ends <- end{path: path}
	})
	pool.SetOnJobFailed(func(path, _, errMsg string, _ float64, _ uuid.UUID, _ time.Time) {
		ends <- end{path: path, errMsg: errMsg}
	})
	for _, title := range []string{"Quiet", "Loud"} {
		tr := a.tracks[title]
		spec, err := buildOptimizeSpec(tr, filepath.Join(a.libDir, tr.Path), outDir, tempDir, caps)
		if err != nil {
			t.Fatalf("buildOptimizeSpec(%s): %v", title, err)
		}
		spec.SourceMTimeNS, spec.SourceSize = tr.ModTime.UnixNano(), tr.Size
		if err := pool.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue(%s): %v", title, err)
		}
	}
	for range 2 {
		select {
		case e := <-ends:
			if e.errMsg != "" {
				t.Fatalf("render of %s failed: %s", e.path, e.errMsg)
			}
		case <-time.After(3 * time.Minute):
			t.Fatal("the renders did not finish")
		}
	}
	a.assertAlbumBalanced(t)
}

// TestRenderCLIAlbumGainEndToEnd_RealToolchain: `bridge optimize` through
// runUpscaleBatch, the path a bridge without the auto-optimize sweep moves
// its renditions by. On one worker the loud track renders first and measures
// the quiet mate through cliAlbumMateSpec.
func TestRenderCLIAlbumGainEndToEnd_RealToolchain(t *testing.T) {
	a := newToneAlbum(t)
	var stdout, stderr bytes.Buffer
	code := runUpscaleBatch(context.Background(), &stdout, &stderr, a.store, &config.Config{},
		bridgefs.New([]string{a.libDir}), runUpscaleParams{
			quality:   transcode.QualityVeryHigh,
			workers:   1,
			outputDir: filepath.Join(a.dir, "variants"),
			tempDir:   filepath.Join(a.dir, "scratch"),
			kind:      transcode.JobKindOptimize,
			dsdCaps:   transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true},
		})
	if code != 0 {
		t.Fatalf("exit %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	a.assertAlbumBalanced(t)
}

// soxRMS is the file's overall RMS level in dBFS, from `sox … -n stats`.
func soxRMS(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("sox", path, "-n", "stats").CombinedOutput()
	if err != nil {
		t.Fatalf("sox stats %s: %v\n%s", path, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "RMS lev dB"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					return v
				}
			}
		}
	}
	t.Fatalf("no RMS level in sox stats output:\n%s", out)
	return 0
}
