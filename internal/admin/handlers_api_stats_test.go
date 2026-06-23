package admin

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestGetStatsSnapshotServesLastGoodOnDBError pins the degrade-to-last-good
// contract: once a successful snapshot has cached the DB-derived numbers, a
// subsequent DB read failure (here the store is closed under it, the same
// err branch a snapshotDBTimeout would take) must reuse those numbers
// rather than zeroing the dashboard tiles. Without the cache the SSE tick
// would flash "0 tracks / 0 variants" during scan-time lock contention —
// exactly when the timeout is most likely to fire.
func TestGetStatsSnapshotServesLastGoodOnDBError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, p := range []string{"Music/A/1.flac", "Music/A/2.flac", "Music/B/3.flac"} {
		if err := srv.deps.Manifest.UpsertTrack(context.Background(),
			&manifest.Track{Path: p, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
	}

	first := srv.getStatsSnapshot()
	if first.TracksIndexed != 3 {
		t.Fatalf("first snapshot TracksIndexed = %d, want 3", first.TracksIndexed)
	}
	if !srv.statsDBValid {
		t.Fatal("statsDBValid = false after a successful snapshot, want true")
	}

	// Force every subsequent DB read to error.
	if err := srv.deps.Manifest.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	second := srv.getStatsSnapshot()
	if second.TracksIndexed != 3 {
		t.Errorf("after DB error TracksIndexed = %d, want 3 (last-good, not zeroed)", second.TracksIndexed)
	}
	// Cheap non-DB fields must still populate from config/runtime.
	if second.LibraryName != first.LibraryName || second.Fingerprint != first.Fingerprint {
		t.Errorf("non-DB fields changed on degrade: name %q/%q fp %q/%q",
			first.LibraryName, second.LibraryName, first.Fingerprint, second.Fingerprint)
	}
}

// TestGetStatsSnapshotZeroWithoutCacheOnDBError covers the first-paint
// edge: a DB error before ANY successful snapshot has no last-good to
// serve, so it degrades to zero gracefully (no panic, no stale garbage)
// and leaves statsDBValid false so a later good read still populates the
// cache. Proves the fallback can't surface an uninitialised statsDBPart.
func TestGetStatsSnapshotZeroWithoutCacheOnDBError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if err := srv.deps.Manifest.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	snap := srv.getStatsSnapshot()
	if snap.TracksIndexed != 0 {
		t.Errorf("no-cache DB error TracksIndexed = %d, want 0", snap.TracksIndexed)
	}
	if srv.statsDBValid {
		t.Error("statsDBValid = true with no successful read, want false")
	}
	// Sanity: the cheap fields still render so the tile isn't blank.
	if snap.LibraryName == "" {
		t.Error("LibraryName empty on degrade; non-DB fields should still populate")
	}
}
