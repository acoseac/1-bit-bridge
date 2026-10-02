package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The CLI half of what the sidecar walks compare and look inside
// (2026-10-02, backlog B206 and B207): `upscale --gc` (optimize and render
// reach it too), `analyze --gc` and `bridge doctor`'s variants-index read
// the shared inventory, and analyze builds its known set of its own.

// plantInSkippedDirs writes, under dir, a file named name inside each kind
// of directory a library walk skips, at the top (a NAS share's recycle bin
// and snapshot) and beside an album (Synology's @eaDir), all older than the
// forward sweep's grace. It returns their paths.
func plantInSkippedDirs(t *testing.T, dir, name string) []string {
	t.Helper()
	planted := []string{
		filepath.Join(dir, "#recycle", "Artist", "Album", name),
		filepath.Join(dir, "#snapshot", "GMT+01_2026-09-01-0300", "Artist", "Album", name),
		filepath.Join(dir, "@Recycle", "Artist", name),
		filepath.Join(dir, "$RECYCLE.BIN", "S-1-5-21-1004", name),
		filepath.Join(dir, "Artist", "Album 0", "@eaDir", name, "SYNOINDEX_MEDIA_INFO"),
	}
	for _, p := range planted {
		writeFixtureFile(t, p, 10)
	}
	ageFiles(t, planted...)
	return planted
}

// requireAllExist fails the test for each of paths that is gone.
func requireAllExist(t *testing.T, what string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %s is gone (%v)", what, p, err)
		}
	}
}

// TestEveryGCSweepPassesOverADirectoryALibraryWalkSkips — a variants or
// waveform directory on a NAS share holds the share's recycle bin and
// snapshots, and Synology's @eaDir beside every folder it indexes. Both
// `--gc` sweeps counted their files as orphans and unlinked them (measured
// on main: the recycle bin's renditions, the snapshot's copies and the
// @eaDir metadata, all gone, exit 0). They are passed over now, and the
// sweep's own orphans still go.
func TestEveryGCSweepPassesOverADirectoryALibraryWalkSkips(t *testing.T) {
	t.Run("upscale --gc", func(t *testing.T) {
		dir := t.TempDir()
		store, stranded := strandedTree(t, dir, 4, 2)
		planted := plantInSkippedDirs(t, dir, "00.flac.upscaled-v2-176400-24.flac")
		runGCExpectingSuccess(t, store, dir, "--gc beside a recycle bin")
		requireAllExist(t, "--gc unlinked a file in a directory a library walk skips", planted...)
		requireAllGone(t, "--gc left a stranded rendition", stranded...)
	})
	t.Run("analyze --gc", func(t *testing.T) {
		dir := t.TempDir()
		store, stranded := waveformTree(t, dir, 4, 2)
		planted := plantInSkippedDirs(t, dir, "00.flac"+analyze.WaveformExt)
		var stdout, stderr bytes.Buffer
		if rc := runAnalyzeGC(context.Background(), &stdout, &stderr, store, dir, analyzeGCOptions{}); rc != 0 {
			t.Fatalf("analyze --gc beside a recycle bin: rc=%d\n%s%s", rc, stdout.String(), stderr.String())
		}
		requireAllExist(t, "analyze --gc unlinked a file in a directory a library walk skips", planted...)
		requireAllGone(t, "analyze --gc left a stranded waveform", stranded...)
	})
}

// requireAllGone fails the test for each of paths that is still there.
func requireAllGone(t *testing.T, what string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %s (%v)", what, p, err)
		}
	}
}

// TestDoctorVariantsIndexPassesOverASnapshot — a NAS share with visible
// snapshots holds a copy of the whole variants tree per snapshot under
// #snapshot, which the doctor counted as orphans and reported as a lost
// index ("REFUSES"), on a healthy bridge. It reads the tree's own files.
func TestDoctorVariantsIndexPassesOverASnapshot(t *testing.T) {
	dir := t.TempDir()
	cfgPath, variantsDir := variantsIndexInstall(t, dir, 6, 0)
	for i := 0; i < 3; i++ {
		snapshot := filepath.Join(variantsDir, "#snapshot", "GMT+01_2026-09-0"+string(rune('1'+i))+"-0300")
		seedVariantCopies(t, variantsDir, snapshot)
	}
	rep := doctor.Run(context.Background(), buildDoctorDeps(cfgPath))
	c := findCheck(t, rep, "variants-index")
	if c.Status != doctor.OK || !strings.Contains(c.Summary, "6 variant row(s), 6 sidecar file(s), all referenced") {
		t.Errorf("a healthy bridge with visible snapshots: %v %q / %q", c.Status, c.Summary, c.Hint)
	}
}

