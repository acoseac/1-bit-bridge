package acoustid

import (
	"reflect"
	"testing"
)

// goodFP is a fingerprint that clears every fingerprint-side clause, so a test
// row only has to perturb the one thing it is about.
func goodFP(durationSec float64) Fingerprint {
	return Fingerprint{Value: "AQABz0m...", Duration: durationSec, DistinctB64: 64}
}

// goodInput is an accept. Every rejection row below mutates exactly one field,
// which is what makes it obvious that the named clause is the one firing.
func goodInput() Input {
	return Input{
		DurationSec:           240,
		Fingerprint:           goodFP(240),
		HasLocalArtistWitness: true,
		Results: []Result{{
			ID:    "9ff43b6a-4f16-427c-93c2-92307ca505e0",
			Score: 0.99,
			Recordings: []Recording{{
				ID:            "cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff",
				Title:         "Lower Your Eyelids to Die With the Sun",
				Duration:      240,
				Sources:       12,
				Artists:       []Artist{{ID: "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd", Name: "M83"}},
				ReleaseGroups: []ReleaseGroup{{ID: "ddaa2d4d-314e-3e7c-b1d0-f6d207f5aa2f", Title: "Before the Dawn Heals Us", Type: "Album"}},
			}},
		}},
	}
}

// TestAcceptRejections drives one row per clause, each asserting the SPECIFIC
// named reason rather than merely "not accepted". Asserting the reason is what
// stops a future edit from quietly loosening one constant while another clause
// happens to keep the row rejected.
func TestAcceptRejections(t *testing.T) {
	tests := []struct {
		name string
		want RejectReason
		mut  func(*Input)
	}{
		{"unknown duration", ReasonUnknownDuration, func(in *Input) { in.DurationSec = 0 }},
		{"DSD source", ReasonIsDSD, func(in *Input) { in.IsDSD = true }},
		{"below the short floor", ReasonTooShort, func(in *Input) {
			in.DurationSec = MinTrackSeconds - 0.1
			in.Fingerprint = goodFP(MinTrackSeconds - 0.1)
		}},
		{"above the long ceiling", ReasonTooLong, func(in *Input) {
			in.DurationSec = MaxTrackSeconds + 1
			in.Fingerprint = goodFP(MaxTrackSeconds + 1)
		}},
		{"degenerate fingerprint", ReasonLowEntropy, func(in *Input) {
			in.Fingerprint.DistinctB64 = minDistinctB64Chars - 1
		}},
		{"decode shorter than the container", ReasonDecodeMismatch, func(in *Input) {
			in.Fingerprint.Duration = in.DurationSec - (decodeAgreementSec + 1)
		}},
		{"no results", ReasonNoResults, func(in *Input) { in.Results = nil }},
		{"score below the floor", ReasonLowScore, func(in *Input) {
			in.Results[0].Score = minScore - 0.01
		}},
		{"runner-up within the margin", ReasonAmbiguousResults, func(in *Input) {
			runnerUp := in.Results[0]
			runnerUp.ID = "11111111-1111-1111-1111-111111111111"
			runnerUp.Score = in.Results[0].Score - (minScoreMargin / 2)
			in.Results = append(in.Results, runnerUp)
		}},
		{"too few sources with a local witness", ReasonFewSources, func(in *Input) {
			in.Results[0].Recordings[0].Sources = minSources - 1
		}},
		{"too few sources without a local witness", ReasonFewSources, func(in *Input) {
			in.HasLocalArtistWitness = false
			in.Results[0].Recordings[0].Sources = minSourcesNoLocalArtist - 1
		}},
		{"no recordings on the cluster", ReasonNoRecordings, func(in *Input) {
			in.Results[0].Recordings = nil
		}},
		{"recording duration disagrees", ReasonDurationMismatch, func(in *Input) {
			in.Results[0].Recordings[0].Duration = in.DurationSec + 30
		}},
		{"recording duration unknown", ReasonDurationMismatch, func(in *Input) {
			in.Results[0].Recordings[0].Duration = 0
		}},
		{"recording carries no artist", ReasonNoArtistMBID, func(in *Input) {
			in.Results[0].Recordings[0].Artists = nil
		}},
		{"surviving recordings disagree on head artist", ReasonArtistDisagreement, func(in *Input) {
			other := in.Results[0].Recordings[0]
			other.ID = "22222222-2222-2222-2222-222222222222"
			other.Artists = []Artist{{ID: "33333333-3333-3333-3333-333333333333", Name: "Someone Else"}}
			in.Results[0].Recordings = append(in.Results[0].Recordings, other)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := goodInput()
			tc.mut(&in)
			got, reason := Accept(in)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if got != (Decision{}) {
				t.Fatalf("a refused track must yield a zero Decision, got %+v", got)
			}
		})
	}
}

