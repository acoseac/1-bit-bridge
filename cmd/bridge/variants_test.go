package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestVariantsMoveRefusesWithoutConfirm pins the typed-phrase
// confirmation gate: without `--confirm MOVE` AND without
// `--dry-run`, the command must exit 2 with a clear refusal
// message BEFORE touching the config or manifest store.
//
// CodeRabbit Major on PR #245 required this gate; Gemini medium
// on PR #246 asked for regression coverage so a future refactor
// doesn't accidentally weaken or remove it.
func TestVariantsMoveRefusesWithoutConfirm(t *testing.T) {
	// `t.TempDir()` gives a per-platform absolute path so the
	// `filepath.IsAbs` gate in `variantsMoveCmd` passes uniformly
	// on Unix + Windows. A literal `/tmp/variants-test` would
	// fail `IsAbs` on Windows (no volume name) and short-circuit
	// the test before reaching the typed-phrase gate that's
	// actually under test. CodeRabbit Major on PR #246 round-2.
	toDir := filepath.Join(t.TempDir(), "variants-test")
	cases := []struct {
		name      string
		args      []string
		wantCode  int
		wantInErr string
	}{
		{
			name:      "no confirm",
			args:      []string{"--to", toDir},
			wantCode:  2,
			wantInErr: "refusing to proceed without --confirm MOVE",
		},
		{
			name:      "wrong confirm value (prefix)",
			args:      []string{"--to", toDir, "--confirm", "MOV"},
			wantCode:  2,
			wantInErr: "refusing to proceed without --confirm MOVE",
		},
		{
			name:      "wrong confirm value (lowercase)",
			args:      []string{"--to", toDir, "--confirm", "move"},
			wantCode:  2,
			wantInErr: "refusing to proceed without --confirm MOVE",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := variantsMoveCmd(context.Background(), c.args, &stdout, &stderr)
			if code != c.wantCode {
				t.Errorf("exit code: got %d, want %d (stderr=%q)",
					code, c.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.wantInErr) {
				t.Errorf("stderr should contain %q, got %q",
					c.wantInErr, stderr.String())
			}
		})
	}
}

// TestVariantsMoveDryRunSkipsConfirm: `--dry-run` is the explicit
// preview path; the typed-phrase gate is skipped because no files
// or DB rows are touched. The dry-run still requires a real config
// to load, so this test only verifies it gets PAST the confirm
// gate (subsequent config-load failure is acceptable and not what
// we're pinning).
func TestVariantsMoveDryRunSkipsConfirm(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Per-platform absolute paths so `filepath.IsAbs` is satisfied
	// uniformly (Windows volume requirement). Point at a
	// nonexistent config so the command fails after the gate; we
	// just want to confirm the gate doesn't trip.
	baseDir := t.TempDir()
	missingConfig := filepath.Join(baseDir, "missing-bridge.yaml")
	toDir := filepath.Join(baseDir, "variants-test")
	code := variantsMoveCmd(context.Background(),
		[]string{"--config", missingConfig, "--to", toDir, "--dry-run"},
		&stdout, &stderr)
	if code == 2 && strings.Contains(stderr.String(), "refusing to proceed") {
		t.Errorf("--dry-run should bypass the confirm gate; got refusal: %q",
			stderr.String())
	}
}