// seedVariantCopies copies every file under variantsDir, outside to, into
// to at the same relative path: what a NAS snapshot of the tree holds.
func seedVariantCopies(t *testing.T, variantsDir, to string) {
	t.Helper()
	var rels []string
	if err := filepath.WalkDir(variantsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), "#") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(variantsDir, p)
			if err != nil {
				return err
			}
			rels = append(rels, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range rels {
		writeFixtureFile(t, filepath.Join(to, rel), 50)
	}
}

// TestEveryGCSweepKeepsAFileItsRowSpellsInAnotherNormalization — on an
// HFS+ volume a file comes back from a walk decomposed (NFD) whatever
// spelling created it, while its row records the spelling it was written
// by, composed (NFC) on most filesystems. Both `--gc` sweeps keyed the
// known set on the case-folded path alone, so the file was an orphan and
// was unlinked (measured on main), while the row stayed (a stat of the
// composed path reaches the file there): rendered again, unlinked again.
// Planted decomposed on any filesystem, and written by the row's own path,
// as the pool writes it, which only an HFS+ volume decomposes (TMPDIR on an
// hdiutil HFS+ image reproduces it there). On a filesystem that does not
// normalize, the planted file's row names no file, which the reverse sweep
// may reap, so the FILE is the assertion.
func TestEveryGCSweepKeepsAFileItsRowSpellsInAnotherNormalization(t *testing.T) {
	const source = "Beyoncé/Café Tacvba/01 Révolución.flac"
	ctx := context.Background()
	for _, placement := range []struct {
		name   string
		onDisk func(recorded, decomposed string) string
		// rowOpensIt: the row's own path names the file on every
		// filesystem, so the reverse sweep keeps the row too.
		rowOpensIt bool
	}{
		{"planted decomposed", func(_, decomposed string) string { return decomposed }, false},
		{"written by its row's path", func(recorded, _ string) string { return recorded }, true},
	} {
		t.Run("upscale --gc, "+placement.name, func(t *testing.T) {
			dir := t.TempDir()
			store := emptyCatalogStore(t)
			const variant = "upscaled-v2-176400-24"
			recorded := transcode.VariantSidecarPath(dir, source, variant)
			if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertVariant(ctx, manifest.VariantRow{
				SourcePath: source, VariantID: variant, SidecarPath: recorded, Format: "flac",
				SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
			}); err != nil {
				t.Fatal(err)
			}
			onDisk := placement.onDisk(recorded, transcode.VariantSidecarPath(dir, norm.NFD.String(source), variant))
			writeFixtureFile(t, onDisk, 50)
			ageFiles(t, onDisk)
			runGCExpectingSuccess(t, store, dir, "--gc over a rendition its row spells in another normalization")
			if got := regularFilesUnderDir(t, dir); got != 1 {
				t.Errorf("%d file(s) left, want the rendition its row names", got)
			}
			if rows, err := store.AllVariants(ctx); placement.rowOpensIt && (err != nil || len(rows) != 1) {
				t.Errorf("%d row(s) left (%v), want the rendition's row", len(rows), err)
			}
		})
		t.Run("analyze --gc, "+placement.name, func(t *testing.T) {
			dir := t.TempDir()
			store := emptyCatalogStore(t)
			recorded := analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: source}.SidecarPath()
			if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 10, ModTime: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertAnalysis(ctx, manifest.AnalysisRow{
				SourcePath: source, WaveformPath: recorded, SourceMTimeNS: 1, SourceSize: 10, SchemaVersion: "wf4",
			}); err != nil {
				t.Fatal(err)
			}
			onDisk := placement.onDisk(recorded, analyze.AnalyzeSpec{OutputDir: dir, SourceLibraryRel: norm.NFD.String(source)}.SidecarPath())
			writeFixtureFile(t, onDisk, 20)
			ageFiles(t, onDisk)
			var stdout, stderr bytes.Buffer
			if rc := runAnalyzeGC(ctx, &stdout, &stderr, store, dir, analyzeGCOptions{}); rc != 0 {
				t.Fatalf("analyze --gc over a waveform its row spells in another normalization: rc=%d\n%s%s", rc, stdout.String(), stderr.String())
			}
			if got := regularFilesUnderDir(t, dir); got != 1 {
				t.Errorf("%d file(s) left, want the waveform its row names", got)
			}
		})
	}
}
