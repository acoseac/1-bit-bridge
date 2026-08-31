package upnpingest

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// A MediaServer publishes whatever metadata it has, and some have very
// little. On a real Chord 2Go (MiniDLNA) 1,942 of 15,283 walked items
// carried a title and nothing else — every DSF among them, because
// MiniDLNA does not read DSF tags. The enricher skips a row with no
// artist or no album (skipReasonNoSearchTerms), so on that library 0 of
// 1,730 DSF rows had artwork against 7,657 FLAC rows that did.

func walkedUntagged(path string) upnp.Walked {
	return upnp.Walked{
		Path:  path,
		Res:   "http://h:8200/MediaItems/1",
		Size:  1,
		Title: "Some Title",
	}
}

// TestUntaggedRoutedTrackGetsSearchableArtistAndAlbum is the fix itself:
// without artist and album the row is unreachable to the enricher no
// matter how long it sits there.
func TestUntaggedRoutedTrackGetsSearchableArtistAndAlbum(t *testing.T) {
	tr, _ := buildTrackAndRouting(
		walkedUntagged("2go/Music/Otis Redding/Otis Blue/03 - Change Gonna Come.dsf"),
		"uuid:x", time.Now())

	if tr.Artist != "Otis Redding" {
		t.Errorf("Artist = %q; want %q — the enricher cannot search without it",
			tr.Artist, "Otis Redding")
	}
	if tr.Album != "Otis Blue" {
		t.Errorf("Album = %q; want %q", tr.Album, "Otis Blue")
	}
	// AlbumArtist rides along: it is what album identity keys on first,
	// and the DIDL rarely separates the two.
	if tr.AlbumArtist != "Otis Redding" {
		t.Errorf("AlbumArtist = %q; want %q", tr.AlbumArtist, "Otis Redding")
	}
}

// TestDiscSubfolderDoesNotBecomeTheAlbum is the regression guard on WHICH
// derivation is used.
//
// manifest's fillFromPath — the obvious thing to reach for, and what the
// filesystem scanner uses — takes the directory two levels up verbatim,
// which for a multi-disc set is the disc folder. Writing that would split
// every such release into one album per disc, and would do it to rows
// that group CORRECTLY today, because the display layer resolves them
// through dupes, which strips disc folders. 46 rows on the reference
// library sit in this shape.
func TestDiscSubfolderDoesNotBecomeTheAlbum(t *testing.T) {
	for _, disc := range []string{"CD1", "Disc 1", "CD 2"} {
		t.Run(disc, func(t *testing.T) {
			tr, _ := buildTrackAndRouting(
				walkedUntagged("2go/Music/Puccini/Turandot/"+disc+"/01 - Popolo di Pekino!.dsf"),
				"uuid:x", time.Now())

			if tr.Album != "Turandot" {
				t.Errorf("Album = %q; want %q — a disc folder in the album field "+
					"splits the release into one album per disc", tr.Album, "Turandot")
			}
			if tr.Artist != "Puccini" {
				t.Errorf("Artist = %q; want %q", tr.Artist, "Puccini")
			}
		})
	}
}

// TestUpstreamMetadataIsNeverRewritten pins that this only ever FILLS.
//
// The fixtures carry numeric brackets deliberately: dupes.Resolve CLEANS
// the names it returns (cleanDisplayName strips "[65616303]"-style tags),
// and the reference library is full of them — "Cover Sessions (Live)
// [65616303] [2016]" is a real album folder on the 2Go. So a resolved
// value is not always the upstream's value, and writing it through would
// silently normalise metadata the upstream owns.
//
// The cost is not cosmetic. walkFieldsEqual compares Artist and Album, so
// a rewritten row differs from its stored twin on the very next walk:
// every one of the 13,341 tagged rows would re-upsert, which resets
// enriched_at to 0 and pushes an indexed_at bump — a full re-enrichment
// pass and a whole-library delta to every paired device, to change
// nothing a reader asked for.
//
// A plain value like "Tagged Artist" cleans to itself and would pass this
// test against code that overwrites unconditionally — it pins nothing.
//
// What this catches is layered: a both-present row is protected FIRST by
// the early return, so removing only the per-field guards leaves it green
// (removing both turns it red). That is not a hole — the partial-row test
// below covers the guards directly — but it is why this one alone is not
// evidence that they exist.
func TestUpstreamMetadataIsNeverRewritten(t *testing.T) {
	const (
		artist = "John Adams*"
		album  = "Cover Sessions (Live) [65616303] [2016]"
	)
	w := walkedUntagged("2go/Music/Dir Artist/Dir Album/01 - x.flac")
	w.Artist = artist
	w.Album = album

	tr, _ := buildTrackAndRouting(w, "uuid:x", time.Now())
	if tr.Artist != artist {
		t.Errorf("Artist = %q; want %q verbatim — normalising it re-upserts "+
			"every tagged row on the next walk", tr.Artist, artist)
	}
	if tr.Album != album {
		t.Errorf("Album = %q; want %q verbatim", tr.Album, album)
	}
}

