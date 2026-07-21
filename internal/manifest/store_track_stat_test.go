package manifest

import (
	"context"
	"testing"
	"time"
)

// TestGetTrackStat pins the light skip-gate query: it must return the
// same size / mtime-instant / artworkMBID the full GetTrack would
// surface, without touching the tags_json blob. The mtime contract is
// nanosecond-exact — mtimeNS == stored ModTime.UnixNano() — because the
// scanner gate compares it against os.FileInfo.ModTime().UnixNano() as
// the instant-equality check.
func TestGetTrackStat(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Non-UTC offset + sub-second precision: the instant must survive
	// exactly (UnixNano is location-independent).
	mtime := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.FixedZone("X", 3600))
	if err := s.UpsertTrack(ctx, &Track{
		Path: "A/01.flac", Size: 12345, ModTime: mtime,
		ArtworkMBID: "local-deadbeef",
	}); err != nil {
		t.Fatalf("UpsertTrack with mbid: %v", err)
	}
	// No ArtworkMBID: json_extract yields NULL, surfaced as "".
	if err := s.UpsertTrack(ctx, &Track{
		Path: "A/02.flac", Size: 0, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("UpsertTrack without mbid: %v", err)
	}

	cases := []struct {
		name        string
		path        string
		wantOK      bool
		wantSize    int64
		wantMtimeNS int64
		wantMBID    string
	}{
		{"with artwork mbid", "A/01.flac", true, 12345, mtime.UnixNano(), "local-deadbeef"},
		{"no artwork mbid", "A/02.flac", true, 0, time.Unix(0, 0).UnixNano(), ""},
		{"missing row", "A/nope.flac", false, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, mtimeNS, mbid, ok, err := s.GetTrackStat(ctx, tc.path)
			if err != nil {
				t.Fatalf("GetTrackStat(%q): %v", tc.path, err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if size != tc.wantSize {
				t.Errorf("size = %d, want %d", size, tc.wantSize)
			}
			if mtimeNS != tc.wantMtimeNS {
				t.Errorf("mtimeNS = %d, want %d", mtimeNS, tc.wantMtimeNS)
			}
			if mbid != tc.wantMBID {
				t.Errorf("artworkMBID = %q, want %q", mbid, tc.wantMBID)
			}
		})
	}
}

// TestGetTrackStat_MatchesGetTrack cross-checks the light query against
// the blob-backed GetTrack on the same row: the gate's three compared
// scalars must agree, so swapping the gate onto GetTrackStat cannot
// change skip semantics (2026-07-21 review M7).
func TestGetTrackStat_MatchesGetTrack(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	mtime := time.Date(2025, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if err := s.UpsertTrack(ctx, &Track{
		Path: "x.flac", Size: 777, ModTime: mtime,
		Title: "t", Artist: "a", ArtworkMBID: "uuid-form-mbid",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	size, mtimeNS, mbid, ok, err := s.GetTrackStat(ctx, "x.flac")
	if err != nil || !ok {
		t.Fatalf("GetTrackStat = (ok=%v, err=%v)", ok, err)
	}
	full, err := s.GetTrack(ctx, "x.flac")
	if err != nil || full == nil {
		t.Fatalf("GetTrack = (%v, %v)", full, err)
	}
	if size != full.Size {
		t.Errorf("size = %d, GetTrack.Size = %d", size, full.Size)
	}
	if !time.Unix(0, mtimeNS).UTC().Equal(full.ModTime) {
		t.Errorf("mtimeNS instant = %v, GetTrack.ModTime = %v",
			time.Unix(0, mtimeNS).UTC(), full.ModTime)
	}
	if mbid != full.ArtworkMBID {
		t.Errorf("artworkMBID = %q, GetTrack.ArtworkMBID = %q", mbid, full.ArtworkMBID)
	}
}
