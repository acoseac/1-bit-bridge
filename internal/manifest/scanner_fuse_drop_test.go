//go:build !windows
// +build !windows

package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScannerSparesRowsUnderUnreachableRoot pins FUSE drop mode (a):
// when a configured library root becomes unreadable / nonexistent
// between scans, the scanner must NOT wipe its rows. Without the
// upfront os.Stat + sentinel guard, a `os.RemoveAll(root)` (proxy
// for an rclone mount that fully unmounted) would surface as an
// empty WalkDir for the root prefix and the deletion pass would
// reap every row under it.
//
// Multi-root setup confirms the loop CONTINUES to the next root
// rather than fatal-aborting — the healthy SSD-rooted library must
// still get its legitimate deletions cleaned.
func TestScannerSparesRowsUnderUnreachableRoot(t *testing.T) {
	parent := t.TempDir()
	healthy := filepath.Join(parent, "ssd")
	flaky := filepath.Join(parent, "fuse")
	for _, sub := range []string{healthy, flaky} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(healthy, "h.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flaky, "f.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{healthy, flaky}, s, "")

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	// Multi-root storage: paths begin with the root's base directory name.
	for _, p := range []string{"ssd/h.flac", "fuse/f.flac"} {
		if got, _ := s.GetTrack(context.Background(), p); got == nil {
			t.Fatalf("first scan didn't index %q", p)
		}
	}

	// Take the FUSE root completely offline — simulates rclone unmounted
	// + the host dir removed (or rclone never re-bound after a daemon
	// restart). os.Stat(root) will now fail.
	if err := os.RemoveAll(flaky); err != nil {
		t.Fatal(err)
	}

	// Also create a missing file under the HEALTHY root so the deletion
	// pass has a legitimate row to reap there — pins that the
	// multi-root continue doesn't over-suppress.
	healthyOrphan := filepath.Join(healthy, "gone.flac")
	if err := os.WriteFile(healthyOrphan, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("scan for orphan-seed: %v", err)
	}
	if got, _ := s.GetTrack(context.Background(), "ssd/gone.flac"); got == nil {
		t.Fatal("orphan-seed scan didn't index ssd/gone.flac")
	}

	// Now remove the healthy orphan AND keep FUSE root offline. The
	// healthy-side row must be reaped; the FUSE-side row must be spared.
	if err := os.Remove(healthyOrphan); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("recovery scan: %v", err)
	}
	if got, _ := s.GetTrack(context.Background(), "fuse/f.flac"); got == nil {
		t.Error("fuse/f.flac was wiped after root went unreachable — FUSE drop mode (a) guard regressed")
	}
	if got, _ := s.GetTrack(context.Background(), "ssd/gone.flac"); got != nil {
		t.Error("ssd/gone.flac survived even though sibling root failure shouldn't block legitimate deletion (multi-root continue regressed)")
	}
}

// TestScannerSparesRowsOnCleanEmptyMount pins FUSE drop mode (b):
// a cleanly-unmounted FUSE mount leaves the host directory empty but
// existent — `os.Stat(root)` and `WalkDir(root)` both succeed
// silently. Without the per-root clean-empty interrogation, the
// deletion pass would nuke every row under the dropped mount on the
// very first re-scan.
func TestScannerSparesRowsOnCleanEmptyMount(t *testing.T) {
	root := t.TempDir()
	subA := filepath.Join(root, "Artist")
	if err := os.MkdirAll(subA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subA, "song.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got == nil {
		t.Fatal("first scan didn't index Artist/song.flac")
	}

	// Wipe the contents but keep the host directory (mimics rclone
	// cleanly unmounting + host dir still present).
	if err := os.RemoveAll(subA); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("recovery scan: %v", err)
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got == nil {
		t.Error("Artist/song.flac was wiped on a clean-empty root — FUSE drop mode (b) guard regressed")
	}
}

