package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// syncNotes records the hooks the store calls after a commit.
type syncNotes struct {
	fav []int64
	pl  []string
	lib []int64
}

func (n *syncNotes) hooks() SyncHooks {
	return SyncHooks{
		Favorites: func(rev int64) { n.fav = append(n.fav, rev) },
		Playlists: func(epoch string) { n.pl = append(n.pl, epoch) },
		Library:   func(ns int64) { n.lib = append(n.lib, ns) },
	}
}

func openNotifyingStore(t *testing.T) (*Store, string, *syncNotes) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	notes := &syncNotes{}
	s.SetSyncHooks(notes.hooks())
	return s, path, notes
}

func rev(n int64) *int64 { return &n }

func TestFavoritesRevisionPutPublishesTheNewRevisionAfterCommit(t *testing.T) {
	s, path, _ := openNotifyingStore(t)
	ctx := context.Background()
	tracks, albums := sampleFavorites()
	var seen bool
	s.SetSyncHooks(SyncHooks{Favorites: func(rev int64) {
		seen = true
		other, err := OpenStore(path)
		if err != nil {
			t.Errorf("second open: %v", err)
			return
		}
		defer other.Close()
		doc, err := other.ReadFavorites(ctx)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		if doc.Revision != rev {
			t.Errorf("revision %d is not committed (reader sees %d)", rev, doc.Revision)
		}
	}})

	res, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks, Albums: albums,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seen || res.Revision != 1 {
		t.Fatalf("save revision %d, hook seen %v", res.Revision, seen)
	}
}

func TestAnIdenticalFavoritesPutPublishesNothing(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	tracks, albums := sampleFavorites()
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks, Albums: albums,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(1), Tracks: tracks, Albums: albums,
	}); err != nil {
		t.Fatal(err)
	}
	if len(notes.fav) != 1 || notes.fav[0] != 1 {
		t.Fatalf("notes %v, want [1]", notes.fav)
	}
}

func TestAStaleFavoritesPutPublishesNothing(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	tracks, _ := sampleFavorites()
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks[:1],
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks,
	})
	if !errors.Is(err, ErrFavoritesStale) {
		t.Fatalf("err %v, want ErrFavoritesStale", err)
	}
	if len(notes.fav) != 1 {
		t.Fatalf("stale put noted %v", notes.fav)
	}
}

