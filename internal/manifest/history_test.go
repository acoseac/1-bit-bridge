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
	if len(top) != 2 || top[0].Label != "hit.flac" || top[0].Count != 2 {
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