// TestScannerHonoursAllowEmptySentinel pins the operator escape
// hatch: when the bridge has been deliberately wiped (operator
// migrated libraries, switched mounts, etc.), placing
// `.bridge-allow-empty` at the root authorises the deletion pass
// to proceed on a clean-empty root.
func TestScannerHonoursAllowEmptySentinel(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Artist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "song.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Wipe contents AND place the sentinel.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, allowEmptySentinelFilename), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("sentinel-authorised scan: %v", err)
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got != nil {
		t.Error("operator placed .bridge-allow-empty but the deletion pass still spared rows — sentinel escape hatch regressed")
	}
}

// TestScanSubtreeHardErrorOnDroppedMount pins FUSE drop mode (c)
// sub-case 1: when the subtree AND its owning root are both
// `fs.ErrNotExist`, ScanSubtree must return a hard error and the
// bounded deletion pass MUST be skipped.
func TestScanSubtreeHardErrorOnDroppedMount(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Artist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "song.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Capture the subtree's absolute path BEFORE we remove the root, so
	// ScanSubtree can attempt to resolve it via filepath.Abs (which
	// only manipulates the path string, no I/O).
	absSub := sub

	// Take both subtree AND root offline (mount fully removed).
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.ScanSubtree(context.Background(), absSub); err == nil {
		t.Error("ScanSubtree returned nil despite owning root being gone — FUSE drop mode (c) hard-stop regressed")
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got == nil {
		t.Error("Artist/song.flac was wiped despite ScanSubtree on a dropped mount — bounded deletion pass should have been skipped")
	}
}

// TestScanSubtreeHardErrorOnCleanUnmountLoophole pins FUSE drop mode
// (c) sub-case 2: the clean-unmount loophole. The owning root exists
// AND is empty (host directory still bound after a clean unmount),
// no `.bridge-allow-empty` sentinel, DB has rows — ScanSubtree must
// audit, detect the loophole, and refuse to delete.
func TestScanSubtreeHardErrorOnCleanUnmountLoophole(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Artist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "song.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	absSub := sub
	// Empty the root contents but keep the root host dir present
	// (the loophole: os.Stat(root) still succeeds).
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	_, err = sc.ScanSubtree(context.Background(), absSub)
	if err == nil {
		t.Error("ScanSubtree returned nil on a clean-unmount loophole — Mode (c) audit regressed")
	} else if !strings.Contains(err.Error(), "suspected mount drop") && !strings.Contains(err.Error(), "audit owning root") {
		t.Errorf("ScanSubtree returned unexpected error shape %q — should be the audit failure", err.Error())
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got == nil {
		t.Error("Artist/song.flac was wiped on a clean-unmount loophole — bounded deletion pass should have been skipped")
	}
}

// TestScanSubtreeProceedsOnLegitimateFolderDelete pins the inverse
// of mode (c): when only the target subtree is gone but the owning
// root has other content (clear sign of a deliberate folder
// deletion, not a mount drop), ScanSubtree returns nil and the
// deletion pass reaps the rows for the missing subtree.
func TestScanSubtreeProceedsOnLegitimateFolderDelete(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Artist")
	sibling := filepath.Join(root, "Sibling")
	for _, d := range []string{sub, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sub, "song.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "other.flac"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	absSub := sub
	// Delete only the target subtree — root still has the sibling, so
	// the audit verifies "root is alive and non-empty" and trusts the
	// deletion.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.ScanSubtree(context.Background(), absSub); err != nil {
		t.Fatalf("ScanSubtree on legitimate folder delete: %v (should return nil)", err)
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/song.flac"); got != nil {
		t.Error("Artist/song.flac survived a legitimate folder delete — Mode (c) audit is over-suppressing")
	}
	if got, _ := s.GetTrack(context.Background(), "Sibling/other.flac"); got == nil {
		t.Error("Sibling/other.flac was wiped despite being untouched on disk — scope bleed regressed")
	}
}

// TestAuditOwningRootOnSubtreeMissReturnTypes pins the decision
// matrix of the helper directly — cheaper than exercising every
// case through ScanSubtree.
func TestAuditOwningRootOnSubtreeMissReturnTypes(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// (a) Root missing → error.
	t.Run("missing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "does-not-exist")
		if err := auditOwningRootOnSubtreeMiss(context.Background(), s, root, false); err == nil {
			t.Error("expected error for missing root, got nil")
		}
	})

	// (b) Root exists and is non-empty → nil (legitimate delete).
	t.Run("root with siblings", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "other.flac"), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := auditOwningRootOnSubtreeMiss(context.Background(), s, root, false); err != nil {
			t.Errorf("non-empty root should pass audit, got %v", err)
		}
	})

	// (c) Empty root, sentinel present → nil (operator-authorised).
	t.Run("empty root with allow-empty sentinel", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, allowEmptySentinelFilename), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		// Note: the sentinel file itself counts as an entry under
		// os.ReadDir, so the audit returns nil via the
		// "has entries" branch even before checking the sentinel.
		// Both outcomes are correct (pass-through).
		if err := auditOwningRootOnSubtreeMiss(context.Background(), s, root, false); err != nil {
			t.Errorf("sentinel-bearing root should pass audit, got %v", err)
		}
	})

	// (d) Empty root, no sentinel, DB empty → nil (fresh install).
	t.Run("empty root no history", func(t *testing.T) {
		root := t.TempDir()
		if err := auditOwningRootOnSubtreeMiss(context.Background(), s, root, false); err != nil {
			t.Errorf("empty root with no DB history should pass audit, got %v", err)
		}
	})
}

