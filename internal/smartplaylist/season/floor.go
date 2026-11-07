package season

import "strings"

// The Christmas family's floor, the owner's rule (backlog B750): a mix
// holds MinTracks tracks at least, from MinSpread distinct albums or
// MinSpread distinct artists. A single album by a single artist is never a
// family, even a 40-track box set: that is an album, which can be played as
// one.
//
// It is one predicate, MeetsFloor, asked by both layers (round 4 of backlog
// B489): the engine asks it of the whole qualifying pool, since a client that
// declares every gated format may be sent all of it, and the API asks it of
// each request's view of the cached family, which is the family less the
// formats the request does not declare, and leaves the family out of a
// response whose view falls below it.
const (
	MinTracks = 15
	MinSpread = 3
)

// MeetsFloor reports whether tracks clear the Christmas family's floor:
// MinTracks of them at least, from MinSpread distinct albums or MinSpread
// distinct artists. album and artist read a track's album and its artist (the
// track's own, so a Various Artists compilation counts by its performers),
// each named as the engine's diversity caps name them, case and surrounding
// space aside; an empty one names nothing. A track whose album is not known
// (a family cached before its items stored one) is judged on its artist
// alone.
func MeetsFloor[T any](tracks []T, album, artist func(T) string) bool {
	if len(tracks) < MinTracks {
		return false
	}
	albums := map[string]struct{}{}
	artists := map[string]struct{}{}
	for _, t := range tracks {
		if k := spreadKey(album(t)); k != "" {
			albums[k] = struct{}{}
		}
		if k := spreadKey(artist(t)); k != "" {
			artists[k] = struct{}{}
		}
		if len(albums) >= MinSpread || len(artists) >= MinSpread {
			return true
		}
	}
	return false
}

// spreadKey names an album or an artist as the engine's diversity caps do
// (smartplaylist's diversityKey, which this leaf cannot import): lower case,
// without surrounding space.
func spreadKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
