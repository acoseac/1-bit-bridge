package manifest

import (
	"sort"
	"strings"
)

// ReconcileTarget is the lightweight projection the reconciliation passes
// operate on — just the few fields they need. The scanner streams the
// whole library into `[]ReconcileTarget` (instead of materializing every
// full `Track`, which would spike memory / risk OOM on low-memory hosts
// like a Raspberry Pi), then loads the full `Track` only for the handful
// of rows that actually need rewriting.
type ReconcileTarget struct {
	Path        string
	Album       string
	AlbumArtist string
	Year        *int
	TrackNumber *int
}

// reconcileAlbumArtists finds physical albums — tracks that share a
// directory AND an album title — whose AlbumArtist tags DISAGREE, and
// returns a copy of each outlier track with its AlbumArtist rewritten to
// the group's dominant value. One physical album then carries one
// AlbumArtist, and therefore resolves to ONE album identity on the iOS
// side (where album identity is `normalize(albumArtist)|normalize(album)|year`).
//
// Why this exists: the scanner sets AlbumArtist from raw per-file tags,
// so an album whose tracks were tagged inconsistently — some crediting
// the band, some the leader; a single comma-separated string vs the
// scanner's own "; " multi-value join — splits into 2+ album rows on
// iOS, and cover art lands on only one of them ("split-album syndrome").
//
// Design (consult 2026-06-14):
//   - DIRECTORY-scoped: a group is (directory, normalized album). We
//     NEVER reconcile across directories — that protects classical
//     box-sets with deliberately different per-disc performers, and
//     distinct same-named albums ("Greatest Hits" / "Live") sitting in
//     separate folders, from being falsely merged.
//   - DOMINANT value, not MusicBrainz: pick the most-frequent existing
//     AlbumArtist rather than overriding with the MB release credit —
//     this preserves the user's own curation (classical composer-vs-
//     performer sorting, jazz "feat." credits).
//   - DISAGREEMENT-DRIVEN: a group whose AlbumArtist values already
//     agree is left untouched (do no harm).
//
// Returns only the targets that need changing, sorted by path for
// deterministic output (tests + stable logs). Pure — no I/O.
func reconcileAlbumArtists(tracks []ReconcileTarget) []ReconcileTarget {
	groups := map[string][]int{}
	for i := range tracks {
		key, ok := albumArtistGroupKey(tracks[i].Path, tracks[i].Album)
		if !ok {
			continue
		}
		groups[key] = append(groups[key], i)
	}

	var changed []ReconcileTarget
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue // a lone track can't disagree with itself
		}
		values := make([]string, len(idxs))
		allEqual := true
		for n, i := range idxs {
			values[n] = tracks[i].AlbumArtist
			if values[n] != values[0] {
				allEqual = false
			}
		}
		if allEqual {
			continue // already consistent — do no harm
		}
		dominant, ok := dominantAlbumArtist(values)
		if !ok {
			continue // every value blank — nothing to reconcile to
		}
		for _, i := range idxs {
			if tracks[i].AlbumArtist == dominant {
				continue
			}
			fix := tracks[i]
			fix.AlbumArtist = dominant
			changed = append(changed, fix)
		}
	}

	sort.Slice(changed, func(a, b int) bool { return changed[a].Path < changed[b].Path })
	return changed
}

// reconcileYears finds physical albums — tracks that share a directory AND
// an album title — where some tracks carry a year and others are MISSING
// one, and fills the missing years from the group's dominant year. One
// untagged track (no year) otherwise splits into its own album row on iOS
// (album identity = `normalize(albumArtist)|normalize(album)|year`), exactly
// the field-reported Alphaville "[A] Eternally Yours Bonus-EP" case where a
// single bonus track lost its year tag.
//
// Conservative by design, mirroring reconcileAlbumArtists:
//   - DIRECTORY-scoped: the same (directory, normalized album) grouping —
//     NEVER crosses directories (classical box-set + distinct-same-name
//     protection).
//   - FILL-MISSING only: a present-but-DIFFERENT year is LEFT ALONE. It may
//     be a deliberate per-track value, and overwriting it is a sharper,
//     riskier call than filling an absent one. We only fill years that are
//     absent (nil) or non-positive (the bridge's year=0 "tag absent"
//     sentinel, which iOS already treats as nil).
//   - DOMINANT existing value, not MusicBrainz — preserve the user's tags.
//
// Returns only the targets needing a fill (Year set to the dominant value),
// sorted by path for deterministic output. Pure — no I/O.
func reconcileYears(tracks []ReconcileTarget) []ReconcileTarget {
	groups := map[string][]int{}
	for i := range tracks {
		key, ok := albumArtistGroupKey(tracks[i].Path, tracks[i].Album)
		if !ok {
			continue
		}
		groups[key] = append(groups[key], i)
	}

	var changed []ReconcileTarget
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue // a lone track can't borrow a year from a sibling
		}
		years := make([]*int, len(idxs))
		anyMissing := false
		for n, i := range idxs {
			years[n] = tracks[i].Year
			if !hasYear(tracks[i].Year) {
				anyMissing = true
			}
		}
		if !anyMissing {
			continue // every track already has a year — do no harm
		}
		dom, ok := dominantYear(years)
		if !ok {
			continue // no present year to fill from
		}
		for _, i := range idxs {
			if hasYear(tracks[i].Year) {
				continue
			}
			fix := tracks[i]
			y := dom // fresh per fix; never alias the loop/group value
			fix.Year = &y
			changed = append(changed, fix)
		}
	}

	sort.Slice(changed, func(a, b int) bool { return changed[a].Path < changed[b].Path })
	return changed
}

