//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// A render into a variants directory whose volume is away (backlog B727's
// first item). The variants directory is on a volume mounted at mnt; while the
// volume is away, mnt is a bare directory and the variants directory below it
// is not there. A render made it there (Run's MkdirAll of the sidecar's
// directory) and published a rendition into it, and the mount-loss probe
// (integrity.VariantsDirSweepBlock), which reads a directory holding a
// rendition as mounted, then let serve's reapers delete the rows of the
// renditions on the volume: the watcher's tick, under the mass-delete floor of
// ten rows, and a download's reap of a rendition it could not open, at any
// size of catalog. These tests render through the CLI's worker
// (runUpscaleWorker, transcode.Run and a stand-in sox) and run serve's two
// reapers through their adapters, over the real store.

// awayVariant is the variant every catalog row here names.
const awayVariant = "upscaled-v2-176400-24"

// awayVolume is a library of tracks whose renditions are in the variants
// directory dir, on the volume mounted at mnt, with the volume then gone: mnt
// moved aside to volume and a bare directory left at its path.
type awayVolume struct {
	store                 *manifest.Store
	lib, mnt, dir, volume string
	// rows are the catalog's rows of the renditions on the volume.
	rows []manifest.VariantRow
}

// newAwayVolume records n renditions in dir and takes their volume away.
func newAwayVolume(t *testing.T, n int) *awayVolume {
	t.Helper()
	root := t.TempDir()
	a := &awayVolume{lib: filepath.Join(root, "lib"), mnt: filepath.Join(root, "mnt")}
	a.dir = filepath.Join(a.mnt, "variants")
	store, err := manifest.OpenStore(filepath.Join(root, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a.store = store
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("Artist/Album/%02d.flac", i)
		fi := a.writeSource(t, rel)
		sidecar := transcode.VariantSidecarPath(a.dir, rel, awayVariant)
		writeFixtureFile(t, sidecar, 1000+i)
		row := manifest.VariantRow{
			SourcePath: rel, VariantID: awayVariant, SidecarPath: sidecar, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: int64(1000 + i),
			SourceMTimeNS: fi.ModTime().UnixNano(), SourceSize: fi.Size(), CreatedAt: int64(1_000_000 + i),
		}
		if err := store.UpsertVariant(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		a.rows = append(a.rows, row)
	}
	a.volume = a.mnt + ".volume"
	if err := os.Rename(a.mnt, a.volume); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	return a
}

// writeSource writes the library's file rel and indexes it, a 96 kHz / 24-bit
// FLAC row a 48 kHz / 16-bit optimize job renders.
func (a *awayVolume) writeSource(t *testing.T, rel string) os.FileInfo {
	t.Helper()
	src := filepath.Join(a.lib, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("source-bytes of "+rel), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	rate, bits := 96000.0, 24
	if err := a.store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: fi.Size(),
		ModTime: fi.ModTime(), Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
		t.Fatal(err)
	}
	return fi
}

// render renders the library's file rel into dir through the CLI's worker and
// returns the job's spec, how many were done and failed, and what the worker
// printed.
func (a *awayVolume) render(t *testing.T, rel string) (spec transcode.JobSpec, done, failed uint64, stderr string) {
	t.Helper()
	fi := a.writeSource(t, rel)
	spec = transcode.JobSpec{SourceAbsPath: filepath.Join(a.lib, filepath.FromSlash(rel)), SourceLibraryRel: rel,
		SourceMTimeNS: fi.ModTime().UnixNano(), SourceSize: fi.Size(), SourceSampleRate: 96000, SourceBits: 24,
		TargetSampleRate: 48000, TargetBits: 16, Kind: transcode.JobKindOptimize,
		Quality: transcode.QualityVeryHigh, OutputDir: a.dir}
	jobs := make(chan upscaleCandidate, 1)
	jobs <- upscaleCandidate{spec: spec, needsRun: true}
	close(jobs)
	var out bytes.Buffer
	runUpscaleWorker(context.Background(), &out, a.store, jobs, &done, &failed)
	return spec, done, failed, out.String()
}

// requireRowsKept fails the test for each of the catalog's rows the store no
// longer holds: their renditions are on the volume, which is only away.
func (a *awayVolume) requireRowsKept(t *testing.T) {
	t.Helper()
	for _, v := range a.rows {
		row, err := a.store.GetVariant(context.Background(), v.SourcePath, v.VariantID)
		switch {
		case err != nil:
			t.Fatal(err)
		case row == nil:
			t.Errorf("the row of %s was deleted while its rendition is on the volume that is away", v.SourcePath)
		}
	}
}

// download asks serve's /v1/download for row's rendition, through the api's
// real handler and serve's adapters, and returns the status and body.
func (a *awayVolume) download(t *testing.T, row manifest.VariantRow) (int, string) {
	t.Helper()
	tokens, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Mint("away")
	if err != nil {
		t.Fatal(err)
	}
	provider := manifest.NewProvider(a.store, nil)
	variantsDir := func() string { return a.dir }
	srv := api.New(&config.Config{LibraryRoots: []string{a.lib}}, tokens, provider, "away-fingerprint").
		WithUpscale(func() bool { return true }, &variantStoreAdapter{provider: provider, store: a.store, variantsDir: variantsDir}).
		WithVariantDeleter(&variantDeleterAdapter{store: a.store, variantsDir: variantsDir})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	req, err := http.NewRequest(http.MethodGet,
		hs.URL+"/v1/download?path="+urlquery.Escape(row.SourcePath)+"&variant="+urlquery.Escape(row.VariantID), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// useRenderingSox puts the stand-in sox alone on PATH.
func useRenderingSox(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte(fakeRenderingSoxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

// TestARenderWritesNothingIntoAVariantsVolumeThatIsAway: the render refuses,
// saying why and how to go on, writes nothing under the bare mount point and
// records nothing. On main it made the variants directory there and published
// a rendition into it.
func TestARenderWritesNothingIntoAVariantsVolumeThatIsAway(t *testing.T) {
	useRenderingSox(t)
	a := newAwayVolume(t, 3)
	spec, done, failed, stderr := a.render(t, "Other/Album/01.flac")
	if done != 0 || failed != 1 {
		t.Errorf("done=%d failed=%d, want the render refused\n%s", done, failed, stderr)
	}
	entries, err := os.ReadDir(a.mnt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the render wrote %v into the mount point of the volume that is away", names)
	}
	for _, want := range []string{"FAIL Other/Album/01.flac", "variants directory", "not mounted", "--allow-mass-delete"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, stderr)
		}
	}
	if row, err := a.store.GetVariant(context.Background(), spec.SourceLibraryRel, spec.VariantID()); err != nil || row != nil {
		t.Errorf("a rendition was recorded (row %v, err %v)", row, err)
	}
}

// TestTheRenderCommandsRefuseAVariantsVolumeThatIsAwayBeforeAnyJob: `bridge
// optimize` (the batch `upscale` and `render` share) refuses the whole run
// once, before any job, rather than a FAIL line per track; with the volume
// back the same run renders every candidate.
func TestTheRenderCommandsRefuseAVariantsVolumeThatIsAwayBeforeAnyJob(t *testing.T) {
	useRenderingSox(t)
	a := newAwayVolume(t, 3)
	batch := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := runUpscaleBatch(context.Background(), &stdout, &stderr, a.store, &config.Config{}, bridgefs.New([]string{a.lib}),
			runUpscaleParams{targetRateFlag: "auto", targetBits: 16, quality: transcode.QualityVeryHigh, workers: 1,
				kind: transcode.JobKindOptimize, outputDir: a.dir})
		return code, stdout.String() + stderr.String()
	}
	code, out := batch()
	if code != 1 || !strings.Contains(out, "Not rendering") || !strings.Contains(out, "not mounted") || strings.Contains(out, "FAIL") {
		t.Errorf("exit %d, want 1 and one refusal before any job:\n%s", code, out)
	}
	if entries, err := os.ReadDir(a.mnt); err != nil || len(entries) != 0 {
		t.Errorf("the refused run left %d entr(ies) in the bare mount point (%v)", len(entries), err)
	}

	if err := os.Remove(a.mnt); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(a.volume, a.mnt); err != nil {
		t.Fatal(err)
	}
	if code, out := batch(); code != 0 || !strings.Contains(out, "Converted 3, failed 0") {
		t.Errorf("with the volume back: exit %d, want 0 and the three candidates rendered:\n%s", code, out)
	}
}

// TestTheWatcherKeepsTheRowsOfAVariantsVolumeThatIsAwayAfterARender: on main,
// three rows (under the mass-delete floor) were deleted by the tick after the
// render; twelve were refused as a relocation, the floor's own guard.
func TestTheWatcherKeepsTheRowsOfAVariantsVolumeThatIsAwayAfterARender(t *testing.T) {
	useRenderingSox(t)
	for _, n := range []int{3, 12} {
		t.Run(fmt.Sprintf("%d rows", n), func(t *testing.T) {
			a := newAwayVolume(t, n)
			a.render(t, "Other/Album/01.flac")
			r, published := runOneVariantSweep(t, a.store, &integrityVariantListerAdapter{store: a.store}, a.dir)
			a.requireRowsKept(t)
			if r.Deleted != 0 || len(published) != 0 {
				t.Errorf("report %+v, published %v as deleted; want nothing deleted", r, published)
			}
		})
	}
}

// TestADownloadKeepsTheRowOfARenditionOnAVariantsVolumeThatIsAway: the client
// gets the 410 it falls back to the source on, and the row stays. On main the
// reap deleted it, whatever the catalog's size.
func TestADownloadKeepsTheRowOfARenditionOnAVariantsVolumeThatIsAway(t *testing.T) {
	useRenderingSox(t)
	for _, n := range []int{3, 12} {
		t.Run(fmt.Sprintf("%d rows", n), func(t *testing.T) {
			a := newAwayVolume(t, n)
			a.render(t, "Other/Album/01.flac")
			status, body := a.download(t, a.rows[0])
			if status != http.StatusGone || !strings.Contains(body, "variant_missing_on_disk") {
				t.Errorf("status %d body %s, want 410 variant_missing_on_disk", status, body)
			}
			a.requireRowsKept(t)
		})
	}
}

// TestARenderMakesTheVariantsDirectoryOfACatalogThatRecordsNoneThere — the
// controls: a fresh install, its catalog empty, and a catalog whose
// renditions are all recorded under another directory (the variants
// directory set to a new one, its renditions not moved yet), each render
// into a variants directory that is not there yet: it is made and the
// rendition published, as before. No record is no evidence of a volume.
func TestARenderMakesTheVariantsDirectoryOfACatalogThatRecordsNoneThere(t *testing.T) {
	useRenderingSox(t)
	for _, elsewhere := range []int{0, 3} {
		t.Run(fmt.Sprintf("%d rows elsewhere", elsewhere), func(t *testing.T) {
			a := newAwayVolume(t, elsewhere)
			a.dir = filepath.Join(t.TempDir(), "new-disk", "variants")
			spec, done, failed, stderr := a.render(t, "Other/Album/01.flac")
			if done != 1 || failed != 0 {
				t.Fatalf("done=%d failed=%d, want the rendition published\n%s", done, failed, stderr)
			}
			if _, err := os.Stat(spec.SidecarPath()); err != nil {
				t.Errorf("the rendition is not at %s: %v", spec.SidecarPath(), err)
			}
		})
	}
}

// TestARenderIntoAVariantsVolumeThatIsBackPublishes — the control: the volume
// back at mnt, the render publishes beside the renditions there, and the
// watcher's tick finds every row present.
func TestARenderIntoAVariantsVolumeThatIsBackPublishes(t *testing.T) {
	useRenderingSox(t)
	a := newAwayVolume(t, 3)
	if err := os.Remove(a.mnt); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(a.volume, a.mnt); err != nil {
		t.Fatal(err)
	}
	spec, done, failed, stderr := a.render(t, "Other/Album/01.flac")
	if done != 1 || failed != 0 {
		t.Fatalf("done=%d failed=%d, want the rendition published\n%s", done, failed, stderr)
	}
	if _, err := os.Stat(spec.SidecarPath()); err != nil {
		t.Errorf("the rendition is not at %s: %v", spec.SidecarPath(), err)
	}
	r, _ := runOneVariantSweep(t, a.store, &integrityVariantListerAdapter{store: a.store}, a.dir)
	a.requireRowsKept(t)
	if r.Deleted != 0 || r.Present != len(a.rows)+1 {
		t.Errorf("report %+v, want all %d rows present", r, len(a.rows)+1)
	}
}
