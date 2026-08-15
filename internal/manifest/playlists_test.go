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
	got, gotItems, err := s.GetPlaylist(ctx, "pl-1")
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

// TestPlaylistCrossDeviceVisibility pins the user-wide contract: every
// paired device belongs to the bridge operator, so a playlist backed up
// from one device is listable and restorable from any other.
func TestPlaylistCrossDeviceVisibility(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "A's list", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Any device (the store is id-scoped) reads devA's playlist.
	got, gotItems, err := s.GetPlaylist(ctx, "pl-1")
	if err != nil || got == nil {
		t.Fatalf("cross-device get: %v (got=%v)", err, got)
	}
	if got.Name != "A's list" || len(gotItems) != 2 {
		t.Errorf("cross-device read mismatch: %+v items=%d", got, len(gotItems))
	}
	// The provenance column records the writing device.
	if got.DeviceToken != "devA" {
		t.Errorf("provenance device token = %q, want devA", got.DeviceToken)
	}
	// The global list includes it.
	list, err := s.ListPlaylists(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "pl-1" {
		t.Errorf("global list mismatch: %v err=%v", list, err)
	}
}

// TestPlaylistCrossDeviceUpsertTransfersProvenance: devB may overwrite
// devA's playlist (newer LWW clock); device_token flips to the last
// writer.
func TestPlaylistCrossDeviceUpsertTransfersProvenance(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "A's", 100)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	pb, ib := samplePlaylist("pl-1", "B's edit", 999)
	if err := s.UpsertPlaylist(ctx, "devB", pb, ib); err != nil {
		t.Fatalf("cross-device upsert should succeed, got %v", err)
	}
	got, _, _ := s.GetPlaylist(ctx, "pl-1")
	if got == nil || got.Name != "B's edit" {
		t.Fatalf("cross-device write didn't land: %+v", got)
	}
	if got.DeviceToken != "devB" {
		t.Errorf("last-writer provenance = %q, want devB", got.DeviceToken)
	}
}

// TestPlaylistLWWGuard: a strictly-older inbound is rejected (regardless
// of which device sends it); equal-or-newer overwrites.
func TestPlaylistLWWGuard(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "v2", 200)
	if err := s.UpsertPlaylist(ctx, "devA", p, items); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	// Older inbound → stale, same device.
	older, oi := samplePlaylist("pl-1", "v1", 100)
	if err := s.UpsertPlaylist(ctx, "devA", older, oi); !errors.Is(err, ErrPlaylistStale) {
		t.Fatalf("want ErrPlaylistStale, got %v", err)
	}
	// Older inbound from ANOTHER device → still stale (the LWW guard is
	// the only gate now that ownership is user-wide).
	if err := s.UpsertPlaylist(ctx, "devB", older, oi); !errors.Is(err, ErrPlaylistStale) {
		t.Fatalf("cross-device stale: want ErrPlaylistStale, got %v", err)
	}
	got, _, _ := s.GetPlaylist(ctx, "pl-1")
	if got == nil || got.Name != "v2" || got.DeviceToken != "devA" {
		t.Errorf("stale write mutated the row: %+v", got)
	}
	// Equal timestamp → accepted (≥, backup hygiene only).
	eq, ei := samplePlaylist("pl-1", "v2-eq", 200)
	if err := s.UpsertPlaylist(ctx, "devA", eq, ei); err != nil {
		t.Fatalf("equal-clock upsert should succeed: %v", err)
	}
	got, _, _ = s.GetPlaylist(ctx, "pl-1")
	if got.Name != "v2-eq" {
		t.Errorf("equal-clock did not overwrite: %+v", got)
	}
}

func TestTombstonePlaylist(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "doomed", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)

	ok, err := s.TombstonePlaylist(ctx, "pl-1")
	if err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	// Gone from get + list.
	got, _, _ := s.GetPlaylist(ctx, "pl-1")
	if got != nil {
		t.Errorf("tombstoned playlist still readable: %+v", got)
	}
	list, _ := s.ListPlaylists(ctx)
	if len(list) != 0 {
		t.Errorf("tombstoned playlist still listed: %d", len(list))
	}
	// Second tombstone is a no-op (no live row).
	ok2, _ := s.TombstonePlaylist(ctx, "pl-1")
	if ok2 {
		t.Error("second tombstone reported a hit")
	}
}

