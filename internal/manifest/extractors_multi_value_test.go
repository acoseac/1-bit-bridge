package manifest

import (
	"path/filepath"
	"testing"

	tag "github.com/dhowden/tag"
)

// TestApplyMultiValueArtistsFromRaw_StringWithNullSeparator pins
// the pure-helper detection for the ID3v2.4 NULL-separated case
// directly (no fixture file), so the contract is locked even if
// dhowden's behavior shifts in a future release.
func TestApplyMultiValueArtistsFromRaw_StringWithNullSeparator(t *testing.T) {
	raw := map[string]any{
		"tpe1": "Abdullah Ibrahim\x00Ekaya",
		"tpe2": "Various Artists",
	}
	values := extractMultiValueTagFromRaw(raw, "tpe1", "©art", "artist")
	if len(values) != 2 {
		t.Fatalf("len = %d, want 2", len(values))
	}
	if values[0] != "Abdullah Ibrahim" || values[1] != "Ekaya" {
		t.Errorf("got %v, want [Abdullah Ibrahim Ekaya]", values)
	}
	// Single value (no NULL) returns nil so caller skips override.
	if v := extractMultiValueTagFromRaw(raw, "tpe2"); v != nil {
		t.Errorf("single-value tpe2: got %v, want nil", v)
	}
}

// TestApplyMultiValueArtistsFromRaw_SliceVariant pins the MP4
// multiple-data-atom case where dhowden surfaces values as
// []string.
func TestApplyMultiValueArtistsFromRaw_SliceVariant(t *testing.T) {
	raw := map[string]any{
		"©art": []string{"Mozart", "Salieri", ""}, // trailing empty should drop
	}
	values := extractMultiValueTagFromRaw(raw, "tpe1", "©art", "artist")
	if len(values) != 2 {
		t.Fatalf("len = %d, want 2 (empty trailing entry must drop)", len(values))
	}
	if values[0] != "Mozart" || values[1] != "Salieri" {
		t.Errorf("got %v, want [Mozart Salieri]", values)
	}
}

// TestApplyMultiValueArtistsFromRaw_TrailingNullDropped: ID3v2.4
// taggers sometimes emit a trailing NULL after the last value as
// a frame terminator. The trimNonEmpty pass should drop it so the
// detection doesn't incorrectly count two values when there's
// only one.
func TestApplyMultiValueArtistsFromRaw_TrailingNullDropped(t *testing.T) {
	raw := map[string]any{"tpe1": "Solo Artist\x00"}
	v := extractMultiValueTagFromRaw(raw, "tpe1")
	if len(v) != 1 || v[0] != "Solo Artist" {
		t.Errorf("got %v, want [Solo Artist] (trailing NULL must drop)", v)
	}
}

// TestApplyMultiValueArtistsFromRaw_OverridesTrackArtist locks the
// end-to-end behavior: a multi-value raw map applied to a Track
// joins with "; " and overrides Artist / AlbumArtist.
func TestApplyMultiValueArtistsFromRaw_OverridesTrackArtist(t *testing.T) {
	// Stub Metadata that returns a multi-value raw map.
	m := &stubMetadata{raw: map[string]any{
		"tpe1": "Artist A\x00Artist B\x00Artist C",
		"tpe2": []string{"Curator 1", "Curator 2"},
	}}
	track := &Track{Artist: "OldValue", AlbumArtist: "OldAA"}
	applyMultiValueArtistsFromRaw(m, track)
	if track.Artist != "Artist A; Artist B; Artist C" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Artist A; Artist B; Artist C")
	}
	if track.AlbumArtist != "Curator 1; Curator 2" {
		t.Errorf("AlbumArtist = %q, want %q", track.AlbumArtist, "Curator 1; Curator 2")
	}
}

