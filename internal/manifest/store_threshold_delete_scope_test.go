package manifest

import (
	"context"
	"testing"
	"time"
)

// seedMissingCount drives a row's missing_count up to `n` by naming it
// in that many consecutive increment passes, using a threshold high
// enough that none of them reaps it.
func seedMissingCount(t *testing.T, s *Store, path string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(
			ctx, []string{path}, n+10); err != nil {
			t.Fatalf("seed increment %d: %v", i, err)
		}
	}
}

// The threshold DELETE must only reap rows the caller named as missing
// THIS pass.
//
// Unscoped, a bare `missing_count >= ?` reaps any row already sitting at
// the threshold even on a pass that never looked at it — and a pass that
// never looked at a path is exactly the pass where its subtree errored
// and the scanner deliberately withheld it. The route in is lowering
// DeleteAfterMissingScans: rows parked below the old threshold are
// instantly at-or-above the new one, and the next scan sweeps them
// whether or not it could see them.
func TestThresholdDeleteOnlyReapsPathsNamedThisPass(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const (
		observed = "Music/Artist/Album/observed.flac"
		withheld = "Music/Artist/Album/withheld.flac"
	)
	for _, p := range []string{observed, withheld} {
		if err := s.UpsertTrack(ctx, &Track{
			Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	// Both rows reach a count of 2 over earlier scans.
	seedMissingCount(t, s, observed, 2)
	seedMissingCount(t, s, withheld, 2)

	// The operator lowers DeleteAfterMissingScans to 2, and this scan
	// sees only `observed` missing — `withheld` sat under a subtree
	// whose walk errored, so the scanner kept it out of the list.
	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{observed}, 2)
	if err != nil {
		t.Fatalf("IncrementMissingTracksAndDeleteAtThreshold: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only the observed row)", deleted)
	}

	if got, _ := s.GetTrack(ctx, observed); got != nil {
		t.Error("the observed-missing row should have been reaped at the threshold")
	}
	if got, _ := s.GetTrack(ctx, withheld); got == nil {
		t.Fatal("a row the scanner deliberately withheld from this pass was " +
			"reaped anyway — the errored-subtree guard has no effect on an " +
			"unscoped `missing_count >= ?` DELETE, and lowering " +
			"DeleteAfterMissingScans is enough to reach it")
	}
}

// Scoping must not strand anything: a genuinely-absent row is in the
// next pass's list, gets incremented, and is reaped in that pass — at
// most one scan later, and only ever after a pass that observed it.
func TestThresholdDeleteStillReapsOnTheNextObservingPass(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const p = "Music/Artist/Album/gone.flac"
	if err := s.UpsertTrack(ctx, &Track{
		Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	seedMissingCount(t, s, p, 2)

	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{p}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if got, _ := s.GetTrack(ctx, p); got != nil {
		t.Error("a row observed missing at the threshold must still be reaped")
	}
}

// The folders twin had NO exclusion at all — not even the routed
// anti-join its sibling carries — so it was the weaker of the two.
func TestFolderThresholdDeleteOnlyReapsPathsNamedThisPass(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const (
		observed = "Music/Artist/Observed"
		withheld = "Music/Artist/Withheld"
	)
	for _, p := range []string{observed, withheld} {
		if err := s.UpsertFolder(ctx, &Folder{Path: p, ModTime: time.Unix(0, 0).UTC()}); err != nil {
			t.Fatalf("seed folder %q: %v", p, err)
		}
	}
	for i := 0; i < 2; i++ {
		for _, p := range []string{observed, withheld} {
			if _, err := s.IncrementMissingFoldersAndDeleteAtThreshold(
				ctx, []string{p}, 12); err != nil {
				t.Fatalf("seed folder increment: %v", err)
			}
		}
	}

	deleted, err := s.IncrementMissingFoldersAndDeleteAtThreshold(ctx, []string{observed}, 2)
	if err != nil {
		t.Fatalf("IncrementMissingFoldersAndDeleteAtThreshold: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only the observed folder)", deleted)
	}

	folders, err := s.FolderPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range folders {
		have[f] = true
	}
	if have[observed] {
		t.Error("the observed-missing folder should have been reaped")
	}
	if !have[withheld] {
		t.Error("a folder the scanner deliberately withheld from this pass was " +
			"reaped anyway — the folders DELETE carried no exclusion of any kind")
	}
}

// The routed anti-join must survive the rescope. It is layer 2 of the
// PR #370 two-layer guard, and a routed row must never be
// threshold-deleted by ANY caller regardless of accumulated counters —
// its lifecycle belongs to the ingest's last_seen_at reap.
//
// Named explicitly in the missing list here, which is the case the
// scanner-side layer normally prevents: this asserts layer 2 alone
// still holds.
func TestThresholdDeleteStillSparesRoutedRowsWhenScoped(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const routed = "Chord 2Go/Music/ABBA/Gold/01.flac"
	if err := s.UpsertTrack(ctx, &Track{
		Path: routed, Size: 1, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: routed,
		ServerUDN:  "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
		ObjectID:   "64$0$0$0",
		ResURL:     "http://192.168.0.62:8200/MediaItems/1.flac",
		LastSeenAt: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	seedMissingCount(t, s, routed, 5)

	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{routed}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 — a routed row must never be "+
			"threshold-deleted", deleted)
	}
	if got, _ := s.GetTrack(ctx, routed); got == nil {
		t.Fatal("a UPnP-routed row was threshold-deleted; its lifecycle belongs " +
			"solely to the ingest's last_seen_at reap")
	}
}