func TestALegacyFavoritesPutPublishesWhenTheDocumentChanges(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	tracks, _ := sampleFavorites()
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks[:1],
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.SaveFavorites(ctx, "dev", FavoritesSave{Legacy: true, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 2 || len(notes.fav) != 2 || notes.fav[1] != 2 {
		t.Fatalf("revision %d notes %v", res.Revision, notes.fav)
	}
	if _, err := s.SaveFavorites(ctx, "dev", FavoritesSave{Legacy: true, Tracks: tracks}); err != nil {
		t.Fatal(err)
	}
	if len(notes.fav) != 2 {
		t.Fatalf("identical legacy put noted %v", notes.fav)
	}
}

func TestFavoriteTombstoneCollectionPublishesAfterItsCommit(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	tracks, _ := sampleFavorites()
	res, err := s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(0), Tracks: tracks[:1],
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err = s.SaveFavorites(ctx, "dev", FavoritesSave{
		BaseRevision: rev(res.Revision),
		Tombstones:   []FavoriteTombstone{{Path: tracks[0].Path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 2 {
		t.Fatalf("tombstone revision %d", res.Revision)
	}
	now = now.Add(91 * 24 * time.Hour)
	res, err = s.SaveFavorites(ctx, "dev", FavoritesSave{BaseRevision: rev(res.Revision)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 3 || len(notes.fav) != 3 || notes.fav[2] != 3 {
		t.Fatalf("after collection revision %d notes %v, want a third note of 3", res.Revision, notes.fav)
	}
}

func TestPlaylistPutDeleteRestoreAndCoverPublishAfterCommit(t *testing.T) {
	s, path, notes := openNotifyingStore(t)
	ctx := context.Background()
	items := []PlaylistItemRow{{Position: 0, Path: "a/b.flac", Title: "T", Artist: "A"}}
	row := PlaylistRow{ID: "pl1", Name: "Mix", LastModifiedAt: 100}
	var committed bool
	s.SetSyncHooks(SyncHooks{
		Playlists: func(epoch string) {
			notes.pl = append(notes.pl, epoch)
			other, err := OpenStore(path)
			if err != nil {
				t.Errorf("second open: %v", err)
				return
			}
			defer other.Close()
			got, _, err := other.GetPlaylist(ctx, "pl1")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			if got != nil {
				committed = true
			}
		},
	})
	if _, err := s.PutPlaylist(ctx, "dev", row, items, nil); err != nil {
		t.Fatal(err)
	}
	if len(notes.pl) != 1 || !committed {
		t.Fatalf("create notes %d committed %v", len(notes.pl), committed)
	}
	base := row.LastModifiedAt
	changed := row
	changed.Name = "Mix 2"
	changed.LastModifiedAt = 200
	if _, err := s.PutPlaylist(ctx, "dev", changed, items, &base); err != nil {
		t.Fatal(err)
	}
	if len(notes.pl) != 2 {
		t.Fatalf("changed body notes %d", len(notes.pl))
	}
	ok, err := s.TombstonePlaylist(ctx, "pl1", "dev")
	if err != nil || !ok {
		t.Fatalf("delete ok %v err %v", ok, err)
	}
	ok, err = s.RestorePlaylist(ctx, "pl1")
	if err != nil || !ok {
		t.Fatalf("restore ok %v err %v", ok, err)
	}
	if len(notes.pl) != 4 {
		t.Fatalf("after delete and restore notes %d, want 4", len(notes.pl))
	}
	cover := PlaylistCover{Scope: CoverScopePlaylist, Key: "pl1", ImageHash: "h1", Ext: "jpg", UpdatedAt: 1}
	if err := s.SetPlaylistCover(ctx, cover); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlaylistCover(ctx, cover); err != nil {
		t.Fatal(err)
	}
	cover.ImageHash = "h2"
	if err := s.SetPlaylistCover(ctx, cover); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.DeletePlaylistCover(ctx, CoverScopePlaylist, "pl1"); err != nil || !ok {
		t.Fatalf("delete cover ok %v err %v", ok, err)
	}
	if len(notes.pl) != 7 {
		t.Fatalf("cover notes %d, want 7 (set, change, delete)", len(notes.pl))
	}
}

func TestAnIdenticalPlaylistBodyAndAMismatchPublishNothing(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	items := []PlaylistItemRow{{Position: 0, Path: "a/b.flac", Title: "T", Artist: "A"}}
	row := PlaylistRow{ID: "pl1", Name: "Mix", LastModifiedAt: 100}
	if _, err := s.PutPlaylist(ctx, "dev", row, items, nil); err != nil {
		t.Fatal(err)
	}
	base := row.LastModifiedAt
	got, err := s.PutPlaylist(ctx, "dev", row, items, &base)
	if err != nil || !got.Unchanged {
		t.Fatalf("identical: %+v %v", got, err)
	}
	wrong := int64(1)
	if _, err := s.PutPlaylist(ctx, "dev", PlaylistRow{ID: "pl1", Name: "Other", LastModifiedAt: 300}, items, &wrong); err == nil {
		t.Fatal("mismatch was stored")
	}
	if len(notes.pl) != 1 {
		t.Fatalf("notes %d, want the create only", len(notes.pl))
	}
	ok, err := s.TombstonePlaylist(ctx, "missing", "dev")
	if err != nil || ok || len(notes.pl) != 1 {
		t.Fatalf("missing delete ok %v err %v notes %d", ok, err, len(notes.pl))
	}
}

func TestReplaceBackupEpochPublishesTheNewEpoch(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	before, err := s.BackupEpoch(ctx)
	if err != nil || before == "" {
		t.Fatal(err)
	}
	if _, err := s.ReplaceBackupEpochAndNotify(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.BackupEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after == before || len(notes.pl) != 1 || notes.pl[0] != after {
		t.Fatalf("before %s after %s notes %v", before, after, notes.pl)
	}
}

func TestAnIndexedAtBumpPublishesTheWatermarkAndAMissPublishesNothing(t *testing.T) {
	s, path, notes := openNotifyingStore(t)
	ctx := context.Background()
	var committed bool
	s.SetSyncHooks(SyncHooks{Library: func(ns int64) {
		notes.lib = append(notes.lib, ns)
		other, err := OpenStore(path)
		if err != nil {
			t.Errorf("second open: %v", err)
			return
		}
		defer other.Close()
		got, ok := other.LibraryWatermark(ctx)
		if !ok || got != ns {
			t.Errorf("watermark %d ok %v, hook was given %d", got, ok, ns)
			return
		}
		committed = true
	}})
	if err := s.UpsertTrack(ctx, &Track{Path: "Album/a.flac", Size: 10, ModTime: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if len(notes.lib) != 1 || !committed {
		t.Fatalf("upsert notes %v committed %v", notes.lib, committed)
	}
	tr, err := s.GetTrack(ctx, "Album/a.flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEnriched(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if len(notes.lib) != 2 {
		t.Fatalf("enrich notes %v", notes.lib)
	}
	stale, err := s.GetTrack(ctx, "Album/a.flac")
	if err != nil {
		t.Fatal(err)
	}
	stale.rowVersion = 1
	if err := s.MarkEnriched(ctx, stale); !errors.Is(err, ErrTrackChanged) {
		t.Fatalf("stale enrich: %v", err)
	}
	if len(notes.lib) != 2 {
		t.Fatalf("miss noted %v", notes.lib)
	}
}

func TestLibraryScanEndedFiresOnceAfterTheScanningFlagClears(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.flac"), audioBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sc := NewScanner([]string{root}, s, "")
	var calls int
	var stillScanning bool
	sc.SetLibraryScanEnded(func() {
		calls++
		stillScanning = sc.IsScanning()
	})
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || stillScanning {
		t.Fatalf("calls %d scanning %v", calls, stillScanning)
	}
	if len(notes.lib) == 0 {
		t.Fatal("scan wrote no library note")
	}
}
