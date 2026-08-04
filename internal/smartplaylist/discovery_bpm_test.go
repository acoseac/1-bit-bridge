package smartplaylist

import (
	"testing"
)

func bpmPtr(v int) *int { return &v }

// An unknown BPM must not score as a PERFECT match.
//
// discoveryScore added nothing when f.BPM was nil, and the caller sorts
// ASCENDING with no threshold — so an unanalysed track scored 0.0,
// which no BPM-bearing candidate can beat (they score
// |bpm − median|, > 0 for anything not exactly at the median). The
// tracks promoted to the front of the discovery pool were therefore
// precisely the ones analysis had failed on, inverting the intent.
func TestDiscoveryScore_UnknownBPMIsNotAPerfectMatch(t *testing.T) {
	const medBPM = 120.0

	nilBPM := TrackFeature{Path: "/unknown.flac", Genre: "Jazz"}
	closeBPM := TrackFeature{Path: "/close.flac", Genre: "Jazz", BPM: bpmPtr(124)}

	nilScore := discoveryScore(nilBPM, "Jazz", medBPM)
	closeScore := discoveryScore(closeBPM, "Jazz", medBPM)

	if nilScore == 0 {
		t.Fatal("unknown BPM scored 0.0 — a perfect match no real candidate can beat")
	}
	if nilScore <= closeScore {
		t.Fatalf("unknown BPM (%.1f) still outranks a near-median candidate (%.1f); "+
			"un-analysed tracks would crowd the front of the discovery pool",
			nilScore, closeScore)
	}
}

// It must be a MID-RANGE penalty, not an exclusion: a track with an
// unknown BPM should still beat one that's wildly off-tempo, so the
// mix doesn't become "analysed tracks only".
func TestDiscoveryScore_UnknownBPMBeatsAFarOutlier(t *testing.T) {
	const medBPM = 120.0

	nilScore := discoveryScore(
		TrackFeature{Path: "/unknown.flac", Genre: "Jazz"}, "Jazz", medBPM)
	farScore := discoveryScore(
		TrackFeature{Path: "/far.flac", Genre: "Jazz", BPM: bpmPtr(220)}, "Jazz", medBPM)

	if nilScore >= farScore {
		t.Fatalf("unknown BPM (%.1f) ranks no better than a 100-BPM-off outlier (%.1f); "+
			"the penalty should be mid-range, not an effective exclusion",
			nilScore, farScore)
	}
}

// The genre penalty must still dominate — the BPM term only reorders
// WITHIN a genre, which is what this function exists to produce.
func TestDiscoveryScore_GenreStillDominatesUnknownBPM(t *testing.T) {
	const medBPM = 120.0

	sameGenreUnknown := discoveryScore(
		TrackFeature{Path: "/a.flac", Genre: "Jazz"}, "Jazz", medBPM)
	otherGenreExact := discoveryScore(
		TrackFeature{Path: "/b.flac", Genre: "Metal", BPM: bpmPtr(120)}, "Jazz", medBPM)

	if sameGenreUnknown >= otherGenreExact {
		t.Fatalf("an unknown-BPM track in the dominant genre (%.1f) lost to an "+
			"exact-BPM track outside it (%.1f); the genre penalty must dominate",
			sameGenreUnknown, otherGenreExact)
	}
}

// With no median available (nothing in the familiar set carries a BPM),
// the term contributes nothing either way — no free penalty for tracks
// that simply have nothing to be compared against.
func TestDiscoveryScore_NoMedianMeansNoBPMTerm(t *testing.T) {
	withBPM := discoveryScore(
		TrackFeature{Path: "/a.flac", Genre: "Jazz", BPM: bpmPtr(140)}, "Jazz", 0)
	withoutBPM := discoveryScore(
		TrackFeature{Path: "/b.flac", Genre: "Jazz"}, "Jazz", 0)

	if withBPM != 0 || withoutBPM != 0 {
		t.Fatalf("with no median BPM both should score 0 (got %.1f / %.1f)",
			withBPM, withoutBPM)
	}
}

// A stored non-positive BPM is an extraction artefact, not a tempo. It
// must score as UNKNOWN, matching medianBPM's own `*f.BPM > 0` filter —
// otherwise |0 − median| charges the full median, a HARSHER penalty
// than the honest "unknown" one, purely because the analyser wrote a
// placeholder instead of nothing.
func TestDiscoveryScore_ZeroBPMScoresAsUnknown(t *testing.T) {
	const medBPM = 120.0

	zero := discoveryScore(
		TrackFeature{Path: "/zero.flac", Genre: "Jazz", BPM: bpmPtr(0)}, "Jazz", medBPM)
	unknown := discoveryScore(
		TrackFeature{Path: "/nil.flac", Genre: "Jazz"}, "Jazz", medBPM)

	if zero != unknown {
		t.Fatalf("BPM=0 scored %.1f but a nil BPM scored %.1f; a placeholder "+
			"zero must not be penalised harder than no data at all", zero, unknown)
	}
}
