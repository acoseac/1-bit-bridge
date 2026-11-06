package smartplaylistgen

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
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

// seedXmas stores n tracks of one album, each by the artist artistOf(i)
// gives, with genre genre, and edit (when not nil) applied to each. It
// returns their paths.
func seedXmas(t *testing.T, s *manifest.Store, album string, n int, artistOf func(int) string, genre string, edit func(*manifest.Track)) []string {
	t.Helper()
	tracks := make([]*manifest.Track, 0, n)
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s/%02d.flac", album, i+1)
		tr := &manifest.Track{
			Path: p, Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("%s %d", album, i+1), Artist: artistOf(i), Album: album,
			Genre: genre, Duration: fp(180),
		}
		if edit != nil {
			edit(tr)
		}
		tracks = append(tracks, tr)
		paths = append(paths, p)
	}
	if err := s.UpsertTrackBatch(context.Background(), tracks); err != nil {
		t.Fatalf("UpsertTrackBatch(%s): %v", album, err)
	}
	return paths
}

func by(name string) func(int) string { return func(int) string { return name } }

// threeXmasAlbums is the smallest library the floor takes: 15 tracks on 3
// albums by 3 artists.
func threeXmasAlbums(t *testing.T, s *manifest.Store, edit func(*manifest.Track)) []string {
	t.Helper()
	var paths []string
	for i, a := range []string{"Carols A", "Carols B", "Carols C"} {
		paths = append(paths, seedXmas(t, s, a, 5, by(fmt.Sprintf("Choir %d", i)), "Christmas", edit)...)
	}
	return paths
}