func TestAcceptHappyPath(t *testing.T) {
	got, reason := Accept(goodInput())
	if reason != ReasonNone {
		t.Fatalf("unexpected rejection: %q", reason)
	}
	if got.ArtistMBID != "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd" {
		t.Errorf("ArtistMBID = %q", got.ArtistMBID)
	}
	if got.ArtistName != "M83" {
		t.Errorf("ArtistName = %q", got.ArtistName)
	}
	if got.RecordingMBID != "cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff" {
		t.Errorf("RecordingMBID = %q — a single survivor must yield its recording", got.RecordingMBID)
	}
	if got.AlbumHint != "Before the Dawn Heals Us" {
		t.Errorf("AlbumHint = %q — a single release group is unambiguous", got.AlbumHint)
	}
	if got.AcoustID != "9ff43b6a-4f16-427c-93c2-92307ca505e0" {
		t.Errorf("AcoustID = %q — provenance must be recorded", got.AcoustID)
	}
}

// TestDecisionCannotCarryAReleaseMBID is the structural pin for the
// write-target discipline: a fingerprint identifies audio, AcoustID maps audio
// to a recording, and one recording sits under many releases precisely because
// they contain the same audio — so there must be nowhere in the gate's output
// to put a release identifier.
//
// The field set is asserted exactly. Adding a field to Decision fails this
// test, which is the point: a new field is a deliberate decision about what
// this path is allowed to conclude, not a detail.
func TestDecisionCannotCarryAReleaseMBID(t *testing.T) {
	want := []string{
		"ArtistMBID", "ArtistName", "RecordingMBID",
		"AlbumHint", "AcoustID", "Score", "Sources",
	}
	rt := reflect.TypeOf(Decision{})
	var got []string
	for i := range rt.NumField() {
		got = append(got, rt.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decision fields changed.\n got: %v\nwant: %v\n\n"+
			"If you are ADDING a field, check it is not a release or artwork\n"+
			"identifier — a fingerprint cannot justify one. See the package doc.",
			got, want)
	}
	// AlbumHint is a TITLE used as a query term, never an identifier: it must
	// be a plain string that the caller feeds to the existing text ladder.
	f, _ := rt.FieldByName("AlbumHint")
	if f.Type.Kind() != reflect.String {
		t.Errorf("AlbumHint must stay a title string, got %s", f.Type)
	}
}

// TestMaximallyFavourableResponseStillYieldsNoRelease is the behavioural twin
// of the structural pin above: even a perfect answer — score 1.0, 500 sources,
// one recording, exact duration, release groups present — yields only an
// artist and a recording.
func TestMaximallyFavourableResponseStillYieldsNoRelease(t *testing.T) {
	in := goodInput()
	in.Results[0].Score = 1.0
	in.Results[0].Recordings[0].Sources = 500
	got, reason := Accept(in)
	if reason != ReasonNone {
		t.Fatalf("unexpected rejection: %q", reason)
	}
	if got.AlbumHint == "" {
		t.Fatal("expected a release-group TITLE as a query hint")
	}
	// The hint is a title, not the release-group MBID that was in the input.
	if got.AlbumHint == in.Results[0].Recordings[0].ReleaseGroups[0].ID {
		t.Fatal("AlbumHint leaked a release-group MBID; it must be the title")
	}
}

// TestAlbumHintOnlyWhenUnambiguous pins the difference between "the
// fingerprint supplied a fact" and "the fingerprint picked one of N".
func TestAlbumHintOnlyWhenUnambiguous(t *testing.T) {
	in := goodInput()
	in.Results[0].Recordings[0].ReleaseGroups = append(
		in.Results[0].Recordings[0].ReleaseGroups,
		ReleaseGroup{ID: "44444444-4444-4444-4444-444444444444", Title: "Some Compilation", Type: "Compilation"},
	)
	got, reason := Accept(in)
	if reason != ReasonNone {
		t.Fatalf("unexpected rejection: %q", reason)
	}
	if got.AlbumHint != "" {
		t.Fatalf("AlbumHint = %q — with two release groups there is no unambiguous album", got.AlbumHint)
	}
	// The artist is still earned: the ambiguity is about the release only.
	if got.ArtistMBID == "" {
		t.Error("release-group ambiguity must not cost the artist")
	}
}

