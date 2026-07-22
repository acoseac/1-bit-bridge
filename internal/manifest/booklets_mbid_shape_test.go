package manifest

import (
	"context"
	"slices"
	"testing"
)

// TestDistinctAlbumReleaseMBIDsRejectsMalformedShapes pins the uuidGlob
// filter in DistinctAlbumReleaseMBIDs.
//
// The values it returns become the LEADING path component of the booklet
// cache filename, and the writer MkdirAll's that parent — so a hostile or
// merely malformed `musicbrainz_albumid` file tag reaching this list is a
// path-traversal primitive. The GLOB is the source-side half of that
// defense (the writer's anchored regex is the authoritative half), and it
// is easy to break silently: one mis-positioned hyphen in the 8-4-4-4-12
// run still compiles, still runs, and quietly stops matching.
//
// Nothing covered this filter when it landed, so this is the regression
// gate: every non-UUID shape must be excluded, and a real UUID must
// survive — an over-strict glob that drops legitimate releases would
// disable booklets library-wide just as quietly.
func TestDistinctAlbumReleaseMBIDsRejectsMalformedShapes(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const (
		validLower = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
		validUpper = "A1B2C3D4-E5F6-7A8B-9C0D-1E2F3A4B5C6D"
	)
	// Each entry is one track's musicBrainzAlbumID; only the two real
	// UUIDs may come back.
	seed := []struct {
		path string
		mbid string
	}{
		{"a/1.flac", validLower},
		{"a/2.flac", validUpper},
		{"b/1.flac", "../../etc/passwd"},                        // traversal
		{"b/2.flac", "local-abc123"},                            // local-artwork sentinel
		{"b/3.flac", ""},                                        // empty
		{"b/4.flac", "3f2504e04f8941d39a0c0305e82c3301"},        // no hyphens
		{"b/5.flac", "3f2504e0-4f89-41d3-9a0c-0305e82c33"},      // too short
		{"b/6.flac", "3f2504e0-4f89-41d3-9a0c-0305e82c3301aa"},  // too long
		{"b/7.flac", "3f2504e0-4f89-41d3-9a0c-0305e82c330z"},    // non-hex
		{"b/8.flac", "3f2504e04-f89-41d3-9a0c-0305e82c3301"},    // hyphen misplaced
		{"b/9.flac", "3f2504e0-4f89-41d3-9a0c-0305e82c3301/.."}, // valid prefix + traversal
	}
	for _, row := range seed {
		tr := &Track{Path: row.path, Title: "t"}
		if row.mbid != "" {
			tr.MusicBrainzAlbumID = row.mbid
		}
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("upsert %q: %v", row.path, err)
		}
	}

	got, err := s.DistinctAlbumReleaseMBIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{validLower, validUpper}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("DistinctAlbumReleaseMBIDs() = %q, want %q", got, want)
	}
}
