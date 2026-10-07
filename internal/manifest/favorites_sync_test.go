package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func setClock(s *Store, n int64) {
	s.now = func() time.Time { return time.Unix(0, n) }
}

func collectFavoriteTombstones(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.collectFavoriteTombstonesLocked(ctx); err != nil {
		t.Fatal(err)
	}
}

func revPtr(n int64) *int64 { return &n }

func trackPaths(doc FavoritesDocument) []string {
	out := make([]string, 0, len(doc.Tracks))
	for _, t := range doc.Tracks {
		out = append(out, t.Path)
	}
	return out
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func tombPaths(doc FavoritesDocument) []string {
	out := make([]string, 0, len(doc.Tombstones))
	for _, t := range doc.Tombstones {
		out = append(out, t.Path)
	}
	return out
}

// Two clients both based on {H1}. The phone replaces it with {H2}. The
// iPad, still holding the first revision, must not wipe H2 by writing
// {H1, H3}. Removing the baseRevision check lets that second save land.
func TestTwoClientsCannotClobberFavorites(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, 1_000)

	first, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if first.Revision != 1 {
		t.Fatalf("seed revision %d, want 1", first.Revision)
	}

	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(first.Revision),
		Tracks:       []FavoriteTrackRow{{Path: "H2.flac", FavoritedAt: 2}},
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac"}},
	}); err != nil {
		t.Fatalf("phone save: %v", err)
	}

	_, err = s.SaveFavorites(ctx, "ipad", FavoritesSave{
		BaseRevision: revPtr(first.Revision),
		Tracks: []FavoriteTrackRow{
			{Path: "H1.flac", FavoritedAt: 1},
			{Path: "H3.flac", FavoritedAt: 3},
		},
	})
	if !errors.Is(err, ErrFavoritesStale) {
		t.Fatalf("iPad save: %v, want ErrFavoritesStale", err)
	}

	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths := trackPaths(doc)
	if !hasPath(paths, "H2.flac") || hasPath(paths, "H3.flac") || hasPath(paths, "H1.flac") {
		t.Fatalf("live paths %v, want only H2", paths)
	}
	if !hasPath(tombPaths(doc), "H1.flac") {
		t.Fatalf("H1 must stay tombstoned, tombs %v", tombPaths(doc))
	}
	if doc.Revision != 2 {
		t.Fatalf("revision %d, want 2 (the conflict wrote nothing)", doc.Revision)
	}
}

// A PUT with no baseRevision is additions only. It never 409s, never
// drops a key it omitted, and never revives a tombstone.
func TestLegacyFavoritesPutNeverConflicts(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, 2_000)

	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", Title: "One", FavoritedAt: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(1),
		Tracks:       nil,
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac"}},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.SaveFavorites(ctx, "legacy", FavoritesSave{
		Tracks: []FavoriteTrackRow{
			{Path: "H1.flac", Title: "revived", FavoritedAt: 99},
			{Path: "H2.flac", Title: "Two", FavoritedAt: 20},
		},
	})
	if err != nil {
		t.Fatalf("legacy put: %v, want 200-equivalent accept", err)
	}
	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasPath(trackPaths(doc), "H1.flac") || !hasPath(trackPaths(doc), "H2.flac") {
		t.Fatalf("legacy must add H2 and leave H1 tombstoned, live %v tombs %v", trackPaths(doc), tombPaths(doc))
	}
	if res.Revision != doc.Revision || doc.Revision < 2 {
		t.Fatalf("revision result %d doc %d", res.Revision, doc.Revision)
	}

	again, err := s.SaveFavorites(ctx, "legacy", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H2.flac", Title: "Two", FavoritedAt: 20}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != res.Revision {
		t.Fatalf("identical legacy retry moved revision %d → %d", res.Revision, again.Revision)
	}
}

