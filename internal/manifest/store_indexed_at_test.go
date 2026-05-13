package manifest

import (
	"context"
	"testing"
	"time"
)

// TestUpsertTrackEqualClockStillAdvances: when the injected clock returns
// EXACTLY the existing row's indexed_at (rapid back-to-back UpsertTrack
// at the same nanosecond — fake clocks, low-resolution wall clocks, an
// mtime-changed-but-clock-stable scan tick), the CASE WHEN form must
// produce a strict advance. Without it, a client that synced at the
// equal timestamp would miss the second mutation under
// `WHERE indexed_at > since`. Mirrors TestUpsertVariantEqualClockStillAdvances
// for the track-write path.
func TestUpsertTrackEqualClockStillAdvances(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var initialIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&initialIndexedAt); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}

	// Pin the clock to EXACTLY the existing indexed_at — equality case.
	s.now = func() time.Time { return time.Unix(0, initialIndexedAt) }

	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    "Music/A/1.flac",
		Size:    200, // a real change so the upsert isn't a no-op
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read indexed_at after UpsertTrack: %v", err)
	}
	if afterIndexedAt != initialIndexedAt+1 {
		t.Errorf("equal-clock should produce existing+1 strict bump: expected %d, got %d (initial=%d)",
			initialIndexedAt+1, afterIndexedAt, initialIndexedAt)
	}
}

// TestUpsertTrackMonotonicGuard: an injected clock that returns a
// timestamp in the PAST must NOT regress indexed_at. The CASE WHEN
// form takes existing+1 in that case (past-clock branch lands in the
// `tracks.indexed_at >= excluded.indexed_at` arm). Mirrors
// TestUpsertVariantMonotonicGuard for the track-write path.
func TestUpsertTrackMonotonicGuard(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var initialIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&initialIndexedAt); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}

	pastTimestamp := initialIndexedAt - (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, pastTimestamp) }

	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    "Music/A/1.flac",
		Size:    300,
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read indexed_at after UpsertTrack: %v", err)
	}
	if afterIndexedAt < initialIndexedAt {
		t.Errorf("indexed_at regressed under past-clock injection: before=%d after=%d (clock returned %d)",
			initialIndexedAt, afterIndexedAt, pastTimestamp)
	}
	if afterIndexedAt != initialIndexedAt+1 {
		t.Errorf("past-clock should produce existing+1 strict bump: expected %d, got %d",
			initialIndexedAt+1, afterIndexedAt)
	}
}

// TestUpsertTrackBatchEqualClockEachRowAdvances: when every row in a
// batch flush is already at the injected clock's `now`, each one must
// independently advance to `now+1`. The shared batch-level `now` is
// bound once per row's `excluded.indexed_at`; the CASE WHEN evaluates
// per-row against ITS own existing `tracks.indexed_at`. A regression
// that switched to a single-row check (e.g. a global comparison) would
// flatten the post-batch state and reveal here.
func TestUpsertTrackBatchEqualClockEachRowAdvances(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// Seed three rows via UpsertTrack at a known clock value.
	const t0 int64 = 1_000_000_000
	s.now = func() time.Time { return time.Unix(0, t0) }
	paths := []string{"Music/A/1.flac", "Music/A/2.flac", "Music/A/3.flac"}
	for _, p := range paths {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path: p, Size: 100, ModTime: time.Unix(0, t0),
		}); err != nil {
			t.Fatalf("seed UpsertTrack(%q): %v", p, err)
		}
	}

	// Verify seed state — every row at t0.
	for _, p := range paths {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, p,
		).Scan(&got); err != nil {
			t.Fatalf("read seed indexed_at(%q): %v", p, err)
		}
		if got != t0 {
			t.Fatalf("seed indexed_at(%q) = %d, want %d", p, got, t0)
		}
	}

	// Pin the clock to t0 again — the batch's shared `now` will equal
	// every row's existing indexed_at, exercising the CASE WHEN per-row.
	batch := make([]*Track, len(paths))
	for i, p := range paths {
		batch[i] = &Track{Path: p, Size: 200, ModTime: time.Unix(0, t0)}
	}
	if err := s.UpsertTrackBatch(context.Background(), batch); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}

	// Each row should have strictly advanced to t0+1.
	for _, p := range paths {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, p,
		).Scan(&got); err != nil {
			t.Fatalf("read post-batch indexed_at(%q): %v", p, err)
		}
		if got != t0+1 {
			t.Errorf("post-batch indexed_at(%q) = %d, want %d (t0=%d)", p, got, t0+1, t0)
		}
	}
}

// TestUpsertTrackBatchFreshInsertUsesClock: a batch flush that inserts
// NEW rows (no conflict) must stamp indexed_at = `now`, not `now+1`.
// The CASE WHEN only fires on the ON CONFLICT branch; brand-new rows
// take the unconditional VALUES path. Locks the no-conflict contract.
func TestUpsertTrackBatchFreshInsertUsesClock(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	const t0 int64 = 2_000_000_000
	s.now = func() time.Time { return time.Unix(0, t0) }
	batch := []*Track{
		{Path: "Music/B/1.flac", Size: 100, ModTime: time.Unix(0, t0)},
		{Path: "Music/B/2.flac", Size: 100, ModTime: time.Unix(0, t0)},
	}
	if err := s.UpsertTrackBatch(context.Background(), batch); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}
	for _, b := range batch {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, b.Path,
		).Scan(&got); err != nil {
			t.Fatalf("read indexed_at(%q): %v", b.Path, err)
		}
		if got != t0 {
			t.Errorf("fresh-insert indexed_at(%q) = %d, want %d", b.Path, got, t0)
		}
	}
}