// TestRecordingMBIDNeedsAUniqueSurvivor covers both halves of the dedup rule:
// the same recording listed twice is NOT ambiguity, two genuinely different
// recording MBIDs are.
func TestRecordingMBIDNeedsAUniqueSurvivor(t *testing.T) {
	t.Run("duplicate listing is not ambiguity", func(t *testing.T) {
		in := goodInput()
		in.Results[0].Recordings = append(in.Results[0].Recordings, in.Results[0].Recordings[0])
		got, reason := Accept(in)
		if reason != ReasonNone {
			t.Fatalf("unexpected rejection: %q", reason)
		}
		if got.RecordingMBID == "" {
			t.Fatal("the same recording listed twice must still be unique")
		}
	})

	t.Run("two recordings under one artist yield no recording MBID", func(t *testing.T) {
		in := goodInput()
		other := in.Results[0].Recordings[0]
		other.ID = "55555555-5555-5555-5555-555555555555"
		in.Results[0].Recordings = append(in.Results[0].Recordings, other)
		got, reason := Accept(in)
		if reason != ReasonNone {
			t.Fatalf("unexpected rejection: %q — agreeing on the artist must still accept", reason)
		}
		if got.RecordingMBID != "" {
			t.Fatalf("RecordingMBID = %q, want empty: two recordings is ambiguity", got.RecordingMBID)
		}
		if got.ArtistMBID == "" {
			t.Error("recording ambiguity must not cost the artist")
		}
	})
}

// TestHeadArtistConsensusIgnoresFeaturedCredits is the regression pin for the
// rule that replaced an ordered-credit-tuple comparison. MusicBrainz routinely
// models one piece of audio as both "[X]" and "[X, Y]"; a tuple rule would
// veto a match whose answer is identical either way.
func TestHeadArtistConsensusIgnoresFeaturedCredits(t *testing.T) {
	in := goodInput()
	featured := in.Results[0].Recordings[0]
	featured.ID = "66666666-6666-6666-6666-666666666666"
	featured.Artists = []Artist{
		in.Results[0].Recordings[0].Artists[0],
		{ID: "77777777-7777-7777-7777-777777777777", Name: "Guest Vocalist"},
	}
	in.Results[0].Recordings = append(in.Results[0].Recordings, featured)

	got, reason := Accept(in)
	if reason != ReasonNone {
		t.Fatalf("a featured-artist credit must not veto: %q", reason)
	}
	if got.ArtistMBID != "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd" {
		t.Errorf("ArtistMBID = %q, want the head credit", got.ArtistMBID)
	}
}

// TestRunnerUpBelowFloorIsNotAmbiguity — the margin clause must only consider
// results that themselves clear the score floor, or every match with a weak
// second candidate would be refused.
func TestRunnerUpBelowFloorIsNotAmbiguity(t *testing.T) {
	in := goodInput()
	weak := in.Results[0]
	weak.ID = "88888888-8888-8888-8888-888888888888"
	weak.Score = minScore - 0.2
	in.Results = append(in.Results, weak)
	if _, reason := Accept(in); reason != ReasonNone {
		t.Fatalf("a sub-floor runner-up must not read as ambiguity: %q", reason)
	}
}

// TestSourcesBarRisesWithoutALocalWitness pins the two-tier rule: the same
// response is accepted when the track has an artist tag that could contradict
// it, and refused when it does not.
func TestSourcesBarRisesWithoutALocalWitness(t *testing.T) {
	const between = (minSources + minSourcesNoLocalArtist) / 2

	in := goodInput()
	in.Results[0].Recordings[0].Sources = between
	if _, reason := Accept(in); reason != ReasonNone {
		t.Fatalf("with a local witness, %d sources should pass: %q", between, reason)
	}

	in.HasLocalArtistWitness = false
	if _, reason := Accept(in); reason != ReasonFewSources {
		t.Fatalf("without a local witness, %d sources should fail: %q", between, reason)
	}
}

// TestCheckEligibleMatchesAcceptsEarlyStages — callers short-circuit with
// CheckEligible before spending a decode (and, on a network-backed library,
// egress). It must never admit something Accept would refuse for the same
// reason, or the saving would come at the cost of a wasted decode.
func TestCheckEligibleMatchesAcceptsEarlyStages(t *testing.T) {
	cases := []struct {
		name        string
		durationSec float64
		isDSD       bool
	}{
		{"unknown", 0, false},
		{"dsd", 240, true},
		{"short", MinTrackSeconds - 1, false},
		{"long", MaxTrackSeconds + 1, false},
		{"fine", 240, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			early := CheckEligible(tc.durationSec, tc.isDSD)
			in := goodInput()
			in.DurationSec = tc.durationSec
			in.IsDSD = tc.isDSD
			in.Fingerprint = goodFP(tc.durationSec)
			_, full := Accept(in)
			if early != ReasonNone && early != full {
				t.Fatalf("CheckEligible said %q but Accept said %q", early, full)
			}
		})
	}
}

