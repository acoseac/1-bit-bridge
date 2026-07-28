package manifest

import "testing"

// dhowden stores MP4 ©-atoms with a SINGLE 0xA9 lead byte (`\xa9day`,
// `\xa9ART`, `\xa9wrt`), which is invalid UTF-8. Before the
// normaliseRawTagKey canonicalization, strings.ToLower mangled that byte to
// utf8.RuneError, so the source-literal aliases (UTF-8 `"©day"` = 0xC2 0xA9)
// could never match — and year / composer / multi-value artist were SILENTLY
// DROPPED for M4A/ALAC. These tests drive the REAL single-byte keys through
// the actual extraction path (stubMetadata → populateFromTagMetadata): red
// before the fix, green after.

// TestNormaliseRawTagKey_CopyrightAtomCanonicalized is the direct unit — a
// single-byte 0xA9 lead becomes UTF-8 © (so the "©…" aliases can match),
// while every ASCII key (ID3 frames, MP4 non-© atoms) is untouched.
func TestNormaliseRawTagKey_CopyrightAtomCanonicalized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\xa9day", "©day"},              // date atom
		{"\xa9ART", "©art"},              // artist atom (uppercase tail lowercased)
		{"\xa9wrt", "©wrt"},              // composer atom
		{"tyer", "tyer"},                 // ASCII ID3 frame — unchanged
		{"trkn", "trkn"},                 // ASCII MP4 non-© atom — unchanged
		{"Track Number", "track_number"}, // existing lower + space→underscore preserved
	}
	for _, c := range cases {
		if got := normaliseRawTagKey(c.in); got != c.want {
			t.Errorf("normaliseRawTagKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPopulateFromTag_M4A_YearFromCopyrightDayAtom — year recovered from
// dhowden's real \xa9day atom (was nil for M4A pre-fix; the exact user report).
func TestPopulateFromTag_M4A_YearFromCopyrightDayAtom(t *testing.T) {
	track := &Track{}
	populateFromTagMetadata(&stubMetadata{raw: map[string]any{"\xa9day": "1985"}}, track)
	if track.Year == nil {
		t.Fatalf("Year = nil, want 1985 (real \\xa9day atom must be recognised)")
	}
	if *track.Year != 1985 {
		t.Errorf("Year = %d, want 1985", *track.Year)
	}
}

// TestPopulateFromTag_M4A_ComposerFromCopyrightWrtAtom — composer recovered
// from dhowden's real \xa9wrt atom. populateFromTagMetadata sets Composer only
// via stringOf(raw, "tcom", "composer", "©wrt") — there is no m.Composer()
// fallback for the stub, so a mis-encoded alias dropped it entirely.
func TestPopulateFromTag_M4A_ComposerFromCopyrightWrtAtom(t *testing.T) {
	track := &Track{}
	populateFromTagMetadata(&stubMetadata{raw: map[string]any{"\xa9wrt": "Johann Sebastian Bach"}}, track)
	if track.Composer != "Johann Sebastian Bach" {
		t.Errorf("Composer = %q, want %q", track.Composer, "Johann Sebastian Bach")
	}
}

// TestApplyMultiValue_M4A_MultiArtistFromCopyrightArtAtom — the multi-value
// "; "-join override recovered from dhowden's real \xa9ART atom (the artist
// multi-value path is applyMultiValueArtistsFromRaw, NOT populateFromTagMetadata,
// which only takes dhowden's single m.Artist()). Pre-fix a multi-artist M4A
// collapsed to dhowden's last-wins single artist.
func TestApplyMultiValue_M4A_MultiArtistFromCopyrightArtAtom(t *testing.T) {
	m := &stubMetadata{raw: map[string]any{
		"\xa9ART": []string{"Ella Fitzgerald", "Louis Armstrong"},
	}}
	track := &Track{}
	applyMultiValueArtistsFromRaw(m, track)
	if track.Artist != "Ella Fitzgerald; Louis Armstrong" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Ella Fitzgerald; Louis Armstrong")
	}
}
