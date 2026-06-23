package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestCollectAnalysisCandidatesSkipsZeroByteSource pins that a zero-byte
// source file (a failed/incomplete upload — sox can't probe it and the ffmpeg
// fallback hits EOF) is skipped at collection time instead of being enqueued
// and re-failed on every sweep. A non-empty sibling still becomes a candidate,
// and the skip self-heals on re-upload (size > 0). Field-reported on
// bridge.ars.md: 26 zero-byte FLACs from truncated B2 syncs spammed
// "analyze: failed" each sweep.
func TestCollectAnalysisCandidatesSkipsZeroByteSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.flac"), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.flac"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, tr := range []struct {
		path string
		size int64
	}{{"good.flac", 18}, {"empty.flac", 0}} {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: tr.path, Size: tr.size, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.path, err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", res.emptySkipped)
	}
	var gotGood, gotEmpty bool
	for _, c := range res.candidates {
		switch c.SourceLibraryRel {
		case "good.flac":
			gotGood = true
		case "empty.flac":
			gotEmpty = true
		}
	}
	if !gotGood {
		t.Error("good.flac (non-empty) should be a candidate")
	}
	if gotEmpty {
		t.Error("empty.flac (zero-byte) must NOT be enqueued for analysis")
	}
}
