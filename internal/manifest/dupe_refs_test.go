package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// Pointer helpers reused from sibling test files in this package:
// intptr/f64ptr (store_analysis_test.go) and the production boolPtr
// (store.go).

// seedDupeTrack inserts a track with the grouping-relevant tag fields
// populated the way the scanner populates them.
func seedDupeTrack(t *testing.T, s *Store, tr *Track) {
	t.Helper()
	if tr.ModTime.IsZero() {
		tr.ModTime = time.Unix(0, 0).UTC()
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("seed track %q: %v", tr.Path, err)
	}
}

func TestStreamTrackDupeRefs_ProjectionAndTagPresence(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedDupeTrack(t, s, &Track{
		Path: "Artist/Album/01 Song.flac", Size: 26634341,
		Title: "Song", Artist: "Artist", AlbumArtist: "Artist", Album: "Album",
		TrackNumber: intptr(1), DiscNumber: intptr(0), Year: intptr(2020),
		Duration: f64ptr(263.73), SampleRate: f64ptr(44100),
		BitsPerSample: intptr(16), IsDSD: boolPtr(false), Codec: "FLAC",
	})
	// Tag-absent disc/track: NULL must surface as DiscTagged=false, NOT 0.
	seedDupeTrack(t, s, &Track{
		Path: "Artist/Album/untagged.flac", Size: 100,
		Title: "Untagged", Artist: "Artist", Album: "Album",
	})

	refs := map[string]dupes.Row{}
	err := s.StreamTrackDupeRefsUnderPrefix(ctx, "", false, func(r dupes.Row, _ DupeStampState) error {
		refs[r.Path] = r // value copy — safe to retain
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}

	full := refs["Artist/Album/01 Song.flac"]
	if full.Title != "Song" || full.Album != "Album" || full.AlbumArtist != "Artist" ||
		full.Artist != "Artist" || full.Year != 2020 || full.Size != 26634341 {
		t.Fatalf("tag projection wrong: %+v", full)
	}
	// The nil-vs-Some(0) distinction: DiscNumber was an EXPLICIT 0.
	if !full.DiscTagged || full.Disc != 0 {
		t.Fatalf("explicit disc 0 must read as tagged: %+v", full)
	}
	if !full.TrackTagged || full.Track != 1 {
		t.Fatalf("track: %+v", full)
	}
	if full.Duration != 263.73 || full.SampleRate != 44100 ||
		full.BitsPerSample != 16 || full.IsDSD || full.Codec != "FLAC" {
		t.Fatalf("geometry projection wrong: %+v", full)
	}

	un := refs["Artist/Album/untagged.flac"]
	if un.DiscTagged || un.TrackTagged {
		t.Fatalf("absent disc/track must read as untagged: %+v", un)
	}
	if un.SampleRate != 0 || un.BitsPerSample != 0 || un.Codec != "" {
		t.Fatalf("absent geometry must read as unknown zeros: %+v", un)
	}
}

func TestStreamTrackDupeRefs_ExcludesRoutedRowsByDefault(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedFSTrack(t, s, "music/Artist/a.flac")
	seedRoutedTrack(t, s, "2go/Server/c.flac")

	var got []string
	if err := s.StreamTrackDupeRefsUnderPrefix(ctx, "", false, func(r dupes.Row, _ DupeStampState) error {
		got = append(got, r.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "music/Artist/a.flac" {
		t.Fatalf("default stream must exclude routed rows, got %v", got)
	}

	got = nil
	if err := s.StreamTrackDupeRefsUnderPrefix(ctx, "", true, func(r dupes.Row, _ DupeStampState) error {
		got = append(got, r.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("includeRouted must surface the routing-backed row, got %v", got)
	}
}

func TestStreamTrackDupeRefs_PrefixScopesByByteRange(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedFSTrack(t, s, "music/Artist/a.flac")
	seedFSTrack(t, s, "music0trap/Artist/b.flac") // '0' successor byte — must NOT match "music"
	seedFSTrack(t, s, "other/Artist/c.flac")

	var got []string
	// Trailing slash must be tolerated (subtreeRangeBase trims it).
	if err := s.StreamTrackDupeRefsUnderPrefix(ctx, "music/", false, func(r dupes.Row, _ DupeStampState) error {
		got = append(got, r.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "music/Artist/a.flac" {
		t.Fatalf("prefix scope wrong: %v", got)
	}
}
