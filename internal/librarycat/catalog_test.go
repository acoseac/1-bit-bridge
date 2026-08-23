package librarycat

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

var epoch = time.Unix(0, 0).UTC()

func build(rows ...Row) *Catalog {
	b := New()
	for _, r := range rows {
		b.Add(r)
	}
	return b.Build(epoch)
}

// TestAlbumPartitionMatchesDupes is THE contract test. If the catalog's
// grouping and dupes.KeyFor's AlbumID ever disagree, an album tile in
// the browser and an album tile on the phone are different albums —
// which is the one thing this whole package exists to prevent.
func TestAlbumPartitionMatchesDupes(t *testing.T) {
	rows := []Row{
		{Path: "Artist/Album/01 - A.flac", Title: "A", Artist: "Artist", Album: "Album", Year: 2001},
		{Path: "Artist/Album/02 - B.flac", Title: "B", Artist: "Artist", Album: "Album", Year: 2001},
		{Path: "2go/Music/Aerosmith/O, YEAH!/01-Mama Kin.dsf"},
		{Path: "2go/Music/Aerosmith/O, YEAH!/02-Dream On.dsf"},
		{Path: "V/Comp [2014]/03. T.flac", AlbumArtist: "Various Artists", Album: "Comp [2014]"},
		{Path: "X/Alph/1.flac", AlbumArtist: "Alphaville; Deutsches Filmorchester", Album: "Alph"},
		{Path: "X/Alph/2.flac", AlbumArtist: "Alphaville", Album: "Alph"},
	}
	cat := build(rows...)

	// Group the same rows independently through dupes and require the
	// two partitions to be identical as SETS OF PATH SETS.
	viaDupes := map[string][]string{}
	for _, r := range rows {
		k := dupes.KeyFor(r.dupeRow()).AlbumID
		viaDupes[k] = append(viaDupes[k], r.Path)
	}
	viaCatalog := map[string][]string{}
	for _, a := range cat.Albums {
		viaCatalog[a.ID] = append([]string(nil), a.TrackPaths...)
	}
	if len(viaDupes) != len(viaCatalog) {
		t.Fatalf("partition size: dupes %d albums, catalog %d", len(viaDupes), len(viaCatalog))
	}
	for key, paths := range viaDupes {
		got, ok := viaCatalog[HashID(key)]
		if !ok {
			t.Fatalf("dupes album %q has no catalog counterpart", key)
		}
		sort.Strings(paths)
		gotCopy := append([]string(nil), got...)
		sort.Strings(gotCopy)
		if !reflect.DeepEqual(paths, gotCopy) {
			t.Errorf("album %q members: dupes %v, catalog %v", key, paths, gotCopy)
		}
	}
}

// TestUntaggedRowsSplitByDirectory is the measured case: on the
// reference library 1,943 tracks carry no album tag, and a raw-tag
// GROUP BY collapses all of them into one absurd 1,943-track
// pseudo-album. The path-inferred fallback is what splits them.
func TestUntaggedRowsSplitByDirectory(t *testing.T) {
	cat := build(
		Row{Path: "2go/Music/Aerosmith/O, YEAH!/01-Mama Kin.dsf"},
		Row{Path: "2go/Music/Aerosmith/O, YEAH!/02-Dream On.dsf"},
		Row{Path: "2go/Music/Dire Straits/Brothers in Arms/01-So Far Away.flac"},
	)
	if len(cat.Albums) != 2 {
		t.Fatalf("got %d albums, want 2 — untagged rows must split by directory, "+
			"not collapse into one bucket", len(cat.Albums))
	}
	byTitle := map[string]Album{}
	for _, a := range cat.Albums {
		byTitle[a.Title] = a
	}
	if a, ok := byTitle["O, YEAH!"]; !ok || a.TrackCount != 2 || a.AlbumArtist != "Aerosmith" {
		t.Errorf("Aerosmith album not derived from the path: %+v", a)
	}
	if _, ok := byTitle["Brothers in Arms"]; !ok {
		t.Errorf("Dire Straits album not derived from the path; got %v", byTitle)
	}
}

