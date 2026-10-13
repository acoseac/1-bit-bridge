package manifest

import (
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

func TestReconcileAlbumArtists(t *testing.T) {
	mk := func(path, album, aa string) ReconcileTarget {
		return ReconcileTarget{Path: path, Album: album, AlbumArtist: aa}
	}
	fixes := func(changed []ReconcileTarget) map[string]string {
		m := map[string]string{}
		for _, tr := range changed {
			m[tr.Path] = tr.AlbumArtist
		}
		return m
	}

	cases := []struct {
		name string
		in   []ReconcileTarget
		want map[string]string // path -> reconciled AlbumArtist (changed rows only)
	}{
		{
			name: "consistent group is untouched",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", "X"),
				mk("A/Alb/2.flac", "Alb", "X"),
			},
			want: map[string]string{},
		},
		{
			name: "dominant wins, minority fixed",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", "Aspiration"),
				mk("A/Alb/2.flac", "Alb", "Aspiration"),
				mk("A/Alb/3.flac", "Alb", "Aspiration"),
				mk("A/Alb/4.flac", "Alb", "Peter Asplund; Aspiration"),
			},
			want: map[string]string{"A/Alb/4.flac": "Aspiration"},
		},
		{
			name: "comma vs semicolon separator unified to dominant",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", "Peter, Paul & Mary; Chad Mitchell Trio"),
				mk("A/Alb/2.flac", "Alb", "Peter, Paul & Mary; Chad Mitchell Trio"),
				mk("A/Alb/3.flac", "Alb", "Peter, Paul & Mary, Chad Mitchell Trio"),
			},
			want: map[string]string{"A/Alb/3.flac": "Peter, Paul & Mary; Chad Mitchell Trio"},
		},
		{
			name: "blank album-artist filled from dominant (compilation hole)",
			in: []ReconcileTarget{
				mk("A/Comp/1.flac", "Comp", "Various Artists"),
				mk("A/Comp/2.flac", "Comp", "Various Artists"),
				mk("A/Comp/3.flac", "Comp", ""),
			},
			want: map[string]string{"A/Comp/3.flac": "Various Artists"},
		},
		{
			name: "all blank: nothing to reconcile to",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", ""),
				mk("A/Alb/2.flac", "Alb", ""),
			},
			want: map[string]string{},
		},
		{
			name: "different directories are NEVER merged",
			in: []ReconcileTarget{
				mk("CopyA/Alb/1.flac", "Alb", "X"),
				mk("CopyB/Alb/1.flac", "Alb", "Y"),
			},
			want: map[string]string{},
		},
		{
			name: "box-set per-disc performers stay separate (different dirs)",
			in: []ReconcileTarget{
				mk("Box/Disc 1/1.flac", "Box", "Soloist A"),
				mk("Box/Disc 1/2.flac", "Box", "Soloist A"),
				mk("Box/Disc 2/1.flac", "Box", "Soloist B"),
				mk("Box/Disc 2/2.flac", "Box", "Soloist B"),
			},
			want: map[string]string{},
		},
		{
			name: "single-track group untouched",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", "X"),
			},
			want: map[string]string{},
		},
		{
			name: "loose tracks without album are skipped",
			in: []ReconcileTarget{
				mk("A/1.flac", "", "X"),
				mk("A/2.flac", "", "Y"),
			},
			want: map[string]string{},
		},
		{
			name: "album title case/space differences group together",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "All My Septembers", "Aspiration"),
				mk("A/Alb/2.flac", "all my septembers ", "Aspiration"),
				mk("A/Alb/3.flac", "All My Septembers", "Peter Asplund; Aspiration"),
			},
			want: map[string]string{"A/Alb/3.flac": "Aspiration"},
		},
		{
			name: "tie breaks toward the longer (more complete) credit",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", "Short"),
				mk("A/Alb/2.flac", "Alb", "A Longer Credit"),
			},
			want: map[string]string{"A/Alb/1.flac": "A Longer Credit"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fixes(reconcileAlbumArtists(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d fix(es) %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for p, aa := range tc.want {
				if got[p] != aa {
					t.Errorf("path %q: got AlbumArtist %q, want %q", p, got[p], aa)
				}
			}
		})
	}
}

