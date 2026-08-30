package manifest

import (
	"context"
	"testing"
	"time"
)

func histEvent(dt, path string, started int64, dur float64, codec, iface string) PlaybackHistoryRow {
	return PlaybackHistoryRow{
		DeviceToken: dt, Path: path, StartedAt: started, DurationUsed: dur,
		Codec: codec, IfaceType: iface, OutputRate: 176400, IsDoP: true,
	}
}

func TestInsertHistoryBatchAndList(t *testing.T) {
	s := newDeviceTestStore(t)
	fixed := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return fixed }
	ctx := context.Background()

	events := []PlaybackHistoryRow{
		histEvent("devA", "A/1.flac", 100, 30.5, "FLAC", "USB-DAC"),
		histEvent("devA", "A/2.dsf", 200, 184.25, "DSF", "CarPlay"),
		histEvent("devB", "B/1.flac", 150, 10, "FLAC", "Bluetooth"),
	}
	if err := s.InsertHistoryBatch(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// devA sees only its 2 events, newest started first.
	list, err := s.ListHistory(ctx, "devA", 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 devA events, got %d", len(list))
	}
	if list[0].StartedAt != 200 || list[1].StartedAt != 100 {
		t.Errorf("not ordered newest-first: %+v", list)
	}
	// REAL duration_used round-trips with fractional precision.
	if list[0].DurationUsed != 184.25 {
		t.Errorf("fractional duration lost: got %v want 184.25", list[0].DurationUsed)
	}
	if list[0].IfaceType != "CarPlay" || !list[0].IsDoP || list[0].OutputRate != 176400 {
		t.Errorf("hardware target mangled: %+v", list[0])
	}

	// devB scoping.
	bl, _ := s.ListHistory(ctx, "devB", 100, 0)
	if len(bl) != 1 || bl[0].Path != "B/1.flac" {
		t.Errorf("devB scoping wrong: %+v", bl)
	}
}

