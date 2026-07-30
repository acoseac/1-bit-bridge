package acoustid

import (
	"fmt"
	"testing"
)

// The exact production cluster, from a live AcoustID lookup for
// ABBA/The Singles/05. Waterloo.flac on the test host. Reproduced rather than
// invented, because the shape is the whole point: one overwhelmingly attested
// real credit sitting next to a placeholder that clears the sources floor.
const (
	abbaMBID        = "d87e52c5-bb8d-4da8-b941-9f4928627dc8"
	sweetboxMBID    = "6a987199-99f9-4531-960f-16e18002bd80"
	waterlooCluster = "d984c2f6-1114-47e9-9eac-bb479b8ce347"
)

func waterlooInput() Input {
	// Distinct recording IDs: MusicBrainz models the several ABBA pressings as
	// separate recordings, and collapsing them in the fixture would make
	// soleRecordingMBID see one entity where production sees four.
	n := 0
	rec := func(artistID, artistName, title string, sources int) Recording {
		n++
		return Recording{
			ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Title: title,
			Duration: 167, Sources: sources,
			Artists: []Artist{{ID: artistID, Name: artistName}},
		}
	}
	return Input{
		DurationSec:           167,
		Fingerprint:           goodFP(167),
		HasLocalArtistWitness: true,
		Results: []Result{{
			ID: waterlooCluster, Score: 0.9997,
			Recordings: []Recording{
				rec(abbaMBID, "ABBA", "Waterloo", 8),
				rec(abbaMBID, "ABBA", "Waterloo", 5),
				rec(sweetboxMBID, "sweetbox", "Waterfall", 1),
				rec(abbaMBID, "ABBA", "Waterloo", 1),
				rec(abbaMBID, "ABBA", "Waterloo", 6259),
				rec(unknownArtistMBID, "[unknown]", "Waterloo", 8),
				rec(abbaMBID, "ABBA", "No Doubt About It", 1),
			},
		}},
	}
}

// TestPlaceholderArtistDoesNotVetoAnOtherwiseUnanimousCluster is the
// regression this fix exists for, and the fixture is real production data.
//
// Before the fix this cluster was rejected artist_disagreement. The dissenting
// vote came from a recording credited to MusicBrainz's [unknown] — a
// placeholder whose own disambiguation reads "Special Purpose Artist – Do not
// add releases here, if possible." It asserts no knowledge of the performer
// rather than a competing one, and it beat 6,259 submissions naming ABBA.
//
// Note which recordings are load-bearing here: sweetbox and the stray "No
// Doubt About It" both carry 1 source and are removed by the sources filter
// before consensus runs, so they are NOT what caused the rejection. The
// placeholder cleared that floor with 8.
func TestPlaceholderArtistDoesNotVetoAnOtherwiseUnanimousCluster(t *testing.T) {
	d, reason := Accept(waterlooInput())
	if reason != ReasonNone {
		t.Fatalf("reason = %q, want accept — a placeholder credit must not out-vote %d "+
			"submissions naming the artist", reason, 6259)
	}
	if d.ArtistMBID != abbaMBID {
		t.Errorf("ArtistMBID = %q, want ABBA %q", d.ArtistMBID, abbaMBID)
	}
	if d.ArtistName != "ABBA" {
		t.Errorf("ArtistName = %q, want ABBA", d.ArtistName)
	}
	// Write-target discipline is unchanged by this fix.
	if d.RecordingMBID != "" {
		t.Errorf("RecordingMBID = %q, want empty — four ABBA recordings survive, so none is sole",
			d.RecordingMBID)
	}
}

// TestPlaceholderArtistCanNeverBecomeTheAnswer is the other half. Excluding
// [unknown] from the vote would be a real loosening if it could then be
// written as the artist, so a cluster of nothing but placeholders must be
// refused rather than accepted with a placeholder MBID.
func TestPlaceholderArtistCanNeverBecomeTheAnswer(t *testing.T) {
	in := waterlooInput()
	for i := range in.Results[0].Recordings {
		in.Results[0].Recordings[i].Artists = []Artist{{ID: unknownArtistMBID, Name: "[unknown]"}}
	}
	d, reason := Accept(in)
	if reason != ReasonOnlyPlaceholderArtist {
		t.Fatalf("reason = %q, want %q", reason, ReasonOnlyPlaceholderArtist)
	}
	if d.ArtistMBID != "" {
		t.Errorf("ArtistMBID = %q, want empty — the placeholder is not a performer", d.ArtistMBID)
	}
}

// TestRealArtistDisagreementIsStillRejected pins that this narrows nothing
// else. Two genuinely different performers above the sources floor is a dirty
// cluster and stays refused; only the placeholder loses its vote.
func TestRealArtistDisagreementIsStillRejected(t *testing.T) {
	in := waterlooInput()
	// Give the sweetbox recording enough submissions to clear the floor.
	for i := range in.Results[0].Recordings {
		if in.Results[0].Recordings[i].Artists[0].ID == sweetboxMBID {
			in.Results[0].Recordings[i].Sources = 40
			in.Results[0].Recordings[i].Duration = 167
		}
	}
	if _, reason := Accept(in); reason != ReasonArtistDisagreement {
		t.Fatalf("reason = %q, want %q — two real performers is still a dirty cluster",
			reason, ReasonArtistDisagreement)
	}
}

// TestRecordingWithNoArtistStillRefused pins the deliberate asymmetry in
// recordingsWithNamedArtist: it drops the PLACEHOLDER but leaves a recording
// with no artist at all in place, so headArtistConsensus still refuses it.
// Dropping those too would let a cluster be accepted on whatever remained.
func TestRecordingWithNoArtistStillRefused(t *testing.T) {
	in := waterlooInput()
	in.Results[0].Recordings = []Recording{
		{ID: "r1", Title: "Waterloo", Duration: 167, Sources: 6259,
			Artists: []Artist{{ID: abbaMBID, Name: "ABBA"}}},
		{ID: "r2", Title: "Waterloo", Duration: 167, Sources: 20, Artists: nil},
	}
	if _, reason := Accept(in); reason != ReasonNoArtistMBID {
		t.Fatalf("reason = %q, want %q", reason, ReasonNoArtistMBID)
	}
}
