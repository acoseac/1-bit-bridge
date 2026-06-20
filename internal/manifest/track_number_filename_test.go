package manifest

import "testing"

func TestParseLeadingTrackNumber(t *testing.T) {
	cases := []struct {
		stem string
		want int
		ok   bool
	}{
		// the dominant real-world shape — "NN. Title"
		{"06. Congeniality", 6, true},
		{"1. Intro", 1, true},
		{"08. Bouncin' With Bud", 8, true},
		// other accepted separators
		{"01 - Quelqu'un m'a dit", 1, true}, // space-dash-space
		{"12 - The Show Must Go On", 12, true},
		{"03_The Moon Shines Bright", 3, true}, // underscore
		{"7)Title", 7, true},                   // paren
		{"04.Big Shot", 4, true},               // no space after dot
		// boundaries
		{"100. Long Album Track", 100, true},
		{"999. X", 999, true},
		// rejects — year / numeric-title prefixes that must NOT be misread
		{"1984 - Smells Like", 0, false}, // 4-digit year, space-dash
		{"2001 A Space Odyssey", 0, false},
		{"12 Monkeys", 0, false}, // bare space, no punctuation separator
		{"1000. X", 0, false},    // >3-digit run
		// rejects — not a track prefix at all
		{"Congeniality", 0, false},
		{"", 0, false},
		{"06", 0, false},       // digits alone, no title
		{"0. Intro", 0, false}, // track 0 is not valid
		{"00. Hidden", 0, false},
	}
	for _, c := range cases {
		got, ok := parseLeadingTrackNumber(c.stem)
		if got != c.want || ok != c.ok {
			t.Errorf("parseLeadingTrackNumber(%q) = (%d,%v), want (%d,%v)", c.stem, got, ok, c.want, c.ok)
		}
	}
}

func TestFillTrackNumberFromFilename(t *testing.T) {
	intp := func(n int) *int { return &n }

	t.Run("fills when absent (nil)", func(t *testing.T) {
		tr := &Track{}
		fillTrackNumberFromFilename("/lib/Artist/Album/06. Congeniality.flac", tr)
		if tr.TrackNumber == nil || *tr.TrackNumber != 6 {
			t.Fatalf("want 6, got %v", tr.TrackNumber)
		}
	})

	t.Run("fills when absent (0 sentinel)", func(t *testing.T) {
		tr := &Track{TrackNumber: intp(0)}
		fillTrackNumberFromFilename("/lib/Artist/Album/03. Unravel.flac", tr)
		if tr.TrackNumber == nil || *tr.TrackNumber != 3 {
			t.Fatalf("want 3, got %v", tr.TrackNumber)
		}
	})

	t.Run("never overrides a real tag value", func(t *testing.T) {
		tr := &Track{TrackNumber: intp(5)}
		fillTrackNumberFromFilename("/lib/Artist/Album/06. Mistagged.flac", tr)
		if tr.TrackNumber == nil || *tr.TrackNumber != 5 {
			t.Fatalf("want 5 (unchanged), got %v", tr.TrackNumber)
		}
	})

	t.Run("leaves nil when filename has no number", func(t *testing.T) {
		tr := &Track{}
		fillTrackNumberFromFilename("/lib/Artist/Album/Untitled.flac", tr)
		if tr.TrackNumber != nil {
			t.Fatalf("want nil, got %v", *tr.TrackNumber)
		}
	})

	t.Run("uses basename, not the full path digits", func(t *testing.T) {
		// directory components carry numbers ("CD 02", "1996") that must
		// not leak into the parse — only the basename stem is considered.
		tr := &Track{}
		fillTrackNumberFromFilename("/lib/2001/CD 02/04. Time.flac", tr)
		if tr.TrackNumber == nil || *tr.TrackNumber != 4 {
			t.Fatalf("want 4, got %v", tr.TrackNumber)
		}
	})
}
