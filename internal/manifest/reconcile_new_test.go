package manifest

import (
	"reflect"
	"testing"
)

// reconcileAlbumTitles rewrites tracks whose album tag is just the folder name
// to their folder's single clean-sibling title (the dub folder-name-garbage
// case). Conservative: fires only with a garbage tag AND exactly one clean
// sibling title.
func TestReconcileAlbumTitles(t *testing.T) {
	mk := func(path, album string) ReconcileTarget {
		return ReconcileTarget{Path: path, Album: album}
	}
	// path -> rewritten album (changed rows only)
	fixes := func(changed []ReconcileTarget) map[string]string {
		m := map[string]string{}
		for _, c := range changed {
			m[c.Path] = c.Album
		}
		return m
	}

	// The Alphaville case: folder name = the garbage album string; 2 clean
	// siblings + 2 folder-name-garbage tracks → garbage rewritten to clean.
	const folder = "Alphaville/[A] Eternally Yours [287388724] [2023]"
	const garbage = "[A] Eternally Yours [287388724] [2023]"
	const clean = "Eternally Yours"

	cases := []struct {
		name string
		in   []ReconcileTarget
		want map[string]string
	}{
		{
			name: "folder-name garbage rewritten to single clean sibling",
			in: []ReconcileTarget{
				mk(folder+"/1.flac", clean),
				mk(folder+"/2.flac", clean),
				mk(folder+"/3.flac", garbage),
				mk(folder+"/4.flac", garbage),
			},
			want: map[string]string{
				folder + "/3.flac": clean,
				folder + "/4.flac": clean,
			},
		},
		{
			name: "garbage is the MAJORITY but still rewritten (not a vote)",
			in: []ReconcileTarget{
				mk(folder+"/1.flac", clean),
				mk(folder+"/2.flac", garbage),
				mk(folder+"/3.flac", garbage),
				mk(folder+"/4.flac", garbage),
			},
			want: map[string]string{
				folder + "/2.flac": clean,
				folder + "/3.flac": clean,
				folder + "/4.flac": clean,
			},
		},
		{
			name: "all tracks album == folder name (legit) → no clean sibling → skip",
			in: []ReconcileTarget{
				mk(folder+"/1.flac", garbage),
				mk(folder+"/2.flac", garbage),
			},
			want: map[string]string{},
		},
		{
			name: "multiple distinct clean titles → ambiguous → skip",
			in: []ReconcileTarget{
				mk(folder+"/1.flac", clean),
				mk(folder+"/2.flac", "Something Else"),
				mk(folder+"/3.flac", garbage),
			},
			want: map[string]string{},
		},
		{
			name: "no garbage → skip",
			in: []ReconcileTarget{
				mk(folder+"/1.flac", clean),
				mk(folder+"/2.flac", clean),
			},
			want: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fixes(reconcileAlbumTitles(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// reconcileYearsByMBID fills a year-0 stray's year from a same-MBID sibling,
// bounded to genuine strays so it can't merge full copies / editions.
func TestReconcileYearsByMBID(t *testing.T) {
	yp := func(y int) *int { return &y }
	mk := func(path string, year *int, mbid string) ReconcileTarget {
		return ReconcileTarget{Path: path, Year: year, MusicBrainzAlbumID: mbid}
	}
	fixes := func(changed []ReconcileTarget) map[string]int {
		m := map[string]int{}
		for _, c := range changed {
			if c.Year != nil {
				m[c.Path] = *c.Year
			}
		}
		return m
	}

	const mbid = "f51a95d4-98a5-43f1-8c4a-c2cab20a9cbd"

	cases := []struct {
		name string
		in   []ReconcileTarget
		want map[string]int
	}{
		{
			name: "single stray filled from same-MBID sibling (Birdy)",
			in: []ReconcileTarget{
				mk("A/Birdy/1.flac", yp(2011), mbid),
				mk("A/Birdy/2.flac", yp(2011), mbid),
				mk("B/loose/stray.flac", yp(0), mbid), // year-0 stray in another folder
			},
			want: map[string]int{"B/loose/stray.flac": 2011},
		},
		{
			name: "too many year-0 tracks (full copy / edition) → skip",
			in: []ReconcileTarget{
				mk("A/x/1.flac", yp(1998), mbid),
				mk("B/copy/1.flac", yp(0), mbid),
				mk("B/copy/2.flac", yp(0), mbid),
				mk("B/copy/3.flac", yp(0), mbid),
				mk("B/copy/4.flac", yp(0), mbid), // 4 > maxStrayTracksForYearFill(3)
			},
			want: map[string]int{},
		},
		{
			name: "at the stray cap (3) → filled",
			in: []ReconcileTarget{
				mk("A/x/1.flac", yp(2023), mbid),
				mk("B/s/1.flac", nil, mbid),
				mk("B/s/2.flac", yp(0), mbid),
				mk("B/s/3.flac", nil, mbid),
			},
			want: map[string]int{"B/s/1.flac": 2023, "B/s/2.flac": 2023, "B/s/3.flac": 2023},
		},
		{
			name: "empty MBID → skip",
			in: []ReconcileTarget{
				mk("A/x/1.flac", yp(2020), ""),
				mk("A/x/2.flac", yp(0), ""),
			},
			want: map[string]int{},
		},
		{
			name: "local- artwork sentinel is not a release id → skip",
			in: []ReconcileTarget{
				mk("A/x/1.flac", yp(2020), "local-abc123"),
				mk("A/x/2.flac", yp(0), "local-abc123"),
			},
			want: map[string]int{},
		},
		{
			name: "no present year to fill from → skip",
			in: []ReconcileTarget{
				mk("A/x/1.flac", yp(0), mbid),
				mk("A/x/2.flac", nil, mbid),
			},
			want: map[string]int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fixes(reconcileYearsByMBID(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
