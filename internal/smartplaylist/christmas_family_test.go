package smartplaylist

import (
	"fmt"
	"sort"
	"testing"
)

// xmasTrack is a Christmas track as the regenerator hands it over.
func xmasTrack(path, artist, album string) TrackFeature {
	return TrackFeature{Path: path, Title: "T " + path, Artist: artist, Album: album, Genre: "Christmas", Duration: 180}
}

// xmasAlbum is n tracks of one album, each by the artist artistOf(i) gives.
func xmasAlbum(album string, n int, artistOf func(i int) string) []TrackFeature {
	out := make([]TrackFeature, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, xmasTrack(fmt.Sprintf("%s/%02d.flac", album, i+1), artistOf(i), album))
	}
	return out
}

func oneArtist(name string) func(int) string { return func(int) string { return name } }

// xmasInputs is what the regenerator assembles for a library holding these
// Christmas tracks and nothing else: the pool, each track in Features (the
// Favorites pattern, which signEnergy reads), and the 2026 season's seed.
func xmasInputs(pools ...[]TrackFeature) Inputs {
	in := Inputs{Features: map[string]TrackFeature{}, ChristmasSeed: SeedFromSeason(2026)}
	for _, p := range pools {
		for _, f := range p {
			in.Christmas = append(in.Christmas, f)
			in.Features[f.Path] = f
		}
	}
	return in
}

func itemPaths(p GeneratedPlaylist) []string {
	out := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, it.Path)
	}
	return out
}

// The floor by emitted tracks: 14 is no family, 15 is.
func TestBuildChristmasTheFloorByEmittedTracks(t *testing.T) {
	opts := DefaultOptions(false)
	albums := func(n int) Inputs {
		var tracks []TrackFeature
		for i := 0; i < n; i++ {
			tracks = append(tracks, xmasTrack(fmt.Sprintf("x/%02d.flac", i), fmt.Sprintf("Artist %d", i%3), fmt.Sprintf("Album %d", i%3)))
		}
		return xmasInputs(tracks)
	}
	if _, ok := buildChristmas(albums(14), opts); ok {
		t.Fatal("14 tracks from 3 albums by 3 artists made a family; the floor is 15")
	}
	pl, ok := buildChristmas(albums(15), opts)
	if !ok || len(pl.Items) != 15 {
		t.Fatalf("15 tracks from 3 albums by 3 artists: ok %v, %d items, want a family of 15", ok, len(pl.Items))
	}
	if pl.Slug != "christmas" || pl.Kind != KindChristmas || pl.Title != "Christmas" ||
		pl.Subtitle != "Carols and Christmas albums from your library" {
		t.Errorf("the family is %q %q %q %q", pl.Slug, pl.Kind, pl.Title, pl.Subtitle)
	}
	// A zero Options is the production floor, never a 1-track family.
	if _, ok := buildChristmas(albums(14), Options{}); ok {
		t.Error("a zero Options let 14 tracks through")
	}
}

// The floor by spread: 3 albums or 3 artists, among the tracks that qualify.
func TestBuildChristmasTheFloorBySpread(t *testing.T) {
	opts := DefaultOptions(false)
	cases := []struct {
		name  string
		pools [][]TrackFeature
		want  bool
	}{
		{"2 albums by 2 artists", [][]TrackFeature{
			xmasAlbum("A", 8, oneArtist("X")), xmasAlbum("B", 8, oneArtist("Y"))}, false},
		{"3 albums by 1 artist", [][]TrackFeature{
			xmasAlbum("A", 5, oneArtist("X")), xmasAlbum("B", 5, oneArtist("X")), xmasAlbum("C", 5, oneArtist("X"))}, true},
		{"1 album by 3 artists", [][]TrackFeature{
			xmasAlbum("A", 15, func(i int) string { return []string{"X", "Y", "Z"}[i%3] })}, true},
		{"1 album by 2 artists", [][]TrackFeature{
			xmasAlbum("A", 16, func(i int) string { return []string{"X", "Y"}[i%2] })}, false},
		{"untagged albums, 3 artists", [][]TrackFeature{
			xmasAlbum("", 15, func(i int) string { return []string{"X", "Y", "Z"}[i%3] })}, true},
		{"untagged albums and artists", [][]TrackFeature{xmasAlbum("", 20, oneArtist(""))}, false},
		{"one artist spelled three ways", [][]TrackFeature{
			xmasAlbum("A", 15, func(i int) string { return []string{"Bing Crosby", "bing crosby", " BING CROSBY "}[i%3] })}, false},
	}
	for _, c := range cases {
		if _, ok := buildChristmas(xmasInputs(c.pools...), opts); ok != c.want {
			t.Errorf("%s: family %v, want %v", c.name, ok, c.want)
		}
	}
}

