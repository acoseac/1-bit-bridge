package manifest

import (
	"context"
	"errors"
	"testing"
)

// A matching base skips the stamp guard. A client clock behind the
// stored stamp is accepted, and the stamp becomes the stored value plus
// one nanosecond. Putting the stamp guard back on this path rejects it.
func TestPlaylistMatchingBaseAcceptsABehindClock(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-behind", "Favorites", 5000)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	p.Name = "Renamed"
	p.LastModifiedAt = 1000
	base := int64(5000)
	res, err := s.PutPlaylist(ctx, "devA", p, items, &base)
	if err != nil {
		t.Fatalf("matching base: %v", err)
	}
	if res.Unchanged || res.LastModifiedAt != 5001 {
		t.Fatalf("result %+v, want stamp 5001 written", res)
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil || got == nil || got.Name != "Renamed" || got.LastModifiedAt != 5001 {
		t.Fatalf("stored %+v err %v", got, err)
	}
}

// An equal body with a matching base writes nothing, even when the
// client's stamp is newer. Dropping the equality short-circuit stores
// that newer stamp.
func TestPlaylistEqualBodyDoesNotMoveTheStamp(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-equal", "Favorites", 1000)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	p.LastModifiedAt = 9000
	base := int64(1000)
	res, err := s.PutPlaylist(ctx, "devA", p, items, &base)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged || res.LastModifiedAt != 1000 {
		t.Fatalf("result %+v, want the stored stamp and no write", res)
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil || got == nil || got.LastModifiedAt != 1000 {
		t.Fatalf("stored %+v err %v", got, err)
	}
}

func TestIdenticalPlaylistBodyDoesNotRevive(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-revive", "Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TombstonePlaylist(ctx, p.ID, "devA"); err != nil || !ok {
		t.Fatalf("tombstone ok=%v err=%v", ok, err)
	}
	p.LastModifiedAt = 9_000_000_000
	if err := s.UpsertPlaylist(ctx, "devB", p, items); err != nil {
		t.Fatalf("identical re-put: %v", err)
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("identical body revived a deleted playlist: %+v", got)
	}
}

func TestMatchingBaseRevivesADeletedPlaylist(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-base", "Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TombstonePlaylist(ctx, p.ID, "devA"); err != nil || !ok {
		t.Fatalf("tombstone ok=%v err=%v", ok, err)
	}
	p.Name = "Changed"
	base := int64(100)
	res, err := s.PutPlaylist(ctx, "devA", p, items, &base)
	if err != nil {
		t.Fatalf("matching base on a deleted playlist: %v", err)
	}
	if res.Unchanged {
		t.Fatal("a matching base with a different body must revive")
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil || got == nil || got.Name != "Changed" || got.Deleted {
		t.Fatalf("revived %+v err %v", got, err)
	}
}

func TestABaseThatDoesNotPredateTheDeletionRevives(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-late", "Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TombstonePlaylist(ctx, p.ID, "devA"); err != nil || !ok {
		t.Fatalf("tombstone ok=%v err=%v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE playlists SET last_modified_at = updated_at WHERE id = ?
	`, p.ID); err != nil {
		t.Fatal(err)
	}
	var stamp int64
	if err := s.db.QueryRowContext(ctx, `SELECT last_modified_at FROM playlists WHERE id = ?`, p.ID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	p.Name = "After"
	p.LastModifiedAt = stamp + 10
	res, err := s.PutPlaylist(ctx, "devA", p, items, &stamp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unchanged {
		t.Fatal("a base that does not predate the deletion must revive")
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil || got == nil || got.Name != "After" || got.Deleted {
		t.Fatalf("revived %+v err %v", got, err)
	}
}

func TestPlaylistBaseMismatchCarriesTheStoredPlaylist(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-miss", "Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	p.Name = "Other"
	base := int64(50)
	_, err := s.PutPlaylist(ctx, "devA", p, items, &base)
	var mismatch *PlaylistBaseMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err %v, want PlaylistBaseMismatch", err)
	}
	if mismatch.Row.Name != "Favorites" || mismatch.Row.LastModifiedAt != 100 || len(mismatch.Items) != 2 {
		t.Fatalf("mismatch %+v items %d", mismatch.Row, len(mismatch.Items))
	}
	got, _, err := s.GetPlaylist(ctx, p.ID)
	if err != nil || got == nil || got.Name != "Favorites" {
		t.Fatalf("mismatch must write nothing: %+v err %v", got, err)
	}
}

func TestIdenticalPlaylistBodyWithAStaleBaseWritesNothing(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-same", "Favorites", 1000)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatal(err)
	}
	base := int64(1)
	res, err := s.PutPlaylist(ctx, "devA", p, items, &base)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged || res.LastModifiedAt != 1000 {
		t.Fatalf("result %+v, want unchanged at 1000", res)
	}
}

func TestTwoBridgesAcceptARePushOfTheOlderCopy(t *testing.T) {
	ctx := context.Background()
	a := newDeviceTestStore(t)
	b := newDeviceTestStore(t)
	older, items := samplePlaylist("pl-repush", "A copy", 100)
	if err := a.UpsertPlaylist(ctx, "devA", older, items); err != nil {
		t.Fatal(err)
	}
	newer, _ := samplePlaylist("pl-repush", "B copy", 5000)
	if err := b.UpsertPlaylist(ctx, "devB", newer, items); err != nil {
		t.Fatal(err)
	}
	base := int64(5000)
	res, err := b.PutPlaylist(ctx, "devA", older, items, &base)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastModifiedAt != 5001 {
		t.Fatalf("stamp %d, want 5001", res.LastModifiedAt)
	}
	got, _, err := b.GetPlaylist(ctx, older.ID)
	if err != nil || got == nil || got.Name != "A copy" || got.LastModifiedAt != 5001 {
		t.Fatalf("stored %+v err %v", got, err)
	}
}
