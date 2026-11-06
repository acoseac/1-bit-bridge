package smartplaylist

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Every fixture of the match's tables goes through the store's real read
// too: tagged on a track, stored, and read back by MatchingTrackFeatures
// with MatchChristmas as its match, the regenerator's call. Each positive
// comes back, no negative does, every served row reaches the match (so
// nothing is filtered in SQL first), and a suppressed duplicate of a
// positive does not come back.
func TestEveryChristmasFixtureThroughTheStoresRead(t *testing.T) {
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	all := append(append(append([]xmasCase{}, xmasByRule...), xmasExclusions...), xmasForms...)
	for _, title := range xmasCarolSpellings {
		all = append(all, xmasCase{title, "Some Artist", "Some Album", "Pop", ChristmasByTitle, "the carol table"})
	}
	tracks := make([]*manifest.Track, 0, len(all)+1)
	want := map[string]xmasCase{}
	for i, c := range all {
		p := fmt.Sprintf("cases/%03d.flac", i)
		tracks = append(tracks, &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: c.title, Artist: c.artist, Album: c.album, Genre: c.genre,
		})
		if c.want != ChristmasNone {
			want[p] = c
		}
	}
	const suppressed = "cases/suppressed.flac"
	tracks = append(tracks, &manifest.Track{
		Path: suppressed, Size: 1, ModTime: time.Unix(1, 0),
		Title: "Silent Night", Artist: "Bing Crosby", Album: "Merry Christmas", Genre: "Christmas",
	})
	if err := s.UpsertTrackBatch(ctx, tracks); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}
	if _, err := s.ApplyDupeStamps(ctx, []manifest.DupeStamp{
		{Path: suppressed, GroupID: "g", Tier: string(dupes.TierSameFormat), Suppressed: true},
	}); err != nil {
		t.Fatalf("ApplyDupeStamps: %v", err)
	}

	seen := 0
	rows, err := s.MatchingTrackFeatures(ctx, func(r *manifest.TrackFeatureRow) bool {
		seen++
		return MatchChristmas(r.Title, r.Artist, r.Album, r.Genre) != ChristmasNone
	})
	if err != nil {
		t.Fatalf("MatchingTrackFeatures: %v", err)
	}
	if seen != len(all) {
		t.Errorf("the read handed %d rows to the match, want every served row, %d", seen, len(all))
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Path] = true
	}
	for p, c := range want {
		if !got[p] {
			t.Errorf("%s (title %q, artist %q, album %q, genre %q: %s) qualifies in Go and not through the store",
				p, c.title, c.artist, c.album, c.genre, c.why)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s came back from the store and is no positive fixture", p)
		}
	}
	if len(want) < 100 {
		t.Fatalf("only %d positive fixtures; the tables shrank", len(want))
	}
}
