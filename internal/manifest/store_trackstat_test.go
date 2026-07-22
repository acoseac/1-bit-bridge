package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestGetTrackStatMatchesGetTrack pins the equivalence the scanner's
// skip gate now depends on: the `size` / `mtime_ns` COLUMNS agree with
// the `Track.Size` / `Track.ModTime` carried inside the `tags_json`
// BLOB, so projecting the columns is not a behaviour change.
//
// This is the load-bearing assertion behind GetTrackStat. CLAUDE.md
// warns that `Track.ModTime` lives inside `tags_json` and that
// `GetTrack` never reads the standalone `mtime_ns` column — which is
// exactly why swapping the gate onto the column needs a pin rather
// than an assumption.
//
// A sub-second mtime is deliberate: truncating to whole seconds would
// let a lossy column write pass.
func TestGetTrackStatMatchesGetTrack(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	want := &Track{
		Path:        "Artist/Album/01 - Track.flac",
		Size:        1234567,
		ModTime:     time.Unix(1693162888, 518134356).UTC(),
		ArtworkMBID: "local-" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := s.UpsertTrack(ctx, want); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	full, err := s.GetTrack(ctx, want.Path)
	if err != nil || full == nil {
		t.Fatalf("GetTrack: %v (nil=%v)", err, full == nil)
	}
	st, err := s.GetTrackStat(ctx, want.Path)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat: %v (nil=%v)", err, st == nil)
	}

	if st.Size != full.Size {
		t.Errorf("Size: column %d, tags_json %d", st.Size, full.Size)
	}
	if st.MTimeNS != full.ModTime.UnixNano() {
		t.Errorf("mtime: column %d, tags_json %d", st.MTimeNS, full.ModTime.UnixNano())
	}
	if st.MTimeNS != want.ModTime.UnixNano() {
		t.Errorf("mtime column %d, want %d (sub-second precision lost?)",
			st.MTimeNS, want.ModTime.UnixNano())
	}
	if st.ArtworkMBID != full.ArtworkMBID {
		t.Errorf("ArtworkMBID: projected %q, tags_json %q", st.ArtworkMBID, full.ArtworkMBID)
	}
}

// TestGetTrackStatMissingReturnsNilNil pins the (nil, nil) miss
// contract GetTrack already had — the scanner's gate branches on
// `existing != nil`, so a miss that returned a zero-value struct would
// make every new file look like an unchanged 0-byte row.
func TestGetTrackStatMissingReturnsNilNil(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	st, err := s.GetTrackStat(context.Background(), "nope/missing.flac")
	if err != nil {
		t.Fatalf("GetTrackStat on missing row: %v", err)
	}
	if st != nil {
		t.Fatalf("want nil for a missing row, got %+v", st)
	}
}

// TestGetTrackStatColumnsSurviveEnrichment is the drift guard.
// MarkEnriched rewrites `tags_json` with `SET tags_json = ?` and does
// NOT touch `size` / `mtime_ns`, so if it ever round-tripped a Track
// that lost its ModTime the column and the JSON would silently
// diverge — and the skip gate would start comparing against a stale
// mtime, permanently skipping (or permanently re-extracting) the row.
func TestGetTrackStatColumnsSurviveEnrichment(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	tr := &Track{
		Path:    "Artist/Album/02 - Track.flac",
		Size:    999,
		ModTime: time.Unix(1700000000, 123456789).UTC(),
	}
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	// Enrich exactly as the enricher does: read the row back, add MBIDs,
	// write it through MarkEnriched.
	got, err := s.GetTrack(ctx, tr.Path)
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	got.MusicBrainzAlbumID = "11111111-2222-3333-4444-555555555555"
	if err := s.MarkEnriched(ctx, got); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}

	st, err := s.GetTrackStat(ctx, tr.Path)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat after enrichment: %v", err)
	}
	if st.MTimeNS != tr.ModTime.UnixNano() {
		t.Errorf("mtime_ns drifted after enrichment: got %d, want %d",
			st.MTimeNS, tr.ModTime.UnixNano())
	}
	if st.Size != tr.Size {
		t.Errorf("size drifted after enrichment: got %d, want %d", st.Size, tr.Size)
	}

	after, err := s.GetTrack(ctx, tr.Path)
	if err != nil || after == nil {
		t.Fatalf("GetTrack after enrichment: %v", err)
	}
	if after.ModTime.UnixNano() != st.MTimeNS {
		t.Errorf("column/tags_json disagree after enrichment: json %d, column %d",
			after.ModTime.UnixNano(), st.MTimeNS)
	}
}