// The design's four libraries.
func TestBuildChristmasTheFourLibraries(t *testing.T) {
	opts := DefaultOptions(false)
	// A 40-track box set by one artist is an album, not a mix.
	if _, ok := buildChristmas(xmasInputs(xmasAlbum("The Christmas Box", 40, oneArtist("Bach Collegium Japan"))), opts); ok {
		t.Error("a 40-track box set by one artist made a family")
	}
	// Three artists, five tracks each.
	three := xmasInputs(xmasAlbum("A", 5, oneArtist("X")), xmasAlbum("B", 5, oneArtist("Y")), xmasAlbum("C", 5, oneArtist("Z")))
	if pl, ok := buildChristmas(three, opts); !ok || len(pl.Items) != 15 {
		t.Errorf("three artists by five tracks: ok %v, %d items, want 15", ok, len(pl.Items))
	}
	// A Various Artists compilation counts by its performers.
	va := xmasInputs(xmasAlbum("Now That's What I Call Christmas", 20, func(i int) string { return fmt.Sprintf("Performer %d", i%7) }))
	if pl, ok := buildChristmas(va, opts); !ok || len(pl.Items) != 20 {
		t.Errorf("a 20-track compilation by 7 performers: ok %v, %d items, want 20", ok, len(pl.Items))
	}
	// The demo's shape: 30 tracks across 3 albums, one performer each.
	demo := xmasInputs(xmasAlbum("Kolędy polskie", 12, oneArtist("Studio Accantus")),
		xmasAlbum("Julsånger", 10, oneArtist("Musikverket")), xmasAlbum("Chorales for Christmas", 8, oneArtist("Musikverket")))
	pl, ok := buildChristmas(demo, opts)
	if !ok || len(pl.Items) != 30 {
		t.Fatalf("the demo's shape: ok %v, %d items, want all 30", ok, len(pl.Items))
	}
}

// The cap spreads the artists and never starves the family: one prolific
// artist among three still gives min(qualifying, 100), with at most 8 of
// the prolific artist's tracks ahead of the others'.
func TestBuildChristmasTheCapSpreadsAndFills(t *testing.T) {
	opts := DefaultOptions(false)
	prolific := xmasAlbum("Big Box", 100, oneArtist("Prolific"))
	others := append(xmasAlbum("Small A", 5, oneArtist("Other A")), xmasAlbum("Small B", 5, oneArtist("Other B"))...)
	pl, ok := buildChristmas(xmasInputs(prolific, others), opts)
	if !ok || len(pl.Items) != 100 {
		t.Fatalf("110 qualifying: ok %v, %d items, want the cap, 100", ok, len(pl.Items))
	}
	byPath := map[string]string{}
	for _, f := range append(append([]TrackFeature{}, prolific...), others...) {
		byPath[f.Path] = f.Artist
	}
	head, othersSeen := 0, 0
	for i, it := range pl.Items {
		if byPath[it.Path] != "Prolific" {
			othersSeen++
			continue
		}
		if i < 18 {
			head++
		}
	}
	if othersSeen != 10 {
		t.Errorf("the family holds %d of the other artists' 10 tracks, want all of them", othersSeen)
	}
	if head != 8 {
		t.Errorf("the first 18 items hold %d of the prolific artist's tracks, want 8 ahead of the others' 10", head)
	}
	// Below the cap every track is kept, however few artists there are.
	pl, ok = buildChristmas(xmasInputs(xmasAlbum("Big Box", 40, oneArtist("Prolific")), others), opts)
	if !ok || len(pl.Items) != 50 {
		t.Errorf("50 qualifying, 3 artists: ok %v, %d items, want all 50", ok, len(pl.Items))
	}
	// Many artists: the cap holds, and each artist has at most 8.
	var many []TrackFeature
	for a := 0; a < 15; a++ {
		many = append(many, xmasAlbum(fmt.Sprintf("Album %d", a), 9, oneArtist(fmt.Sprintf("Artist %d", a)))...)
	}
	pl, ok = buildChristmas(xmasInputs(many), opts)
	if !ok || len(pl.Items) != 100 {
		t.Fatalf("135 qualifying by 15 artists: ok %v, %d items, want 100", ok, len(pl.Items))
	}
	counts := map[string]int{}
	for _, it := range pl.Items {
		counts[it.Artist]++
	}
	for a, n := range counts {
		if n > 8 {
			t.Errorf("%s has %d tracks in a family that had room for every artist's 8", a, n)
		}
	}
}

