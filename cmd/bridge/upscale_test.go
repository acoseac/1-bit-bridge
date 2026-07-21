package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestClassifyUpscaleTrackSkipsNonPositiveSampleRate pins the graceful-
// skip guard: a PCM track whose SampleRate is present but non-positive
// (0 / corrupt tag) with bits > 16 must be a silent skip that bumps the
// notPCM counter, NOT a fatal exit. Pre-fix the optimize path let such a
// track through OptimizeEligible (which returns true for a PCM source
// with bits > 16 regardless of rate), then ResolveTargetRateForOptimize(0)
// errored — propagating exitCode 2 up through runUpscaleBatch and
// ABORTING the entire optimize run. One bogus tag killed the whole batch,
// unlike every other per-track anomaly which is a graceful skip.
func TestClassifyUpscaleTrackSkipsNonPositiveSampleRate(t *testing.T) {
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := bridgefs.New([]string{t.TempDir()})

	zeroRate := 0.0
	bits := 24
	track := manifest.Track{
		Path:          "Artist/Album/bogus.flac",
		Codec:         "FLAC", // PCM → OptimizeEligible depends only on rate/bits
		SampleRate:    &zeroRate,
		BitsPerSample: &bits, // > 16 so OptimizeEligible would return true
	}

	// Both kinds must skip gracefully; the optimize path is the one that
	// aborted pre-fix, the upscale path is covered for symmetry.
	for _, kind := range []struct {
		name string
		k    transcode.JobKind
	}{
		{"optimize", transcode.JobKindOptimize},
		{"upscale", transcode.JobKindUpscale},
	} {
		t.Run(kind.name, func(t *testing.T) {
			var counters upscaleSkipCounters
			var stderr bytes.Buffer
			p := runUpscaleParams{
				targetRateFlag: "auto",
				targetBits:     24,
				quality:        transcode.QualityVeryHigh,
				kind:           kind.k,
			}
			c, exit := classifyUpscaleTrack(context.Background(), &stderr, store, resolver, track, p, &counters)
			if exit != 0 {
				t.Fatalf("exitCode = %d, want 0 (graceful skip, not fatal abort); stderr=%q", exit, stderr.String())
			}
			if c != nil {
				t.Errorf("candidate = %+v, want nil (skip)", c)
			}
			if counters.notPCM != 1 {
				t.Errorf("notPCM = %d, want 1", counters.notPCM)
			}
		})
	}
}

