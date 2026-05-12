package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestRunGCReverseSweepRemovesOrphanRows pins the load-bearing fix:
// a `track_variants` row whose `sidecar_path` is missing on disk
// must be removed by `bridge upscale --gc`, AND the parent track's
// `indexed_at` must be bumped so the next iOS delta sync sees the
// variant disappear.
//
// Field-confirmed scenario this guards against (Abdullah Ibrahim /
// "The Balance" album, 2026-05-12): operator generated v1 upscale
// variants under a 64-char-hash sidecar naming scheme; later upscale
// pass migrated to v2 with 16-char-hash naming; v1 sidecar files
// were removed but the v1 `track_variants` rows survived. Every iOS
// play attempt of the affected tracks resolved to the dead v1
// variant, hit `410 Gone` on `/v1/download`, fell back to source via
// PR #351; next manifest sync re-pulled the same dead v1 ID and the
// loop restarted. The reverse-sweep gc breaks the loop at the
// source-of-truth layer.
func TestRunGCReverseSweepRemovesOrphanRows(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	transcodedDir := filepath.Join(dir, "transcoded")
	if err := os.MkdirAll(transcodedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Stage: one row with a LIVE sidecar (must survive), one row
	// with a missing sidecar (must be removed). Parent track rows
	// for both so the variant FK doesn't violate; UpsertTrack first.
	liveSrc := "Artist/Album/01 - alive.flac"
	deadSrc := "Artist/Album/02 - phantom.flac"
	// Use an old fixed mtime so the upsert chain doesn't slide indexed_at
	// under us — we want to measure the gc's bump in isolation.
	oldTime := time.Now().Add(-time.Hour)
	for _, p := range []string{liveSrc, deadSrc} {
		if err := store.UpsertTrack(&manifest.Track{
			Path: p, Size: 100, ModTime: oldTime,
		}); err != nil {
			t.Fatalf("UpsertTrack %s: %v", p, err)
		}
	}

	liveSidecar := filepath.Join(transcodedDir, "abc123-upscaled-v2-176400-24.flac")
	if err := os.WriteFile(liveSidecar, []byte("flac-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(manifest.VariantRow{
		SourcePath: liveSrc, VariantID: "upscaled-v2-176400-24",
		SidecarPath: liveSidecar, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatalf("UpsertVariant live: %v", err)
	}

	deadSidecar := filepath.Join(transcodedDir, "def456-upscaled-v1-176400-24.flac")
	// NOTE: deliberately do NOT create deadSidecar — that's the bug
	// scenario. The DB row will reference a path that never existed
	// (equivalent to one whose file was later removed).
	if err := store.UpsertVariant(manifest.VariantRow{
		SourcePath: deadSrc, VariantID: "upscaled-v1-176400-24",
		SidecarPath: deadSidecar, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatalf("UpsertVariant dead: %v", err)
	}

	// Third scenario: a row whose `sidecar_path` IS a live symlink,
	// but the symlink's target is missing. The bridge's
	// `/v1/download` path opens through the symlink and would 410
	// on the broken target, so the gc must treat this identically
	// to a directly-missing file. `os.Stat` follows the link and
	// returns ENOENT on the missing target; `os.Lstat` would have
	// succeeded (the link itself exists) and left this row in
	// place, which is wrong. Per Gemini on PR #207.
	brokenLinkSrc := "Artist/Album/03 - phantom-link.flac"
	if err := store.UpsertTrack(&manifest.Track{
		Path: brokenLinkSrc, Size: 100, ModTime: oldTime,
	}); err != nil {
		t.Fatalf("UpsertTrack brokenLink: %v", err)
	}
	missingTarget := filepath.Join(transcodedDir, "this-target-never-existed.flac")
	brokenLink := filepath.Join(transcodedDir, "ghi789-upscaled-v1-176400-24.flac")
	if err := os.Symlink(missingTarget, brokenLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := store.UpsertVariant(manifest.VariantRow{
		SourcePath: brokenLinkSrc, VariantID: "upscaled-v1-176400-24",
		SidecarPath: brokenLink, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatalf("UpsertVariant brokenLink: %v", err)
	}

	// Snapshot of indexed_at via the public API: `ListTracks(since:)`
	// returns rows where `indexed_at > since`. Captured just before
	// runGC so the reverse-sweep bump for the dead-row's parent
	// crosses this watermark.
	watermark := time.Now()
	// Brief sleep so the bump (UnixNano) is strictly > watermark.
	time.Sleep(2 * time.Millisecond)

	var stdout, stderr bytes.Buffer
	rc := runGC(&stdout, &stderr, store, transcodedDir)
	if rc != 0 {
		t.Fatalf("runGC rc=%d, stderr=%s", rc, stderr.String())
	}

	// Live row + live sidecar must survive. Dead row + broken-link
	// row must be removed (both are phantom variants from the
	// bridge's `/v1/download` perspective: opening through either
	// yields ENOENT). Per Gemini on PR #207: the broken-link case
	// is exactly why `os.Stat` (follows links) is correct here
	// rather than `os.Lstat` (link presence ≠ servability).
	all, err := store.AllVariants()
	if err != nil {
		t.Fatal(err)
	}
	var keptLive, keptDead, keptBrokenLink bool
	for _, v := range all {
		switch v.SourcePath {
		case liveSrc:
			keptLive = true
		case deadSrc:
			keptDead = true
		case brokenLinkSrc:
			keptBrokenLink = true
		}
	}
	if !keptLive {
		t.Error("expected live variant row to survive reverse sweep")
	}
	if keptDead {
		t.Error("expected dead variant row (missing sidecar) to be removed")
	}
	if keptBrokenLink {
		t.Error("expected broken-symlink variant row to be removed — os.Lstat-based check would erroneously keep it")
	}
	if _, err := os.Stat(liveSidecar); err != nil {
		t.Errorf("expected live sidecar to survive forward sweep: %v", err)
	}

	// indexed_at must be bumped on the dead row's parent track so
	// iOS delta-sync (`WHERE indexed_at > ?`) surfaces the removal.
	// `ListTracks(since: watermark)` is the iOS-facing read path:
	// the dead row's parent MUST appear (its indexed_at was bumped
	// past watermark by DeleteVariant); the live row's parent MUST
	// NOT (no bump happened for it).
	delta, err := store.ListTracks(&watermark)
	if err != nil {
		t.Fatal(err)
	}
	var sawDead, sawLive bool
	for _, tr := range delta {
		if tr.Path == deadSrc {
			sawDead = true
		}
		if tr.Path == liveSrc {
			sawLive = true
		}
	}
	if !sawDead {
		t.Error("expected dead-row's parent track to be returned by ListTracks(since:) — indexed_at bump missing")
	}
	if sawLive {
		t.Error("expected live-row's parent track NOT to be returned by ListTracks(since:) — false bump would re-pull untouched tracks")
	}

	// Stdout must mention both sweeps so an operator running the
	// command sees confirmation that both directions ran.
	out := stdout.String()
	if !strings.Contains(out, "forward sweep") {
		t.Errorf("expected stdout to mention forward sweep: %s", out)
	}
	if !strings.Contains(out, "reverse sweep") {
		t.Errorf("expected stdout to mention reverse sweep: %s", out)
	}
	if !strings.Contains(out, "removed 2 orphan row") {
		t.Errorf("expected reverse-sweep removal count to be 2 (dead + broken-link): %s", out)
	}
}

// TestRunGCRefusesWhenOutputDirMissingButRowsExist locks the
// mass-delete safety guard. Scenario: operator's transcoded
// directory disappears (external drive disconnected, broken
// mount, filesystem error) but `track_variants` still has rows.
// Without the guard, the reverse sweep would `os.Stat` every
// row, get ENOENT for all of them, and call `DeleteVariant` on
// every single one — wiping the entire variant catalog.
// CodeRabbit on PR #207.
//
// The legitimate empty-state case (`len(allRows) == 0`, no
// upscales ever generated) is NOT covered here because the
// forward sweep's `WalkDir` already handles a missing
// `outputDir` via `filepath.SkipDir` and the reverse sweep's
// loop body is a no-op against an empty row set — both pass
// trivially without the guard. The guard only fires when
// there's something to lose.
func TestRunGCRefusesWhenOutputDirMissingButRowsExist(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed one variant row whose sidecar_path lives under a
	// transcoded directory that we WILL NOT create.
	src := "Artist/Album/01 - protected.flac"
	if err := store.UpsertTrack(&manifest.Track{
		Path: src, Size: 100, ModTime: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	missingDir := filepath.Join(dir, "transcoded-never-created")
	if err := store.UpsertVariant(manifest.VariantRow{
		SourcePath: src, VariantID: "upscaled-v2-176400-24",
		SidecarPath: filepath.Join(missingDir, "abc-upscaled-v2-176400-24.flac"),
		Format:      "flac",
		SampleRate:  176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(&stdout, &stderr, store, missingDir)
	if rc == 0 {
		t.Fatalf("expected runGC to fail when outputDir is missing but rows exist; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	// Critical assertion: the row MUST still be in the DB. The
	// guard's whole purpose is to prevent a mass-delete on
	// environmental failure.
	all, err := store.AllVariants()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SourcePath != src {
		t.Errorf("expected the variant row to survive the refused gc; got %d rows: %+v", len(all), all)
	}

	// Stderr must explain WHY the gc refused, so an operator
	// running this from cron / a script sees an actionable
	// message rather than a silent failure.
	se := stderr.String()
	if !strings.Contains(se, "refusing to delete rows") {
		t.Errorf("expected stderr to explain the refusal: %s", se)
	}
}
