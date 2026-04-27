//go:build !windows
// +build !windows

package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestScannerSparesTracksUnderWalkErrorSubtree pins the contract added
// by PR #N: a transient WalkDir error during a scan must NOT result in
// `DeleteTrack` for paths under the affected subtree. Pre-fix, a
// single permission flap or NAS drop wiped every track in the
// affected directory from the manifest — files on disk were untouched
// but the bridge served an empty/partial library until the next clean
// rescan.
//
// We simulate the failure by `chmod 0` on a subtree mid-scan. WalkDir
// surfaces an EACCES via the err callback, the walker records the
// affected subtree, and the deletion pass spares its tracks. The
// healthy sibling subtree still gets its missing tracks deleted to
// prove the spare doesn't over-suppress legitimate deletions.
//
// `//go:build !windows` because chmod 0 doesn't reliably block dir
// reads on Windows; the equivalent Windows test would need an ACL
// API we don't import.
func TestScannerSparesTracksUnderWalkErrorSubtree(t *testing.T) {
	root := t.TempDir()
	// Two sibling subtrees, each with one track.
	healthy := filepath.Join(root, "healthy")
	flaky := filepath.Join(root, "flaky")
	for _, sub := range []string{healthy, flaky} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	healthyTrack := filepath.Join(healthy, "h.flac")
	flakyTrack := filepath.Join(flaky, "f.flac")
	for _, p := range []string{healthyTrack, flakyTrack} {
		if err := os.WriteFile(p, []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s)

	// First scan: both tracks land in the manifest.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	for _, p := range []string{"healthy/h.flac", "flaky/f.flac"} {
		got, _ := s.GetTrack(p)
		if got == nil {
			t.Fatalf("first scan didn't index %q", p)
		}
	}

	// Now simulate a transient I/O error on the `flaky` subtree by
	// removing read permission. The track on disk is unchanged, but
	// WalkDir's err callback fires when the walker tries to descend.
	if err := os.Chmod(flaky, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(flaky, 0o755) })

	// Second scan: the flaky subtree is unreadable, the healthy one
	// is fine. Pre-fix this would DeleteTrack the flaky entry.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if got, _ := s.GetTrack("flaky/f.flac"); got == nil {
		t.Errorf("flaky/f.flac was wiped from manifest after a transient walk error — regression of PR #N's deletion-pass guard")
	}
	if got, _ := s.GetTrack("healthy/h.flac"); got == nil {
		t.Errorf("healthy/h.flac was wiped (it should still be present — sibling subtree wasn't affected)")
	}
}

// TestScannerStillDeletesTracksUnderHealthySubtree pins the OTHER
// half of the contract: the spare must NOT over-suppress. A track
// genuinely removed from a healthy subtree (no walk error in scope)
// is still cleared from the manifest on the next scan.
func TestScannerStillDeletesTracksUnderHealthySubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "albums"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "albums", "song.flac")
	if err := os.WriteFile(target, []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s)

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTrack("albums/song.flac"); got == nil {
		t.Fatal("first scan didn't index albums/song.flac")
	}

	// Genuinely remove the file. No walk error this pass — the
	// deletion pass should clean the row.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTrack("albums/song.flac"); got != nil {
		t.Error("legitimately-removed track still in manifest — the spare-on-walk-error guard is over-suppressing")
	}
}

// TestIsUnderErroredSubtree pins the prefix-matching contract for the
// helper: the trailing-slash guard preventing "foo-other" from
// matching "foo", AND the root-level normalization where relPath
// returns "." for the root itself (qodo + gemini bot review on
// PR #N).
func TestIsUnderErroredSubtree(t *testing.T) {
	cases := []struct {
		name string
		path string
		subs []string
		want bool
	}{
		{"empty subtree set", "albums/song.flac", nil, false},
		{"exact match", "albums", []string{"albums"}, true},
		{"under match", "albums/song.flac", []string{"albums"}, true},
		{"deep under match", "albums/2024/song.flac", []string{"albums"}, true},
		{"sibling false match", "albums-other/song.flac", []string{"albums"}, false},
		{"unrelated", "music/song.flac", []string{"albums"}, false},
		{"multi-subtree, hit", "flaky/song.flac", []string{"healthy", "flaky"}, true},
		{"multi-subtree, miss", "other/song.flac", []string{"healthy", "flaky"}, false},

		// Root-level error normalization. `relPath(root, root, false)`
		// returns "." which without normalization wouldn't prefix-
		// match any track path. A whole-library outage (root
		// unreachable) MUST spare every track.
		{"root-level dot spares single-segment path", "song.flac", []string{"."}, true},
		{"root-level dot spares deep path", "albums/2024/song.flac", []string{"."}, true},
		{"empty-string subtree spares everything", "song.flac", []string{""}, true},
		// Coexistence: a root-level dot plus a specific subtree
		// still spares paths outside the specific subtree (the
		// dot's blanket clause wins). Sentinel for "if any root
		// errored, no deletions this pass."
		{"dot plus specific spares unrelated path", "elsewhere/song.flac", []string{".", "albums"}, true},

		// Multi-root whole-root sentinel. `relPath(root, root, true)`
		// produces "<rootBase>/." which the helper must treat as
		// "spare everything under <rootBase>/". coderabbit CRITICAL
		// bug catch on PR #74 follow-up — without this, multi-root
		// configs with a downed root still wiped the manifest for
		// that root.
		{"multi-root sentinel spares path under same root", "music-nas/song.flac", []string{"music-nas/."}, true},
		{"multi-root sentinel spares deep path under same root", "music-nas/Artist/Album/song.flac", []string{"music-nas/."}, true},
		// And does NOT spare paths under a sibling root that didn't
		// error — the dotted-suffix prefix is scoped to the right
		// rootBase.
		{"multi-root sentinel does not spare sibling root", "other-nas/song.flac", []string{"music-nas/."}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := make(map[string]struct{}, len(tc.subs))
			for _, s := range tc.subs {
				set[s] = struct{}{}
			}
			if got := isUnderErroredSubtree(tc.path, set); got != tc.want {
				t.Errorf("isUnderErroredSubtree(%q, %v) = %v, want %v",
					tc.path, tc.subs, got, tc.want)
			}
		})
	}
}