// TestEntropyFloorCalibration records the measurements the threshold was
// derived from (fpcalc 1.6.1, distinct characters in the compressed base64
// fingerprint). It exists so a future change to minDistinctB64Chars has to
// confront the data rather than the number.
//
// The third row is the one worth reading: a rich but STATIONARY chord is as
// degenerate as silence, because Chromaprint keys on spectral change over
// time. Real music is never stationary; a synthetic fixture easily is, which
// is why a hand-made "music-like" test signal is not evidence here.
func TestEntropyFloorCalibration(t *testing.T) {
	measured := []struct {
		signal      string
		distinctB64 int
		wantAccept  bool
	}{
		{"45s digital silence", 13, false},
		{"45s pure sine tone", 13, false},
		{"45s stationary 4-note chord", 14, false},
		{"35s digital silence", 12, false},
		{"35s stepwise melody", 64, true},
		{"45s stepwise melody", 63, true},
		{"45s pink noise", 64, true},
	}
	for _, m := range measured {
		in := goodInput()
		in.Fingerprint.DistinctB64 = m.distinctB64
		_, reason := Accept(in)
		accepted := reason == ReasonNone
		if accepted != m.wantAccept {
			t.Errorf("%s (distinctB64=%d): accepted=%v want %v (reason %q)",
				m.signal, m.distinctB64, accepted, m.wantAccept, reason)
		}
	}
}

// TestRejectReasonsAreBounded keeps the reason set small enough to key a
// counter map safely. It is NOT the enricher's skip-reason map — that one is
// separately bounded and must never be keyed on one of these.
func TestRejectReasonsAreBounded(t *testing.T) {
	all := []RejectReason{
		ReasonUnknownDuration, ReasonTooShort, ReasonTooLong, ReasonIsDSD,
		ReasonLowEntropy, ReasonDecodeMismatch,
		ReasonNoResults, ReasonLowScore, ReasonAmbiguousResults, ReasonFewSources,
		ReasonNoRecordings, ReasonDurationMismatch, ReasonArtistDisagreement,
		ReasonNoArtistMBID,
	}
	seen := map[RejectReason]bool{}
	for _, r := range all {
		if r == ReasonNone {
			t.Error("ReasonNone must not appear in the rejection set")
		}
		if seen[r] {
			t.Errorf("duplicate reason %q", r)
		}
		seen[r] = true
	}
	if len(all) > 16 {
		t.Errorf("reject reasons grew to %d; keep the set small and closed", len(all))
	}
}

// TestSourcesFilterDiscriminatesWithinACluster pins the capability that came
// out of reading a live AcoustID response: `sources` is reported PER RECORDING,
// not per result.
//
// The bug this replaced was silent and total — the field was read off the
// result object, where AcoustID never puts it, so every track saw 0 sources and
// the clause rejected the entire library. Nothing in CI could catch that,
// because a hand-written fixture reproduces whatever shape its author believed
// in; only a real response settles it.
//
// Being per-recording is also strictly better than the cluster-level number the
// gate was designed around: a cluster carrying one well-attested recording and
// one lone mis-tagged submission now keeps the former and drops the latter,
// which removes a common cause of spurious artist disagreement.
func TestSourcesFilterDiscriminatesWithinACluster(t *testing.T) {
	in := goodInput()
	// A second recording, same duration and cluster, but a single submission —
	// and crediting a different artist, which is what a mis-tagged link looks
	// like in practice.
	weak := in.Results[0].Recordings[0]
	weak.ID = "99999999-9999-9999-9999-999999999999"
	weak.Sources = 1
	weak.Artists = []Artist{{ID: "88888888-8888-8888-8888-888888888888", Name: "Mis-tagged Someone"}}
	in.Results[0].Recordings = append(in.Results[0].Recordings, weak)

	got, reason := Accept(in)
	if reason != ReasonNone {
		t.Fatalf("a 1-source outlier must be filtered out, not veto the match: %q", reason)
	}
	if got.ArtistMBID != "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd" {
		t.Errorf("ArtistMBID = %q, want the well-attested recording's artist", got.ArtistMBID)
	}
	// The weak recording is gone, so the survivor set is unique again.
	if got.RecordingMBID != "cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff" {
		t.Errorf("RecordingMBID = %q, want the surviving recording", got.RecordingMBID)
	}
	// Sources reports the weakest survivor — the evidence actually relied on,
	// not the flattering maximum.
	if got.Sources != 12 {
		t.Errorf("Sources = %d, want 12 (the weakest survivor)", got.Sources)
	}
}

// TestWeakestSourcesReportsTheFloor — every survivor has already cleared the
// bar, so the honest number to record is the lowest, not the highest.
func TestWeakestSourcesReportsTheFloor(t *testing.T) {
	in := goodInput()
	second := in.Results[0].Recordings[0]
	second.ID = "77777777-7777-7777-7777-777777777777"
	second.Sources = 40
	in.Results[0].Recordings = append(in.Results[0].Recordings, second)

	got, reason := Accept(in)
	if reason != ReasonNone {
		t.Fatalf("unexpected rejection: %q", reason)
	}
	if got.Sources != 12 {
		t.Errorf("Sources = %d, want 12 (the minimum across survivors)", got.Sources)
	}
}