func TestListPlaylistsOrderedByLastModifiedDesc(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	a, ai := samplePlaylist("pl-a", "A", 100)
	b, bi := samplePlaylist("pl-b", "B", 300)
	c, ci := samplePlaylist("pl-c", "C", 200)
	_ = s.UpsertPlaylist(ctx, "devA", a, ai)
	// pl-b backed up by a DIFFERENT device — the user-wide list
	// interleaves all devices' playlists in one mtime-DESC order.
	_ = s.UpsertPlaylist(ctx, "devB", b, bi)
	_ = s.UpsertPlaylist(ctx, "devA", c, ci)
	list, err := s.ListPlaylists(ctx)
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
	_, gotItems, _ := s.GetPlaylist(ctx, "pl-1")
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

// TestTombstoneCrossDevice: deletion initiated from a device other than
// the one that backed the playlist up still lands (user-wide deletes).
func TestTombstoneCrossDevice(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "shared", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)
	// The store API is id-scoped, so "devB deleting" is simply a delete
	// by id — this pins that there is no hidden device filter left.
	ok, err := s.TombstonePlaylist(ctx, "pl-1")
	if err != nil || !ok {
		t.Fatalf("cross-device tombstone: ok=%v err=%v", ok, err)
	}
	if got, _, _ := s.GetPlaylist(ctx, "pl-1"); got != nil {
		t.Errorf("playlist survived cross-device delete: %+v", got)
	}
}

// TestListPlaylistTombstoneIDs pins the sweep-propagation feed: a
// tombstoned playlist's id appears in the tombstone list (and only
// there), the list is empty when nothing was deleted, and a revive
// (newer-clock upsert) removes the id again.
func TestListPlaylistTombstoneIDs(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()

	// Empty store → empty tombstone list.
	ids, err := s.ListPlaylistTombstoneIDs(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("empty store: ids=%v err=%v, want none", ids, err)
	}

	a, ai := samplePlaylist("pl-live", "kept", 100)
	b, bi := samplePlaylist("pl-doomed", "deleted", 100)
	_ = s.UpsertPlaylist(ctx, "devA", a, ai)
	_ = s.UpsertPlaylist(ctx, "devA", b, bi)

	// Nothing deleted yet → still empty.
	ids, _ = s.ListPlaylistTombstoneIDs(ctx)
	if len(ids) != 0 {
		t.Fatalf("pre-delete tombstones = %v, want none", ids)
	}

	if ok, err := s.TombstonePlaylist(ctx, "pl-doomed"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	// Exactly the tombstoned id — and it left the live list.
	ids, err = s.ListPlaylistTombstoneIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "pl-doomed" {
		t.Fatalf("tombstones = %v err=%v, want [pl-doomed]", ids, err)
	}
	live, _ := s.ListPlaylists(ctx)
	if len(live) != 1 || live[0].ID != "pl-live" {
		t.Errorf("live list = %v, want only pl-live", live)
	}

	// Revive (LWW-newer upsert) → the id leaves the tombstone list.
	rev, ri := samplePlaylist("pl-doomed", "revived", 200)
	if err := s.UpsertPlaylist(ctx, "devB", rev, ri); err != nil {
		t.Fatalf("revive upsert: %v", err)
	}
	ids, err = s.ListPlaylistTombstoneIDs(ctx)
	if err != nil {
		t.Fatalf("post-revive tombstone query: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("post-revive tombstones = %v, want none", ids)
	}
}

// TestUpsertPlaylistRevivesTombstone: a backup sweep PUT after a delete
// (e.g. another device that still has the playlist re-uploads it with a
// newer clock) revives the row — LWW decides, not the tombstone.
func TestUpsertPlaylistRevivesTombstone(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	p, items := samplePlaylist("pl-1", "v1", 100)
	_ = s.UpsertPlaylist(ctx, "devA", p, items)
	if ok, _ := s.TombstonePlaylist(ctx, "pl-1"); !ok {
		t.Fatal("tombstone failed")
	}
	rev, ri := samplePlaylist("pl-1", "revived", 200)
	if err := s.UpsertPlaylist(ctx, "devB", rev, ri); err != nil {
		t.Fatalf("revive upsert: %v", err)
	}
	got, _, _ := s.GetPlaylist(ctx, "pl-1")
	if got == nil || got.Name != "revived" {
		t.Errorf("tombstoned row not revived: %+v", got)
	}
}
