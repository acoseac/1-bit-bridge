package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestRunGCForwardSweepSkipsDotDirectories: the forward sweep reaps every
// file under the variants dir that no row claims — and a dot-directory is
// never ours to reap (a `.Trash`, an rclone cache, render scratch an
// operator pointed beneath the variants dir). The planted file must
// survive while a genuine orphan beside it goes.
func TestRunGCForwardSweepSkipsDotDirectories(t *testing.T) {
	outputDir := t.TempDir()
	planted := filepath.Join(outputDir, ".scratch", "1-bit-bridge-render", "job.stageA.sox")
	orphan := filepath.Join(outputDir, "Artist", "Album", "01.dsf.optimized-dsd-v1-44100-16.flac")
	for _, p := range []string{planted, orphan} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, kept, failed, exitCode := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, outputDir, map[string]bool{})
	if exitCode != 0 || failed != 0 {
		t.Fatalf("sweep unexpected: exit=%d failed=%d", exitCode, failed)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want exactly the orphan sidecar", removed)
	}
	if kept != 0 {
		t.Errorf("kept = %d, want 0 (the dot-directory's file is not counted — it was never considered)", kept)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("the file under the dot-directory must survive: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the orphan sidecar must be reaped (stat err=%v)", err)
	}
}

// TestRunGCPurgesStaleRenderScratch: `--gc` reclaims crash-orphaned
// Stage A scratch (the deferred remove cannot run after a SIGKILL),
// bounded to the bridge-owned subdirectory and to files past the purge
// age — a fresh scratch another instance may be writing survives.
func TestRunGCPurgesStaleRenderScratch(t *testing.T) {
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	outputDir := filepath.Join(dir, "variants")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(dir, "scratch")
	scratch := transcode.RenderScratchDir(tempDir)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	touch := func(path string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(scratch, "aaaa0001.stageA.sox")
	fresh := filepath.Join(scratch, "aaaa0002.stageA.sox")
	touch(stale, 13*time.Hour)
	touch(fresh, time.Hour)

	var stdout, stderr bytes.Buffer
	if rc := runGC(context.Background(), &stdout, &stderr, store, outputDir, tempDir); rc != 0 {
		t.Fatalf("runGC exit=%d\nstdout=%s\nstderr=%s", rc, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the 13 h scratch must be gone (stat err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh scratch must survive: %v", err)
	}
	if !strings.Contains(stdout.String(), "GC render scratch: removed 1") {
		t.Errorf("stdout must report the purge, got:\n%s", stdout.String())
	}
}
