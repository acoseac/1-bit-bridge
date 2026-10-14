package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The variants sweeps (upscale, optimize and render, one runGC) reclaim a
// rendition no row names, in either layout, and leave every other file.
// A .DS_Store, a .keep, a README or a dot-file is never a candidate, and
// neither is a plain .flac: that is the background sweep's predicate, and
// these sweeps share the probe's looksLikeVariantSidecar instead. Foreign
// files are dated past the grace, so a sweep that still takes every file
// removes them rather than leaving them as recent.

const gcRenditionVariant = "upscaled-v2-176400-24"

// gcForeignNames are files a rendition sweep must leave. notes.flac is
// here so a Consider of every .flac, the background sweep's, fails too.
var gcForeignNames = []string{".DS_Store", ".keep", "README.txt", ".hidden", "notes.flac"}

// plantAgedForeignFiles writes each foreign name at the variants root and
// dates it an hour back, past the forward sweep's grace.
func plantAgedForeignFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths := make([]string, len(gcForeignNames))
	for i, name := range gcForeignNames {
		paths[i] = filepath.Join(dir, name)
		writeAgedFile(t, paths[i], time.Hour)
	}
	return paths
}

// gcRenditionFixture records one live rendition and writes two stranded
// ones, a source-mirrored file and a legacy hash-flat file, plus the
// foreign files. The stranded pair is aged past the grace.
func gcRenditionFixture(t *testing.T, store *manifest.Store, dir string) (known string, stranded, foreign []string) {
	t.Helper()
	ctx := context.Background()
	source := "Artist/Kept/01.flac"
	known = transcode.VariantSidecarPath(dir, source, gcRenditionVariant)
	writeFixtureFile(t, known, 50)
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: source, VariantID: gcRenditionVariant, SidecarPath: known, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}

	mirrored := transcode.VariantSidecarPath(dir, "Artist/Gone/01.flac", gcRenditionVariant)
	flat := filepath.Join(dir, fmt.Sprintf("%040x-upscaled-v1-88200-24.flac", 1))
	writeAgedFile(t, mirrored, time.Hour)
	writeAgedFile(t, flat, time.Hour)
	stranded = []string{mirrored, flat}
	foreign = plantAgedForeignFiles(t, dir)
	return known, stranded, foreign
}

