package manifest

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// TestSmartPlaylistPoolsExcludeSuppressed pins the persisted-path
// hazard: smart-playlist snapshots serve raw paths to iOS, so a
// suppressed duplicate must never enter a pool — neither via the
// analyzed candidate pool nor via history-path hydration.
func TestSmartPlaylistPoolsExcludeSuppressed(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	mustUpsertTrack(t, s, &Track{Path: "a/kept.flac", Title: "Kept", Artist: "A"})
	mustUpsertTrack(t, s, &Track{Path: "a/suppressed.flac", Title: "Sup", Artist: "A"})
	for _, p := range []string{"a/kept.flac", "a/suppressed.flac"} {
		mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: p, KeyRoot: spInt(5), KeyMode: "major"})
	}
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "a/suppressed.flac", GroupID: "g", Tier: string(dupes.TierSameFormat), Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}

	pool, err := s.AnalyzedTrackFeatures(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].Path != "a/kept.flac" {
		t.Fatalf("analyzed pool must exclude suppressed rows, got %+v", pool)
	}

	// History hydration drops the suppressed path via the same shared
	// predicate (TrackFeaturesForPaths' drop-missing contract).
	feats, err := s.TrackFeaturesForPaths(ctx, []string{"a/kept.flac", "a/suppressed.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if len(feats) != 1 || feats[0].Path != "a/kept.flac" {
		t.Fatalf("history hydration must drop suppressed paths, got %+v", feats)
	}
}