// TestIsUnderAnyLibraryRoot pins the containment predicate that
// rejects destinations equal to OR nested under any library root.
//
// Pre-fix shape (PR #245) used `rel[0] != '.'` which silently
// allowed:
//   - rel == "."         (destination identical to a root)
//   - rel == ".cache/x"  (literal dot-prefixed subpath)
//
// CodeRabbit Major caught it; Gemini medium on PR #246 asked for
// regression tests on those exact cases.
func TestIsUnderAnyLibraryRoot(t *testing.T) {
	roots := []string{"/library/music", "/library/audio"}
	cases := []struct {
		name string
		to   string
		want string // "" means safe; otherwise the matching root.
	}{
		{
			name: "outside every root — safe",
			to:   "/data/transcoded",
			want: "",
		},
		{
			name: "outside every root — sibling of root",
			to:   "/library/transcoded",
			want: "",
		},
		{
			name: "equal to root — REJECT (was bypassable pre-fix)",
			to:   "/library/music",
			want: "/library/music",
		},
		{
			name: "direct child — REJECT",
			to:   "/library/music/transcoded",
			want: "/library/music",
		},
		{
			name: "deep child — REJECT",
			to:   "/library/music/Artist/Album/cache",
			want: "/library/music",
		},
		{
			name: "dot-prefixed child — REJECT (was bypassable pre-fix)",
			to:   "/library/music/.cache",
			want: "/library/music",
		},
		{
			name: "dot-prefixed deep child — REJECT (was bypassable pre-fix)",
			to:   "/library/music/.cache/variants",
			want: "/library/music",
		},
		{
			name: "matches second root",
			to:   "/library/audio/variants",
			want: "/library/audio",
		},
		{
			name: "empty root slot is ignored",
			to:   "/data/transcoded",
			want: "",
		},
		{
			name: "trailing slash on root — same shape after clean",
			to:   "/library/music/transcoded",
			want: "/library/music",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			testRoots := roots
			if c.name == "empty root slot is ignored" {
				testRoots = []string{"", "/library/music", "/library/audio"}
			}
			if c.name == "trailing slash on root — same shape after clean" {
				testRoots = []string{"/library/music/", "/library/audio/"}
			}
			got := isUnderAnyLibraryRoot(c.to, testRoots)
			// Compare via filepath.Clean so the test isn't fragile
			// against trailing-slash variations in `want`.
			if filepath.Clean(got) != filepath.Clean(c.want) {
				t.Errorf("got %q, want %q (to=%q)", got, c.want, c.to)
			}
		})
	}
}

// TestComputeNewSidecarPath_MatchesPoolWriter pins V1: the move CLI must
// recompute the sidecar filename with the SAME FAT-sanitization +
// 255-byte cap the pool writer applies (transcode.VariantSidecarBasename),
// so a move to a FAT/exFAT target doesn't fail on colon/`?`-bearing
// classical filenames and an over-long name doesn't hit ENAMETOOLONG.
// Pre-fix this used a raw fmt.Sprintf that skipped both.
func TestComputeNewSidecarPath_MatchesPoolWriter(t *testing.T) {
	cases := []struct {
		name       string
		sourcePath string
		variantID  string
	}{
		{"fat-illegal chars", `Artist/Album/Track: A? "Live".flac`, "upscaled-v1-96000-24"},
		{"over-long basename", "Artist/Album/" + strings.Repeat("x", 300) + ".flac", "optimized-v1-44100-16"},
		{"clean name", "Artist/Album/Song.flac", "upscaled-v1-192000-24"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeNewSidecarPath(filepath.Join("out", "variants"), manifest.VariantRow{
				SourcePath: c.sourcePath,
				VariantID:  c.variantID,
			})
			gotBase := filepath.Base(got)
			wantBase := transcode.VariantSidecarBasename(filepath.Base(c.sourcePath), c.variantID)
			if gotBase != wantBase {
				t.Errorf("basename = %q, want pool-writer basename %q", gotBase, wantBase)
			}
			for _, bad := range []string{":", "?", `"`, "*", "<", ">", "|"} {
				if strings.Contains(gotBase, bad) {
					t.Errorf("filename %q still contains FAT-illegal %q", gotBase, bad)
				}
			}
			if len(gotBase) > 255 {
				t.Errorf("basename length %d exceeds 255-byte cap: %q", len(gotBase), gotBase)
			}
		})
	}
}

// TestIsUnderAnyLibraryRoot_ResolvesSymlinkedParent pins V2: a --to whose
// PARENT symlinks into a library root must be caught even though --to
// itself doesn't exist yet. Pre-fix (lexical Clean only) this slipped
// through, and os.MkdirAll would then write variant FLACs through the
// symlink into the read-only library.
func TestIsUnderAnyLibraryRoot_ResolvesSymlinkedParent(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlinks unsupported on this platform/host: %v", err)
	}
	// link -> realRoot; "variants" under it doesn't exist yet.
	to := filepath.Join(link, "variants")
	if got := isUnderAnyLibraryRoot(to, []string{realRoot}); got == "" {
		t.Fatalf("symlinked-parent --to %q not detected as under root %q", to, realRoot)
	}
}