// TestDiscInferenceOrdersMultiDiscAlbums is the single most likely
// real-world breakage: discNumber is tagged on 38 of 15,370 rows on
// the reference library, so a catalog that ordered on raw tags would
// interleave both discs of every box set.
func TestDiscInferenceOrdersMultiDiscAlbums(t *testing.T) {
	// Disc folders "CD 2" and "CD 10" are chosen so LEXICOGRAPHIC PATH
	// ORDER INVERTS numeric disc order ("CD 10" < "CD 2" as strings).
	// Without that inversion a fixture passes even when the sort has
	// fallen back to path — which is how the first two versions of this
	// test passed under a control that broke the ordering. The
	// directory component dominates a path compare, so varying only the
	// filenames is not enough.
	cat := build(
		Row{Path: "A/Set/CD 10/01 D10T1.flac", Album: "Set", AlbumArtist: "A"},
		Row{Path: "A/Set/CD 2/02 D2T2.flac", Album: "Set", AlbumArtist: "A"},
		Row{Path: "A/Set/CD 2/01 D2T1.flac", Album: "Set", AlbumArtist: "A"},
		Row{Path: "A/Set/CD 10/02 D10T2.flac", Album: "Set", AlbumArtist: "A"},
	)
	if len(cat.Albums) != 1 {
		t.Fatalf("disc subfolders must stay ONE album, got %d", len(cat.Albums))
	}
	a := cat.Albums[0]
	if a.DiscCount != 2 {
		t.Errorf("DiscCount = %d, want 2", a.DiscCount)
	}
	want := []string{
		"A/Set/CD 2/01 D2T1.flac",
		"A/Set/CD 2/02 D2T2.flac",
		"A/Set/CD 10/01 D10T1.flac",
		"A/Set/CD 10/02 D10T2.flac",
	}
	if !reflect.DeepEqual(a.TrackPaths, want) {
		t.Errorf("track order = %v, want %v", a.TrackPaths, want)
	}
}

