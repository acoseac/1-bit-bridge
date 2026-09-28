package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
	removed, _, failed, code := runGCForwardSweep(context.Background(), &stdout, &stderr, inv)
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
	walkedOrphan := writeWalkedPathFixture(t, filepath.Join(base, "walked", "Artist", "orphan.1bwf"))
	walkedScratch := writeWalkedPathFixture(t, filepath.Join(base, "walked", "Artist", "half.1bwf.tmp"))
	listedOrphan := writeWalkedPathFixture(t, filepath.Join(base, "listed", "Artist", "orphan.1bwf"))
	listedScratch := writeWalkedPathFixture(t, filepath.Join(base, "listed", "Artist", "half.1bwf.tmp"))
	inv := integrity.SidecarInventory{
		Files: 1, Orphans: 1,
		OrphanPaths:        []string{listedOrphan},
		OrphanWalkedPaths:  []string{walkedOrphan},
		ScratchPaths:       []string{listedScratch},
		ScratchWalkedPaths: []string{walkedScratch},
	}

	var stderr bytes.Buffer
	removed, failed, interrupted := removeAnalysisGCFiles(context.Background(), &stderr, inv)
	if removed != 2 || failed != 0 || interrupted {
		t.Fatalf("removed %d, failed %d, interrupted %v, want 2, 0, false\nstderr: %s", removed, failed, interrupted, stderr.String())
	}
	requireRemoved(t, walkedOrphan, "the orphan the walk visited")
	requireRemoved(t, walkedScratch, "the scratch file the walk visited")
	requireKept(t, listedOrphan, "the orphan's name under the configured spelling")
	requireKept(t, listedScratch, "the scratch file's name under the configured spelling")
}
