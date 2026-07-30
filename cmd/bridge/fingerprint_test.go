package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
)

// TestBuildResultRowsKeepsArtistsAndTitlesAligned guards against a display bug
// that was silent and looked like real data.
//
// Two earlier shapes got this wrong. The first appended to a shared Artists
// slice only when a recording carried one, so a single artist-less recording
// shifted every later entry and printed a real artist name beside the wrong
// title. The fix kept the slices in step by hand; the current shape keeps one
// struct per recording instead, which makes the misalignment unrepresentable
// rather than merely tested-against. The test remains because the property is
// what matters, not the mechanism that currently enforces it.
func TestBuildResultRowsKeepsArtistsAndTitlesAligned(t *testing.T) {
	results := []acoustid.Result{{
		ID:    "acoustid-1",
		Score: 0.99,
		Recordings: []acoustid.Recording{
			{ID: "r1", Title: "First", Sources: 9, Artists: []acoustid.Artist{{ID: "a1", Name: "Artist One"}}},
			{ID: "r2", Title: "Second (no artist)", Sources: 2}, // the shifting case
			{ID: "r3", Title: "Third", Sources: 40, Artists: []acoustid.Artist{{ID: "a3", Name: "Artist Three"}}},
		},
	}}

	rows := buildResultRows(results)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if len(row.Recordings) != 3 {
		t.Fatalf("got %d recordings, want one row per recording", len(row.Recordings))
	}

	// The load-bearing assertion: the third recording's artist must still line
	// up with the third title, despite the artist-less recording before it.
	want := []fingerprintRecordingRow{
		{Artist: "Artist One", Title: "First", Sources: 9},
		{Artist: "?", Title: "Second (no artist)", Sources: 2},
		{Artist: "Artist Three", Title: "Third", Sources: 40},
	}
	for j, w := range want {
		if row.Recordings[j] != w {
			t.Errorf("recording %d = %+v, want %+v", j, row.Recordings[j], w)
		}
	}
	if row.ID != "acoustid-1" || row.Score != 0.99 {
		t.Errorf("scalar fields lost: %+v", row)
	}
}

func TestBuildResultRowsEmpty(t *testing.T) {
	if rows := buildResultRows(nil); len(rows) != 0 {
		t.Fatalf("got %d rows, want none", len(rows))
	}
}