func TestReconcileAlbumArtistsWindowsPaths(t *testing.T) {
	in := []ReconcileTarget{
		{Path: `A\Alb\1.flac`, Album: "Alb", AlbumArtist: "X"},
		{Path: `A\Alb\2.flac`, Album: "Alb", AlbumArtist: "X"},
		{Path: `A\Alb\3.flac`, Album: "Alb", AlbumArtist: "Y"},
		{Path: `A\Other\1.flac`, Album: "Alb", AlbumArtist: "Z"}, // different dir — untouched
	}
	changed := reconcileAlbumArtists(in)
	if len(changed) != 1 {
		t.Fatalf("got %d change(s), want 1: %+v", len(changed), changed)
	}
	if changed[0].Path != `A\Alb\3.flac` || changed[0].AlbumArtist != "X" {
		t.Errorf("got %q -> %q, want %q -> %q", changed[0].Path, changed[0].AlbumArtist, `A\Alb\3.flac`, "X")
	}
}

func TestDominantAlbumArtist(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		want   string
		wantOK bool
	}{
		{"empty", nil, "", false},
		{"all blank", []string{"", ""}, "", false},
		{"clear majority", []string{"A", "A", "B"}, "A", true},
		{"blanks ignored", []string{"", "A", ""}, "A", true},
		{"tie longest wins", []string{"AA", "BBBB"}, "BBBB", true},
		{"tie same length lexicographic", []string{"BB", "AA"}, "AA", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dominantAlbumArtist(tc.in)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestTrackDir(t *testing.T) {
	cases := map[string]string{
		"A/B/c.flac": "A/B",
		`A\B\c.flac`: `A\B`,
		"c.flac":     "",
		"A/c.flac":   "A",
	}
	for in, want := range cases {
		if got := trackDir(in); got != want {
			t.Errorf("trackDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReconcileYears(t *testing.T) {
	yp := func(y int) *int { return &y }
	mk := func(path, album string, year *int) ReconcileTarget {
		return ReconcileTarget{Path: path, Album: album, Year: year}
	}
	fixes := func(changed []ReconcileTarget) map[string]int {
		m := map[string]int{}
		for _, tr := range changed {
			if tr.Year != nil {
				m[tr.Path] = *tr.Year
			}
		}
		return m
	}

	cases := []struct {
		name string
		in   []ReconcileTarget
		want map[string]int // path -> filled year (changed rows only)
	}{
		{
			name: "all have year — untouched",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", yp(2023)),
				mk("A/Alb/2.flac", "Alb", yp(2023)),
			},
			want: map[string]int{},
		},
		{
			name: "missing year filled from dominant (Alphaville bonus track)",
			in: []ReconcileTarget{
				mk("A/EP/1.flac", "EP", yp(2023)),
				mk("A/EP/2.flac", "EP", yp(2023)),
				mk("A/EP/3.flac", "EP", nil),
			},
			want: map[string]int{"A/EP/3.flac": 2023},
		},
		{
			name: "year=0 sentinel treated as missing",
			in: []ReconcileTarget{
				mk("A/EP/1.flac", "EP", yp(2023)),
				mk("A/EP/2.flac", "EP", yp(0)),
			},
			want: map[string]int{"A/EP/2.flac": 2023},
		},
		{
			name: "present-but-different year LEFT ALONE (no overwrite)",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", yp(2023)),
				mk("A/Alb/2.flac", "Alb", yp(2023)),
				mk("A/Alb/3.flac", "Alb", yp(1999)),
			},
			want: map[string]int{},
		},
		{
			name: "fills missing AND leaves present-different alone",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", yp(2023)),
				mk("A/Alb/2.flac", "Alb", yp(2023)),
				mk("A/Alb/3.flac", "Alb", nil),
				mk("A/Alb/4.flac", "Alb", yp(1999)),
			},
			want: map[string]int{"A/Alb/3.flac": 2023},
		},
		{
			name: "cross-directory NOT reconciled",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", yp(2023)),
				mk("B/Alb/2.flac", "Alb", nil),
			},
			want: map[string]int{},
		},
		{
			name: "all missing — nothing to fill",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", nil),
				mk("A/Alb/2.flac", "Alb", yp(0)),
			},
			want: map[string]int{},
		},
		{
			name: "dominant tie breaks to larger year",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", yp(2002)),
				mk("A/Alb/2.flac", "Alb", yp(2003)),
				mk("A/Alb/3.flac", "Alb", nil),
			},
			want: map[string]int{"A/Alb/3.flac": 2003},
		},
		{
			name: "lone track untouched",
			in: []ReconcileTarget{
				mk("A/Alb/1.flac", "Alb", nil),
			},
			want: map[string]int{},
		},
		{
			name: "empty album never grouped",
			in: []ReconcileTarget{
				mk("A/1.flac", "", yp(2023)),
				mk("A/2.flac", "", nil),
			},
			want: map[string]int{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fixes(reconcileYears(c.in))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for p, y := range c.want {
				if got[p] != y {
					t.Fatalf("path %s: got %d, want %d (full: %v)", p, got[p], y, got)
				}
			}
		})
	}
}