// TestApplyMultiValueArtistsFromRaw_NilMetadataNoCrash defensive
// guard: nil Metadata input must early-return rather than panic.
func TestApplyMultiValueArtistsFromRaw_NilMetadataNoCrash(t *testing.T) {
	track := &Track{Artist: "Preserved"}
	applyMultiValueArtistsFromRaw(nil, track)
	if track.Artist != "Preserved" {
		t.Errorf("Artist mutated despite nil Metadata: got %q", track.Artist)
	}
}

// TestApplyMultiValueArtistsFromRaw_SingleValueNoOverride: a
// single-value tag (no NULL separator, no slice multi-element)
// must NOT override the existing value populated by dhowden.
func TestApplyMultiValueArtistsFromRaw_SingleValueNoOverride(t *testing.T) {
	m := &stubMetadata{raw: map[string]any{"tpe1": "Single Artist"}}
	track := &Track{Artist: "AlreadySet"}
	applyMultiValueArtistsFromRaw(m, track)
	if track.Artist != "AlreadySet" {
		t.Errorf("single-value override fired: Artist = %q, want %q", track.Artist, "AlreadySet")
	}
}

// TestExtractMP3_DhowdenStripsNullsRegression locks dhowden/tag's
// current behavior of NULL-stripping in ID3v2 text frames (see
// readTFrame in dhowden/id3v2frames.go). The bridge's
// applyMultiValueArtistsFromRaw cannot detect ID3v2.4 multi-value
// today because of this stripping — a future dhowden release that
// preserves NULLs would let the helper start firing. This test
// documents the boundary by asserting:
//
//   - dhowden strips the NULL byte from a multi-value TPE1 frame
//     (Artist field arrives as concatenated single string)
//   - applyMultiValueArtistsFromRaw correctly no-ops (the helper
//     only detects NULL-separated strings; absent NULL → fall-through)
//
// If this test fails because Artist == "Artist A; Artist B", that
// means dhowden has started preserving NULLs and the helper is
// now active — a step forward for ID3v2 multi-value support that
// would not be a regression.
func TestExtractMP3_DhowdenStripsNullsRegression(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title":  "Track",
		"artist": "Artist A\x00Artist B",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Today's dhowden: concatenates without separator.
	// Future-good: "Artist A; Artist B".
	switch track.Artist {
	case "Artist AArtist B":
		// Expected pre-fix behavior — dhowden strips NULL, helper
		// can't detect multi-value. Documented limitation.
	case "Artist A; Artist B":
		// Future-good: dhowden started preserving NULLs, helper
		// fired. Not a regression — actually the goal.
	default:
		t.Errorf("Artist = %q, want either pre-fix \"Artist AArtist B\" or future-good \"Artist A; Artist B\"",
			track.Artist)
	}
}

// stubMetadata is a minimal tag.Metadata for unit-testing the
// multi-value helper without spinning up real fixture files.
// Only Raw() is exercised by applyMultiValueArtistsFromRaw —
// every other method returns zero values.
type stubMetadata struct {
	raw map[string]any
}

func (s *stubMetadata) Format() tag.Format     { return "" }
func (s *stubMetadata) FileType() tag.FileType { return "" }
func (s *stubMetadata) Title() string          { return "" }
func (s *stubMetadata) Album() string          { return "" }
func (s *stubMetadata) Artist() string         { return "" }
func (s *stubMetadata) AlbumArtist() string    { return "" }
func (s *stubMetadata) Composer() string       { return "" }
func (s *stubMetadata) Year() int              { return 0 }
func (s *stubMetadata) Genre() string          { return "" }
func (s *stubMetadata) Track() (int, int)      { return 0, 0 }
func (s *stubMetadata) Disc() (int, int)       { return 0, 0 }
func (s *stubMetadata) Picture() *tag.Picture  { return nil }
func (s *stubMetadata) Lyrics() string         { return "" }
func (s *stubMetadata) Comment() string        { return "" }
func (s *stubMetadata) Raw() map[string]any    { return s.raw }