// A suppressed duplicate is not counted toward the floor, and never served.
func TestRegenerate_ChristmasCountsNoSuppressedDuplicate(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	paths := threeXmasAlbums(t, s, nil)
	if _, err := s.ApplyDupeStamps(ctx, []manifest.DupeStamp{
		{Path: paths[0], GroupID: "g", Tier: string(dupes.TierSameFormat), Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	if row := christmasRow(t, s); row != nil {
		t.Fatalf("15 tracks with one a suppressed duplicate made a family of %d; the floor counts 14", len(cachedPaths(t, row)))
	}
	seedXmas(t, s, "Carols D", 1, by("Choir 3"), "Christmas", nil)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	row := christmasRow(t, s)
	if row == nil {
		t.Fatal("15 served tracks made no family")
	}
	for _, p := range cachedPaths(t, row) {
		if p == paths[0] {
			t.Fatalf("the suppressed duplicate %s is in the family", p)
		}
	}
}

// A Christmas track in no other pool, with its ReplayGain and rate in its
// tags and analysis off, signs the family's energy and rate.
func TestRegenerate_ChristmasEnergyFromItsTagsWithAnalysisOff(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	i := 0
	threeXmasAlbums(t, s, func(tr *manifest.Track) {
		i++
		tr.SampleRate = fp(44100)
		tr.ReplayGainTrackDB = fp(float64(-3 * (i % 5)))
	})
	now := time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	row := christmasRow(t, s)
	if row == nil {
		t.Fatal("no christmas family")
	}
	var energy []float64
	if err := json.Unmarshal(row.EnergyJSON, &energy); err != nil || len(energy) != 15 {
		t.Fatalf("energy %s (%v), want a bar for each of the 15 tracks from their tags", row.EnergyJSON, err)
	}
	if row.ModalRateHz != 44100 {
		t.Fatalf("modal rate %d, want 44100 from the tracks' tags", row.ModalRateHz)
	}
}

// An all-Christmas family with analysis on signs its energy from the
// analysis rows' loudness.
func TestRegenerate_ChristmasEnergyFromAnalysis(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	paths := threeXmasAlbums(t, s, nil)
	for i, p := range paths {
		if err := s.UpsertAnalysis(ctx, manifest.AnalysisRow{
			SourcePath: p, CreatedAt: 1, SourceMTimeNS: 1, SourceSize: 1,
			ReplayGainTrackDB: fp(float64(-2 * (i % 7))),
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), true)); err != nil {
		t.Fatal(err)
	}
	row := christmasRow(t, s)
	if row == nil {
		t.Fatal("no christmas family")
	}
	var energy []float64
	if err := json.Unmarshal(row.EnergyJSON, &energy); err != nil || len(energy) == 0 {
		t.Fatalf("energy %s (%v), want a contour from the analysis rows", row.EnergyJSON, err)
	}
}

// A library with no Christmas music has no family, and the others build as
// they did.
func TestRegenerate_NoChristmasMusicNoFamily(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	seedXmas(t, s, "Kind of Blue", 10, by("Miles Davis"), "Jazz", nil)
	seedXmas(t, s, "Holiday", 10, by("Madonna"), "Pop", nil)
	seedXmas(t, s, "Winter", 10, by("Antonio Vivaldi"), "Classical", nil)
	now := time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	seedDailyPlays(t, s, now, "", []int{3, 2, 1}, "Kind of Blue/01.flac", "Kind of Blue/02.flac", "Kind of Blue/03.flac")
	if _, err := Regenerate(ctx, s, regenTestOptions(now.UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	if row := christmasRow(t, s); row != nil {
		t.Fatalf("a library of jazz, Madonna's Holiday and Vivaldi's Winter made a Christmas family: %v", cachedPaths(t, row))
	}
	if _, kinds := loadKinds(t, s); !kinds["heavyRotation"] {
		t.Fatalf("heavy rotation is gone; kinds %v", kinds)
	}
}

// One season is one order: regenerations in its weeks, 31 December and 1
// January among them, and one before the window opens. The next season
// moves it, from the day the window closes.
func TestRegenerate_OneSeasonIsOneOrder(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	for i, a := range []string{"Carols A", "Carols B", "Carols C", "Carols D"} {
		seedXmas(t, s, a, 8, by(fmt.Sprintf("Choir %d", i)), "Christmas", nil)
	}
	order := func(at time.Time) string {
		t.Helper()
		if _, err := Regenerate(ctx, s, DefaultOptions(at.UnixNano(), false)); err != nil {
			t.Fatal(err)
		}
		row := christmasRow(t, s)
		if row == nil {
			t.Fatalf("no christmas family at %s", at)
		}
		return fmt.Sprint(cachedPaths(t, row))
	}
	season := order(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	for _, at := range []time.Time{
		time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 15, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 5, 23, 59, 59, 0, time.UTC),
	} {
		if got := order(at); got != season {
			t.Fatalf("the 2026 season reshuffled at %s:\n%s\n%s", at.Format(time.RFC3339), season, got)
		}
	}
	next := order(time.Date(2027, 12, 1, 0, 0, 0, 0, time.UTC))
	if next == season {
		t.Fatal("the 2027 season kept the 2026 season's order")
	}
	if got := order(time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC)); got != next {
		t.Fatalf("6 January 2027 is the 2027 season and gave another order:\n%s\n%s", next, got)
	}
}

// What the Christmas family holds is recorded nowhere: it is no discovery
// shelf, so a discovery shelf built in the same run records its own rows and
// none of the Christmas family's; a per-family rebuild of it writes none
// either.
func TestRegenerate_ChristmasRecordsNothingServed(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	xmas := threeXmasAlbums(t, s, nil)
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("Jazz/%02d.flac", i)
		track(t, s, p, "Jazz")
		analysis(t, s, p, i%12, "major", 100+i)
	}
	now := time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	opts := regenTestOptions(now.UnixNano(), true)
	if _, err := Regenerate(ctx, s, opts); err != nil {
		t.Fatal(err)
	}
	if christmasRow(t, s) == nil {
		t.Fatal("no christmas family")
	}
	if generated, _, _, err := RegenerateFamily(ctx, s, opts, "christmas"); err != nil || !generated {
		t.Fatalf("RegenerateFamily(christmas): generated %v, %v", generated, err)
	}
	served, err := s.SmartPlaylistServedOnEarlierDays(ctx, now.Add(24*time.Hour).UnixNano(), discoveryServedDays)
	if err != nil {
		t.Fatal(err)
	}
	if len(served) == 0 {
		t.Fatal("Auto Mix recorded nothing, so this test shows nothing")
	}
	for _, p := range xmas {
		if _, ok := served[p]; ok {
			t.Fatalf("%s, a Christmas track, was recorded as served by a discovery shelf", p)
		}
	}
}

// The demo's shape: three Christmas albums tagged GENRE=Christmas, beside
// classical albums the analyzer has keyed, no history, no hearts. The
// family holds all 30 tracks, at most 8 by one performer ahead of the
// other's, and it is the first shelf: on a bridge with no history nothing
// comes before it.
func TestRegenerate_TheDemosShape(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	var xmas []string
	xmas = append(xmas, seedXmas(t, s, "Kolędy polskie", 12, by("Studio Accantus"), "Christmas", nil)...)
	xmas = append(xmas, seedXmas(t, s, "Julsånger", 10, by("Musikverket"), "Christmas", nil)...)
	xmas = append(xmas, seedXmas(t, s, "Christmas Chorales", 8, by("Musikverket"), "Christmas", nil)...)
	for i, p := range seedXmas(t, s, "Goldberg Variations", 30, by("Kimiko Ishizaka"), "Classical", nil) {
		analysis(t, s, p, i%12, "major", 60+i)
	}
	now := time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC)
	if _, err := Regenerate(ctx, s, DefaultOptions(now.UnixNano(), true)); err != nil {
		t.Fatal(err)
	}
	row := christmasRow(t, s)
	if row == nil {
		_, kinds := loadKinds(t, s)
		t.Fatalf("no christmas family on the demo's shape; kinds %v", kinds)
	}
	if row.Position != 0 {
		t.Errorf("the Christmas family is at position %d, want the first shelf on a bridge with no history", row.Position)
	}
	got := cachedPaths(t, row)
	if len(got) != len(xmas) {
		t.Fatalf("the family holds %d tracks, want all %d", len(got), len(xmas))
	}
	performer := func(p string) string {
		if strings.HasPrefix(p, "Kolędy polskie/") {
			return "Studio Accantus"
		}
		return "Musikverket"
	}
	head := map[string]int{}
	for _, p := range got[:16] {
		head[performer(p)]++
	}
	if head["Studio Accantus"] != 8 || head["Musikverket"] != 8 {
		t.Errorf("the first 16 tracks are %v, want 8 by each performer ahead of the rest", head)
	}
	if _, kinds := loadKinds(t, s); !kinds["autoMix"] {
		t.Errorf("Auto Mix is missing beside the Christmas family; kinds %v", kinds)
	}
}