// compositionAlbum is one directory-and-album group. years are written in
// track order; a second copy of a track is another year in the same group.
func compositionAlbum(dir, album string, years ...int) []ReconcileTarget {
	out := make([]ReconcileTarget, 0, len(years))
	for i, y := range years {
		yy := y
		out = append(out, ReconcileTarget{
			Path:        fmt.Sprintf("%s/%02d.flac", dir, i+1),
			Album:       album,
			AlbumArtist: "Artist",
			Year:        &yy,
		})
	}
	return out
}

// yearAfterReconcile is the year path holds once reconcileYears has run.
// A path the pass does not return keeps the year it was given.
func yearAfterReconcile(tracks []ReconcileTarget, path string) (year int, changed bool) {
	for _, c := range reconcileYears(tracks) {
		if c.Path != path {
			continue
		}
		if c.Year == nil {
			return 0, true
		}
		return *c.Year, true
	}
	for _, tr := range tracks {
		if tr.Path == path && tr.Year != nil {
			return *tr.Year, false
		}
	}
	return 0, false
}

func requireYear(t *testing.T, tracks []ReconcileTarget, path string, want int, wantChanged bool) {
	t.Helper()
	got, changed := yearAfterReconcile(tracks, path)
	if got != want || changed != wantChanged {
		t.Errorf("%s: year %d changed=%v, want %d changed=%v", path, got, changed, want, wantChanged)
	}
}

// TestReconcileYearsTreatsACompositionDateAsMissing is backlog B342.
// A year before firstPhonographYear is a composition date. When the
// same folder's album has any recording year, the pass fills every
// composition date from the dominant recording year, and composition
// dates do not vote. A folder with no recording year keeps its early
// years. A year of 1877 or later is left as it was tagged.
func TestReconcileYearsTreatsACompositionDateAsMissing(t *testing.T) {
	t.Run("ellipses", func(t *testing.T) {
		// Seven tracks tagged 2022 and five tagged 1654, the second copy
		// of track 03 among the five.
		tracks := compositionAlbum("Anastasia Kobekina/Ellipses", "Ellipses",
			2022, 2022, 2022, 2022, 2022, 2022, 2022,
			1654, 1654, 1654, 1654, 1654)
		tracks[11].Path = "Anastasia Kobekina/Ellipses/03. Bachianas Brasileiras_ No. 5.flac"
		for _, tr := range tracks {
			requireYear(t, tracks, tr.Path, 2022, tr.Year != nil && *tr.Year == 1654)
		}
	})

	t.Run("all-early", func(t *testing.T) {
		tracks := compositionAlbum("Composer/Suite", "Suite", 1654, 1654, 1654)
		for _, tr := range tracks {
			requireYear(t, tracks, tr.Path, 1654, false)
		}
	})

	t.Run("two-plausible-years", func(t *testing.T) {
		tracks := compositionAlbum("Band/Album", "Album", 1974, 2022, 2022)
		requireYear(t, tracks, tracks[0].Path, 1974, false)
		requireYear(t, tracks, tracks[1].Path, 2022, false)
		requireYear(t, tracks, tracks[2].Path, 2022, false)
	})

	t.Run("boundary-1876", func(t *testing.T) {
		tracks := compositionAlbum("Band/Album", "Album", 1876, 2022)
		requireYear(t, tracks, tracks[0].Path, 2022, true)
		requireYear(t, tracks, tracks[1].Path, 2022, false)
	})

	t.Run("boundary-1877", func(t *testing.T) {
		tracks := compositionAlbum("Band/Album", "Album", 1877, 2022)
		requireYear(t, tracks, tracks[0].Path, 1877, false)
		requireYear(t, tracks, tracks[1].Path, 2022, false)
	})

	t.Run("other-folder", func(t *testing.T) {
		// The other folder's years are composition dates too. A grouping
		// that merged the two folders would fill them from 2022.
		here := compositionAlbum("Artist/Ellipses", "Ellipses", 2022, 2022, 1654)
		there := compositionAlbum("Other/Ellipses", "Ellipses", 1654, 1654)
		tracks := append(here, there...)
		requireYear(t, tracks, here[2].Path, 2022, true)
		requireYear(t, tracks, there[0].Path, 1654, false)
		requireYear(t, tracks, there[1].Path, 1654, false)
	})

	t.Run("missing-beside-an-early-year", func(t *testing.T) {
		tracks := compositionAlbum("Composer/Suite", "Suite", 1654, 1654, 1654)
		tracks[2].Year = nil
		requireYear(t, tracks, tracks[0].Path, 1654, false)
		requireYear(t, tracks, tracks[1].Path, 1654, false)
		requireYear(t, tracks, tracks[2].Path, 1654, true)
	})

	t.Run("minority-recording-year", func(t *testing.T) {
		// Seven composition dates and five recording years. The recording
		// year wins, so every 1654 takes 2022.
		tracks := compositionAlbum("Composer/Suite", "Suite",
			1654, 1654, 1654, 1654, 1654, 1654, 1654,
			2022, 2022, 2022, 2022, 2022)
		for _, tr := range tracks {
			requireYear(t, tracks, tr.Path, 2022, tr.Year != nil && *tr.Year == 1654)
		}
	})

	t.Run("two-recording-years", func(t *testing.T) {
		// 2019 ×3, 2021 ×2, 1720 ×4. The composition dates take the
		// dominant recording year, 2019, and the 2021s stay.
		tracks := compositionAlbum("Band/Album", "Album",
			2019, 2019, 2019, 2021, 2021, 1720, 1720, 1720, 1720)
		for _, tr := range tracks {
			want, changed := 2019, false
			if tr.Year != nil && *tr.Year == 2021 {
				want = 2021
			}
			if tr.Year != nil && *tr.Year == 1720 {
				changed = true
			}
			requireYear(t, tracks, tr.Path, want, changed)
		}
	})

	t.Run("recording-year-tie", func(t *testing.T) {
		// 2018 and 2020 are tied, so the larger recording year wins.
		// The three composition dates take 2020 and do not vote.
		tracks := compositionAlbum("Band/Album", "Album",
			2018, 2018, 2020, 2020, 1685, 1685, 1685)
		for _, tr := range tracks {
			want, changed := 2020, false
			if tr.Year != nil && *tr.Year == 2018 {
				want = 2018
			}
			if tr.Year != nil && *tr.Year == 1685 {
				changed = true
			}
			requireYear(t, tracks, tr.Path, want, changed)
		}
	})
}