// One seed is one order, whatever order the tracks arrive in; another seed
// moves it.
func TestBuildChristmasOrdersBySeasonSeed(t *testing.T) {
	opts := DefaultOptions(false)
	pool := append(append(xmasAlbum("A", 10, oneArtist("X")), xmasAlbum("B", 10, oneArtist("Y"))...), xmasAlbum("C", 10, oneArtist("Z"))...)
	first, _ := buildChristmas(xmasInputs(pool), opts)
	reversed := make([]TrackFeature, len(pool))
	for i, f := range pool {
		reversed[len(pool)-1-i] = f
	}
	again, _ := buildChristmas(xmasInputs(reversed), opts)
	if fmt.Sprint(itemPaths(first)) != fmt.Sprint(itemPaths(again)) {
		t.Fatalf("the same season gave two orders for the same tracks:\n%v\n%v", itemPaths(first), itemPaths(again))
	}
	next := xmasInputs(pool)
	next.ChristmasSeed = SeedFromSeason(2027)
	moved, _ := buildChristmas(next, opts)
	if fmt.Sprint(itemPaths(first)) == fmt.Sprint(itemPaths(moved)) {
		t.Fatal("the next season kept this season's order")
	}
	a, b := itemPaths(first), itemPaths(moved)
	sort.Strings(a)
	sort.Strings(b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatal("the next season holds other tracks; only the order may move")
	}
}

// The family needs nothing but its tracks: no plays, no hearts, no
// analysis, and analysis on or off builds the same family.
func TestBuildChristmasNeedsNoHistoryHeartsOrAnalysis(t *testing.T) {
	in := xmasInputs(xmasAlbum("A", 5, oneArtist("X")), xmasAlbum("B", 5, oneArtist("Y")), xmasAlbum("C", 5, oneArtist("Z")))
	off := Generate(in, DefaultOptions(false))
	on := Generate(in, DefaultOptions(true))
	if len(off) != 1 || off[0].Kind != KindChristmas {
		t.Fatalf("a library of Christmas tracks alone generated %v, want the Christmas family", kinds(off))
	}
	if len(on) != 1 || fmt.Sprint(itemPaths(on[0])) != fmt.Sprint(itemPaths(off[0])) {
		t.Fatalf("analysis on generated %v, want the same Christmas family", kinds(on))
	}
}

// Christmas comes after Favorites and before Drive Mix, and Daily Mix
// still leads.
func TestGenerateChristmasSitsAfterFavoritesAndBeforeDriveMix(t *testing.T) {
	in := richInputs()
	for _, f := range append(append(xmasAlbum("A", 5, oneArtist("X")), xmasAlbum("B", 5, oneArtist("Y"))...), xmasAlbum("C", 5, oneArtist("Z"))...) {
		in.Christmas = append(in.Christmas, f)
		in.Features[f.Path] = f
	}
	in.ChristmasSeed = SeedFromSeason(2026)
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		in.Favorites = append(in.Favorites, in.Features[p])
	}
	in.Drive = []PlayStat{stat("a", 4), stat("b", 3)}
	out := Generate(in, testOpts(true))
	want := []Kind{KindDailyMix, KindHeavyRotation, KindFavorites, KindChristmas, KindDriveMix}
	got := kinds(out)
	if len(got) < len(want) {
		t.Fatalf("families %v, want them to begin %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Fatalf("families %v, want them to begin %v", got, want)
		}
	}
}

