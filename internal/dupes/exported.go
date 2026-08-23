package dupes

import "strings"

// Exported façade over the mirror internals.
//
// The unexported functions in clientkey.go stay the source of truth —
// each carries the annotation naming its Swift twin, and each is
// covered by literals lifted verbatim from MetadataNormalizerTests.
// These wrappers exist so internal/librarycat can fold a catalog using
// EXACTLY the client's vocabulary instead of growing a second copy of
// it. A second copy is how the partition drifts, and a drifted
// partition means an album tile in the browser and an album tile on
// the phone are different albums.
//
// Nothing here adds behaviour. SortName is the one genuinely new
// mirror in this file; it had no Go twin because dedup never needed a
// sort order, and the catalog does.

// Normalize mirrors MetadataNormalizer.normalize(_:lower:true):
// trim → collapse whitespace runs → lowercase.
func Normalize(s string) string { return normalize(s) }

// CleanDisplayName mirrors MetadataNormalizer.cleanDisplayName.
func CleanDisplayName(s string) string { return cleanDisplayName(s) }

// CleanArtistName mirrors MetadataNormalizer.cleanArtistName.
func CleanArtistName(s string) string { return cleanArtistName(s) }

// SplitArtistDisplayName mirrors
// MetadataNormalizer.splitArtistDisplayName: split on "; " ONLY.
func SplitArtistDisplayName(s string) []string { return splitArtistDisplayName(s) }

// PrimaryAlbumArtistForGrouping mirrors
// MetadataNormalizer.primaryAlbumArtistForGrouping.
func PrimaryAlbumArtistForGrouping(albumArtist string) string {
	return primaryAlbumArtistForGrouping(albumArtist)
}

// IsCompilationMarker mirrors MetadataNormalizer.isCompilationMarker.
func IsCompilationMarker(s string) bool { return isCompilationMarker(s) }

// SortName mirrors MetadataNormalizer.sortName — the article-stripped
// key for ALPHABETICAL sorting and bucketing ONLY, never for display.
//
// Strips a single leading "the " (case-insensitive) so "The Beatles"
// files under B and "The Cars" under C, matching Apple Music / Plex /
// Roon. Strips ONLY "The" — not "A"/"An", not non-English articles —
// by deliberate product scope, and only when a non-empty remainder
// follows, so a band literally named "The" stays under T and
// "Theremin" is untouched (no trailing space after the article).
// Idempotent.
//
// Allocation-light on the bucketing hot path, the same way the Swift
// twin is: the trim runs only when the input actually carries
// surrounding whitespace, and the first-character check fires before
// the bounded prefix lowercase, so the common clean non-"The" name
// allocates nothing.
func SortName(s string) string {
	leading := len(s) > 0 && isSpaceByte(s[0])
	trailing := len(s) > 0 && isSpaceByte(s[len(s)-1])
	base := s
	if leading || trailing {
		base = trimSpace(s)
	}
	if base == "" {
		return base
	}
	if c := base[0]; c != 't' && c != 'T' {
		return base
	}
	if len(base) < 4 || !strings.EqualFold(base[:4], "the ") {
		return base
	}
	rest := trimSpace(base[4:])
	if rest == "" {
		return base
	}
	return rest
}

// isSpaceByte is the ASCII fast path for the leading/trailing peek.
// A multi-byte Unicode space never starts with an ASCII space byte, so
// a false negative here only costs the trim that trimSpace would have
// performed anyway — trimSpace itself is Unicode-aware.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// ArtistID mirrors MetadataNormalizer.artistID: multi-value display
// strings split on "; " and the PRIMARY segment owns the track's
// identity, matching the Apple Music / Spotify convention where a
// featured credit doesn't change which artist a track belongs to.
func ArtistID(artist string) string {
	segs := splitArtistDisplayName(artist)
	if len(segs) == 0 {
		return normalize(artist)
	}
	return normalize(segs[0])
}
