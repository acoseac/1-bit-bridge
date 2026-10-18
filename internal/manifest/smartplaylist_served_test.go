package manifest

import (
	"context"
	"testing"
	"time"
)

func TestSmartPlaylistServed_RecordsAndPrunesPastSevenDays(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	day := int64(24 * time.Hour)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixNano()

	// Plant both ages with a cutoff that keeps them, then prune at 7 days.
	if err := s.RecordSmartPlaylistServed(ctx, []SmartPlaylistServedRow{
		{ServedAt: now - 8*day, Slug: "auto-mix", Path: "/old.flac"},
		{ServedAt: now - day, Slug: "daily-mix", Path: "/recent.flac"},
		{ServedAt: now - day, Slug: "lift-off", Path: "/recent.flac"},
	}, now-30*day); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.SmartPlaylistServedSince(ctx, now-30*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/old.flac"]; !ok {
		t.Fatal("row inside the planted window was not stored")
	}
	if _, ok := got["/recent.flac"]; !ok {
		t.Fatal("recent row was not stored")
	}

	if err := s.RecordSmartPlaylistServed(ctx, nil, now-7*day); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err = s.SmartPlaylistServedSince(ctx, now-30*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/old.flac"]; ok {
		t.Fatal("a path served 8 days ago survived the 7-day prune")
	}
	if _, ok := got["/recent.flac"]; !ok {
		t.Fatal("a path served yesterday was pruned")
	}
	// The lookup the regenerator uses is the same 7-day window.
	got, err = s.SmartPlaylistServedSince(ctx, now-7*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/recent.flac"]; !ok || len(got) != 1 {
		t.Fatalf("7-day lookup = %v, want only /recent.flac", got)
	}
}