// TestBootstrapTranscodeCmdHonorsVariantsDir pins that the upscale /
// optimize / --gc CLI writes sidecars to `upscale.variantsDir` when set
// (so a host whose data disk is too small for the full variant set can
// relocate them, e.g. onto a network mount), and falls back to the
// historical `<dataDir>/transcoded/` when the field is empty. Both paths
// flow through the shared bootstrapTranscodeCmd → r.outputDir, which is
// also what `--gc` walks.
func TestBootstrapTranscodeCmdHonorsVariantsDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	libDir := filepath.Join(dir, "lib")
	for _, d := range []string{dataDir, libDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg := func(extra string) string {
		p := filepath.Join(dir, "bridge.yaml")
		body := "dataDir: " + dataDir + "\nlibraryRoots:\n    - " + libDir + "\n" + extra
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("variantsDir set", func(t *testing.T) {
		variantsDir := filepath.Join(dir, "elsewhere", "variants")
		cfgPath := writeCfg("upscale:\n    enabled: true\n    variantsDir: " + variantsDir + "\n")
		var stderr bytes.Buffer
		r, code := bootstrapTranscodeCmd(context.Background(), &stderr, cfgPath, "very-high", true /* gcMode: skip the sox precheck so the test runs on sox-less CI; outputDir resolves identically */)
		if r == nil {
			t.Fatalf("bootstrap failed (code=%d): %s", code, stderr.String())
		}
		defer r.store.Close()
		if r.outputDir != variantsDir {
			t.Errorf("outputDir = %q, want variantsDir %q", r.outputDir, variantsDir)
		}
	})

	t.Run("variantsDir unset falls back to dataDir/transcoded", func(t *testing.T) {
		cfgPath := writeCfg("upscale:\n    enabled: true\n")
		var stderr bytes.Buffer
		r, code := bootstrapTranscodeCmd(context.Background(), &stderr, cfgPath, "very-high", true /* gcMode: skip the sox precheck so the test runs on sox-less CI; outputDir resolves identically */)
		if r == nil {
			t.Fatalf("bootstrap failed (code=%d): %s", code, stderr.String())
		}
		defer r.store.Close()
		want := filepath.Join(dataDir, "transcoded")
		if r.outputDir != want {
			t.Errorf("outputDir = %q, want default %q", r.outputDir, want)
		}
	})
}

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
// The forward sweep must not delete a live sidecar when the DB
// SidecarPath and the on-disk (WalkDir) path differ only in case — the
// data-loss hazard on case-insensitive filesystems (Windows / macOS).
// The known-set is built case-folded (as runGC does), so the mixed-case
// on-disk file must still match. On the pre-fix raw lookup the mixed-case
// walk path misses the lowercase-keyed map and the file is deleted.
func TestRunGCForwardSweepCaseInsensitive(t *testing.T) {
	outputDir := t.TempDir()
	dir := filepath.Join(outputDir, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "Track.flac.upscaled-v1-192000-24.flac")
	if err := os.WriteFile(sidecar, []byte("flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Known-set keyed exactly as runGC keys it: case-folded + cleaned.
	// The lowercase key vs the mixed-case on-disk walk path is the casing
	// delta the fix bridges.
	known := map[string]bool{strings.ToLower(filepath.Clean(sidecar)): true}

	removed, kept, failed, exitCode := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, outputDir, known)
	if exitCode != 0 || failed != 0 {
		t.Fatalf("sweep unexpected: exit=%d failed=%d", exitCode, failed)
	}
	if removed != 0 {
		t.Errorf("live sidecar deleted over a casing delta (removed=%d)", removed)
	}
	if kept != 1 {
		t.Errorf("kept=%d, want 1", kept)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("live sidecar %s was deleted: %v", sidecar, err)
	}
}

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
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 100, ModTime: oldTime,
		}); err != nil {
			t.Fatalf("UpsertTrack %s: %v", p, err)
		}
	}

	liveSidecar := filepath.Join(transcodedDir, "abc123-upscaled-v2-176400-24.flac")
	if err := os.WriteFile(liveSidecar, []byte("flac-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
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
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
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
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: brokenLinkSrc, Size: 100, ModTime: oldTime,
	}); err != nil {
		t.Fatalf("UpsertTrack brokenLink: %v", err)
	}
	missingTarget := filepath.Join(transcodedDir, "this-target-never-existed.flac")
	brokenLink := filepath.Join(transcodedDir, "ghi789-upscaled-v1-176400-24.flac")
	if err := os.Symlink(missingTarget, brokenLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
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
	rc := runGC(context.Background(), &stdout, &stderr, store, transcodedDir)
	if rc != 0 {
		t.Fatalf("runGC rc=%d, stderr=%s", rc, stderr.String())
	}

	// Live row + live sidecar must survive. Dead row + broken-link
	// row must be removed (both are phantom variants from the
	// bridge's `/v1/download` perspective: opening through either
	// yields ENOENT). Per Gemini on PR #207: the broken-link case
	// is exactly why `os.Stat` (follows links) is correct here
	// rather than `os.Lstat` (link presence ≠ servability).
	all, err := store.AllVariants(context.Background())
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
	delta, err := store.ListTracks(context.Background(), &watermark)
	if err != nil {
		t.Fatal(err)
	}
	var sawDead, sawLive, sawBrokenLink bool
	for _, tr := range delta {
		switch tr.Path {
		case deadSrc:
			sawDead = true
		case liveSrc:
			sawLive = true
		case brokenLinkSrc:
			sawBrokenLink = true
		}
	}
	if !sawDead {
		t.Error("expected dead-row's parent track to be returned by ListTracks(since:) — indexed_at bump missing")
	}
	if !sawBrokenLink {
		t.Error("expected broken-link-row's parent track to be returned by ListTracks(since:) — indexed_at bump missing for the symlink case (CodeRabbit on PR #207 round 3)")
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
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: src, Size: 100, ModTime: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	missingDir := filepath.Join(dir, "transcoded-never-created")
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: src, VariantID: "upscaled-v2-176400-24",
		SidecarPath: filepath.Join(missingDir, "abc-upscaled-v2-176400-24.flac"),
		Format:      "flac",
		SampleRate:  176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, store, missingDir)
	if rc == 0 {
		t.Fatalf("expected runGC to fail when outputDir is missing but rows exist; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	// Critical assertion: the row MUST still be in the DB. The
	// guard's whole purpose is to prevent a mass-delete on
	// environmental failure.
	all, err := store.AllVariants(context.Background())
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

// TestGCCheckOutputDirBeforeReverseSweep pins the guard matrix
// directly (2026-07-21 review M15): with rows in the catalog, a
// missing OR exists-but-empty outputDir refuses (exit 1) — the
// cleanly-unmounted-mountpoint signature — while a non-empty dir
// proceeds, and zero rows proceed regardless of dir state.
func TestGCCheckOutputDirBeforeReverseSweep(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the outputDir to probe.
		setup   func(t *testing.T) string
		rows    int
		wantRC  int
		wantMsg string // stderr substring expected when refusing; "" otherwise
	}{
		{
			name: "missing dir with rows refuses",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "unmounted-mountpoint")
			},
			rows:    2,
			wantRC:  1,
			wantMsg: "refusing to delete rows",
		},
		{
			name: "empty dir with rows refuses",
			setup: func(t *testing.T) string {
				return t.TempDir() // exists, holds nothing — unmounted mountpoint
			},
			rows:    2,
			wantRC:  1,
			wantMsg: "refusing to delete rows",
		},
		{
			name: "non-empty dir with rows proceeds",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "sidecar.flac"), []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return dir
			},
			rows:    2,
			wantRC:  0,
			wantMsg: "",
		},
		{
			name: "zero rows proceeds regardless of missing dir",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "never-created")
			},
			rows:    0,
			wantRC:  0,
			wantMsg: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			rc := gcCheckOutputDirBeforeReverseSweep(&stderr, tc.setup(t), tc.rows)
			if rc != tc.wantRC {
				t.Fatalf("gcCheckOutputDirBeforeReverseSweep rc = %d, want %d (stderr=%s)", rc, tc.wantRC, stderr.String())
			}
			if tc.wantMsg != "" && !strings.Contains(stderr.String(), tc.wantMsg) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantMsg)
			}
			if tc.wantMsg == "" && stderr.Len() != 0 {
				t.Errorf("expected no stderr output on proceed, got %q", stderr.String())
			}
		})
	}
}