// Christmas neither reads nor feeds the discovery shelves' exclusion: it is
// no discovery shelf, what the shelves showed takes nothing from it, and its
// tracks stay free for a discovery shelf.
func TestChristmasStaysOutOfTheDiscoveryExclusion(t *testing.T) {
	if DiscoveryShelf(christmasSlug) {
		t.Fatal("christmas is a discovery shelf, so the regenerator would record what it served")
	}
	pool := append(append(xmasAlbum("A", 6, oneArtist("X")), xmasAlbum("B", 6, oneArtist("Y"))...), xmasAlbum("C", 6, oneArtist("Z"))...)
	in := xmasInputs(pool)
	plain, _ := buildChristmas(in, DefaultOptions(true))
	in.ServedRecently = map[string]bool{}
	for _, f := range pool {
		in.ServedRecently[f.Path] = true
	}
	excluded, _ := buildChristmas(in, DefaultOptions(true))
	if fmt.Sprint(itemPaths(plain)) != fmt.Sprint(itemPaths(excluded)) {
		t.Fatal("what the discovery shelves showed changed the Christmas family")
	}
	// The same tracks, quiet and slow, make Wind Down too: the Christmas
	// family takes none of them away from it.
	in = xmasInputs(pool)
	for _, f := range pool {
		f.BPM, f.ReplayGainTrackDB = iptr(70), fptr(-4)
		in.QuietSlowPool = append(in.QuietSlowPool, f)
	}
	out := Generate(in, DefaultOptions(true))
	wind, xmas := byKind(out, KindWindDown), byKind(out, KindChristmas)
	if wind == nil || xmas == nil {
		t.Fatalf("families %v, want Christmas and Wind Down", kinds(out))
	}
	// Excluded, every track would be gone from Wind Down's pool, and it
	// would come back only through the relaxed rebuild.
	if wind.ExclusionRelaxed || len(wind.Items) != len(pool) {
		t.Errorf("Wind Down holds %d of %d items (relaxed %v); the Christmas family must leave its tracks to it",
			len(wind.Items), len(pool), wind.ExclusionRelaxed)
	}
}

// signEnergy finds a Christmas track's loudness and rate in the feature map
// the regenerator fills, with analysis off; a track with no loudness is the
// midpoint, as everywhere.
func TestChristmasEnergyAndRateFromItsOwnTags(t *testing.T) {
	pool := append(append(xmasAlbum("A", 6, oneArtist("X")), xmasAlbum("B", 6, oneArtist("Y"))...), xmasAlbum("C", 6, oneArtist("Z"))...)
	for i := range pool {
		pool[i].SampleRate = iptr(96000)
		switch i % 3 {
		case 1:
			pool[i].ReplayGainTrackDB = fptr(0) // energy 1
		case 2:
			pool[i].ReplayGainTrackDB = fptr(-24) // energy 0
		}
	}
	out := Generate(xmasInputs(pool), DefaultOptions(false))
	xmas := byKind(out, KindChristmas)
	if xmas == nil {
		t.Fatalf("families %v, want Christmas", kinds(out))
	}
	if len(xmas.Energy) != len(pool) || xmas.ModalRateHz != 96000 {
		t.Fatalf("energy %v, modal rate %d, want one bar a track and 96000 from the tracks' own tags",
			xmas.Energy, xmas.ModalRateHz)
	}
	byPath := map[string]TrackFeature{}
	for _, f := range pool {
		byPath[f.Path] = f
	}
	// 18 bars of 0, ½ and 1 are neither downsampled (48 at most) nor near
	// flat, so each bar is its track's own value and no noise moves it.
	for i, it := range xmas.Items {
		want := energyMidpoint
		if rg := byPath[it.Path].ReplayGainTrackDB; rg != nil {
			want = LoudnessToEnergy(*rg)
		}
		if xmas.Energy[i] != want {
			t.Errorf("item %d (%s) has energy %v, want %v", i, it.Path, xmas.Energy[i], want)
		}
	}
}