// TestCountTracksUnderRoot pins the storage-form coverage that PR 1
// relies on for the FUSE drop guards. Two distinct path layouts in
// the DB; a query that uses the wrong form returns 0 and silently
// bypasses the protective gate. Lock the behaviour in both forms
// AND the cross-form-miss case.
func TestCountTracksUnderRoot(t *testing.T) {
	t.Run("single-root counts whole table", func(t *testing.T) {
		s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// Seed three tracks with single-root path shapes (no prefix).
		for _, p := range []string{"Artist/Album/01.flac", "Artist/Album/02.flac", "Other/song.flac"} {
			if err := s.UpsertTrack(context.Background(), &Track{Path: p}); err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.CountTracksUnderRoot(context.Background(), "/tmp/anywhere", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("single-root count = %d, want 3", n)
		}
	})

	t.Run("multi-root counts prefix-scoped subset", func(t *testing.T) {
		s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// Seed multi-root tracks: rows under "music/" and "archive/".
		for _, p := range []string{"music/Artist/01.flac", "music/Artist/02.flac", "archive/2024/old.flac"} {
			if err := s.UpsertTrack(context.Background(), &Track{Path: p}); err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.CountTracksUnderRoot(context.Background(), "/some/parent/music", true)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("multi-root music count = %d, want 2", n)
		}
		n, err = s.CountTracksUnderRoot(context.Background(), "/some/parent/archive", true)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("multi-root archive count = %d, want 1", n)
		}
	})

	t.Run("cross-form miss returns zero", func(t *testing.T) {
		// Storage is single-root shape (no prefix) but caller asks
		// in multi-root form — the LIKE pattern won't match. Pin the
		// zero-return so a refactor that drops `multiRoot` from the
		// helper doesn't silently disable the gate everywhere.
		s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.UpsertTrack(context.Background(), &Track{Path: "Artist/Album/01.flac"}); err != nil {
			t.Fatal(err)
		}
		// Multi-root query against single-root storage form: 0.
		n, err := s.CountTracksUnderRoot(context.Background(), "/some/parent/music", true)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("cross-form miss should return 0, got %d", n)
		}
	})
}

// TestHasAllowEmptySentinelOnlyTrustsExplicitFile pins that any
// os.Stat error (including permission errors masquerading as
// "missing") returns false. Only an explicit, successfully-stat'd
// file authorises the bypass.
func TestHasAllowEmptySentinelOnlyTrustsExplicitFile(t *testing.T) {
	root := t.TempDir()
	if hasAllowEmptySentinel(root) {
		t.Error("absent sentinel returned true")
	}
	if err := os.WriteFile(filepath.Join(root, allowEmptySentinelFilename), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasAllowEmptySentinel(root) {
		t.Error("present sentinel returned false")
	}
	// Nonexistent dir → false (os.Stat returns ErrNotExist).
	if hasAllowEmptySentinel(filepath.Join(root, "absent")) {
		t.Error("nonexistent directory returned true")
	}
}
