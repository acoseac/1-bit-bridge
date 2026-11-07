package smartplaylistgen

// The Christmas family fills from what every client can play first (round 4
// of backlog B489). Its pool is every served track the match accepts, WavPack
// and Monkey's Audio files among them, and its cap is 100: shuffled and
// capped as one pool, a thousand .wv files left the fifteen FLACs a client
// that declares neither format can play out of the family. So the tracks
// every client is served are spread and capped first, and the two gated
// formats take what is left of the cap in turn. Each item stores its album
// too, for the response's floor (not on the wire).

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// storedXmasItem is a cached Christmas item as the blob holds it, its album
// included.
type storedXmasItem struct {
	Path  string `json:"path"`
	Album string `json:"album"`
}

func storedXmasItems(t *testing.T, row *manifest.StoredSmartPlaylist) []storedXmasItem {
	t.Helper()
	var items []storedXmasItem
	if err := json.Unmarshal(row.ItemsJSON, &items); err != nil {
		t.Fatalf("decode the christmas items: %v", err)
	}
	return items
}

// seedXmasFiles is seedXmas for files of ext, every track in the genre
// Christmas. It returns their paths.
func seedXmasFiles(t *testing.T, s *manifest.Store, album, ext string, n int, artistOf func(int) string) []string {
	t.Helper()
	tracks := make([]*manifest.Track, 0, n)
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s/%02d%s", album, i+1, ext)
		tracks = append(tracks, &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("%s %d", album, i+1), Artist: artistOf(i), Album: album,
			Genre: "Christmas", Duration: fp(180),
		})
		paths = append(paths, p)
	}
	if err := s.UpsertTrackBatch(context.Background(), tracks); err != nil {
		t.Fatalf("UpsertTrackBatch(%s): %v", album, err)
	}
	return paths
}

// wavpackSingles stores n WavPack Christmas files, each a single of its own
// (album "Single NNNN") by a singer of its own, and returns their paths.
func wavpackSingles(t *testing.T, s *manifest.Store, n int) []string {
	t.Helper()
	tracks := make([]*manifest.Track, 0, n)
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("WavPack/%04d.wv", i)
		tracks = append(tracks, &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("Song %04d", i), Artist: fmt.Sprintf("Singer %04d", i),
			Album: fmt.Sprintf("Single %04d", i), Genre: "Christmas", Duration: fp(180),
		})
		paths = append(paths, p)
	}
	if err := s.UpsertTrackBatch(context.Background(), tracks); err != nil {
		t.Fatalf("UpsertTrackBatch(WavPack singles): %v", err)
	}
	return paths
}

// regenerateXmas runs the regenerator on 20 November 2026 with analysis off
// and returns the cached Christmas family, failing when there is none.
func regenerateXmas(t *testing.T, s *manifest.Store) *manifest.StoredSmartPlaylist {
	t.Helper()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if _, err := Regenerate(context.Background(), s, DefaultOptions(now.UnixNano(), false)); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	row := christmasRow(t, s)
	if row == nil {
		_, kinds := loadKinds(t, s)
		t.Fatalf("no christmas family; cached kinds %v", kinds)
	}
	return row
}

// The review's library: fifteen FLACs, five each by three choirs on three
// albums, beside a thousand WavPack singles. The family holds the fifteen
// FLACs first, then 85 of the WavPack files, and every item stores its album.
func TestRegenerate_ChristmasStoresWhatEveryClientCanPlayFirst(t *testing.T) {
	s := openGenStore(t)
	var flacs []string
	for i, a := range []string{"Carols A", "Carols B", "Carols C"} {
		flacs = append(flacs, seedXmas(t, s, a, 5, by(fmt.Sprintf("Choir %d", i)), "Christmas", nil)...)
	}
	wavpackSingles(t, s, 1000)
	items := storedXmasItems(t, regenerateXmas(t, s))
	if len(items) != 100 {
		t.Fatalf("the family holds %d items, want the cap, 100", len(items))
	}
	var head []string
	for _, it := range items[:15] {
		head = append(head, it.Path)
	}
	slices.Sort(head)
	if want := slices.Sorted(slices.Values(flacs)); !slices.Equal(head, want) {
		t.Errorf("the first 15 items are %v, want the 15 FLACs every client can play %v", head, want)
	}
	for i, it := range items[15:] {
		if path.Ext(it.Path) != ".wv" {
			t.Errorf("item %d is %s, want a WavPack file after the FLACs", 15+i, it.Path)
		}
	}
	for _, it := range items {
		want := path.Dir(it.Path) // a FLAC's folder is its album
		if path.Ext(it.Path) == ".wv" {
			var n int
			if _, err := fmt.Sscanf(path.Base(it.Path), "%04d.wv", &n); err != nil {
				t.Fatalf("%s: %v", it.Path, err)
			}
			want = fmt.Sprintf("Single %04d", n)
		}
		if it.Album != want {
			t.Errorf("%s stores the album %q, want %q", it.Path, it.Album, want)
			break
		}
	}
}

// A thousand WavPack singles cannot crowd fifteen Monkey's Audio files out:
// with no FLAC, the two formats take the cap in turn, so all fifteen .ape
// files are in the family and, while both last, no two neighbours share a
// format.
func TestRegenerate_ChristmasSharesTheGatedRoomBetweenTheFormatsInTurn(t *testing.T) {
	s := openGenStore(t)
	var apes []string
	for i, a := range []string{"APE Carols A", "APE Carols B", "APE Carols C"} {
		apes = append(apes, seedXmasFiles(t, s, a, ".ape", 5, by(fmt.Sprintf("Choir %d", i)))...)
	}
	wavpackSingles(t, s, 1000)
	items := storedXmasItems(t, regenerateXmas(t, s))
	if len(items) != 100 {
		t.Fatalf("the family holds %d items, want the cap, 100", len(items))
	}
	var got []string
	for _, it := range items {
		if path.Ext(it.Path) == ".ape" {
			got = append(got, it.Path)
		}
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(apes)); !slices.Equal(got, want) {
		t.Fatalf("the family holds the .ape files %v, want all 15 %v", got, want)
	}
	for i := 1; i < 30; i++ {
		if path.Ext(items[i].Path) == path.Ext(items[i-1].Path) {
			t.Fatalf("items %d and %d are both %s files, want the formats in turn while both last: %v",
				i-1, i, path.Ext(items[i].Path), items[:30])
		}
	}
}
