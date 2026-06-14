package manifest

import "testing"

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
