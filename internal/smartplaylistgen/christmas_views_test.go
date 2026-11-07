package smartplaylistgen

// The Christmas family as the regenerator stores it (backlog B489). Its pool
// is every served track the match accepts, WavPack and Monkey's Audio files
// among them, and its cap is 100: shuffled and capped as one pool, a
// thousand .wv files left the fifteen FLACs a client that declares neither
// format can play out of the family (round 4), and a cap over the tracks
// every client is served could cut away the album or artist that qualified a
// view's pool (round 5). So each format is stored up to the cap on its own,
// in the season's order spread by artist, the first track of each of its
// first three albums and of its first three artists ahead of the cut, and
// the response picks each request's view from what is stored. Each item
// stores its album too, for the response's floor (not on the wire).

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
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
	return regenerateXmasAt(t, s, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
}

// regenerateXmasAt is regenerateXmas at now.
func regenerateXmasAt(t *testing.T, s *manifest.Store, now time.Time) *manifest.StoredSmartPlaylist {
	t.Helper()
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
// albums, beside a thousand WavPack singles. The family stores each format up
// to the cap on its own (round 5): the fifteen FLACs first, then 100 of the
// WavPack files, and every item stores its album.
func TestRegenerate_ChristmasStoresWhatEveryClientCanPlayFirst(t *testing.T) {
	s := openGenStore(t)
	var flacs []string
	for i, a := range []string{"Carols A", "Carols B", "Carols C"} {
		flacs = append(flacs, seedXmas(t, s, a, 5, by(fmt.Sprintf("Choir %d", i)), "Christmas", nil)...)
	}
	wavpackSingles(t, s, 1000)
	items := storedXmasItems(t, regenerateXmas(t, s))
	if len(items) != 115 {
		t.Fatalf("the family stores %d items, want the 15 FLACs and 100 of the WavPack files", len(items))
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
// each format is stored up to the cap on its own (round 5), so all fifteen
// .ape files are in the family beside 100 of the WavPack files, and a view
// that declares both takes the two in turn at the response
// (TestAThousandWavPackFilesLeaveTheAPEViewItsFifteen).
func TestRegenerate_ChristmasSharesTheGatedRoomBetweenTheFormatsInTurn(t *testing.T) {
	s := openGenStore(t)
	var apes []string
	for i, a := range []string{"APE Carols A", "APE Carols B", "APE Carols C"} {
		apes = append(apes, seedXmasFiles(t, s, a, ".ape", 5, by(fmt.Sprintf("Choir %d", i)))...)
	}
	wavpackSingles(t, s, 1000)
	items := storedXmasItems(t, regenerateXmas(t, s))
	var got []string
	wv := 0
	for _, it := range items {
		switch path.Ext(it.Path) {
		case ".ape":
			got = append(got, it.Path)
		case ".wv":
			wv++
		}
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(apes)); !slices.Equal(got, want) {
		t.Fatalf("the family stores the .ape files %v, want all 15 %v", got, want)
	}
	if wv != 100 {
		t.Fatalf("the family stores %d WavPack files, want the cap, 100, of the thousand", wv)
	}
}

// oneChoirFixture is the first review's library (round 5 of backlog B489):
// one choir's 110 tracks of one album, one of a second and one of a third,
// all FLAC, the paths the season order is pinned by.
func oneChoirFixture() []*manifest.Track {
	var out []*manifest.Track
	add := func(p, album string) {
		out = append(out, &manifest.Track{Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: "Carol " + p, Artist: "The Choir", Album: album, Genre: "Christmas", Duration: fp(180)})
	}
	for i := 1; i <= 110; i++ {
		add(fmt.Sprintf("Choir/A/%03d.flac", i), "Album A")
	}
	add("Choir/B/001.flac", "Album B")
	add("Choir/C/001.flac", "Album C")
	return out
}

// oneChoirSeason is a season whose order puts the second or the third album's
// one track past the cap, which the test asks of the engine's own order
// (requireOneChoirPrecondition): most seasons leave both inside it.
const oneChoirSeason = 2029

// requireOneChoirPrecondition fails the test unless, in the season's order
// the family takes the FLACs in, Choir/B/001.flac or Choir/C/001.flac falls
// past position 100: a cap over that order alone loses an album.
func requireOneChoirPrecondition(t *testing.T, tracks []*manifest.Track) {
	t.Helper()
	pool := make([]smartplaylist.TrackFeature, 0, len(tracks))
	for _, tr := range tracks {
		pool = append(pool, smartplaylist.TrackFeature{Path: tr.Path, Artist: tr.Artist, Album: tr.Album})
	}
	order := smartplaylist.ChristmasOrder(pool, smartplaylist.SeedFromSeason(oneChoirSeason),
		smartplaylist.DefaultOptions(false).ChristmasPerArtistCap)
	at := map[string]int{}
	for i, f := range order {
		at[f.Path] = i
	}
	if at["Choir/B/001.flac"] < 100 && at["Choir/C/001.flac"] < 100 {
		t.Fatalf("precondition: in the %d season the second album's track is at %d and the third's at %d, "+
			"both inside the cap: pin another season", oneChoirSeason, at["Choir/B/001.flac"], at["Choir/C/001.flac"])
	}
}

// TestRegenerate_ChristmasStoresWhatEachViewNeedsToClearTheFloor (round 5 of
// backlog B489, two reviewers): the cap must not erase the spread that
// qualified a view's pool. Each format is stored up to the cap with the first
// track of each of its first three albums and of its first three artists
// moved ahead of the cut, so the response can pick each view's floor from
// what is stored: the one choir's second and third albums though the season
// puts one past the cap; ten WavPack singles beside 120 FLACs of two artists;
// the one WavPack and the one Monkey's Audio file beside 99 FLACs.
func TestRegenerate_ChristmasStoresWhatEachViewNeedsToClearTheFloor(t *testing.T) {
	stored := func(t *testing.T, tracks []*manifest.Track, season int) map[string]bool {
		t.Helper()
		s := openGenStore(t)
		if err := s.UpsertTrackBatch(context.Background(), tracks); err != nil {
			t.Fatal(err)
		}
		items := storedXmasItems(t, regenerateXmasAt(t, s, time.Date(season, 11, 20, 12, 0, 0, 0, time.UTC)))
		out := map[string]bool{}
		for _, it := range items {
			out[it.Path] = true
		}
		return out
	}
	t.Run("one choir, three albums, the second or third past the cap", func(t *testing.T) {
		tracks := oneChoirFixture()
		requireOneChoirPrecondition(t, tracks)
		got := stored(t, tracks, oneChoirSeason)
		for _, p := range []string{"Choir/B/001.flac", "Choir/C/001.flac"} {
			if !got[p] {
				t.Errorf("the family does not store %s, the only track of its album", p)
			}
		}
	})
	t.Run("120 FLACs by two artists on two albums beside ten WavPack singles", func(t *testing.T) {
		tracks := twoArtistFLACs(120)
		tracks = append(tracks, wavpackSingleTracks(10)...)
		got := stored(t, tracks, 2026)
		for _, tr := range tracks[120:] {
			if !got[tr.Path] {
				t.Errorf("the family does not store %s, one of the WavPack view's other artists", tr.Path)
			}
		}
	})
	t.Run("99 FLACs by two artists, one WavPack file, one Monkey's Audio file", func(t *testing.T) {
		tracks := twoArtistFLACs(99)
		tracks = append(tracks, singleTrack("Gated/one.wv", "WavPack Singer", "WavPack Album"),
			singleTrack("Gated/one.ape", "APE Singer", "APE Album"))
		got := stored(t, tracks, 2026)
		for _, p := range []string{"Gated/one.wv", "Gated/one.ape"} {
			if !got[p] {
				t.Errorf("the family does not store %s", p)
			}
		}
	})
}

// twoArtistFLACs is n Christmas FLACs, the first half by one artist on one
// album, the rest by another on another.
func twoArtistFLACs(n int) []*manifest.Track {
	var out []*manifest.Track
	for i := 0; i < n; i++ {
		artist, album := "Duo One", "Duo Album One"
		if i >= n/2 {
			artist, album = "Duo Two", "Duo Album Two"
		}
		out = append(out, &manifest.Track{Path: fmt.Sprintf("Duo/%03d.flac", i), Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("Duo %d", i), Artist: artist, Album: album, Genre: "Christmas", Duration: fp(180)})
	}
	return out
}

// wavpackSingleTracks is n WavPack Christmas files, each a single of its own
// by a singer of its own.
func wavpackSingleTracks(n int) []*manifest.Track {
	var out []*manifest.Track
	for i := 0; i < n; i++ {
		out = append(out, singleTrack(fmt.Sprintf("Singles/%02d.wv", i), fmt.Sprintf("Single Singer %02d", i), fmt.Sprintf("Single %02d", i)))
	}
	return out
}

// singleTrack is one Christmas track at p by artist on album.
func singleTrack(p, artist, album string) *manifest.Track {
	return &manifest.Track{Path: p, Size: 1, ModTime: time.Unix(1, 0), Title: "Song " + p,
		Artist: artist, Album: album, Genre: "Christmas", Duration: fp(180)}
}
