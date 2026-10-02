package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// `bridge doctor`'s variants-index hint names `bridge upscale --gc`, so an
// operator runs it beside a live bridge, whose job pools keep writing: a
// render renames its rendition into place and commits the row after that,
// an analysis does the same with a waveform, and a `.tmp` sits beside each
// while it is written. A `--gc` run lists the catalog first and judges the
// files and rows later, so whatever those writers do in between is judged
// by a listing that predates it. These tests run the real runGC and
// runAnalyzeGC with a store whose listing hook does what the live bridge
// does at that moment (backlog B205 for the files, B250 for the rows).

// gcLiveVariant is the rendition every test here publishes.
const gcLiveVariant = "upscaled-v2-176400-24"

// gcListThenAct is the store a `--gc` run lists from, with a hook that runs
// once, after the first listing returns and before the run sees it: the
// moment inside a run at which a live bridge's writers change what the run
// has listed.
type gcListThenAct struct {
	*manifest.Store
	once sync.Once
	act  func()
}

// AllVariants lists through the store, then runs the hook.
func (s *gcListThenAct) AllVariants(ctx context.Context) ([]manifest.VariantRow, error) {
	rows, err := s.Store.AllVariants(ctx)
	s.once.Do(s.act)
	return rows, err
}

// AllAnalysisRows lists through the store, then runs the hook.
func (s *gcListThenAct) AllAnalysisRows(ctx context.Context) ([]manifest.AnalysisRow, error) {
	rows, err := s.Store.AllAnalysisRows(ctx)
	s.once.Do(s.act)
	return rows, err
}

// gcStartAfterTheGrace is a run start an hour from now, which every file a
// test has just written precedes by more than integrity.OrphanGracePeriod:
// what a test of a forward sweep's other rules passes to runGCForwardSweep
// or removeAnalysisGCFiles, so the grace cannot be what keeps a file (the
// integrity tests' "the tick starts an hour ahead"). A test through runGC or
// runAnalyzeGC, which take their start themselves, dates its files instead
// (writeAgedFile, ageFiles).
func gcStartAfterTheGrace() time.Time { return time.Now().Add(time.Hour) }

