package librarycat

import (
	"reflect"
	"testing"
)

// TestAlbumSourceIDsCoverEveryMix pins the three shapes the source
// facet has to tell apart, including the one an album spanning both
// places creates: it belongs to BOTH, because membership is "has a
// track here", not "belongs here".
func TestAlbumSourceIDsCoverEveryMix(t *testing.T) {
	const key = "uuid:2go"
	cat := build(
		Row{Path: "L/Home/1.flac", Album: "Home", AlbumArtist: "A", Artist: "A"},
		Row{Path: "R/Away/1.flac", Album: "Away", AlbumArtist: "B", Artist: "B", RoutedUDN: key},
		Row{Path: "L/Split/1.flac", Album: "Split", AlbumArtist: "C", Artist: "C"},
		Row{Path: "R/Split/2.flac", Album: "Split", AlbumArtist: "C", Artist: "C", RoutedUDN: key},
	)

	want := map[string][]string{
		"Home":  {LocalSourceID},
		"Away":  {SourceID(key)},
		"Split": {SourceID(key), LocalSourceID}, // sorted: hex before "local"
	}
	for _, a := range cat.Albums {
		if got := want[a.Title]; !reflect.DeepEqual(a.SourceIDs, got) {
			t.Errorf("%s SourceIDs = %v, want %v", a.Title, a.SourceIDs, got)
		}
	}

	// The per-track totals are attributed by the row's own source, so
	// they sum to the library even though Split is in both lists.
	if n := cat.SourceTracks[LocalSourceID]; n != 2 {
		t.Errorf("local track count = %d, want 2", n)
	}
	if n := cat.SourceTracks[SourceID(key)]; n != 2 {
		t.Errorf("upstream track count = %d, want 2", n)
	}
}

// TestLocalSourceIDCannotCollideWithAHashedOne guards the one thing
// that makes a magic token safe beside hashed ids: it is outside their
// alphabet, so no routing key can ever hash onto it and be admitted by
// the wrong branch of the filter.
func TestLocalSourceIDCannotCollideWithAHashedOne(t *testing.T) {
	if len(LocalSourceID) == 16 {
		t.Fatalf("LocalSourceID %q is the same length as a HashID; "+
			"a collision is no longer structurally impossible", LocalSourceID)
	}
	for _, key := range []string{"uuid:x", "manual:abc", "", LocalSourceID} {
		if got := SourceID(key); got == LocalSourceID {
			t.Errorf("SourceID(%q) = %q, which collides with the local sentinel", key, got)
		}
	}
}

// TestSourceIDIsDisjointFromTheOtherIDSpaces pins the "source:" prefix.
// Without it a routing key that happened to equal an album's natural
// key would produce the same id, and a source filter would silently
// accept an album id (or the reverse).
func TestSourceIDIsDisjointFromTheOtherIDSpaces(t *testing.T) {
	const key = "uuid:2go"
	if SourceID(key) == HashID(key) {
		t.Error("SourceID is a bare HashID; the source id space is not disjoint " +
			"from the album/artist/axis one")
	}
}

// TestAlbumTracksAlignsWithAlbumIDs pins the parallel-slice contract
// both narrowing paths read by index. They are emitted together by
// rankAlbums precisely so they cannot drift, and a filtered count is
// only true while that holds.
func TestAlbumTracksAlignsWithAlbumIDs(t *testing.T) {
	cat := build(
		// Two albums for one artist, with different track counts, so a
		// swapped or truncated slice is visible rather than symmetric.
		Row{Path: "A/One/1.flac", Album: "One", AlbumArtist: "Solo", Artist: "Solo", Genre: "G"},
		Row{Path: "A/Two/1.flac", Album: "Two", AlbumArtist: "Solo", Artist: "Solo", Genre: "G"},
		Row{Path: "A/Two/2.flac", Album: "Two", AlbumArtist: "Solo", Artist: "Solo", Genre: "G"},
		Row{Path: "A/Two/3.flac", Album: "Two", AlbumArtist: "Solo", Artist: "Solo", Genre: "G"},
	)

	check := func(what string, ids []string, tracks []int, total int) {
		t.Helper()
		if len(ids) != len(tracks) {
			t.Fatalf("%s: %d album ids but %d counts — the slices have drifted apart",
				what, len(ids), len(tracks))
		}
		sum := 0
		byID := map[string]int{}
		for i, id := range ids {
			sum += tracks[i]
			byID[id] = tracks[i]
		}
		if sum != total {
			t.Errorf("%s: per-album counts sum to %d, want the group's %d", what, sum, total)
		}
		// Resolve one through the catalog: the ids must be the PUBLIC
		// ones, or the narrowing lookups match nothing and silently
		// empty every filtered view.
		for id, n := range byID {
			a, ok := cat.AlbumByID(id)
			if !ok {
				t.Fatalf("%s: album id %q does not resolve in the catalog", what, id)
			}
			if a.TrackCount != n {
				t.Errorf("%s: album %q holds %d of the group's tracks, catalog says %d total",
					what, a.Title, n, a.TrackCount)
			}
		}
	}

	ar := cat.Artists[0]
	check("artist", ar.AlbumIDs, ar.AlbumTracks, ar.TrackCount)
	g := cat.Genres[0]
	check("genre", g.AlbumIDs, g.AlbumTracks, g.TrackCount)
}