// hasYear reports whether a year pointer holds a usable (>0) value. nil and
// the year=0 "tag absent" sentinel both count as missing.
func hasYear(y *int) bool { return y != nil && *y > 0 }

// dominantYear returns the most-frequent present (>0) year and true, or
// (0, false) when none is present. Ties break toward the LARGER (more
// recent) year — the likelier release year for the physical album — keeping
// the result deterministic regardless of map-iteration order.
func dominantYear(years []*int) (int, bool) {
	counts := map[int]int{}
	for _, y := range years {
		if hasYear(y) {
			counts[*y]++
		}
	}
	if len(counts) == 0 {
		return 0, false
	}
	best, bestCount := 0, -1
	for y, c := range counts {
		switch {
		case c > bestCount:
			best, bestCount = y, c
		case c == bestCount && y > best:
			best = y
		}
	}
	return best, true
}

// backfillTrackNumbersFromPath fills a MISSING track number (nil or the 0
// "tag absent" sentinel) from the track's filename — the leading "NN" of the
// basename, e.g. ".../06. Congeniality.flac" → 6 (see parseLeadingTrackNumber).
//
// This is the post-scan MIGRATION twin of the extractor-level backfill in
// fillTrackNumberFromFilename: the extractor only runs on (re-)extraction, and
// the scanner skips unchanged files by mtime, so a library indexed before that
// backfill would keep its untagged-track-number rows unordered on iOS forever.
// Running every scan, this fills them once (a clean library then produces zero
// writes).
//
// Unlike reconcileAlbumArtists / reconcileYears this is purely PER-FILE — no
// directory grouping, no dominant vote — because a numbered filename is self-
// describing. The caller excludes UPnP-routed rows (their track numbers belong
// to the upstream DIDL metadata, not bridge-side filename parsing).
//
// Returns only the targets needing a fill (TrackNumber set to the parsed
// value), sorted by path for deterministic output. Pure — no I/O.
func backfillTrackNumbersFromPath(tracks []ReconcileTarget) []ReconcileTarget {
	var changed []ReconcileTarget
	for _, t := range tracks {
		if hasTrackNumber(t.TrackNumber) {
			continue // already has a usable number — do no harm
		}
		n, ok := parseLeadingTrackNumber(pathStem(t.Path))
		if !ok {
			continue // filename carries no leading "NN" prefix
		}
		fix := t
		v := n // fresh per fix; never alias the loop variable
		fix.TrackNumber = &v
		changed = append(changed, fix)
	}
	sort.Slice(changed, func(a, b int) bool { return changed[a].Path < changed[b].Path })
	return changed
}

// hasTrackNumber reports whether a track-number pointer holds a usable (>0)
// value. nil and the 0 "tag absent" sentinel both count as missing. Mirrors
// hasYear.
func hasTrackNumber(n *int) bool { return n != nil && *n > 0 }

// pathStem returns the filename stem of a relative track path: the segment
// after the last "/" or "\" separator, with its extension removed. Separator-
// agnostic (matching trackDir) so it works for both POSIX and Windows stored
// path forms. A leading-dot or extension-less basename is returned as-is.
func pathStem(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	if dot := strings.LastIndex(p, "."); dot > 0 {
		p = p[:dot]
	}
	return p
}

// albumArtistGroupKey returns the (directory + normalized-album) grouping
// key for a track and true, or ("", false) when the track has no album
// title — loose singles are not "an album" and are never reconciled.
func albumArtistGroupKey(path, album string) (string, bool) {
	al := strings.ToLower(strings.TrimSpace(album))
	if al == "" {
		return "", false
	}
	// NUL separates the two components so a directory whose name ends in
	// the album's leading characters can't collide with a different
	// (dir, album) split.
	return trackDir(path) + "\x00" + al, true
}

// trackDir returns the directory portion of a relative track path,
// treating BOTH "/" and "\" as separators — the scanner stores POSIX
// paths on Linux/macOS and may store Windows paths on Windows, and the
// grouping only needs to be self-consistent within one library's paths.
// Root-level files return "".
func trackDir(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return ""
}

// dominantAlbumArtist returns the most-frequent NON-EMPTY value and
// true, or ("", false) when every value is blank. Ties break toward the
// LONGEST value (the more complete credit, e.g. "Peter Asplund;
// Aspiration" over "Aspiration"), then the lexicographically smallest
// for determinism regardless of map-iteration order.
func dominantAlbumArtist(values []string) (string, bool) {
	counts := map[string]int{}
	for _, v := range values {
		if v != "" {
			counts[v]++
		}
	}
	if len(counts) == 0 {
		return "", false
	}
	best := ""
	bestCount := -1
	for v, c := range counts {
		switch {
		case c > bestCount:
			best, bestCount = v, c
		case c == bestCount:
			if len(v) > len(best) || (len(v) == len(best) && v < best) {
				best = v
			}
		}
	}
	return best, true
}
