package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
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
	cases := []struct {
		name      string
		args      []string
		wantCode  int
		wantInErr string
	}{
		{
			name:      "no confirm",
			args:      []string{"--to", "/tmp/variants-test"},
			wantCode:  2,
			wantInErr: "refusing to proceed without --confirm MOVE",
		},
		{
			name:      "wrong confirm value (prefix)",
			args:      []string{"--to", "/tmp/variants-test", "--confirm", "MOV"},
			wantCode:  2,
			wantInErr: "refusing to proceed without --confirm MOVE",
		},
		{
			name:      "wrong confirm value (lowercase)",
			args:      []string{"--to", "/tmp/variants-test", "--confirm", "move"},
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
	// Point at a nonexistent config so it fails after the gate;
	// we just want to confirm the gate doesn't trip.
	code := variantsMoveCmd(context.Background(),
		[]string{"--config", "/nonexistent/bridge.yaml", "--to", "/tmp/variants-test", "--dry-run"},
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
