package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
)

// writeWalkedPathFixture writes one byte at path, creating its directory.
func writeWalkedPathFixture(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireRemoved fails unless path is gone, and requireKept unless it is
// still there.
func requireRemoved(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s survived: %s (%v)", what, path, err)
	}
}

func requireKept(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was unlinked: %s (%v)", what, path, err)
	}
}

// TestUpscaleGCForwardSweepUnlinksTheWalkedPath pins that `upscale --gc`
// unlinks each orphan by the path its walk visited (OrphanWalkedPaths),
// never by the configured spelling it prints. The inventory here lists a
// file in one tree and names the walked file in another: the two
// spellings a symlinked variants directory leaves when it is repointed
// between the walk and the unlinks, where the configured one reaches a
// tree the mass-orphan guard never counted (CodeRabbit on #1063). Only
// the walked file may go.
func TestUpscaleGCForwardSweepUnlinksTheWalkedPath(t *testing.T) {
	base := t.TempDir()
	walked := writeWalkedPathFixture(t, filepath.Join(base, "walked", "Artist", "orphan.flac"))
	listed := writeWalkedPathFixture(t, filepath.Join(base, "listed", "Artist", "orphan.flac"))
	inv := integrity.SidecarInventory{
		Files: 1, Orphans: 1,
		OrphanPaths:       []string{listed},
		OrphanWalkedPaths: []string{walked},
	}

	var stdout, stderr bytes.Buffer
	removed, _, failed, code, _ := runGCForwardSweep(context.Background(), &stdout, &stderr, inv, gcStartAfterTheGrace())
	if removed != 1 || failed != 0 || code != 0 {
		t.Fatalf("removed %d, failed %d, exit %d, want 1, 0, 0\nstderr: %s", removed, failed, code, stderr.String())
	}
	requireRemoved(t, walked, "the orphan the walk visited")
	requireKept(t, listed, "the file of that name under the configured spelling")
}

// TestAnalyzeGCUnlinksTheWalkedPaths pins the same for `analyze --gc`,
// scratch files included: removeAnalysisGCFiles unlinks
// ScratchWalkedPaths and OrphanWalkedPaths, and nothing the listed
// spellings name.
func TestAnalyzeGCUnlinksTheWalkedPaths(t *testing.T) {
	base := t.TempDir()
	orphan, scratch := "orphan"+analyze.WaveformExt, "half"+analyze.WaveformExt+analyze.AnalysisTmpSuffix
	walkedOrphan := writeWalkedPathFixture(t, filepath.Join(base, "walked", "Artist", orphan))
	walkedScratch := writeWalkedPathFixture(t, filepath.Join(base, "walked", "Artist", scratch))
	listedOrphan := writeWalkedPathFixture(t, filepath.Join(base, "listed", "Artist", orphan))
	listedScratch := writeWalkedPathFixture(t, filepath.Join(base, "listed", "Artist", scratch))
	inv := integrity.SidecarInventory{
		Files: 1, Orphans: 1,
		OrphanPaths:        []string{listedOrphan},
		OrphanWalkedPaths:  []string{walkedOrphan},
		ScratchPaths:       []string{listedScratch},
		ScratchWalkedPaths: []string{walkedScratch},
	}

	var stderr bytes.Buffer
	tally, code := removeAnalysisGCFiles(context.Background(), &stderr, inv, gcStartAfterTheGrace())
	if tally.removed != 2 || tally.failed != 0 || code != 0 {
		t.Fatalf("removed %d, failed %d, exit %d, want 2, 0, 0\nstderr: %s", tally.removed, tally.failed, code, stderr.String())
	}
	requireRemoved(t, walkedOrphan, "the orphan the walk visited")
	requireRemoved(t, walkedScratch, "the scratch file the walk visited")
	requireKept(t, listedOrphan, "the orphan's name under the configured spelling")
	requireKept(t, listedScratch, "the scratch file's name under the configured spelling")
}

// TestUpscaleGCForwardSweepRefusesAnUnpairedInventory — `upscale --gc`
// refuses an inventory whose listed and walked paths do not pair up, before
// removing anything, and exits 1 (Gemini on #1063). Indexing past the
// walked list would panic, and removing by the listed spelling is what the
// walked paths exist to prevent. gcTakeInventory cannot return such an
// inventory; the check is for a later change that builds or trims one.
func TestUpscaleGCForwardSweepRefusesAnUnpairedInventory(t *testing.T) {
	base := t.TempDir()
	a := writeWalkedPathFixture(t, filepath.Join(base, "Artist", "a.flac"))
	b := writeWalkedPathFixture(t, filepath.Join(base, "Artist", "b.flac"))
	inv := integrity.SidecarInventory{
		Files: 2, Orphans: 2,
		OrphanPaths:       []string{a, b},
		OrphanWalkedPaths: []string{a},
	}

	var stdout, stderr bytes.Buffer
	removed, _, failed, code, _ := runGCForwardSweep(context.Background(), &stdout, &stderr, inv, gcStartAfterTheGrace())
	if removed != 0 || failed != 0 || code != 1 {
		t.Fatalf("removed %d, failed %d, exit %d, want 0, 0, 1\nstderr: %s", removed, failed, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to run") || !strings.Contains(stderr.String(), "Nothing was removed") {
		t.Errorf("the refusal should say so, and that nothing was removed: %s", stderr.String())
	}
	requireKept(t, a, "an orphan of a refused inventory")
	requireKept(t, b, "an orphan of a refused inventory")
}

// TestAnalyzeGCRefusesAnUnpairedInventory pins the same for `analyze
// --gc`. It reads only the walked lists, so without the check an unpaired
// inventory would not panic there: it would remove what the walked lists
// hold and report the rest as never there. Refused whole, exit 1.
func TestAnalyzeGCRefusesAnUnpairedInventory(t *testing.T) {
	base := t.TempDir()
	name := "orphan" + analyze.WaveformExt
	a := writeWalkedPathFixture(t, filepath.Join(base, "A", name))
	b := writeWalkedPathFixture(t, filepath.Join(base, "B", name))
	inv := integrity.SidecarInventory{
		Files: 2, Orphans: 2,
		OrphanPaths:       []string{a, b},
		OrphanWalkedPaths: []string{a},
	}

	var stderr bytes.Buffer
	tally, code := removeAnalysisGCFiles(context.Background(), &stderr, inv, gcStartAfterTheGrace())
	if tally.removed != 0 || tally.failed != 0 || code != 1 {
		t.Fatalf("removed %d, failed %d, exit %d, want 0, 0, 1\nstderr: %s", tally.removed, tally.failed, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to run") {
		t.Errorf("the refusal should say so: %s", stderr.String())
	}
	requireKept(t, a, "an orphan of a refused inventory")
	requireKept(t, b, "an orphan of a refused inventory")
}
