package season

import "testing"

// floorTrack is a track as MeetsFloor reads one.
type floorTrack struct{ album, artist string }

func floorAlbum(t floorTrack) string  { return t.album }
func floorArtist(t floorTrack) string { return t.artist }

// tracksOf is n tracks, the i-th on album(i) by artist(i).
func tracksOf(n int, album, artist func(i int) string) []floorTrack {
	out := make([]floorTrack, n)
	for i := range out {
		out[i] = floorTrack{album(i), artist(i)}
	}
	return out
}

func named(names ...string) func(int) string {
	return func(i int) string { return names[i%len(names)] }
}

// The owner's rule, as one predicate both layers ask (round 4 of backlog
// B489): 15 tracks at least, from 3 albums or 3 artists, each named as the
// diversity caps name them; an empty album or artist names nothing, so items
// cached without their album are judged on their artists.
func TestMeetsFloorIsTheOwnersRule(t *testing.T) {
	cases := []struct {
		name   string
		tracks []floorTrack
		want   bool
	}{
		{"14 tracks, 3 albums, 3 artists", tracksOf(14, named("A", "B", "C"), named("X", "Y", "Z")), false},
		{"15 tracks, 3 albums, 3 artists", tracksOf(15, named("A", "B", "C"), named("X", "Y", "Z")), true},
		{"15 tracks, one artist across three albums", tracksOf(15, named("A", "B", "C"), named("X")), true},
		{"15 tracks, one album by three artists", tracksOf(15, named("A"), named("X", "Y", "Z")), true},
		{"15 tracks, two artists on two albums", tracksOf(15, named("A", "B"), named("X", "Y")), false},
		{"40 tracks, one album by one artist", tracksOf(40, named("Box"), named("X")), false},
		{"15 tracks, one artist, no album known", tracksOf(15, named(""), named("X")), false},
		{"15 tracks, three artists, no album known", tracksOf(15, named(""), named("X", "Y", "Z")), true},
		{"20 tracks, nothing known", tracksOf(20, named(""), named("")), false},
		{"15 tracks, one artist spelled three ways", tracksOf(15, named("A"), named("Bing Crosby", "bing crosby", " BING CROSBY ")), false},
		{"15 tracks, one album spelled three ways", tracksOf(15, named("Carols", "CAROLS", " carols"), named("X")), false},
		{"no tracks", nil, false},
	}
	for _, c := range cases {
		if got := MeetsFloor(c.tracks, floorAlbum, floorArtist); got != c.want {
			t.Errorf("%s: MeetsFloor %v, want %v", c.name, got, c.want)
		}
	}
	if MinTracks != 15 || MinSpread != 3 {
		t.Errorf("the floor is %d tracks from %d albums or artists, want the owner's 15 and 3", MinTracks, MinSpread)
	}
	// The spread may come from the last track: the predicate reads them all.
	late := append(tracksOf(14, named("A"), named("X")), floorTrack{"B", "Y"}, floorTrack{"C", "Z"})
	if !MeetsFloor(late, floorAlbum, floorArtist) {
		t.Errorf("%d tracks whose third album is the last: below the floor, want it cleared", len(late))
	}
}
