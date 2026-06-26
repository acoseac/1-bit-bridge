package manifest

import "testing"

// A full-date DATE/YEAR tag ("2023-06-09") makes dhowden's Year() return 0;
// populateFromTagMetadata must recover the 4-digit year via parseYearPrefix
// (the same path OriginalYear already uses) so the release year doesn't
// surface as 0. Regression: Melody Gardot "Entre eux deux (The Paris
// Sessions)" is tagged YEAR=2023-06-09 and was indexed with year 0, making
// it collide with the 2022 standard edition downstream.
func TestPopulateFromTag_Year_ISO8601DateRecovered(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want int
	}{
		{"vorbis year ISO date", map[string]any{"year": "2023-06-09"}, 2023},
		{"vorbis date ISO date", map[string]any{"date": "1999-12-31"}, 1999},
		{"id3v24 tdrc timestamp", map[string]any{"tdrc": "1984-01-01T00:00:00"}, 1984},
		{"plain 4-digit still parses", map[string]any{"year": "2022"}, 2022},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			track := &Track{}
			populateFromTagMetadata(&stubMetadata{raw: c.raw}, track)
			if track.Year == nil {
				t.Fatalf("Year = nil, want pointer to %d", c.want)
			}
			if *track.Year != c.want {
				t.Errorf("Year = %d, want %d", *track.Year, c.want)
			}
		})
	}
}

// An unparseable but PRESENT year tag must stay present at 0 (the
// explicit-zero / "Unknown" wire contract) — the ISO-date fallback must not
// drop presence by leaving Year nil.
func TestPopulateFromTag_Year_UnparseablePresentStaysZero(t *testing.T) {
	track := &Track{}
	populateFromTagMetadata(&stubMetadata{raw: map[string]any{"year": "not-a-year"}}, track)
	if track.Year == nil {
		t.Fatalf("Year = nil, want present pointer to 0")
	}
	if *track.Year != 0 {
		t.Errorf("Year = %d, want 0", *track.Year)
	}
}

// stringOf must resolve the earliest-listed key DETERMINISTICALLY when a
// file carries more than one matching tag — not by Go's randomized map
// iteration order (which would flap the parsed year across scans, the exact
// instability the year fix is meant to remove). 200 iterations reliably
// surfaces map-order non-determinism on a small map. Gemini HIGH on PR #447.
func TestStringOf_DeterministicKeyPriority(t *testing.T) {
	raw := map[string]any{"tdrc": "2023-06-09", "year": "1999"}
	for i := 0; i < 200; i++ {
		got, ok := stringOf(raw, "tdrc", "tdrl", "tyer", "date", "year")
		if !ok || got != "2023-06-09" {
			t.Fatalf("iteration %d: stringOf = (%q, %v), want (%q, true) — priority must be deterministic",
				i, got, ok, "2023-06-09")
		}
	}
}
