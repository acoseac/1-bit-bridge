package manifest

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// stamped is one row TrackPathsIndexedAfter handed over.
type stamped struct {
	path string
	at   int64
}

// indexedAfter is every row TrackPathsIndexedAfter hands over past after.
func indexedAfter(t *testing.T, s *Store, after int64) []stamped {
	t.Helper()
	var out []stamped
	if err := s.TrackPathsIndexedAfter(context.Background(), after, func(p string, at int64) error {
		out = append(out, stamped{p, at})
		return nil
	}); err != nil {
		t.Fatalf("TrackPathsIndexedAfter(%d): %v", after, err)
	}
	return out
}

// TestTrackPathsIndexedAfterIsEveryRowPastAStamp pins the player's delta read
// (B470 PR 5a4): from below every stamp it is every row; past the highest
// stamp it is none; a row inserted after that, and one rewritten, are the rows
// past it, in stamp order, each stamped past it; the rows of one batch share a
// stamp; a deleted row is in no read again; and fn's error, and a cancelled
// context, end the read with that error.
func TestTrackPathsIndexedAfterIsEveryRowPastAStamp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertTrackBatch(ctx, []*Track{
		{Path: "a.flac", Size: 1, ModTime: time.Unix(1, 0)},
		{Path: "b.flac", Size: 1, ModTime: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	all := indexedAfter(t, s, math.MinInt64)
	if len(all) != 2 || all[0].at != all[1].at || all[0].at <= 0 {
		t.Fatalf("every row: %+v, want a and b at one stamp", all)
	}
	high := all[1].at
	if past := indexedAfter(t, s, high); len(past) != 0 {
		t.Fatalf("past the highest stamp: %+v, want none", past)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "c.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "a.flac", Size: 2, ModTime: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	past := indexedAfter(t, s, high)
	if len(past) != 2 || past[0].path != "c.flac" || past[1].path != "a.flac" || past[0].at <= high || past[1].at <= past[0].at {
		t.Fatalf("past %d after an insert and a rewrite: %+v, want c then a, each past it", high, past)
	}
	if err := s.DeleteTrack(ctx, "b.flac"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range indexedAfter(t, s, math.MinInt64) {
		paths = append(paths, r.path)
	}
	if slices.Contains(paths, "b.flac") || len(paths) != 2 {
		t.Fatalf("after b's deletion every row is %v, want c and a", paths)
	}
	stop := errors.New("stop")
	calls := 0
	if err := s.TrackPathsIndexedAfter(ctx, math.MinInt64, func(string, int64) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("fn's error: %v after %d calls, want it returned after one", err, calls)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.TrackPathsIndexedAfter(cancelled, math.MinInt64, func(string, int64) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v, want context.Canceled", err)
	}
}

// TestTrackPathsIndexedAfterReadsTheStampsIndex pins the read's plan: a
// search of idx_tracks_indexed, in its order, never a scan of the table and a
// sort, so a read past the highest stamp costs a seek whatever the library's
// size.
func TestTrackPathsIndexedAfterReadsTheStampsIndex(t *testing.T) {
	s := openTestStore(t)
	plan := explainPlan(t, s, trackPathsIndexedAfterSQL, int64(0))
	if !strings.Contains(plan, "USING INDEX idx_tracks_indexed (indexed_at>?)") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("plan %q, want a search of idx_tracks_indexed with no sort", plan)
	}
}