// TestAFilledYearLeavesTheDuplicateKeyToTheExistingRule pins that the
// year fill does not grow a second duplicate rule. Two copies of track
// 03 with the same title share dupes.KeyFor once their years agree. A
// title that still contains an underscore stays a different key.
func TestAFilledYearLeavesTheDuplicateKeyToTheExistingRule(t *testing.T) {
	// Three recording years outvote the two composition dates, which is
	// the Ellipses shape the fill acts on. The two 1654s are both copies
	// of track 03.
	tracks := compositionAlbum("Anastasia Kobekina/Ellipses", "Ellipses", 2022, 2022, 2022, 1654, 1654)
	tracks[0].Path = "Anastasia Kobekina/Ellipses/03.flac"
	tracks[3].Path = "Anastasia Kobekina/Ellipses/03. Bachianas Brasileiras No. 5.flac"
	tracks[4].Path = "Anastasia Kobekina/Ellipses/03. Bachianas Brasileiras_ No. 5.flac"
	title := "Bachianas Brasileiras No. 5"
	yearOf := map[string]int{}
	for _, tr := range tracks {
		y, _ := yearAfterReconcile(tracks, tr.Path)
		yearOf[tr.Path] = y
	}
	row := func(path, trackTitle string) dupes.Row {
		return dupes.Row{
			Path:        path,
			Title:       trackTitle,
			Album:       "Ellipses",
			AlbumArtist: "Artist",
			Year:        yearOf[path],
			Disc:        1,
			DiscTagged:  true,
			Track:       3,
			TrackTagged: true,
		}
	}
	if yearOf[tracks[3].Path] != 2022 || yearOf[tracks[4].Path] != 2022 {
		t.Fatalf("composition-date copies stayed at %d and %d, want the album's 2022", yearOf[tracks[3].Path], yearOf[tracks[4].Path])
	}
	served := row(tracks[0].Path, title)
	copy := row(tracks[3].Path, title)
	if dupes.KeyFor(served) != dupes.KeyFor(copy) {
		t.Errorf("same title, disc and track after the year fill: keys %v and %v", dupes.KeyFor(served), dupes.KeyFor(copy))
	}
	underscore := row(tracks[4].Path, "Bachianas Brasileiras_ No. 5")
	if dupes.KeyFor(underscore) == dupes.KeyFor(served) {
		t.Errorf("a title that keeps its underscore shares the served copy's key %v", dupes.KeyFor(served))
	}
}