// TestFoldIsInputOrderIndependent — every tie-break resolves on values,
// never on arrival order or map iteration. Cheap, and it catches the
// whole class at once.
func TestFoldIsInputOrderIndependent(t *testing.T) {
	rows := []Row{
		{Path: "A/X/1.flac", Album: "X", AlbumArtist: "A", Artist: "A", Genre: "Rock; Pop",
			Composer: "Beethoven, Ludwig van", Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, IndexedAt: 10},
		{Path: "A/X/2.flac", Album: "X", AlbumArtist: "A", Artist: "A; B", Genre: "Rock",
			Composer: "Ludwig van Beethoven", Codec: "FLAC", SampleRate: 96000, BitsPerSample: 24, IndexedAt: 20},
		{Path: "B/Y/1.flac", Album: "Y", AlbumArtist: "B", Artist: "B", Genre: "Jazz",
			Codec: "MP3", SampleRate: 44100, IndexedAt: 5},
		{Path: "C/Z/1.dsf", Album: "Z", AlbumArtist: "C", IsDSD: true, Codec: "DSF", IndexedAt: 7},
	}
	base := build(rows...)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 12; i++ {
		shuffled := append([]Row(nil), rows...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := build(shuffled...); !reflect.DeepEqual(got, base) {
			t.Fatalf("catalog differs after shuffle %d — the fold is order-dependent", i)
		}
	}
}

// TestPrimaryAlbumTileIsStable — albumIDs[0] is the cover a genre or
// artist tile renders, so a set-order fold would reshuffle it on every
// rebuild and the index would visibly flicker.
func TestPrimaryAlbumTileIsStable(t *testing.T) {
	rows := []Row{
		{Path: "A/Big/1.flac", Album: "Big", AlbumArtist: "A", Genre: "Rock"},
		{Path: "A/Big/2.flac", Album: "Big", AlbumArtist: "A", Genre: "Rock"},
		{Path: "A/Big/3.flac", Album: "Big", AlbumArtist: "A", Genre: "Rock"},
		{Path: "B/Small/1.flac", Album: "Small", AlbumArtist: "B", Genre: "Rock"},
	}
	first := build(rows...).Genres[0].AlbumIDs[0]
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 8; i++ {
		s := append([]Row(nil), rows...)
		rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
		if got := build(s...).Genres[0].AlbumIDs[0]; got != first {
			t.Fatalf("primary tile changed between rebuilds: %q → %q", first, got)
		}
	}
}

// TestSecondaryArtistsGetRows mirrors the client: every "; " segment of
// BOTH credits gets an artist row, so a featured credit is findable
// without owning album identity.
func TestSecondaryArtistsGetRows(t *testing.T) {
	cat := build(
		Row{Path: "A/X/1.flac", Album: "X", AlbumArtist: "Alphaville", Artist: "Alphaville; Marian Gold"},
	)
	names := map[string]bool{}
	for _, a := range cat.Artists {
		names[a.Name] = true
	}
	if !names["Alphaville"] || !names["Marian Gold"] {
		t.Errorf("both credits must get rows, got %v", names)
	}
	// The album artist still owns album identity.
	if len(cat.Albums) != 1 || cat.Albums[0].AlbumArtist != "Alphaville" {
		t.Errorf("album artist = %q, want Alphaville", cat.Albums[0].AlbumArtist)
	}
}

// TestAlbumTitleIsDisplayCleaned — the routed library's folder names
// carry bracket cruft, and the tile must read like the phone's.
func TestAlbumTitleIsDisplayCleaned(t *testing.T) {
	cat := build(Row{Path: "A/x.flac", AlbumArtist: "A", Album: "Almost Heaven [1991]"})
	if got := cat.Albums[0].Title; got != "Almost Heaven" {
		t.Errorf("title = %q, want %q", got, "Almost Heaven")
	}
}

// TestCommonDirComparesWholeSegments — a prefix compare would return a
// directory that exists on neither member.
func TestCommonDirComparesWholeSegments(t *testing.T) {
	for _, tc := range []struct {
		dirs []string
		want string
	}{
		{[]string{"A/Live", "A/Live Deluxe"}, "A"},
		{[]string{"A/Set/CD 01", "A/Set/CD 02"}, "A/Set"},
		{[]string{"A/Album", "A/Album"}, "A/Album"},
		{[]string{"A/X", "B/Y"}, ""},
		{[]string{"A/X"}, "A/X"},
		{nil, ""},
	} {
		if got := commonDir(tc.dirs); got != tc.want {
			t.Errorf("commonDir(%v) = %q, want %q", tc.dirs, got, tc.want)
		}
	}
}

// TestRoutedFlags — an album is "routed" only when EVERY member is,
// so a mixed album isn't wrongly greyed out when its upstream drops.
func TestRoutedFlags(t *testing.T) {
	all := build(
		Row{Path: "2go/A/X/1.flac", Album: "X", AlbumArtist: "A", RoutedUDN: "uuid:1"},
		Row{Path: "2go/A/X/2.flac", Album: "X", AlbumArtist: "A", RoutedUDN: "uuid:1"},
	).Albums[0]
	if !all.Routed || len(all.RoutedUDNs) != 1 {
		t.Errorf("fully routed album: Routed=%v UDNs=%v", all.Routed, all.RoutedUDNs)
	}
	mixed := build(
		Row{Path: "A/X/1.flac", Album: "X", AlbumArtist: "A"},
		Row{Path: "A/X/2.flac", Album: "X", AlbumArtist: "A", RoutedUDN: "uuid:1"},
	).Albums[0]
	if mixed.Routed {
		t.Error("a mixed album must not report as fully routed")
	}
}

// TestAddedAtIsMinIndexedAt — MIN, not MAX. indexed_at is bumped by
// nine writers, so MAX means one enrichment retry makes the whole
// library read as "just added"; MIN degrades gracefully because a
// variant write bumps only the row it touched.
func TestAddedAtIsMinIndexedAt(t *testing.T) {
	a := build(
		Row{Path: "A/X/1.flac", Album: "X", AlbumArtist: "A", IndexedAt: 300},
		Row{Path: "A/X/2.flac", Album: "X", AlbumArtist: "A", IndexedAt: 100},
		Row{Path: "A/X/3.flac", Album: "X", AlbumArtist: "A", IndexedAt: 200},
	).Albums[0]
	if a.AddedAt != 100 {
		t.Errorf("AddedAt = %d, want 100 (the minimum)", a.AddedAt)
	}
	if a.UpdatedAt != 300 {
		t.Errorf("UpdatedAt = %d, want 300 (the maximum)", a.UpdatedAt)
	}
}

func TestTrackAlbumIndex(t *testing.T) {
	cat := build(Row{Path: "A/X/1.flac", Album: "X", AlbumArtist: "A"})
	id, ok := cat.AlbumIDForPath("A/X/1.flac")
	if !ok || id != cat.Albums[0].ID {
		t.Errorf("AlbumIDForPath = %q,%v; want %q,true", id, ok, cat.Albums[0].ID)
	}
	if _, ok := cat.AlbumIDForPath("nope"); ok {
		t.Error("unknown path must miss")
	}
}

func TestHashIDIsBoundedAlphabet(t *testing.T) {
	for _, key := range []string{"", "a|b|1999", "../etc/passwd", "Ünïcödé; ok/slash"} {
		id := HashID(key)
		if len(id) != 16 {
			t.Errorf("HashID(%q) length %d, want 16", key, len(id))
		}
		for _, r := range id {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Errorf("HashID(%q) = %q contains a non-hex rune %q — the route regex "+
					"must be able to validate it before any lookup", key, id, r)
			}
		}
	}
}
