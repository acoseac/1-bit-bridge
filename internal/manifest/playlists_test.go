package manifest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func samplePlaylist(id, name string, lma int64) (PlaylistRow, []PlaylistItemRow) {
	return PlaylistRow{ID: id, Name: name, LastModifiedAt: lma},
		[]PlaylistItemRow{
			{Position: 0, Path: "Pink Floyd/DSOTM/Money.flac"},
			{Position: 1, OriginFingerprint: "AB:CD", OriginPath: "Krall/Live/Romance.flac", Title: "Romance", Artist: "Diana Krall"},
		}
}

func TestUpsertGetPlaylistRoundTrip(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "Favorites", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, gotItems, err := s.GetPlaylist(ctx, "devA", "pl-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (got=%v)", err, got)
	}
	if got.Name != "Favorites" || got.LastModifiedAt != 100 {
		t.Errorf("row mismatch: %+v", got)
	}
	if len(gotItems) != 2 {
		t.Fatalf("want 2 items, got %d", len(gotItems))
	}
	// Local-XOR-foreign distinction must survive the NULL round-trip.
	if gotItems[0].Path != "Pink Floyd/DSOTM/Money.flac" || gotItems[0].OriginFingerprint != "" {
		t.Errorf("local item mangled: %+v", gotItems[0])
	}
	if gotItems[1].Path != "" || gotItems[1].OriginFingerprint != "AB:CD" || gotItems[1].Title != "Romance" {
		t.Errorf("foreign item mangled: %+v", gotItems[1])
	}
}

// TestPlaylistDeviceScoping is the security-critical test: one device
// cannot read another device's playlist.
func TestPlaylistDeviceScoping(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "A's list", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// devB must not see devA's playlist.
	got, _, err := s.GetPlaylist(ctx, "devB", "pl-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Errorf("device scoping breach: devB read devA's playlist %+v", got)
	}
	// devB listing is empty.
	list, _ := s.ListPlaylists(ctx, "devB")
	if len(list) != 0 {
		t.Errorf("devB list should be empty, got %d", len(list))
	}
}

// TestPlaylistOwnershipGuard: devB cannot overwrite a playlist id owned
// by devA.
func TestPlaylistOwnershipGuard(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "A's", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	pb, ib := samplePlaylist("pl-1", "B's hijack", 999)
	err := s.UpsertPlaylist(ctx, "devB", pb, ib)
	if !errors.Is(err, ErrPlaylistOwnedByOther) {
		t.Fatalf("want ErrPlaylistOwnedByOther, got %v", err)
	}
	// devA's row is untouched.
	got, _, _ := s.GetPlaylist(ctx, "devA", "pl-1")
	if got == nil || got.Name != "A's" {
		t.Errorf("devA row was clobbered: %+v", got)
	}
}

// TestPlaylistLWWGuard: a strictly-older inbound is rejected; equal-or-
// newer overwrites.
func TestPlaylistLWWGuard(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "v2", 200)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	// Older inbound → stale.
	older, oi := samplePlaylist("pl-1", "v1", 100)
	if err := s.UpsertPlaylist(ctx, "devA", older, oi); !errors.Is(err, ErrPlaylistStale) {
		t.Fatalf("want ErrPlaylistStale, got %v", err)
	}
	// Equal timestamp → accepted (≥, backup hygiene only).
	eq, ei := samplePlaylist("pl-1", "v2-eq", 200)
	if err := s.UpsertPlaylist(ctx, "devA", eq, ei); err != nil {
		t.Fatalf("equal-clock upsert should succeed: %v", err)
	}
	got, _, _ := s.GetPlaylist(ctx, "devA", "pl-1")
	if got.Name != "v2-eq" {
		t.Errorf("equal-clock did not overwrite: %+v", got)
	}
}

func TestTombstonePlaylist(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "doomed", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)

	ok, err := s.TombstonePlaylist(ctx, "devA", "pl-1")
	if err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	// Gone from get + list.
	got, _, _ := s.GetPlaylist(ctx, "devA", "pl-1")
	if got != nil {
		t.Errorf("tombstoned playlist still readable: %+v", got)
	}
	list, _ := s.ListPlaylists(ctx, "devA")
	if len(list) != 0 {
		t.Errorf("tombstoned playlist still listed: %d", len(list))
	}
	// Second tombstone is a no-op (no live row).
	ok2, _ := s.TombstonePlaylist(ctx, "devA", "pl-1")
	if ok2 {
		t.Error("second tombstone reported a hit")
	}
	// devB tombstoning devA's id is a no-op (scoping).
	_ = s.UpsertPlaylist(ctx, "devA", p, items)
	okB, _ := s.TombstonePlaylist(ctx, "devB", "pl-1")
	if okB {
		t.Error("devB tombstoned devA's playlist")
	}
}

func TestListPlaylistsOrderedByLastModifiedDesc(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	a, ai := samplePlaylist("pl-a", "A", 100)
	b, bi := samplePlaylist("pl-b", "B", 300)
	c, ci := samplePlaylist("pl-c", "C", 200)
	_ = s.UpsertPlaylist(ctx, "devA", a, ai)
	_ = s.UpsertPlaylist(ctx, "devA", b, bi)
	_ = s.UpsertPlaylist(ctx, "devA", c, ci)
	list, err := s.ListPlaylists(ctx, "devA")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"pl-b", "pl-c", "pl-a"} // 300, 200, 100
	if len(list) != 3 {
		t.Fatalf("want 3, got %d", len(list))
	}
	for i, w := range want {
		if list[i].ID != w {
			t.Errorf("order[%d] = %q, want %q", i, list[i].ID, w)
		}
	}
	if list[0].TrackCount != 2 {
		t.Errorf("track count = %d, want 2", list[0].TrackCount)
	}
}

// TestUpsertPlaylistReplacesItems verifies a re-upsert fully replaces the
// item set (no stale rows from the prior version).
func TestUpsertPlaylistReplacesItems(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "v1", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)

	p2 := PlaylistRow{ID: "pl-1", Name: "v2", LastModifiedAt: 200}
	_ = s.UpsertPlaylist(ctx, "devA", p2, []PlaylistItemRow{{Position: 0, Path: "only/one.flac"}})
	_, gotItems, _ := s.GetPlaylist(ctx, "devA", "pl-1")
	if len(gotItems) != 1 || gotItems[0].Path != "only/one.flac" {
		t.Errorf("items not fully replaced: %+v", gotItems)
	}
}

// Ensure the injectable clock drives updated_at (so a future admin sort by
// updated_at is deterministic in tests).
func TestUpsertPlaylistUsesInjectedClock(t *testing.T) {
	s := newDeviceTestStore(t)
	fixed := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return fixed }
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "x", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)
	admin, err := s.ListAllPlaylistsForAdmin(ctx)
	if err != nil || len(admin) != 1 {
		t.Fatalf("admin list: %v len=%d", err, len(admin))
	}
	if admin[0].UpdatedAt != fixed.UnixNano() {
		t.Errorf("updated_at = %d, want %d", admin[0].UpdatedAt, fixed.UnixNano())
	}
	if admin[0].DeviceToken != "devA" {
		t.Errorf("admin row device token = %q", admin[0].DeviceToken)
	}
}
