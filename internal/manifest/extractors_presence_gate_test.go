package manifest

import (
	"path/filepath"
	"testing"

	tag "github.com/dhowden/tag"
)

// TestHasAnyRawKey_TruthTable pins the value-shape-agnostic
// presence helper introduced for CodeRabbit Major on PR #226. The
// key contract: a tag key whose value type is NOT one of stringOf's
// supported shapes (string / []string / int) — e.g. dhowden's
// `*tag.Position` for MP4 `trkn` — must still register as present.
func TestHasAnyRawKey_TruthTable(t *testing.T) {
	raw := map[string]any{
		"tyer":      "1985",                               // string
		"\xa9ART":   []string{"Mozart", "Salieri"},        // []string — REAL dhowden single-byte MP4 atom (was UTF-8 "©ART", which masked the bug)
		"tbpm":      120,                                  // int
		"trkn":      &tag.Picture{MIMEType: "image/jpeg"}, // arbitrary non-supported type
		"album_art": []byte{0x89, 0x50, 0x4e, 0x47},       // []byte
		"date":      "2020",                               // for case-insensitive alias check
	}
	// Present (any value type counts).
	for _, k := range []string{"tyer", "©art", "tbpm", "trkn", "album_art", "date"} {
		if !hasAnyRawKey(raw, k) {
			t.Errorf("hasAnyRawKey(%q) = false, want true (value type doesn't matter)", k)
		}
	}
	// Absent.
	for _, k := range []string{"composer", "tit2", "unknown"} {
		if hasAnyRawKey(raw, k) {
			t.Errorf("hasAnyRawKey(%q) = true, want false (key not in map)", k)
		}
	}
	// nil map fast-path.
	if hasAnyRawKey(nil, "anything") {
		t.Errorf("hasAnyRawKey(nil, ...) = true, want false (defensive fast-path)")
	}
	// No aliases.
	if hasAnyRawKey(raw) {
		t.Errorf("hasAnyRawKey with no aliases returned true, want false")
	}
	// Case-fold + space normalisation via normaliseRawTagKey: a
	// raw map key with mixed case + spaces matches its lowercase-
	// underscored alias form.
	if !hasAnyRawKey(map[string]any{"Track Number": "5"}, "track_number") {
		t.Errorf("normalised-key match failed: 'Track Number' should match 'track_number'")
	}
}

// nilRawMetadata is a tag.Metadata stub whose Raw() returns nil —
// used to exercise populateFromTagMetadata's defensive fallback
// branch (CodeRabbit Trivial on PR #226). Lifted here as a local
// type so the test stays self-contained; the multi-value test file
// on a sibling branch has a richer stubMetadata which we deliberately
// don't share to avoid coupling between PRs.
type nilRawMetadata struct {
	year, track, disc int
}

func (n nilRawMetadata) Format() tag.Format          { return "" }
func (n nilRawMetadata) FileType() tag.FileType      { return "" }
func (n nilRawMetadata) Title() string               { return "" }
func (n nilRawMetadata) Album() string               { return "" }
func (n nilRawMetadata) AlbumArtist() string         { return "" }
func (n nilRawMetadata) Artist() string              { return "" }
func (n nilRawMetadata) Composer() string            { return "" }
func (n nilRawMetadata) Year() int                   { return n.year }
func (n nilRawMetadata) Genre() string               { return "" }
func (n nilRawMetadata) Track() (int, int)           { return n.track, 0 }
func (n nilRawMetadata) Disc() (int, int)            { return n.disc, 0 }
func (n nilRawMetadata) Picture() *tag.Picture       { return nil }
func (n nilRawMetadata) Lyrics() string              { return "" }
func (n nilRawMetadata) Comment() string             { return "" }
func (n nilRawMetadata) Raw() map[string]interface{} { return nil }

// TestPopulateFromTag_RawNilFallback locks the defensive branch
// at populateFromTagMetadata: when m.Raw() returns nil (rare —
// most format parsers return at least an empty map), the function
// must fall back to assigning Year / TrackNumber / DiscNumber as
// non-nil pointers from the dhowden accessors. CodeRabbit Trivial
// nitpick on PR #226.
func TestPopulateFromTag_RawNilFallback(t *testing.T) {
	m := nilRawMetadata{year: 1999, track: 3, disc: 2}
	track := &Track{}
	populateFromTagMetadata(m, track)
	if track.Year == nil || *track.Year != 1999 {
		t.Errorf("Year = %v, want pointer to 1999 (Raw==nil fallback must assign)", track.Year)
	}
	if track.TrackNumber == nil || *track.TrackNumber != 3 {
		t.Errorf("TrackNumber = %v, want pointer to 3", track.TrackNumber)
	}
	if track.DiscNumber == nil || *track.DiscNumber != 2 {
		t.Errorf("DiscNumber = %v, want pointer to 2", track.DiscNumber)
	}
}

