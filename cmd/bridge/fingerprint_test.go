package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
)

// TestBuildResultRowsKeepsArtistsAndTitlesAligned pins an index-parallel
// contract that is easy to break and silent when broken.
//
// The printer pairs Artists[j] with Titles[j]. An earlier version appended to
// Artists only when a recording carried one, so a single artist-less recording
// shifted every later entry by one and printed a real artist name beside the
// wrong title — output that looks like a genuine mismatch rather than a
// formatting bug, which is the worst way for a diagnostic to be wrong.
func TestBuildResultRowsKeepsArtistsAndTitlesAligned(t *testing.T) {
	results := []acoustid.Result{{
		ID:      "acoustid-1",
		Score:   0.99,
		Sources: 7,
		Recordings: []acoustid.Recording{
			{ID: "r1", Title: "First", Artists: []acoustid.Artist{{ID: "a1", Name: "Artist One"}}},
			{ID: "r2", Title: "Second (no artist)"}, // the shifting case
			{ID: "r3", Title: "Third", Artists: []acoustid.Artist{{ID: "a3", Name: "Artist Three"}}},
		},
	}}

	rows := buildResultRows(results)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]

	if len(row.Artists) != len(row.Titles) {
		t.Fatalf("artists (%d) and titles (%d) must stay index-parallel",
			len(row.Artists), len(row.Titles))
	}
	if len(row.Titles) != 3 {
		t.Fatalf("got %d titles, want one per recording", len(row.Titles))
	}

	// The load-bearing assertion: the third recording's artist must still line
	// up with the third title, despite the artist-less recording before it.
	want := []struct{ artist, title string }{
		{"Artist One", "First"},
		{"?", "Second (no artist)"},
		{"Artist Three", "Third"},
	}
	for j, w := range want {
		if row.Artists[j] != w.artist || row.Titles[j] != w.title {
			t.Errorf("row %d = (%q, %q), want (%q, %q)",
				j, row.Artists[j], row.Titles[j], w.artist, w.title)
		}
	}
	if row.Recordings != 3 || row.Sources != 7 || row.ID != "acoustid-1" {
		t.Errorf("scalar fields lost: %+v", row)
	}
}

func TestBuildResultRowsEmpty(t *testing.T) {
	if rows := buildResultRows(nil); len(rows) != 0 {
		t.Fatalf("got %d rows, want none", len(rows))
	}
}
