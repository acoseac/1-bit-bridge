package dupes

import (
	"strings"
	"testing"
)

// flacRow builds a fully-known FLAC member for tier tests.
func flacRow(path string, rate, bits int, dur float64, size int64) Row {
	return Row{
		Path: path, Title: "Song", Album: "Album", Artist: "Artist",
		Track: 1, TrackTagged: true, Disc: 1, DiscTagged: true,
		Size: size, Duration: dur, SampleRate: rate, BitsPerSample: bits,
		Codec: "FLAC",
	}
}

func TestSelfNestDepth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"Chicago/CD 01/CD 01/CD 01/x.flac", 3},
		{"Chicago/CD 01/CD 01/x.flac", 2},
		{"A/CD 01/CD 02/x.flac", 1},             // negative pin: distinct discs
		{"A/Live/Live at the Apollo/x.flac", 1}, // negative pin: prefix ≠ identity
		{"A/B/x.flac", 1},
		{"", 0},
	}
	for _, c := range cases {
		if got := SelfNestDepth(c.in); got != c.want {
			t.Errorf("SelfNestDepth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCollapseNestedSegments(t *testing.T) {
	cases := []struct{ in, want string }{
		{"A/CD 01/CD 01/CD 01/x.flac", "A/CD 01/x.flac"},
		{"A/CD 01/CD 02/x.flac", "A/CD 01/CD 02/x.flac"},
		{"A/Live/Live at the Apollo/x.flac", "A/Live/Live at the Apollo/x.flac"},
		{`A\CD 01\CD 01\x.flac`, "A/CD 01/x.flac"}, // Windows-hosted store paths
	}
	for _, c := range cases {
		if got := collapseNestedSegments(c.in); got != c.want {
			t.Errorf("collapseNestedSegments(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestClassify_SelfNestedNeedsNestTwins pins the tier-membership rule:
// collapsed-path EQUALITY between members, not a bare depth≥2 anywhere.
// An eponymous album ("Metallica/Metallica/…", a run of 2) must NOT read
// as an upload accident when its duplicate sibling lives elsewhere.
func TestClassify_SelfNestedNeedsNestTwins(t *testing.T) {
	// Real nest twins: same file at two nesting depths.
	nested := []Row{
		flacRow("Chicago/CD 01/CD 01/x.flac", 44100, 16, 200, 1000),
		flacRow("Chicago/CD 01/CD 01/CD 01/x.flac", 44100, 16, 200, 1000),
	}
	if got := classify(nested); got != TierSelfNested {
		t.Fatalf("nest twins: got %s, want self-nested", got)
	}
	// Eponymous album + a remaster copy elsewhere: run of 2 exists, but
	// the collapsed paths differ → NOT self-nested (falls through to the
	// format tiers).
	eponymous := []Row{
		flacRow("Metallica/Metallica/One.flac", 44100, 16, 200, 1000),
		flacRow("Metallica Remastered/Metallica/One.flac", 44100, 16, 200, 1100),
	}
	if got := classify(eponymous); got == TierSelfNested {
		t.Fatal("eponymous-album pair must not classify as self-nested")
	}
}

func TestClassify_Tiers(t *testing.T) {
	cases := []struct {
		name    string
		members []Row
		want    Tier
	}{
		{
			name: "same-format",
			members: []Row{
				flacRow("A/B/x.flac", 44100, 16, 263.73, 26634341),
				flacRow("C/B/x.flac", 44100, 16, 263.73, 26817127), // the Patty Griffin shape: size differs, audio agrees
			},
			want: TierSameFormat,
		},
		{
			name: "different-format is different masters",
			members: []Row{
				flacRow("A/B/x.flac", 96000, 24, 200, 5000),
				flacRow("C/B/x.flac", 48000, 24, 200, 3000),
			},
			want: TierDifferentFormat,
		},
		{
			name: "dsd vs pcm is different-format",
			members: []Row{
				flacRow("A/B/x.flac", 96000, 24, 200, 5000),
				func() Row {
					r := flacRow("C/B/x.dsf", 2822400, 1, 200, 9000)
					r.Codec = "DSF"
					r.IsDSD = true
					return r
				}(),
			},
			want: TierDifferentFormat,
		},
		{
			name: "duration disagreement demotes",
			members: []Row{
				flacRow("A/B/x.flac", 44100, 16, 200, 5000),
				flacRow("C/B/x.flac", 44100, 16, 205, 5000),
			},
			want: TierInconclusive,
		},
		{
			name: "unknown geometry demotes",
			members: []Row{
				flacRow("A/B/x.flac", 44100, 16, 200, 5000),
				func() Row {
					r := flacRow("C/B/x.flac", 0, 0, 200, 5000)
					r.Codec = ""
					return r
				}(),
			},
			want: TierInconclusive,
		},
		{
			name: "unknown duration demotes",
			members: []Row{
				flacRow("A/B/x.flac", 44100, 16, 0, 5000),
				flacRow("C/B/x.flac", 44100, 16, 200, 5000),
			},
			want: TierInconclusive,
		},
		{
			name: "version-token asymmetry demotes",
			members: []Row{
				flacRow("A/Album/x.flac", 44100, 16, 200, 5000),
				flacRow("A/Album (Live)/x.flac", 44100, 16, 200, 5000),
			},
			want: TierInconclusive,
		},
		{
			name: "symmetric version tokens do NOT demote (The Mix, both sides)",
			members: []Row{
				flacRow("Kraftwerk/The Mix/x.flac", 44100, 16, 200, 5000),
				flacRow("Copies/The Mix/x.flac", 44100, 16, 200, 5000),
			},
			want: TierSameFormat,
		},
	}
	for _, c := range cases {
		if got := classify(c.members); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestVersionTokenSet_WholeTokensOnly(t *testing.T) {
	// "mono" must not hit "Monomania" (one letter-run), but must hit
	// "(Mono)" and digit-broken runs like "Mono2009".
	if toks := versionTokenSet(Row{Path: "Deerhunter/Monomania/x.flac"}); len(toks) != 0 {
		t.Fatalf("Monomania must not match: %v", toks)
	}
	if toks := versionTokenSet(Row{Album: "Pet Sounds (Mono)"}); len(toks) != 1 || toks[0] != "mono" {
		t.Fatalf("(Mono) must match: %v", toks)
	}
	if toks := versionTokenSet(Row{Album: "Abbey Road Remastered2009"}); len(toks) != 1 || toks[0] != "remastered" {
		t.Fatalf("Remastered2009 must match remastered: %v", toks)
	}
}

func TestCollector_TwoPassRetainsOnlyGroups(t *testing.T) {
	a1 := flacRow("A/B/x.flac", 44100, 16, 200, 1000)
	a2 := flacRow("C/B/x.flac", 44100, 16, 200, 900)
	solo := flacRow("D/E/y.flac", 44100, 16, 100, 500)
	solo.Title = "Other"

	c := NewCollector()
	for _, r := range []Row{a1, a2, solo} {
		c.Note(r)
	}
	c.Seal()
	for _, r := range []Row{a1, a2, solo} {
		c.Collect(r)
	}
	groups := c.Groups()
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]
	if len(g.Members) != 2 || g.Tier != TierSameFormat {
		t.Fatalf("group = %d members, tier %s", len(g.Members), g.Tier)
	}
	// Members sorted by path.
	if g.Members[0].Path != "A/B/x.flac" || g.Members[1].Path != "C/B/x.flac" {
		t.Fatalf("members not path-sorted: %s, %s", g.Members[0].Path, g.Members[1].Path)
	}
	if c.Observed() != 3 {
		t.Fatalf("observed = %d, want 3", c.Observed())
	}
	// Bytes in the non-largest copies.
	if got := g.RedundantBytes(); got != 900 {
		t.Fatalf("RedundantBytes = %d, want 900", got)
	}
}

// TestCollector_RowVanishingBetweenPassesDropsGroup covers the live-DB
// drift case: a key counted twice in pass 1 but collected once in pass 2
// must not surface as a one-member "group".
func TestCollector_RowVanishingBetweenPassesDropsGroup(t *testing.T) {
	a1 := flacRow("A/B/x.flac", 44100, 16, 200, 1000)
	a2 := flacRow("C/B/x.flac", 44100, 16, 200, 900)
	c := NewCollector()
	c.Note(a1)
	c.Note(a2)
	c.Seal()
	c.Collect(a1) // a2 deleted between passes
	if groups := c.Groups(); len(groups) != 0 {
		t.Fatalf("got %d groups, want 0", len(groups))
	}
	// And a key that appears only in pass 2 (row born between passes) is
	// ignored rather than invented.
	born := flacRow("New/B/z.flac", 44100, 16, 100, 100)
	born.Title = "Born"
	c.Collect(born)
	if groups := c.Groups(); len(groups) != 0 {
		t.Fatalf("pass-2-only key must be ignored, got %d groups", len(groups))
	}
}

func TestGroupsAreDeterministicallyOrdered(t *testing.T) {
	mk := func(album string, path string) Row {
		r := flacRow(path, 44100, 16, 200, 100)
		r.Album = album
		return r
	}
	rows := []Row{
		mk("Zeta", "z1/Zeta/x.flac"), mk("Zeta", "z2/Zeta/x.flac"),
		mk("Alpha", "a1/Alpha/x.flac"), mk("Alpha", "a2/Alpha/x.flac"),
	}
	c := NewCollector()
	for _, r := range rows {
		c.Note(r)
	}
	c.Seal()
	for _, r := range rows {
		c.Collect(r)
	}
	groups := c.Groups()
	if len(groups) != 2 {
		t.Fatalf("got %d groups", len(groups))
	}
	if !strings.Contains(groups[0].Key.AlbumID, "alpha") || !strings.Contains(groups[1].Key.AlbumID, "zeta") {
		t.Fatalf("groups not key-ordered: %q, %q", groups[0].Key.AlbumID, groups[1].Key.AlbumID)
	}
}
