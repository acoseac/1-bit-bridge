package season

import (
	"fmt"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// pickItem is a stored item as Pick reads one.
type pickItem struct{ path, album, artist string }

func pickPath(it pickItem) string   { return it.path }
func pickAlbum(it pickItem) string  { return it.album }
func pickArtist(it pickItem) string { return it.artist }

// run is n items of ext under dir, the i-th on album(i) by artist(i).
func run(dir, ext string, n int, album, artist func(int) string) []pickItem {
	out := make([]pickItem, n)
	for i := range out {
		out[i] = pickItem{fmt.Sprintf("%s/%03d%s", dir, i, ext), album(i), artist(i)}
	}
	return out
}

func pathsOfPick(items []pickItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.path
	}
	return out
}

// TestPickKeepsTheFloorWheneverTheItemsCan is the response's pick of one
// request's view (round 5 of backlog B489): the items the view takes, those
// every client is served first, then the gated formats in turn; the first of
// each of the first three albums, or failing that of three artists, ahead of
// the cap of 100; and the floor asked of the result.
func TestPickKeepsTheFloorWheneverTheItemsCan(t *testing.T) {
	both := formatgate.All()
	t.Run("the view's formats, served first, the gated ones in turn", func(t *testing.T) {
		items := append(append(run("W", ".wv", 3, named("Wa"), named("Wx")), run("F", ".flac", 3, named("F1", "F2", "F3"), named("Fx"))...),
			run("A", ".ape", 3, named("Aa"), named("Ax"))...)
		got, _ := Pick(items, pickPath, pickAlbum, pickArtist, both)
		want := []string{"F/000.flac", "F/001.flac", "F/002.flac", "W/000.wv", "A/000.ape", "W/001.wv", "A/001.ape", "W/002.wv", "A/002.ape"}
		if !slices.Equal(pathsOfPick(got), want) {
			t.Errorf("picked %v, want %v", pathsOfPick(got), want)
		}
		got, _ = Pick(items, pickPath, pickAlbum, pickArtist, formatgate.Set{})
		if !slices.Equal(pathsOfPick(got), want[:3]) {
			t.Errorf("a view that declares nothing picked %v, want the FLACs alone", pathsOfPick(got))
		}
	})
	t.Run("three albums ahead of the cap", func(t *testing.T) {
		// One choir: 110 of one album, then one each of two more, past the cap.
		items := append(run("C", ".flac", 110, named("A"), named("Choir")),
			pickItem{"C/B.flac", "B", "Choir"}, pickItem{"C/C.flac", "C", "Choir"})
		got, ok := Pick(items, pickPath, pickAlbum, pickArtist, both)
		if !ok || len(got) != MaxTracks || got[1].path != "C/B.flac" || got[2].path != "C/C.flac" || got[0].path != "C/000.flac" {
			t.Errorf("ok %v, %d picked beginning %v; want the cap, the first of each album first", ok, len(got), pathsOfPick(got[:min(3, len(got))]))
		}
	})
	t.Run("failing albums, three artists ahead of the cap", func(t *testing.T) {
		// Two albums, 120 by two artists, then one gated track by a third.
		items := append(run("D", ".flac", 120, func(i int) string { return []string{"One", "Two"}[i/60] },
			func(i int) string { return []string{"P", "Q"}[i/60] }), pickItem{"S/001.wv", "", "R"})
		got, ok := Pick(items, pickPath, pickAlbum, pickArtist, both)
		if !ok || len(got) != MaxTracks || !slices.Contains(pathsOfPick(got), "S/001.wv") {
			t.Errorf("ok %v, %d picked; want the cap, the third artist's track among them", ok, len(got))
		}
		if _, ok := Pick(items, pickPath, pickAlbum, pickArtist, formatgate.Set{}); ok {
			t.Error("a view that declares nothing cleared the floor with two artists on two albums")
		}
	})
	t.Run("no spread to keep, below the floor", func(t *testing.T) {
		items := run("X", ".flac", 40, named("Box"), named("One"))
		got, ok := Pick(items, pickPath, pickAlbum, pickArtist, both)
		if ok || !slices.Equal(pathsOfPick(got), pathsOfPick(items)) {
			t.Errorf("ok %v, picked %v; want the items as they were and below the floor", ok, pathsOfPick(got))
		}
	})
	t.Run("fourteen is below the floor whatever the spread", func(t *testing.T) {
		if _, ok := Pick(run("Y", ".flac", 14, named("A", "B", "C"), named("P", "Q", "R")), pickPath, pickAlbum, pickArtist, both); ok {
			t.Error("fourteen tracks cleared the floor")
		}
	})
}

// TestKeepSpreadKeepsBothSpreadsAheadOfTheCut is one format as the
// regenerator stores it: the first of each of its first three albums and of
// its first three artists ahead of its cut at 100, both whether or not the
// format reaches either alone, since another format may complete it in a view
// that declares both.
func TestKeepSpreadKeepsBothSpreadsAheadOfTheCut(t *testing.T) {
	// 150 tracks: the first 140 of album X by P, then one of Y by P, then
	// nine of Z by Q.
	items := run("F", ".flac", 140, named("X"), named("P"))
	items = append(items, pickItem{"F/Y.flac", "Y", "P"})
	items = append(items, run("G", ".flac", 9, named("Z"), named("Q"))...)
	got := KeepSpread(items, pickAlbum, pickArtist)
	if len(got) != MaxTracks {
		t.Fatalf("kept %d, want the cap, 100", len(got))
	}
	paths := pathsOfPick(got)
	for _, p := range []string{"F/Y.flac", "G/000.flac"} {
		if !slices.Contains(paths, p) {
			t.Errorf("kept no %s, the first of its album (or artist)", p)
		}
	}
	if !slices.Equal(paths[:3], []string{"F/000.flac", "F/Y.flac", "G/000.flac"}) {
		t.Errorf("kept %v first, want the spread in the order the items give it", paths[:3])
	}
}
