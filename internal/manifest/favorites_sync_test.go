package manifest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func setClock(s *Store, n int64) {
	s.now = func() time.Time { return time.Unix(0, n) }
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
	held, err := s.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPath(tombPaths(held), "H1.flac") || held.Revision != saved.Revision {
		t.Fatalf("one nanosecond short of 90 days must keep the tombstone: rev %d tombs %v", held.Revision, tombPaths(held))
	}

	setClock(s, removedAt+90*day)
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
	if _, err := s.ReadFavorites(ctx); err != nil {
		t.Fatal(err)
	}
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