func TestFavoriteTombstoneCollectionAtTheRetentionBoundary(t *testing.T) {
	const day = int64(24 * time.Hour)
	removedAt := int64(1_000_000)
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, removedAt)

	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	setClock(s, removedAt)
	saved, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(1),
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	setClock(s, removedAt+90*day-1)
	collectFavoriteTombstones(t, s, ctx)
	held, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPath(tombPaths(held), "H1.flac") || held.Revision != saved.Revision {
		t.Fatalf("one nanosecond short of 90 days must keep the tombstone: rev %d tombs %v", held.Revision, tombPaths(held))
	}

	setClock(s, removedAt+90*day)
	collectFavoriteTombstones(t, s, ctx)
	collected, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collected.Tombstones) != 0 || collected.Revision != saved.Revision+1 {
		t.Fatalf("exactly 90 days collects and bumps once: rev %d tombs %v", collected.Revision, tombPaths(collected))
	}

	resent, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(collected.Revision),
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac", RemovedAt: removedAt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resent.Revision != collected.Revision {
		t.Fatalf("resending a collected tombstone bumped %d → %d", collected.Revision, resent.Revision)
	}
	after, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tombstones) != 0 {
		t.Fatalf("collected tombstone came back: %v", tombPaths(after))
	}
}

func TestARecentLegacyWriterBlocksTombstoneCollection(t *testing.T) {
	const day = int64(24 * time.Hour)
	removedAt := int64(5_000_000)
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, removedAt)
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(1),
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac"}},
	}); err != nil {
		t.Fatal(err)
	}
	// legacy_put_at == removedAt does not block (the predicate is strict >).
	// A legacy save one nanosecond later does, for every tombstone.
	setClock(s, removedAt+1)
	if _, err := s.SaveFavorites(ctx, "old-phone", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H2.flac", FavoritedAt: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	setClock(s, removedAt+90*day)
	collectFavoriteTombstones(t, s, ctx)
	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPath(tombPaths(doc), "H1.flac") {
		t.Fatal("a legacy writer seen after the cutoff must block collection")
	}
}

func TestLegacySaveReaddsACollectedFavorite(t *testing.T) {
	const day = int64(24 * time.Hour)
	removedAt := int64(8_000_000)
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, removedAt)
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", Title: "One", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(1),
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac"}},
	}); err != nil {
		t.Fatal(err)
	}
	setClock(s, removedAt+90*day)
	collectFavoriteTombstones(t, s, ctx)
	if _, err := s.SaveFavorites(ctx, "old-phone", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H1.flac", Title: "One", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPath(trackPaths(doc), "H1.flac") || len(doc.Tombstones) != 0 {
		t.Fatalf("a 2.1 save re-adds a collected key: live %v tombs %v", trackPaths(doc), tombPaths(doc))
	}
}

func TestEqualFavoritesBodyDoesNotBumpTheRevision(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	setClock(s, 3_000)
	tracks := []FavoriteTrackRow{{Path: "H1.flac", Title: "One", Artist: "A", FavoritedAt: 10}}
	first, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       tracks,
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		BaseRevision: revPtr(99),
		Tracks:       tracks,
	})
	if err != nil {
		t.Fatalf("equal body with a stale base: %v", err)
	}
	if again.Revision != first.Revision {
		t.Fatalf("equal body bumped %d → %d", first.Revision, again.Revision)
	}
}

