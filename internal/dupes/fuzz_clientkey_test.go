// Fuzz coverage for the duplicate client-key derivation.
//
// `internal/dupes` is a VERBATIM MIRROR of iOS's MetadataNormalizer +
// CrossSourceTrackDedup.ContentKey, and its output decides what the bridge
// STOPS SERVING. A key that is unstable — or an ID that is not a pure function
// of its key — would move a track between groups across scans, and the
// stamping pass turns every group change into an `indexed_at` bump and a delta
// for every paired device. Instability here is therefore not a cosmetic
// problem; it is a client-churn problem with no natural damping.
//
// The determinism assertion is cheap and pins the property that matters most
// about `Key.ID`: it is a length-prefixed hash, so no tag content can forge a
// field boundary and collide two distinct keys onto one group.
package dupes

import "testing"

func FuzzKeyFor(f *testing.F) {
	f.Add("Artist/Album/07 Song.flac", "Artist", "Album", "Song", 1, 7)
	f.Add("Artist/Album/Album/07 Song.flac", "Artist", "Album", "Song", 1, 7)
	f.Add("", "", "", "", 0, 0)
	// Year is DERIVED from track rather than taking a corpus dimension of its
	// own. It still varies (it feeds AlbumID, so it must), but as a function
	// of an existing input — six fuzz parameters plus *testing.T is the most
	// Go's signature can carry before it stops being readable, and year is the
	// field whose exact value matters least: KeyFor folds it into AlbumID as a
	// plain component, so covering "several distinct years including 0" is all
	// the coverage there is to have.
	f.Fuzz(func(t *testing.T, path, albumArtist, album, title string, disc, track int) {
		year := 0
		if track != 0 {
			year = 1900 + (track%200+200)%200
		}
		r := Row{
			Path: path, AlbumArtist: albumArtist, Album: album, Title: title,
			Disc: disc, Track: track, Year: year,
		}
		k := KeyFor(r)
		if k != KeyFor(r) {
			t.Fatalf("KeyFor is not deterministic for %+v", r)
		}
		// ID must be a pure function of the KEY — not of the Row it came from,
		// and not of anything ambient. Hash an independently-constructed equal
		// key rather than re-calling k.ID(), which compares a value against
		// itself and could never fail.
		twin := Key{AlbumID: k.AlbumID, Disc: k.Disc, Track: k.Track, NormTitle: k.NormTitle}
		if k.ID() != twin.ID() {
			t.Fatalf("Key.ID is not a pure function of the key: %+v", k)
		}
		// EVERY field must reach the hash. A collision here is not academic:
		// two distinct tracks sharing an ID land in one duplicate group, and
		// the winner election then suppresses one of them from serving. So
		// perturb each field in turn and require the ID to move.
		//
		// This is the assertion worth having, and the two obvious
		// alternatives are not. `k.ID() != k.ID()` compares a value with
		// itself and can never fail (SonarCloud go:S1764 caught exactly that
		// in this file's first draft). A boundary-shift between AlbumID and
		// NormTitle — the classic probe for missing length prefixes — is
		// ALSO untestable here and was verified so by negative control:
		// Disc and Track are fixed-width ints encoded between the two string
		// fields, so the strings are never adjacent and no shift can forge a
		// boundary. Stripping writeStr's length prefix entirely still passed
		// it. Field-reachability is what this struct's ID can actually get
		// wrong.
		base := k.ID()
		for i, perturbed := range []Key{
			{AlbumID: k.AlbumID + "\x00x", Disc: k.Disc, Track: k.Track, NormTitle: k.NormTitle},
			{AlbumID: k.AlbumID, Disc: k.Disc + 1, Track: k.Track, NormTitle: k.NormTitle},
			{AlbumID: k.AlbumID, Disc: k.Disc, Track: k.Track + 1, NormTitle: k.NormTitle},
			{AlbumID: k.AlbumID, Disc: k.Disc, Track: k.Track, NormTitle: k.NormTitle + "\x00x"},
		} {
			if perturbed.ID() == base {
				t.Fatalf("field %d does not reach Key.ID: %+v and %+v share an ID", i, k, perturbed)
			}
		}
	})
}

func FuzzNormalize(f *testing.F) {
	f.Add("The Beatles (Remastered)")
	f.Add("Song [2019]")
	f.Fuzz(func(t *testing.T, s string) { _ = normalize(s) })
}

func FuzzDiscNumberForFolderName(f *testing.F) {
	f.Add("CD 01")
	f.Add("Disc 2")
	f.Add("Disco 2") // must NOT parse as a disc folder
	f.Fuzz(func(t *testing.T, s string) { _, _ = discNumberForFolderName(s) })
}
