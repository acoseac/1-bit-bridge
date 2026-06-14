package manifest

import (
	"sort"
	"strings"
)

// ReconcileTarget is the lightweight projection the reconciliation pass
// operates on — just the three fields it needs. The scanner streams the
// whole library into `[]ReconcileTarget` (instead of materializing every
// full `Track`, which would spike memory / risk OOM on low-memory hosts
// like a Raspberry Pi), then loads the full `Track` only for the handful
// of rows that actually need rewriting.
type ReconcileTarget struct {
	Path        string
	Album       string
	AlbumArtist string
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