func TestReadFavoritesDoesNotTakeTheWriterLock(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if _, err := s.SaveFavorites(ctx, "phone", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := s.ReadFavorites(readCtx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("ReadFavorites blocked while s.mu was held")
	}
}

func TestReapingADeviceDropsItsSyncRow(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "dev-gone", "tok-gone", "Gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeviceRegistration(ctx, "dev-kept", "tok-kept", "Kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "dev-gone", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "dev-kept", FavoritesSave{
		BaseRevision: revPtr(1),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReapOrphanDeviceRegistrations(ctx, []string{"tok-kept"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d registrations, want 1", n)
	}
	devices, err := s.ListFavoriteSyncDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("sync devices %d, want the live token's row only", len(devices))
	}
}

func syncDeviceCount(t *testing.T, s *Store, token string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorite_sync_devices WHERE device_token = ?`, token).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnUnregisteredSyncRowIsReaped(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	now := time.Now()
	setClock(s, now.UnixNano())
	if err := s.UpsertDeviceRegistration(ctx, "dev-kept", "tok-kept", "Kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "dev-kept", FavoritesSave{
		Tracks: []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO favorite_sync_devices (device_token, last_seen_at) VALUES ('never-registered', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReapOrphanDeviceRegistrations(ctx, []string{"tok-kept"}); err != nil {
		t.Fatal(err)
	}
	if syncDeviceCount(t, s, "never-registered") != 0 || syncDeviceCount(t, s, "dev-kept") != 1 {
		t.Fatalf("orphan reap left unregistered=%d kept=%d", syncDeviceCount(t, s, "never-registered"), syncDeviceCount(t, s, "dev-kept"))
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO favorite_sync_devices (device_token, last_seen_at) VALUES ('never-registered', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReapStaleDeviceRegistrations(ctx, now.Add(-time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if syncDeviceCount(t, s, "never-registered") != 0 || syncDeviceCount(t, s, "dev-kept") != 1 {
		t.Fatalf("stale reap left unregistered=%d kept=%d", syncDeviceCount(t, s, "never-registered"), syncDeviceCount(t, s, "dev-kept"))
	}
}

func TestReadFavoritesDoesNotCollectTombstones(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	const removedAt = int64(1_000_000)
	day := 24 * time.Hour
	setClock(s, removedAt)
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: revPtr(0),
		Tracks:       []FavoriteTrackRow{{Path: "H1.flac", FavoritedAt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: revPtr(1),
		Tombstones:   []FavoriteTombstone{{Path: "H1.flac", RemovedAt: removedAt}},
	}); err != nil {
		t.Fatal(err)
	}
	setClock(s, removedAt+int64(90*day))
	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Tombstones) != 1 || doc.Tombstones[0].Path != "H1.flac" {
		t.Fatalf("read collected %+v", doc.Tombstones)
	}
}

func TestCapFavoriteRowsKeepsExistingKeysFirst(t *testing.T) {
	existing := []FavoriteTrackRow{{Path: "a", FavoritedAt: 1}, {Path: "b", FavoritedAt: 1}}
	incoming := []FavoriteTrackRow{{Path: "c", FavoritedAt: 1}, {Path: "d", FavoritedAt: 1}}
	merged := append(append([]FavoriteTrackRow{}, existing...), incoming...)
	got := capFavoriteRows(existing, incoming, merged, 2, trackKeyOfRow)
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "b" {
		t.Fatalf("tracks %+v", got)
	}
	albums := capFavoriteRows(
		[]FavoriteAlbumRow{{Album: "A", FavoritedAt: 1}, {Album: "B", FavoritedAt: 1}},
		[]FavoriteAlbumRow{{Album: "C", FavoritedAt: 1}},
		[]FavoriteAlbumRow{{Album: "A", FavoritedAt: 1}, {Album: "B", FavoritedAt: 1}, {Album: "C", FavoritedAt: 1}},
		2, albumKeyOfRow)
	if len(albums) != 2 || albums[0].Album != "A" || albums[1].Album != "B" {
		t.Fatalf("albums %+v", albums)
	}
}

func TestLegacyFavoritesMergeStopsAtTheCap(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	first := make([]FavoriteTrackRow, 30000)
	for i := range first {
		first[i] = FavoriteTrackRow{Path: fmt.Sprintf("e-%05d", i), FavoritedAt: 1}
	}
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{Tracks: first}); err != nil {
		t.Fatal(err)
	}
	second := make([]FavoriteTrackRow, 30000)
	for i := range second {
		second[i] = FavoriteTrackRow{Path: fmt.Sprintf("n-%05d", i), FavoritedAt: 2}
	}
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{Tracks: second}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Tracks) != 50000 {
		t.Fatalf("stored %d tracks, want 50000", len(doc.Tracks))
	}
	have := map[string]bool{}
	for _, row := range doc.Tracks {
		have[row.Path] = true
	}
	for i := 0; i < 30000; i++ {
		if !have[fmt.Sprintf("e-%05d", i)] {
			t.Fatalf("existing e-%05d dropped", i)
		}
	}
	for i := 0; i < 20000; i++ {
		if !have[fmt.Sprintf("n-%05d", i)] {
			t.Fatalf("incoming n-%05d dropped", i)
		}
	}
	if have["n-20000"] {
		t.Fatal("n-20000 stored past the cap")
	}
}
