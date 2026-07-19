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
	Path               string
	Album              string
	AlbumArtist        string
	Year               *int
	TrackNumber        *int
	MusicBrainzAlbumID string
}

// maxStrayTracksForYearFill bounds the MB-ID cross-folder year fill
// (reconcileYearsByMBID) to genuine STRAYS — a handful of loose tracks that
// lost their year tag. A larger year-0 group under one MB-ID is almost always
// a full second copy of the album (two physical folders) or a distinct edition
// MusicBrainz happens to share a release GID with; filling those would merge
// two real albums into one, so they're left alone (flagged as residual).
const maxStrayTracksForYearFill = 3

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
	groups := groupByAlbumKey(tracks)

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
	groups := groupByAlbumKey(tracks)

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

// reconcileAlbumTitles fixes the "album tag is the folder name" garbage: some
// tracks in a directory carry an album tag that EXACTLY EQUALS the directory's
// basename (a scanning fallback / mis-tag — e.g. the dub-convention folder name
// "[A] Eternally Yours Bonus-EP I [287388724] [2023]"), while their siblings in
// the same folder carry the real album title. Left alone these split into a
// separate Album row on iOS (identity = albumArtist|album|year).
//
// Conservative by design:
//   - DIRECTORY-scoped (all tracks in one folder).
//   - Fires ONLY when SOME tracks' album == the folder basename AND the OTHER
//     titled tracks share EXACTLY ONE distinct clean title — that single clean
//     title is the "true" album name, and the folder-name tracks are rewritten
//     to it. If EVERY titled track's album == the folder basename (a legit album
//     named after its folder → no clean sibling) OR there are multiple distinct
//     clean titles (ambiguous), the folder is SKIPPED (do no harm).
//   - Only the album==folder-basename outliers are rewritten; a track with any
//     other distinct title is never touched.
//
// Returns only the targets needing a rewrite, sorted by path. Pure — no I/O.
func reconcileAlbumTitles(tracks []ReconcileTarget) []ReconcileTarget {
	dirs := map[string][]int{}
	for i := range tracks {
		d := trackDir(tracks[i].Path)
		dirs[d] = append(dirs[d], i)
	}
	dirKeys := make([]string, 0, len(dirs))
	for d := range dirs {
		dirKeys = append(dirKeys, d)
	}
	sort.Strings(dirKeys) // deterministic order

	var changed []ReconcileTarget
	for _, dir := range dirKeys {
		idxs := dirs[dir]
		if len(idxs) < 2 {
			continue
		}
		normBase := normTitle(pathBase(dir))
		if normBase == "" {
			continue
		}
		var garbage []int
		cleanNorms := map[string]struct{}{}
		cleanTitle := ""
		for _, i := range idxs {
			al := strings.TrimSpace(tracks[i].Album)
			if al == "" {
				continue // untitled — not a clean sibling, not folder-name garbage
			}
			nt := normTitle(al)
			if nt == normBase {
				garbage = append(garbage, i)
			} else if _, seen := cleanNorms[nt]; !seen {
				cleanNorms[nt] = struct{}{}
				cleanTitle = al // exemplar of the (single) clean title
			}
		}
		if len(garbage) == 0 || len(cleanNorms) != 1 {
			continue // no garbage, no clean sibling, or ambiguous → skip
		}
		for _, i := range garbage {
			fix := tracks[i]
			fix.Album = cleanTitle
			changed = append(changed, fix)
		}
	}
	sort.Slice(changed, func(a, b int) bool { return changed[a].Path < changed[b].Path })
	return changed
}

// reconcileYearsByMBID fills a year-0 STRAY's year from a sibling that shares
// the same MusicBrainz release id — the CROSS-folder analog of reconcileYears
// (which only fills within one directory). A few loose tracks that lost their
// year tag but carry the album's MBID (e.g. a stray bonus track filed in its
// own folder) borrow the dominant year of the MBID group.
//
// Bounded to STRAYS via maxStrayTracksForYearFill: a large year-0 group under
// one MBID is a full second copy / distinct edition MusicBrainz happens to
// share a release GID with, and filling it would merge two real albums — so
// it's left alone (residual). Only non-empty, non-`local-` MBIDs participate
// (a `local-<hash>` artwork sentinel is NOT a release id).
//
// Returns only the targets needing a fill (Year set), sorted by path. Pure.
func reconcileYearsByMBID(tracks []ReconcileTarget) []ReconcileTarget {
	groups := map[string][]int{}
	for i := range tracks {
		// Lowercase: MusicBrainz ids are case-insensitive UUIDs, but a
		// user-tagged file / external source may write mixed case; group them
		// together (Gemini on PR #498). `local-<hash>` is already lowercase.
		mbid := strings.ToLower(strings.TrimSpace(tracks[i].MusicBrainzAlbumID))
		if mbid == "" || strings.HasPrefix(mbid, "local-") {
			continue // no real release id
		}
		groups[mbid] = append(groups[mbid], i)
	}
	mbidKeys := make([]string, 0, len(groups))
	for k := range groups {
		mbidKeys = append(mbidKeys, k)
	}
	sort.Strings(mbidKeys) // deterministic order

	var changed []ReconcileTarget
	for _, mbid := range mbidKeys {
		idxs := groups[mbid]
		if len(idxs) < 2 {
			continue // a lone track can't borrow a year from a sibling
		}
		years := make([]*int, len(idxs))
		var missing []int
		for n, i := range idxs {
			years[n] = tracks[i].Year
			if !hasYear(tracks[i].Year) {
				missing = append(missing, i)
			}
		}
		if len(missing) == 0 || len(missing) > maxStrayTracksForYearFill {
			continue // no strays, or too many (a full copy / edition) → skip
		}
		dom, ok := dominantYear(years)
		if !ok {
			continue // no present year to fill from
		}
		for _, i := range missing {
			fix := tracks[i]
			y := dom // fresh per fix; never alias the loop/group value
			fix.Year = &y
			changed = append(changed, fix)
		}
	}
	sort.Slice(changed, func(a, b int) bool { return changed[a].Path < changed[b].Path })
	return changed
}

// pathBase returns the last path segment (directory or file basename),
// separator-agnostic (POSIX or Windows) with NO extension stripping — mirrors
// trackDir's separator handling. A root path returns itself.
func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// normTitle trims + lowercases an album title for grouping / comparison
// (matches albumArtistGroupKey's album normalization).
func normTitle(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

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

// groupByAlbumKey buckets track indices by their directory-scoped
// (directory + normalized album) key — the shared grouping preamble for
// the directory-scoped reconciliation passes (reconcileAlbumArtists /
// reconcileYears). Tracks with no album title (albumArtistGroupKey
// returns ok=false) are dropped: a loose single is not "an album" and is
// never reconciled. Kept in one place so the two passes group IDENTICALLY
// — the dominant-vote determinism depends on the buckets matching
// byte-for-byte.
func groupByAlbumKey(tracks []ReconcileTarget) map[string][]int {
	groups := map[string][]int{}
	for i := range tracks {
		key, ok := albumArtistGroupKey(tracks[i].Path, tracks[i].Album)
		if !ok {
			continue
		}
		groups[key] = append(groups[key], i)
	}
	return groups
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