// TestPopulateFromTag_YearTrackDisc_AbsentReturnsNil pins the PR-B
// presence-gate contract: when the underlying file has NO year /
// track / disc tag, populateFromTagMetadata leaves the *int
// pointers nil so the iOS client can distinguish "no tag" from
// "explicit zero" and surface "Unknown" cleanly.
func TestPopulateFromTag_YearTrackDisc_AbsentReturnsNil(t *testing.T) {
	// Synthesize an MP3 with only Title — no Year/Track/Disc frames.
	dir := t.TempDir()
	path := filepath.Join(dir, "no_year_track_disc.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title":  "Lonely Track",
		"artist": "Anon",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year != nil {
		t.Errorf("Year = %v, want nil (no TYER/TDRC frame in fixture)", *track.Year)
	}
	if track.TrackNumber != nil {
		t.Errorf("TrackNumber = %v, want nil (no TRCK frame in fixture)", *track.TrackNumber)
	}
	if track.DiscNumber != nil {
		t.Errorf("DiscNumber = %v, want nil (no TPOS frame in fixture)", *track.DiscNumber)
	}
	// Sanity: the present field IS surfaced.
	if track.Title != "Lonely Track" {
		t.Errorf("Title = %q, want %q", track.Title, "Lonely Track")
	}
}

// TestPopulateFromTag_YearTrackDisc_ExplicitZeroSurfaces locks the
// allow-side: when the file carries an explicit zero value for
// year / track / disc, the pointer surfaces as Some(0) — only
// the absent case returns nil.
//
// dhowden/tag parses the raw text into int; "0" parses as 0.
// The presence gate sees the raw key in the map and admits the
// pointer assignment.
func TestPopulateFromTag_YearTrackDisc_ExplicitZeroSurfaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "explicit_zero.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Year Zero",
		"year":  "0",
		"track": "0",
		"disc":  "0",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year == nil || *track.Year != 0 {
		t.Errorf("Year = %v, want pointer to 0 (explicit-zero tag must surface)", track.Year)
	}
	if track.TrackNumber == nil || *track.TrackNumber != 0 {
		t.Errorf("TrackNumber = %v, want pointer to 0", track.TrackNumber)
	}
	if track.DiscNumber == nil || *track.DiscNumber != 0 {
		t.Errorf("DiscNumber = %v, want pointer to 0", track.DiscNumber)
	}
}

// TestPopulateFromTag_YearTrackDisc_RealValuesSurface regression
// guard: realistic non-zero year / track / disc still populate
// correctly through the new gate.
func TestPopulateFromTag_YearTrackDisc_RealValuesSurface(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "real_values.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Real Track",
		"year":  "1974",
		"track": "3",
		"disc":  "2",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year == nil || *track.Year != 1974 {
		t.Errorf("Year = %v, want pointer to 1974", track.Year)
	}
	if track.TrackNumber == nil || *track.TrackNumber != 3 {
		t.Errorf("TrackNumber = %v, want pointer to 3", track.TrackNumber)
	}
	if track.DiscNumber == nil || *track.DiscNumber != 2 {
		t.Errorf("DiscNumber = %v, want pointer to 2", track.DiscNumber)
	}
}

// TestPopulateFromTag_PartialTagsOnlyPresentFieldsSurface: a file
// with year + track but no disc should produce Year=Some(N),
// TrackNumber=Some(N), DiscNumber=nil. Pre-PR all three would be
// Some, with DiscNumber=Some(0) indistinguishable from a real
// disc=0 tag.
func TestPopulateFromTag_PartialTagsOnlyPresentFieldsSurface(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Partial",
		"year":  "1985",
		"track": "5",
		// no disc
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year == nil || *track.Year != 1985 {
		t.Errorf("Year = %v, want pointer to 1985", track.Year)
	}
	if track.TrackNumber == nil || *track.TrackNumber != 5 {
		t.Errorf("TrackNumber = %v, want pointer to 5", track.TrackNumber)
	}
	if track.DiscNumber != nil {
		t.Errorf("DiscNumber = %v, want nil (no TPOS frame)", *track.DiscNumber)
	}
}

// TestPopulateFromTag_VorbisAliases verifies the FLAC / Vorbis
// alias set (DATE / TRACKNUMBER / DISCNUMBER) routes through the
// gate the same way as ID3v2 keys. writeMinimalFLAC seeds the
// Vorbis Comment block with these aliases.
func TestPopulateFromTag_VorbisAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.flac")
	writeMinimalFLAC(t, path, 44100, 16, map[string]string{
		"TITLE":       "Vorbis",
		"DATE":        "2001",
		"TRACKNUMBER": "7",
		"DISCNUMBER":  "1",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year == nil || *track.Year != 2001 {
		t.Errorf("Year = %v, want pointer to 2001 (DATE alias)", track.Year)
	}
	if track.TrackNumber == nil || *track.TrackNumber != 7 {
		t.Errorf("TrackNumber = %v, want pointer to 7 (TRACKNUMBER alias)", track.TrackNumber)
	}
	if track.DiscNumber == nil || *track.DiscNumber != 1 {
		t.Errorf("DiscNumber = %v, want pointer to 1 (DISCNUMBER alias)", track.DiscNumber)
	}
}

// TestPopulateFromTag_VorbisAliases_AbsentReturnsNil: a FLAC
// with title only (no DATE/TRACKNUMBER/DISCNUMBER) returns nil
// pointers — same contract as the MP3 case.
func TestPopulateFromTag_VorbisAliases_AbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture_minimal.flac")
	writeMinimalFLAC(t, path, 44100, 16, map[string]string{"TITLE": "Solo"})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year != nil {
		t.Errorf("Year = %v, want nil", *track.Year)
	}
	if track.TrackNumber != nil {
		t.Errorf("TrackNumber = %v, want nil", *track.TrackNumber)
	}
	if track.DiscNumber != nil {
		t.Errorf("DiscNumber = %v, want nil", *track.DiscNumber)
	}
}