// TestRunGCRefusesWhenOutputDirEmptyButRowsExist is the
// exists-but-empty twin of
// TestRunGCRefusesWhenOutputDirMissingButRowsExist (2026-07-21
// review M15 — prior audit B4's unfixed half): on Linux a
// cleanly-unmounted variants volume leaves its mountpoint in
// place as an EMPTY local dir, which the pre-fix guard waved
// through because os.Stat succeeded. The reverse sweep would
// then ENOENT every sidecar and mass-delete the catalog.
func TestRunGCRefusesWhenOutputDirEmptyButRowsExist(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "bridge.db")
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed one variant row whose sidecar_path lives under a
	// transcoded directory that exists but is EMPTY.
	src := "Artist/Album/01 - protected.flac"
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: src, Size: 100, ModTime: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(dir, "transcoded-empty")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: src, VariantID: "upscaled-v2-176400-24",
		SidecarPath: filepath.Join(emptyDir, "abc-upscaled-v2-176400-24.flac"),
		Format:      "flac",
		SampleRate:  176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, store, emptyDir)
	if rc == 0 {
		t.Fatalf("expected runGC to fail when outputDir is empty but rows exist; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	// Critical assertion: the row MUST still be in the DB. The
	// guard's whole purpose is to prevent a mass-delete on
	// environmental failure.
	all, err := store.AllVariants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SourcePath != src {
		t.Errorf("expected the variant row to survive the refused gc; got %d rows: %+v", len(all), all)
	}

	se := stderr.String()
	if !strings.Contains(se, "refusing to delete rows") {
		t.Errorf("expected stderr to explain the refusal: %s", se)
	}
}
