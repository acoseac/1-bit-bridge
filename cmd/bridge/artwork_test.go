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
)

// TestArtworkGCRemovesOrphans is the load-bearing case: a track row
// references some artwork ids; cached files NOT in the referenced set
// are removed; cached files IN the set survive. Per Gemini A10 / iOS
// bug review #10.
func TestArtworkGCRemovesOrphans(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Two tracks reference distinct artwork ids: one local-* shape
	// (scanner-side), one MBID shape (enricher-side).
	if err := store.UpsertTrack(&manifest.Track{
		Path: "a/t1.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B",
		ArtworkMBID: "local-deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(&manifest.Track{
		Path: "a/t2.flac", Size: 2, ModTime: time.Now(),
		Artist: "C", Album: "D",
		ArtworkMBID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}

	// Stage a cache directory with a mix of referenced + orphan files.
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep1 := filepath.Join(artworkDir, "local-deadbeef-500.jpg")
	keep2 := filepath.Join(artworkDir, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa-500.jpg")
	orphan1 := filepath.Join(artworkDir, "local-staleHashFromOldRetag-500.jpg")
	orphan2 := filepath.Join(artworkDir, "11111111-1111-4111-8111-111111111111-500.jpg")
	stray := filepath.Join(artworkDir, "README.txt") // non-cache file, untouched
	for _, p := range []string{keep1, keep2, orphan1, orphan2, stray} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	rc := runArtworkGC(&stdout, &stderr, store, artworkDir, false /* dryRun */)
	if rc != 0 {
		t.Fatalf("runArtworkGC rc=%d, stderr=%s", rc, stderr.String())
	}

	// Referenced files must survive.
	for _, p := range []string{keep1, keep2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected referenced file %s to survive: %v", p, err)
		}
	}
	// Orphans must be removed.
	for _, p := range []string{orphan1, orphan2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected orphan %s to be removed: %v", p, err)
		}
	}
	// Stray file must be untouched (not part of cache namespace).
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("expected stray file %s to be untouched: %v", stray, err)
	}

	// Output sanity: 2 removed, 2 kept, 1 skipped.
	out := stdout.String()
	if !strings.Contains(out, "removed 2") || !strings.Contains(out, "kept 2") || !strings.Contains(out, "1 skipped") {
		t.Errorf("unexpected stdout: %q", out)
	}
}

// TestArtworkGCDryRunPreservesAll — --dry-run must list orphans
// without touching the filesystem. Operators can audit before
// committing to a destructive sweep.
func TestArtworkGCDryRunPreservesAll(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertTrack(&manifest.Track{
		Path: "a/t1.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B",
		ArtworkMBID: "local-keep",
	}); err != nil {
		t.Fatal(err)
	}

	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(artworkDir, "local-keep-500.jpg")
	orphan := filepath.Join(artworkDir, "local-orphan-500.jpg")
	for _, p := range []string{keep, orphan} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	rc := runArtworkGC(&stdout, &stderr, store, artworkDir, true /* dryRun */)
	if rc != 0 {
		t.Fatalf("dry-run rc=%d, stderr=%s", rc, stderr.String())
	}

	// Both files must still exist after dry-run.
	for _, p := range []string{keep, orphan} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run touched file %s: %v", p, err)
		}
	}
	out := stdout.String()
	if !strings.Contains(out, "would remove") {
		t.Errorf("dry-run should preview removals, got stdout: %q", out)
	}
	if !strings.Contains(out, orphan) {
		t.Errorf("dry-run should name the orphan path, got: %q", out)
	}
}

// TestArtworkGCMissingDirIsOK — first-time bridge with no cache dir
// yet should treat as empty rather than erroring.
func TestArtworkGCMissingDirIsOK(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	missing := filepath.Join(dir, "artwork-not-yet")
	var stdout, stderr bytes.Buffer
	rc := runArtworkGC(&stdout, &stderr, store, missing, false)
	if rc != 0 {
		t.Errorf("missing dir should succeed, got rc=%d stderr=%s", rc, stderr.String())
	}
}

// TestArtworkGCRequiresTypedPhrase exercises the `--confirm` gate
// added in CodeRabbit Major round-1 on PR #167. `--gc` without
// `--dry-run` and without `--confirm GC-ARTWORK` must refuse with a
// non-zero exit and produce no filesystem effect. Driven through
// the public `artworkCmd` entry point so the flag-parsing + gate
// behaviour is exercised end-to-end.
func TestArtworkGCRequiresTypedPhrase(t *testing.T) {
	dir := t.TempDir()
	// A bridge.yaml file is required by config.Load; build a minimal
	// one. DataDir points at the test temp dir so the (unused) DB
	// open path resolves to a known location.
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("dataDir: \""+dir+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		args    []string
		wantRC  int
		wantErr string
	}{
		{
			name:    "no confirm",
			args:    []string{"--config", cfgPath, "--gc"},
			wantRC:  2,
			wantErr: "refusing to delete without --confirm GC-ARTWORK",
		},
		{
			name:    "wrong confirm value",
			args:    []string{"--config", cfgPath, "--gc", "--confirm", "yes"},
			wantRC:  2,
			wantErr: "refusing to delete without --confirm GC-ARTWORK",
		},
		{
			name:    "case-mismatched confirm",
			args:    []string{"--config", cfgPath, "--gc", "--confirm", "gc-artwork"},
			wantRC:  2,
			wantErr: "refusing to delete without --confirm GC-ARTWORK",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := artworkCmd(context.Background(), tc.args, &stdout, &stderr)
			if rc != tc.wantRC {
				t.Errorf("rc: got %d, want %d (stderr: %s)", rc, tc.wantRC, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr should mention %q, got: %q", tc.wantErr, stderr.String())
			}
		})
	}
}

// TestArtworkMBIDsInUseDistinctFiltering pins the store helper:
// distinct values, NULL-filtered, empty-string-filtered.
func TestArtworkMBIDsInUseDistinctFiltering(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rows := []struct {
		path, mbid string
	}{
		{"a/t1.flac", "local-aaa"},
		{"a/t2.flac", "local-aaa"}, // duplicate — should collapse
		{"a/t3.flac", "local-bbb"},
		{"a/t4.flac", ""}, // empty — should be filtered out
		{"a/t5.flac", "00000000-0000-4000-8000-000000000000"},
	}
	for _, r := range rows {
		if err := store.UpsertTrack(&manifest.Track{
			Path: r.path, Size: 1, ModTime: time.Now(),
			Artist: "A", Album: "B", ArtworkMBID: r.mbid,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ArtworkMBIDsInUse()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"local-aaa":                            true,
		"local-bbb":                            true,
		"00000000-0000-4000-8000-000000000000": true,
	}
	if len(got) != len(want) {
		t.Errorf("expected %d distinct ids, got %d (%v)", len(want), len(got), got)
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("unexpected mbid in result: %q", m)
		}
	}
}
