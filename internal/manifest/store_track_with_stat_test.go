package manifest

import (
	"context"
	"testing"
	"time"
)

// TestGetTrackWithStatReadsTheColumnsTheSkipGateReads: the track is
// GetTrack's, and the size and mtime are the columns GetTrackStat reads,
// not the copy inside tags_json: a zeroed mtime_ns, which forces a
// re-extraction, reads 0 here as it does there. A path with no row is none.
func TestGetTrackWithStatReadsTheColumnsTheSkipGateReads(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	rate, bits := 96000.0, 24
	when := time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC)
	if err := store.UpsertTrack(ctx, &Track{Path: "Album/a.flac", Size: 4321, ModTime: when, Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
		t.Fatal(err)
	}
	got, size, mtime, err := store.GetTrackWithStat(ctx, "Album/a.flac")
	if err != nil || got == nil || got.Codec != "FLAC" || size != 4321 || mtime != when.UnixNano() {
		t.Fatalf("track %+v size %d mtime %d err %v", got, size, mtime, err)
	}
	if want, err := store.GetTrack(ctx, "Album/a.flac"); err != nil || want.Codec != got.Codec || want.rowVersion != got.rowVersion {
		t.Fatalf("GetTrack %+v err %v", want, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tracks SET mtime_ns = 0 WHERE path = ?`, "Album/a.flac"); err != nil {
		t.Fatal(err)
	}
	_, _, mtime, err = store.GetTrackWithStat(ctx, "Album/a.flac")
	st, err2 := store.GetTrackStat(ctx, "Album/a.flac")
	if err != nil || err2 != nil || mtime != 0 || st.MTimeNS != 0 {
		t.Fatalf("a zeroed mtime_ns reads %d here and %d in the skip gate (%v, %v)", mtime, st.MTimeNS, err, err2)
	}
	if got, size, mtime, err := store.GetTrackWithStat(ctx, "Album/missing.flac"); got != nil || size != 0 || mtime != 0 || err != nil {
		t.Fatalf("a missing row: %+v %d %d %v", got, size, mtime, err)
	}
}