// ageFiles dates each of paths an hour back, past the forward sweeps'
// grace: what a test of runGC or runAnalyzeGC does to the files it means
// the run to remove, so they read as what a crashed job left long ago.
func ageFiles(t *testing.T, paths ...string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, p := range paths {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// holdUnremovable makes os.Remove of path fail until the test ends: on
// Windows by holding the file open (Windows deletes no file that is open
// without delete sharing, which os.Open does not grant), elsewhere by
// taking the write permission off its directory. Skips as root, whom a
// directory's mode does not stop.
func holdUnremovable(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("root unlinks a file in a directory it may not write")
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// TestAGCForwardSweepCountsAnUnlinkTheFilesystemRefuses — the re-check
// before an unlink must not turn a real failure into a quiet one: a file the
// filesystem will not unlink is counted (the count runGC exits 1 on) and
// named, in both CLI sweeps.
func TestAGCForwardSweepCountsAnUnlinkTheFilesystemRefuses(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "held", "held.flac.upscaled-v2-176400-24.flac")
	writeFixtureFile(t, held, 8)
	holdUnremovable(t, held)
	inv := integrity.SidecarInventory{Files: 1, Orphans: 1, OrphanPaths: []string{held}, OrphanWalkedPaths: []string{held}}

	var stderr bytes.Buffer
	removed, _, failed, code, _ := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &stderr, inv, gcStartAfterTheGrace())
	if removed != 0 || failed != 1 || code != 0 {
		t.Errorf("upscale --gc: removed %d, failed %d, exit %d, want 0, 1, 0", removed, failed, code)
	}
	if !strings.Contains(stderr.String(), "remove "+held) {
		t.Errorf("upscale --gc named no failure: %q", stderr.String())
	}
	stderr.Reset()
	tally, code := removeAnalysisGCFiles(context.Background(), &stderr, inv, gcStartAfterTheGrace())
	if tally.removed != 0 || tally.failed != 1 || code != 0 {
		t.Errorf("analyze --gc: removed %d, failed %d, exit %d, want 0, 1, 0", tally.removed, tally.failed, code)
	}
	if !strings.Contains(stderr.String(), "analyze --gc: remove "+filepath.Base(held)) {
		t.Errorf("analyze --gc named no failure: %q", stderr.String())
	}
	requireFile(t, held, "the file the filesystem would not unlink")
}

// writeAgedFile writes a file at path and dates it age before now: what a
// crashed job left behind, which a `--gc` run is for.
func writeAgedFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	writeFixtureFile(t, path, 64)
	mt := time.Now().Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// requireFile fails the test unless a file is at path.
func requireFile(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

// requireNoFile fails the test while anything is at path.
func requireNoFile(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s is still there (lstat err %v)", what, err)
	}
}

// publishRendition does what a render does once its sox exits: the
// rendition, written under its temp name beside the final one, is renamed
// into place, and its row commits after that (the pool's processJob).
func publishRendition(t *testing.T, store *manifest.Store, source, sidecar string) {
	t.Helper()
	tmp := sidecar + ".9f8e7d6c.tmp"
	writeFixtureFile(t, tmp, 4096)
	if err := os.Rename(tmp, sidecar); err != nil {
		t.Fatal(err)
	}
	commitRenditionRow(t, store, source, sidecar)
}

// commitRenditionRow commits the row of the rendition at sidecar, as the
// pool does once the rendition is in place.
func commitRenditionRow(t *testing.T, store *manifest.Store, source, sidecar string) {
	t.Helper()
	info, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: source, VariantID: gcLiveVariant, SidecarPath: sidecar, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: info.Size(),
		SourceMTimeNS: 1, SourceSize: 100, CreatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

// upsertLiveTracks gives each source a track row, as the library has before
// any job renders or analyses it.
func upsertLiveTracks(t *testing.T, store *manifest.Store, sources ...string) {
	t.Helper()
	for _, source := range sources {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: source, Size: 100, ModTime: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAGCRunBesideALiveRenderKeepsWhatTheRenderWrites — after the run lists
// the catalog, one render publishes its rendition and commits the row, a
// second commits the row of the rendition it renamed into place a minute
// before the run started (the window between its rename and its commit
// straddling the run's start), and a third is still writing its `.tmp`.
// On main the forward sweep took all three files for orphans and unlinked
// them: the published renditions were left with a row and no file, and the
// render still writing failed at its rename and struck a good file. What a
// render that crashed an hour ago left behind, a rendition with no row and
// a `.tmp`, is the positive control: the run still removes it.
func TestAGCRunBesideALiveRenderKeepsWhatTheRenderWrites(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "variants")
	store, _ := relocatedStoreAt(t, filepath.Join(t.TempDir(), "bridge.db"), dir, dir, 20)

	stale := transcode.VariantSidecarPath(dir, "Artist/Crashed/01 - Track.flac", gcLiveVariant)
	staleTmp := transcode.VariantSidecarPath(dir, "Artist/Crashed/02 - Track.flac", gcLiveVariant) + ".0badf00d.tmp"
	writeAgedFile(t, stale, time.Hour)
	writeAgedFile(t, staleTmp, time.Hour)

	const publishedSource, straddlingSource, writingSource = "Artist/Live/01 - Track.flac", "Artist/Live/02 - Track.flac", "Artist/Live/03 - Track.flac"
	upsertLiveTracks(t, store, publishedSource, straddlingSource, writingSource)
	published := transcode.VariantSidecarPath(dir, publishedSource, gcLiveVariant)
	straddling := transcode.VariantSidecarPath(dir, straddlingSource, gcLiveVariant)
	writeAgedFile(t, straddling, time.Minute)
	writing := transcode.VariantSidecarPath(dir, writingSource, gcLiveVariant) + ".1234abcd.tmp"
	live := &gcListThenAct{Store: store, act: func() {
		publishRendition(t, store, publishedSource, published)
		commitRenditionRow(t, store, straddlingSource, straddling)
		writeFixtureFile(t, writing, 4096)
	}}

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, live, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("runGC exit=%d\nstdout=%s\nstderr=%s", rc, stdout.String(), stderr.String())
	}
	for source, sidecar := range map[string]string{publishedSource: published, straddlingSource: straddling} {
		requireFile(t, sidecar, "the rendition a render published as the run went")
		if row, err := store.GetVariant(ctx, source, gcLiveVariant); err != nil || row == nil {
			t.Errorf("the row of %s: %v, %v", source, row, err)
		}
	}
	requireFile(t, writing, "the .tmp a render was still writing")
	requireNoFile(t, stale, "control: the rendition a crashed render left an hour ago")
	requireNoFile(t, staleTmp, "control: the .tmp a crashed render left an hour ago")
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

// publishWaveform does what an analysis does once its decode is done: the
// curve, written to its `.tmp`, is renamed into place, and its row commits
// after that (the analysis pool's processJob).
func publishWaveform(t *testing.T, store *manifest.Store, source, waveform string) {
	t.Helper()
	tmp := waveform + analyze.AnalysisTmpSuffix
	writeFixtureFile(t, tmp, 20)
	if err := os.Rename(tmp, waveform); err != nil {
		t.Fatal(err)
	}
	commitWaveformRow(t, store, source, waveform)
}

// commitWaveformRow commits the row of the waveform at path, as the
// analysis pool does once the curve is in place.
func commitWaveformRow(t *testing.T, store *manifest.Store, source, waveform string) {
	t.Helper()
	if err := store.UpsertAnalysis(context.Background(), manifest.AnalysisRow{
		SourcePath: source, WaveformPath: waveform, SourceMTimeNS: 1, SourceSize: 10, SchemaVersion: "wf4",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAnAnalyzeGCRunBesideALiveAnalysisKeepsWhatTheAnalysisWrites — the
// waveform twin: an analysis publishes its curve during the run, another
// commits the row of the curve it renamed into place a minute before the
// run started, and a third is still writing its `.tmp`. On main the
// published curves were unlinked behind their rows, and nothing brings
// them back but `bridge analyze --force` (the skip gate reads the row),
// and the `.tmp` went with them. The curve and the `.tmp` a crashed
// analysis left an hour ago are the positive controls.
func TestAnAnalyzeGCRunBesideALiveAnalysisKeepsWhatTheAnalysisWrites(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "waveforms")
	store, _ := waveformTree(t, dir, 20, 0)
	waveform := func(source string) string {
		return analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: source}.SidecarPath()
	}

	stale := waveform("Artist/Crashed/01.flac")
	staleTmp := waveform("Artist/Crashed/02.flac") + analyze.AnalysisTmpSuffix
	writeAgedFile(t, stale, time.Hour)
	writeAgedFile(t, staleTmp, time.Hour)

	const publishedSource, straddlingSource, writingSource = "Artist/Live/01.flac", "Artist/Live/02.flac", "Artist/Live/03.flac"
	upsertLiveTracks(t, store, publishedSource, straddlingSource, writingSource)
	published := waveform(publishedSource)
	straddling := waveform(straddlingSource)
	writeAgedFile(t, straddling, time.Minute)
	writing := waveform(writingSource) + analyze.AnalysisTmpSuffix
	live := &gcListThenAct{Store: store, act: func() {
		publishWaveform(t, store, publishedSource, published)
		commitWaveformRow(t, store, straddlingSource, straddling)
		writeFixtureFile(t, writing, 20)
	}}

	var stdout, stderr bytes.Buffer
	if rc := runAnalyzeGC(ctx, &stdout, &stderr, live, dir, analyzeGCOptions{}); rc != 0 {
		t.Fatalf("runAnalyzeGC exit=%d\nstdout=%s\nstderr=%s", rc, stdout.String(), stderr.String())
	}
	requireFile(t, published, "the waveform an analysis published during the run")
	requireFile(t, straddling, "the waveform an analysis renamed into place just before the run")
	requireFile(t, writing, "the .tmp an analysis was still writing")
	requireNoFile(t, stale, "control: the waveform a crashed analysis left an hour ago")
	requireNoFile(t, staleTmp, "control: the .tmp a crashed analysis left an hour ago")
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

// TestAGCRunDuringAMoveKeepsTheRowsTheMoveRelocated — B204's shape, for the
// CLI's reverse sweep (backlog B250): forty rows, six of them moved to X by
// `bridge variants move` after the run listed them, under the mass-delete
// floor. The run judges them as listed, gone at both of the places it looks,
// and on main deleted their rows with their files intact at X. Two sidecars
// removed by hand are the positive control: the run still deletes their
// rows.
func TestAGCRunDuringAMoveKeepsTheRowsTheMoveRelocated(t *testing.T) {
	ctx := context.Background()
	m := newMoveShape(t)
	live := &gcListThenAct{Store: m.store, act: func() {
		if err := moveRows(ctx, m.mover, m.moved, m.to); err != nil {
			t.Fatal(err)
		}
	}}

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, live, m.dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("runGC exit=%d\nstdout=%s\nstderr=%s", rc, stdout.String(), stderr.String())
	}
	m.requireOnlyTheGoneRowsDeleted(t)
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

// gcDeleteThenAct is the store a `--gc` run deletes through, with a hook
// that runs once, just before the run's first row delete: the moment at
// which a render can rewrite a row the run has already judged missing.
type gcDeleteThenAct struct {
	*manifest.Store
	once sync.Once
	act  func()
}

// DeleteVariant runs the hook, then deletes through the store.
func (s *gcDeleteThenAct) DeleteVariant(ctx context.Context, sourcePath, variantID string) error {
	s.once.Do(s.act)
	return s.Store.DeleteVariant(ctx, sourcePath, variantID)
}

// DeleteVariantIfUnchanged runs the hook, then deletes through the store.
func (s *gcDeleteThenAct) DeleteVariantIfUnchanged(ctx context.Context, v manifest.VariantRow) error {
	s.once.Do(s.act)
	return s.Store.DeleteVariantIfUnchanged(ctx, v)
}

// TestAGCRunKeepsARowARenderRewroteAfterTheRunJudgedItMissing — the other
// writer B250 names. Two rows' sidecars are gone when the run classifies
// them; before the run deletes them, a render renders one of them again, at
// the path the row records and at the size it records, and commits its row.
// On main the run deleted the new row and left the new rendition with no
// row. The other row is the positive control: still gone at both places,
// still deleted.
func TestAGCRunKeepsARowARenderRewroteAfterTheRunJudgedItMissing(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "variants")
	store, _ := relocatedStoreAt(t, filepath.Join(t.TempDir(), "bridge.db"), dir, dir, 20)
	listed, err := store.AllVariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rendered, gone := listed[4], listed[12]
	for _, v := range []manifest.VariantRow{rendered, gone} {
		if err := os.Remove(v.SidecarPath); err != nil {
			t.Fatal(err)
		}
	}
	live := &gcDeleteThenAct{Store: store, act: func() {
		writeFixtureFile(t, rendered.SidecarPath, int(rendered.SizeBytes))
		again := rendered
		again.CreatedAt = time.Now().UnixNano()
		if err := store.UpsertVariant(ctx, again); err != nil {
			t.Fatal(err)
		}
	}}

	var stdout, stderr bytes.Buffer
	if rc := runGC(ctx, &stdout, &stderr, live, dir, t.TempDir(), gcOptions{maxDeletePercent: 20}); rc != 0 {
		t.Fatalf("runGC exit=%d\nstdout=%s\nstderr=%s", rc, stdout.String(), stderr.String())
	}
	if row, err := store.GetVariant(ctx, rendered.SourcePath, rendered.VariantID); err != nil || row == nil {
		t.Errorf("the row the render committed after the run judged it missing: %v, %v", row, err)
	}
	requireFile(t, rendered.SidecarPath, "the rendition the render published")
	requireRowsGone(t, store, []manifest.VariantRow{gone})
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}
