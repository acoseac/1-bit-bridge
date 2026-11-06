package smartplaylistgen

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// christmasRow is the cached Christmas family, or nil when the run wrote
// none. The kind is spelled out: it is the wire string.
func christmasRow(t *testing.T, s *manifest.Store) *manifest.StoredSmartPlaylist {
	t.Helper()
	rows, err := s.LoadSmartPlaylists(context.Background())
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	for i := range rows {
		if rows[i].Kind == "christmas" {
			return &rows[i]
		}
	}
	return nil
}

// cachedPaths decodes a flat family's items into their paths, in order.
func cachedPaths(t *testing.T, row *manifest.StoredSmartPlaylist) []string {
	t.Helper()
	var items []manifest.SmartPlaylistItem
	if err := json.Unmarshal(row.ItemsJSON, &items); err != nil {
		t.Fatalf("decode %s items: %v", row.Slug, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

// A Christmas album nobody has played, hearted or analyzed makes the family,
// with analysis off. Every other family follows history, hearts or analysis,
// so a filter over their candidates would never see this album, which is the
// one the mix is for.
func TestRegenerate_AnUnheardUnanalyzedChristmasAlbumMakesTheFamily(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	performers := []string{"The Cambridge Singers", "Bing Crosby", "Ella Fitzgerald", "Nat King Cole"}
	var want []string
	for i := 0; i < 16; i++ {
		p := fmt.Sprintf("Various Artists/A Christmas Gathering/%02d.flac", i+1)
		want = append(want, p)
		if err := s.UpsertTrack(ctx, &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("Song %d", i+1), Artist: performers[i%len(performers)],
			AlbumArtist: "Various Artists", Album: "A Christmas Gathering", Genre: "Christmas",
			Duration: fp(180),
		}); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", p, err)
		}
	}

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), false)); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	row := christmasRow(t, s)
	if row == nil {
		_, kinds := loadKinds(t, s)
		t.Fatalf("no christmas family from a 16-track Christmas album by 4 performers "+
			"(no plays, no hearts, no analysis); cached kinds %v", kinds)
	}
	got := cachedPaths(t, row)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("christmas family holds %v, want the album's 16 tracks %v", got, want)
	}
}
