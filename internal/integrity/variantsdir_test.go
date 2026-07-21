package integrity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVariantsDirSweepBlockReason pins the shared probe's contract
// (2026-07-21 review H4 + M15): missing, empty, and non-directory
// variants dirs all block a deletion sweep with an actionable
// reason; a dir holding at least one entry (file or subdir) is
// healthy and returns "".
func TestVariantsDirSweepBlockReason(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the dir path to probe.
		setup func(t *testing.T) string
		// wantReason is the substring the block reason must
		// contain; "" means healthy (no block).
		wantReason string
	}{
		{
			name: "missing dir blocks",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "never-created")
			},
			wantReason: "missing",
		},
		{
			name: "empty dir blocks",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantReason: "empty",
		},
		{
			name: "regular file blocks",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return p
			},
			wantReason: "not a directory",
		},
		{
			name: "dir with a file is healthy",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "sidecar.flac"), []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return dir
			},
			wantReason: "",
		},
		{
			name: "dir with only a subdir is healthy",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
				return dir
			},
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := VariantsDirSweepBlockReason(tc.setup(t))
			if tc.wantReason == "" {
				if reason != "" {
					t.Errorf("healthy dir blocked: %q", reason)
				}
				return
			}
			if reason == "" {
				t.Fatalf("expected a block reason containing %q, got healthy", tc.wantReason)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("block reason %q does not contain %q", reason, tc.wantReason)
			}
		})
	}
}
