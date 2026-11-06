package manifest

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// MatchingTrackFeatures hands every served row to its match, keeps only the
// rows the match accepts, and carries each kept row's scalars as
// trackFeatureSelect reads them: the tags' title, artist, album, genre,
// duration, ReplayGain and rate, and the analysis row's key and tempo where
// there is one. A suppressed duplicate never reaches the match, and the
// bytes of a tag reach it as stored, so a match in Go can fold what SQL
// cannot (B750: the Christmas family's read).
func TestMatchingTrackFeaturesHandsEveryServedRowToItsMatch(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "keep/tagged.flac", Title: "Noël", Artist: "A", Album: "KOLĘDY",
		Genre: "Christmas", Duration: spFloat(181), ReplayGainTrackDB: spFloat(-7.5), SampleRate: spFloat(96000)})
	mustUpsertTrack(t, s, &Track{Path: "keep/analyzed.flac", Title: "Noe\u0308l", Artist: "B", Album: "Noël Acadien"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "keep/analyzed.flac", KeyRoot: spInt(2), KeyMode: "minor", BPM: spInt(96)})
	mustUpsertTrack(t, s, &Track{Path: "drop/other.flac", Title: "Other", Artist: "C", Album: "Other"})
	mustUpsertTrack(t, s, &Track{Path: "keep/suppressed.flac", Title: "Noël", Artist: "A", Album: "KOLĘDY"})
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "keep/suppressed.flac", GroupID: "g", Tier: string(dupes.TierSameFormat), Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}

	var seen []string
	rows, err := s.MatchingTrackFeatures(ctx, func(r *TrackFeatureRow) bool {
		seen = append(seen, r.Path)
		return strings.HasPrefix(r.Path, "keep/")
	})
	if err != nil {
		t.Fatalf("MatchingTrackFeatures: %v", err)
	}
	sort.Strings(seen)
	if strings.Join(seen, " ") != "drop/other.flac keep/analyzed.flac keep/tagged.flac" {
		t.Fatalf("the match saw %v, want every served row and no suppressed one", seen)
	}
	byPath := map[string]TrackFeatureRow{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	if len(byPath) != 2 {
		t.Fatalf("kept %d rows, want the two the match accepted: %+v", len(byPath), rows)
	}
	tagged := byPath["keep/tagged.flac"]
	if tagged.Title != "Noël" || tagged.Album != "KOLĘDY" || tagged.Genre != "Christmas" || tagged.Artist != "A" {
		t.Errorf("the tagged row's text is %+v, want its tags as stored", tagged)
	}
	if tagged.Duration == nil || *tagged.Duration != 181 || tagged.ReplayGainTrackDB == nil ||
		*tagged.ReplayGainTrackDB != -7.5 || tagged.SampleRate == nil || *tagged.SampleRate != 96000 {
		t.Errorf("the tagged row's scalars are %+v, want 181 s, -7.5 dB and 96000 Hz from its tags", tagged)
	}
	analyzed := byPath["keep/analyzed.flac"]
	if analyzed.KeyRoot == nil || *analyzed.KeyRoot != 2 || analyzed.KeyMode != "minor" ||
		analyzed.BPM == nil || *analyzed.BPM != 96 {
		t.Errorf("the analyzed row is %+v, want its key and tempo from the analysis row", analyzed)
	}
	if analyzed.Title != "Noe\u0308l" {
		t.Errorf("the decomposed title reached the match as %q, want its bytes as written", analyzed.Title)
	}

	if _, err := s.MatchingTrackFeatures(ctx, nil); err == nil {
		t.Error("a nil match was taken")
	}
}