// TestListHistoryGlobalFeed pins the empty-token branch: an admin
// caller passing "" gets a unified all-devices feed (newest-id first),
// not the pre-fix WHERE device_token = ” empty result.
func TestListHistoryGlobalFeed(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{
		histEvent("devA", "A/1.flac", 100, 30, "FLAC", "USB-DAC"),
		histEvent("devA", "A/2.dsf", 200, 60, "DSF", "CarPlay"),
		histEvent("devB", "B/1.flac", 150, 10, "FLAC", "Bluetooth"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	all, err := s.ListHistory(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("global list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("global feed want 3 events across devices, got %d", len(all))
	}
	// Cursor paging on the global feed: first page of 2, then the rest.
	page1, _ := s.ListHistory(ctx, "", 2, 0)
	if len(page1) != 2 {
		t.Fatalf("page1 want 2, got %d", len(page1))
	}
	page2, _ := s.ListHistory(ctx, "", 2, page1[1].ID)
	if len(page2) != 1 || page2[0].ID >= page1[1].ID {
		t.Errorf("global cursor paging wrong: page2=%+v", page2)
	}
}

func TestHistoryEmptyBatchIsNoOp(t *testing.T) {
	s := newDeviceTestStore(t)
	if err := s.InsertHistoryBatch(context.Background(), nil); err != nil {
		t.Fatalf("empty batch should be a no-op, got %v", err)
	}
}

func TestHistoryHistogramsAndTopTracks(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	_ = s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{
		histEvent("devA", "hit.flac", 1, 1, "FLAC", "USB-DAC"),
		histEvent("devA", "hit.flac", 2, 1, "FLAC", "USB-DAC"),
		histEvent("devA", "other.dsf", 3, 1, "DSF", "CarPlay"),
	})
	codecs, err := s.CodecHistogram(ctx, "")
	if err != nil {
		t.Fatalf("codec hist: %v", err)
	}
	// FLAC=2, DSF=1, ordered desc.
	if len(codecs) != 2 || codecs[0].Label != "FLAC" || codecs[0].Count != 2 {
		t.Errorf("codec histogram wrong: %+v", codecs)
	}
	routes, _ := s.RouteHistogram(ctx, "")
	if len(routes) != 2 {
		t.Errorf("route histogram wrong: %+v", routes)
	}
	top, _ := s.TopTracks(ctx, 10)
	if len(top) != 2 || top[0].Path != "hit.flac" || top[0].Count != 2 {
		t.Errorf("top tracks wrong: %+v", top)
	}
	total, _ := s.HistoryEventCount(ctx)
	if total != 3 {
		t.Errorf("total events = %d, want 3", total)
	}
}

func TestListHistoryCursorPaging(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	var events []PlaybackHistoryRow
	for i := int64(1); i <= 5; i++ {
		events = append(events, histEvent("devA", "t.flac", i*10, 1, "FLAC", "USB-DAC"))
	}
	_ = s.InsertHistoryBatch(ctx, events)

	page1, _ := s.ListHistory(ctx, "devA", 2, 0)
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	// Next page after the last id of page1.
	page2, _ := s.ListHistory(ctx, "devA", 2, page1[1].ID)
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if page2[0].ID >= page1[1].ID {
		t.Errorf("cursor paging not strictly descending: %d then %d", page1[1].ID, page2[0].ID)
	}
}

// TestListHistoryDeviceAttribution pins the LEFT JOIN against
// device_registrations: each event carries the playing device's token +
// registered display name, and an unregistered token degrades to an
// empty name (LEFT JOIN, not INNER — events must never drop because a
// registration is missing).
func TestListHistoryDeviceAttribution(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "devA", "tok-1", "iPhone 17"); err != nil {
		t.Fatal(err)
	}
	events := []PlaybackHistoryRow{
		histEvent("devA", "A/1.flac", 100, 30, "FLAC", "USB-DAC"),
		histEvent("devGhost", "G/1.flac", 200, 5, "FLAC", "Bluetooth"),
	}
	if err := s.InsertHistoryBatch(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}
	list, err := s.ListHistory(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 events, got %d", len(list))
	}
	// Newest first: devGhost (id 2), then devA (id 1).
	if list[0].SourceDeviceToken != "devGhost" || list[0].SourceDeviceName != "" {
		t.Errorf("unregistered device attribution wrong: %+v", list[0])
	}
	if list[1].SourceDeviceToken != "devA" || list[1].SourceDeviceName != "iPhone 17" {
		t.Errorf("registered device attribution wrong: %+v", list[1])
	}
}

// TestTopTracksResolvesTitleAndArtist pins the join that makes the
// Listening-history panel readable.
//
// playback_history stores the path, so until this landed the console's
// most human view listed "09. Bye Baby Blue.flac" and
// "03 - Adele - Don't You Remember.flac" — the least readable panel in
// the product, describing the most human data in it.
//
// The unresolved row is the other half and matters as much: a play of a
// file since deleted or renamed is still a real play. It must survive
// with empty metadata so the caller can fall back to the basename,
// rather than vanishing from the counts.
func TestTopTracksResolvesTitleAndArtist(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()

	if err := s.UpsertTrack(ctx, &Track{
		Path:   "Adele/25/03 - Adele - Don't You Remember.flac",
		Title:  "Don't You Remember",
		Artist: "Adele",
		Album:  "25",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	_ = s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{
		histEvent("devA", "Adele/25/03 - Adele - Don't You Remember.flac", 1, 1, "FLAC", "CarPlay"),
		histEvent("devA", "Adele/25/03 - Adele - Don't You Remember.flac", 2, 1, "FLAC", "CarPlay"),
		histEvent("devA", "Gone/Deleted/09. Bye Baby Blue.flac", 3, 1, "FLAC", "CarPlay"),
	})

	top, err := s.TopTracks(ctx, 10)
	if err != nil {
		t.Fatalf("TopTracks: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(top), top)
	}

	if top[0].Title != "Don't You Remember" || top[0].Artist != "Adele" {
		t.Errorf("resolved row = %+v, want title/artist spliced from the catalog", top[0])
	}
	if top[0].Count != 2 {
		t.Errorf("count = %d, want 2", top[0].Count)
	}
	if top[0].Path == "" {
		t.Error("path dropped — the tooltip and the fallback both need it")
	}

	// The deleted track: present, counted, unnamed.
	if top[1].Title != "" || top[1].Artist != "" {
		t.Errorf("unresolved row = %+v, want empty metadata", top[1])
	}
	if top[1].Path != "Gone/Deleted/09. Bye Baby Blue.flac" {
		t.Errorf("unresolved path = %q — a play of a since-deleted file is still a play "+
			"and must not vanish from the counts", top[1].Path)
	}
}
