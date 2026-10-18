package manifest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The analyzed pool used to be ORDER BY path LIMIT, a permanent prefix.
// The week seed has to move the window, stay deterministic, and be able to
// reach every keyed track.
func TestAnalyzedTrackFeatures_WeekSeedMovesTheWindow(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	const n = 12
	const limit = 4
	all := make([]string, n)
	for i := 0; i < n; i++ {
		all[i] = fmt.Sprintf("/lib/%02d.flac", i)
		mustUpsertTrack(t, s, &Track{Path: all[i], Genre: "Jazz"})
		mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: all[i], KeyRoot: spInt(i % 12), KeyMode: "major"})
	}
	window := func(seed uint64) []string {
		t.Helper()
		rows, err := s.AnalyzedTrackFeatures(ctx, "", seed, limit)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if len(rows) != limit {
			t.Fatalf("seed %d: want %d rows, got %d", seed, limit, len(rows))
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Path
		}
		return out
	}
	a := window(7)
	b := window(7)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("same seed diverged: %v vs %v", a, b)
	}
	// A week seed can have its high bit set. database/sql will not bind
	// that as a uint64; the query passes the int64 bit pattern.
	_ = window(1 << 63)

	distinct := map[string]struct{}{}
	seen := map[string]struct{}{}
	for seed := uint64(1); seed <= 80; seed++ {
		w := window(seed)
		distinct[strings.Join(w, "\n")] = struct{}{}
		for _, p := range w {
			seen[p] = struct{}{}
		}
	}
	if len(distinct) < 2 {
		t.Fatal("different week seeds returned the same window")
	}
	for _, p := range all {
		if _, ok := seen[p]; !ok {
			t.Errorf("keyed track %s was never in the window", p)
		}
	}
}

// Deep cuts kept each artist's three lowest paths and then the alphabetical
// first 200 rows. Both levels have to follow the week seed so a later track
// and a later artist can rotate in. The per-artist cap and the pool limit
// stay.
func TestLovedArtistDeepCuts_WeekSeedRotatesArtistsAndTracks(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	artists := []string{"Aaaa", "Bbbb", "Cccc", "Dddd"}
	var eligible []string
	for _, artist := range artists {
		played := "/" + artist + "/played.flac"
		mustUpsertTrack(t, s, &Track{Path: played, Artist: artist})
		for d := int64(1); d <= 3; d++ {
			mustInsertHistory(t, s, PlaybackHistoryRow{
				Path: played, StartedAt: now - d*day, DurationUsed: 200,
			})
		}
		for i := 0; i < 5; i++ {
			p := fmt.Sprintf("/%s/t%02d.flac", artist, i)
			mustUpsertTrack(t, s, &Track{Path: p, Artist: artist})
			eligible = append(eligible, p)
		}
	}
	const perArtist = 3
	const limit = 6
	window := func(seed uint64) []string {
		t.Helper()
		rows, err := s.LovedArtistDeepCuts(ctx,
			now-30*day, 3, now-90*day, 30, perArtist, limit, seed)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if len(rows) != limit {
			t.Fatalf("seed %d: want %d rows, got %d (%+v)", seed, limit, len(rows), rows)
		}
		per := map[string]int{}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Path
			for _, artist := range artists {
				if strings.HasPrefix(r.Path, "/"+artist+"/") {
					per[artist]++
				}
			}
		}
		for artist, n := range per {
			if n > perArtist {
				t.Fatalf("seed %d: %s contributed %d, cap is %d", seed, artist, n, perArtist)
			}
		}
		return out
	}
	a := window(7)
	b := window(7)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("same seed diverged: %v vs %v", a, b)
	}
	distinct := map[string]struct{}{}
	seen := map[string]struct{}{}
	for seed := uint64(1); seed <= 200; seed++ {
		w := window(seed)
		distinct[strings.Join(w, "\n")] = struct{}{}
		for _, p := range w {
			seen[p] = struct{}{}
		}
	}
	if len(distinct) < 2 {
		t.Fatal("different week seeds returned the same deep-cut window")
	}
	for _, p := range eligible {
		if _, ok := seen[p]; !ok {
			t.Errorf("eligible track %s was never in the window", p)
		}
	}
}