// TestPartialUpstreamMetadataFillsOnlyTheGap covers the mixed row: one
// field present, the other not. It has to pass through the fill (the
// both-present case returns early and would prove nothing about it), so
// the present field is again one the cleaner would rewrite.
func TestPartialUpstreamMetadataFillsOnlyTheGap(t *testing.T) {
	const artist = "John Adams*"
	w := walkedUntagged("2go/Music/Dir Artist/Dir Album/01 - x.flac")
	w.Artist = artist

	tr, _ := buildTrackAndRouting(w, "uuid:x", time.Now())
	if tr.Artist != artist {
		t.Errorf("Artist = %q; want %q verbatim", tr.Artist, artist)
	}
	if tr.Album != "Dir Album" {
		t.Errorf("Album = %q; want it derived from the path", tr.Album)
	}
	// AlbumArtist is filled from the row's own artist, so it inherits the
	// verbatim value too — not the cleaned one.
	if tr.AlbumArtist != artist {
		t.Errorf("AlbumArtist = %q; want %q", tr.AlbumArtist, artist)
	}
}

// TestPlaceholderNamesAreNeverPersisted pins the one value that must not
// be written through.
//
// dupes returns "Unknown Artist" / "Unknown Album" for a path too shallow
// to derive from. As display text that is correct. Persisted, it becomes
// a MusicBrainz search term, and any release it matched would be
// attributed to a track that has nothing to do with it — strictly worse
// than the skip it replaced, because a wrong cover looks right.
func TestPlaceholderNamesAreNeverPersisted(t *testing.T) {
	for _, p := range []string{"loose.flac", "OnlyOneLevel/track.flac"} {
		tr, _ := buildTrackAndRouting(walkedUntagged(p), "uuid:x", time.Now())
		if tr.Artist == dupes.UnknownArtist || tr.Album == dupes.UnknownAlbum {
			t.Errorf("path %q persisted a display placeholder: artist=%q album=%q",
				p, tr.Artist, tr.Album)
		}
	}
}

// TestFillingDoesNotMoveAlbumIdentity is the safety property for rows
// ALREADY in the manifest.
//
// The catalog, the duplicate election and the iOS client all key albums
// on dupes.AlbumIDOf(dupes.Resolve(row)). Resolve falls back to the same
// path defaults this fill uses, so persisting them must be a no-op for
// identity — if it were not, every affected album would change id on the
// next walk and each device would see one vanish and another appear.
func TestFillingDoesNotMoveAlbumIdentity(t *testing.T) {
	paths := []string{
		"2go/Music/Otis Redding/Otis Blue/03 - Change Gonna Come.dsf",
		"2go/Music/Puccini/Turandot/Disc 1/01 - Popolo di Pekino!.dsf",
		"2go/Music/Diana Krall/Live in Paris/05-East Of The Sun.dsf",
		"loose.flac",
	}
	for _, p := range paths {
		w := walkedUntagged(p)

		// Identity as the row stood BEFORE the fill: no artist, no album.
		before := dupes.AlbumIDOf(dupes.Resolve(dupes.Row{Path: p, Title: w.Title}))

		tr, _ := buildTrackAndRouting(w, "uuid:x", time.Now())
		after := dupes.AlbumIDOf(dupes.Resolve(dupes.Row{
			Path:        tr.Path,
			Title:       tr.Title,
			Artist:      tr.Artist,
			Album:       tr.Album,
			AlbumArtist: tr.AlbumArtist,
		}))

		if before != after {
			t.Errorf("path %q changed album identity (%s -> %s); every affected "+
				"album would split on the next walk", p, before, after)
		}
	}
}
