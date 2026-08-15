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
	f.Add("Artist/Album/07 Song.flac", "Artist", "Album", "Song", 1, 7, 2020)
	f.Add("Artist/Album/Album/07 Song.flac", "Artist", "Album", "Song", 1, 7, 2020)
	f.Add("", "", "", "", 0, 0, 0)
	f.Fuzz(func(t *testing.T, path, albumArtist, album, title string, disc, track, year int) {
		r := Row{
			Path: path, AlbumArtist: albumArtist, Album: album, Title: title,
			Disc: disc, Track: track, Year: year,
		}
		k := KeyFor(r)
		if k != KeyFor(r) {
			t.Fatalf("KeyFor is not deterministic for %+v", r)
		}
		if k.ID() != k.ID() {
			t.Fatalf("Key.ID is not deterministic for %+v", k)
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