func openGCStore(t *testing.T, dbPath string) *manifest.Store {
	t.Helper()
	store, err := manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestUpscaleGCLeavesForeignFilesAndReclaimsBothRenditionLayouts drives
// the real runGC. Both stranded layouts go; every foreign file stays.
func TestUpscaleGCLeavesForeignFilesAndReclaimsBothRenditionLayouts(t *testing.T) {
	dir := t.TempDir()
	store := openGCStore(t, filepath.Join(t.TempDir(), "bridge.db"))
	known, stranded, foreign := gcRenditionFixture(t, store, dir)

	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc != 0 {
		t.Fatalf("runGC rc=%d\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("removed 2 orphan file(s), kept 1 known sidecar(s)")) {
		t.Fatalf("summary:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	requireFile(t, known, "the rendition a row names")
	for _, p := range stranded {
		requireNoFile(t, p, "stranded rendition "+filepath.Base(p))
	}
	for _, p := range foreign {
		requireFile(t, p, "foreign file "+filepath.Base(p))
	}
}

// TestUpscaleGCOverOnlyADSStoreNeedsNoAllowEmpty — an empty catalog over
// a variants directory that holds only a foreign file proceeds. The file
// is aged, so a sweep that still took it would remove it rather than
// leave it for the grace.
func TestUpscaleGCOverOnlyADSStoreNeedsNoAllowEmpty(t *testing.T) {
	dir := t.TempDir()
	foreign := plantAgedForeignFiles(t, dir)

	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, emptyCatalogStore(t), dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc != 0 || bytes.Contains(stderr.Bytes(), []byte("this sweep would remove")) {
		t.Fatalf("an empty catalog over foreign files: rc=%d\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
	for _, p := range foreign {
		requireFile(t, p, "foreign file "+filepath.Base(p))
	}
}

// TestUpscaleGCMassOrphanRatioCountsOnlyRenditions — foreign files are
// outside the ratio. Two live rows and forty stranded renditions still
// read as 40 of 42, and an allowed sweep reclaims the renditions and
// leaves the foreign files.
func TestUpscaleGCMassOrphanRatioCountsOnlyRenditions(t *testing.T) {
	dir := t.TempDir()
	store, stranded := strandedTree(t, dir, 2, 40)
	foreign := plantAgedForeignFiles(t, dir)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	rc := runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc == 0 {
		t.Fatalf("the rendition ratio was not refused\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("40 of 42 file(s)")) {
		t.Fatalf("the ratio counted more than renditions:\n%s", stderr.String())
	}
	for _, p := range append(append([]string{}, stranded...), foreign...) {
		requireFile(t, p, "a refused sweep unlinked "+filepath.Base(p))
	}

	stdout.Reset()
	stderr.Reset()
	rc = runGC(ctx, &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20, allowMassOrphans: true})
	if rc != 0 {
		t.Fatalf("allow-mass-orphans rc=%d\nstderr: %s", rc, stderr.String())
	}
	for _, p := range stranded {
		requireNoFile(t, p, "stranded rendition "+filepath.Base(p))
	}
	for _, p := range foreign {
		requireFile(t, p, "foreign file "+filepath.Base(p))
	}
}

// TestUpscaleGCReclaimsNothingWhenEveryRenditionHasARow is the catalog
// with live rows and no strays: the sweep removes nothing.
func TestUpscaleGCReclaimsNothingWhenEveryRenditionHasARow(t *testing.T) {
	dir := t.TempDir()
	store, _ := strandedTree(t, dir, 2, 0)
	known := []string{
		transcode.VariantSidecarPath(dir, "Artist/Re-rendered/00.flac", "upscaled-v2-176400-24"),
		transcode.VariantSidecarPath(dir, "Artist/Re-rendered/01.flac", "upscaled-v2-176400-24"),
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	if rc != 0 {
		t.Fatalf("runGC rc=%d\nstderr: %s", rc, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("removed 0 orphan file(s), kept 2 known sidecar(s)")) {
		t.Fatalf("summary:\n%s", stdout.String())
	}
	for _, p := range known {
		requireFile(t, p, "live rendition "+filepath.Base(p))
	}
}

// gcRenditionCLIConfig writes a bridge.yaml whose upscale is on and whose
// variants directory is variantsDir, and returns the config path and the
// data dir the CLI opens its store from.
func gcRenditionCLIConfig(t *testing.T, variantsDir string) (cfgPath, dataDir string) {
	t.Helper()
	root := t.TempDir()
	dataDir = filepath.Join(root, "data")
	libDir := filepath.Join(root, "lib")
	for _, d := range []string{dataDir, libDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(root, "bridge.yaml")
	body := fmt.Sprintf("dataDir: %q\nlibraryRoots:\n    - %q\nupscale:\n    enabled: true\n    variantsDir: %q\n", dataDir, libDir, variantsDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// TestUpscaleCmdGCLeavesForeignFilesAndReclaimsRenditions is the real
// `bridge upscale --gc` path, over the same fixture runGC sees.
func TestUpscaleCmdGCLeavesForeignFilesAndReclaimsRenditions(t *testing.T) {
	variants := t.TempDir()
	cfgPath, dataDir := gcRenditionCLIConfig(t, variants)
	store := openGCStore(t, manifest.DefaultDBPath(dataDir))
	known, stranded, foreign := gcRenditionFixture(t, store, variants)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := upscaleCmd(context.Background(), []string{"--config", cfgPath, "--gc"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("upscale --gc rc=%d\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}
	requireFile(t, known, "the rendition a row names")
	for _, p := range stranded {
		requireNoFile(t, p, "stranded rendition "+filepath.Base(p))
	}
	for _, p := range foreign {
		requireFile(t, p, "foreign file "+filepath.Base(p))
	}
}

// TestOptimizeAndRenderGCLeaveForeignFiles — optimize and render share
// runGC, so a variants directory of foreign files needs no --allow-empty
// from either command.
func TestOptimizeAndRenderGCLeaveForeignFiles(t *testing.T) {
	for _, cmd := range []struct {
		name string
		run  func(context.Context, []string, io.Writer, io.Writer) int
	}{
		{"optimize", optimizeCmd},
		{"render", renderCmd},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			variants := t.TempDir()
			cfgPath, _ := gcRenditionCLIConfig(t, variants)
			foreign := plantAgedForeignFiles(t, variants)
			var stdout, stderr bytes.Buffer
			rc := cmd.run(context.Background(), []string{"--config", cfgPath, "--gc"}, &stdout, &stderr)
			if rc != 0 || bytes.Contains(stderr.Bytes(), []byte("this sweep would remove")) {
				t.Fatalf("%s --gc rc=%d\nstdout: %s\nstderr: %s", cmd.name, rc, stdout.String(), stderr.String())
			}
			for _, p := range foreign {
				requireFile(t, p, "foreign file "+filepath.Base(p))
			}
		})
	}
}

// TestDoctorVariantsIndexWeighsRenditionsOnly — the variants-index line
// reports the number the sweep would act on, so a foreign file is neither
// a file nor an orphan in that count.
func TestDoctorVariantsIndexWeighsRenditionsOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	store := openGCStore(t, dbPath)
	_, stranded, _ := gcRenditionFixture(t, store, dir)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	idx, err := variantsIndexCounts(context.Background(), dbPath, dir, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Files != 3 || idx.Known != 1 || idx.Orphans != len(stranded) {
		t.Fatalf("variants-index Files/Known/Orphans = %d/%d/%d, want 3/1/%d (foreign files weighed)",
			idx.Files, idx.Known, idx.Orphans, len(stranded))
	}
}
