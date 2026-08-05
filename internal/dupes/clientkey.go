// The iOS client-key mirror. Every function is named after and annotated
// with its Swift twin in the 1-bit app repo:
//
//	com.acoseac.dsdplayer/MetadataNormalizer.swift   (the normaliser)
//	com.acoseac.dsdplayer/BridgeSyncActor.swift      (the fallback order)
//	com.acoseac.dsdplayer/CrossSourceTrackDedup.swift (the ContentKey)
//
// Expected values in clientkey_test.go are lifted VERBATIM from
// MetadataNormalizerTests.swift so an iOS-side rule change trips a
// failing test here instead of silently de-synchronising the partition.
//
// Divergences honoured on purpose (do not "fix"):
//
//   - Whitespace is unicode.IsSpace, hand-rolled — Swift regex \s is
//     Unicode-aware, Go regexp's is ASCII-only, so no \s appears in any
//     pattern here.
//   - trackNumberFromFilename accepts a BARE SPACE separator after the
//     digits ("07 Song.flac" → 7) because Swift's trackPrefixRegex does.
//     The bridge's own parseLeadingTrackNumber (internal/manifest/
//     extractors.go) deliberately requires punctuation and returns 0
//     there — it is the WRONG helper for this mirror, which is why it is
//     not reused. Pinned by TestTrackNumberFallbackUsesTheSwiftRule.
//   - strings.ToLower applies Unicode SIMPLE case mappings where Swift's
//     lowercased() applies full ones. The known deltas (Turkish İ, Greek
//     final sigma) either keep the partition identical or make the bridge
//     group two spellings of the same word that the client separates —
//     accepted, and vanishingly rare in real tags.
package dupes

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// trimSpace mirrors Swift `trimmingCharacters(in: .whitespacesAndNewlines)`.
func trimSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}

// collapseSpace mirrors Swift `replacing(/\s+/, with: " ")` LITERALLY:
// every run of Unicode whitespace becomes one ASCII space, including a
// leading or trailing run (callers trim afterwards, exactly like the
// Swift call sites).
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inRun := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inRun {
				b.WriteByte(' ')
			}
			inRun = true
			continue
		}
		inRun = false
		b.WriteRune(r)
	}
	return b.String()
}

// normalize mirrors MetadataNormalizer.normalize(_:lower:true):
// trim → collapse whitespace runs → lowercase.
func normalize(s string) string {
	return strings.ToLower(collapseSpace(trimSpace(s)))
}

// Bracket-cruft patterns from MetadataNormalizer.cleanDisplayName. The
// classes are ASCII on both sides except Swift's \d (Unicode digits) vs
// [0-9] here — see the package-doc divergence note.
var (
	explicitBracketRE = regexp.MustCompile(`(?i)\[(?:e|explicit|clean)\]`)
	numericBracketRE  = regexp.MustCompile(`\[[0-9]+\]`)
)

// cleanDisplayName mirrors MetadataNormalizer.cleanDisplayName: strip
// leading [E]/[Explicit]/[Clean] markers and all-digit bracket groups
// ("[90777496]", "[2014]"), preserve non-numeric brackets ("[Deluxe]"),
// and return the original trimmed string if cleaning would empty it.
// NEVER strips `*` — that is artist-scoped (a title can be "F***").
func cleanDisplayName(s string) string {
	trimmed := trimSpace(s)
	if trimmed == "" {
		return s
	}
	result := explicitBracketRE.ReplaceAllString(trimmed, "")
	result = numericBracketRE.ReplaceAllString(result, "")
	result = trimSpace(collapseSpace(result))
	if result == "" {
		return trimmed
	}
	return result
}

// stripArtistDisambiguation mirrors MetadataNormalizer's Discogs-`*`
// strip: remove each `*` that sits immediately before whitespace, a name
// separator (& , ; /), or end-of-string (the Swift lookahead
// /\*(?=\s|[&,;\/]|$)/ — Go RE2 has no lookahead, hence the scan).
// A mid-token `*` ("A*B") is preserved. Returns the input unchanged if
// stripping would empty it. Fast-paths the no-`*` common case.
func stripArtistDisambiguation(s string) string {
	if !strings.Contains(s, "*") {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range rs {
		if r == '*' {
			if i+1 >= len(rs) {
				continue // boundary: end-of-string
			}
			next := rs[i+1]
			if unicode.IsSpace(next) || next == '&' || next == ',' || next == ';' || next == '/' {
				continue // boundary: separator
			}
		}
		b.WriteRune(r)
	}
	stripped := trimSpace(collapseSpace(b.String()))
	if stripped == "" {
		return s
	}
	return stripped
}

// cleanArtistName mirrors MetadataNormalizer.cleanArtistName — the
// artist-scoped cleaning chokepoint: cleanDisplayName + the `*` strip.
func cleanArtistName(s string) string {
	return stripArtistDisambiguation(cleanDisplayName(s))
}

// splitArtistDisplayName mirrors MetadataNormalizer.splitArtistDisplayName:
// split on the project's multi-value separator `"; "` ONLY (a bare `;` is
// a legitimate name character), trim each segment, drop empties, dedup
// case-insensitively on normalize() keeping the first-seen casing.
// Returns nil for empty / whitespace-only input.
func splitArtistDisplayName(s string) []string {
	if trimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, "; ")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := trimSpace(p)
		if t == "" {
			continue
		}
		key := normalize(t)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}

// compilationMarkers mirrors MetadataNormalizer.compilationMarkers —
// exact-match set, no fuzzy matching (a band literally named "V.A."
// mis-collapsing outweighs the benefit).
var compilationMarkers = map[string]struct{}{
	"various artists": {}, "various": {}, "va": {}, "v.a.": {}, "v/a": {},
	"soundtrack": {}, "ost": {}, "original soundtrack": {},
}

// isCompilationMarker mirrors MetadataNormalizer.isCompilationMarker:
// trimmed + lowercased (NOT normalize — no whitespace collapse, exactly
// like the Swift twin).
func isCompilationMarker(s string) bool {
	_, ok := compilationMarkers[strings.ToLower(trimSpace(s))]
	return ok
}

// primaryAlbumArtistForGrouping mirrors
// MetadataNormalizer.primaryAlbumArtistForGrouping: the first "; "
// segment owns album identity; an empty input and a compilation-marker
// primary are preserved WHOLE.
func primaryAlbumArtistForGrouping(albumArtist string) string {
	segs := splitArtistDisplayName(albumArtist)
	if len(segs) == 0 {
		return albumArtist
	}
	if isCompilationMarker(segs[0]) {
		return albumArtist
	}
	return segs[0]
}

// albumID mirrors MetadataNormalizer.albumID: the client's album identity
// "<aa>|<album>|<year>". The album component runs through
// cleanDisplayName ONLY when it carries a "[" (the Swift fast-gate), and
// year ≤ 0 is absent (Some(0) ≡ nil — the bridge itself sends year: 0 for
// tag-absent, and the client collapses both to the empty suffix).
func albumID(albumArtist, album string, year int) string {
	aa := normalize(primaryAlbumArtistForGrouping(albumArtist))
	al := album
	if strings.Contains(album, "[") {
		al = cleanDisplayName(album)
	}
	y := ""
	if year > 0 {
		y = strconv.Itoa(year)
	}
	return aa + "|" + normalize(al) + "|" + y
}

// discNumberForFolderName mirrors MetadataNormalizer.discNumber(
// forFolderName:) — the whole-match, case-insensitive
// `\s*(?:cd|disc|disk|vol|volume)\s*0*([0-9]{1,2})\s*` rule, hand-rolled
// (Swift \s is Unicode). Greedy-`0*`-then-capture semantics are
// reproduced exactly: "003" → 3, "00" → 0, "1234" → no match.
//
// NOTE this is the CLIENT's disc-folder rule and deliberately includes
// vol/volume — it is NOT the bridge's own discFolderRe (extractors.go),
// which excludes them for the folder-art climb. Different contracts.
func discNumberForFolderName(name string) (int, bool) {
	rs := []rune(name)
	n := len(rs)
	i := 0
	for i < n && unicode.IsSpace(rs[i]) {
		i++
	}
	matched := false
	for _, kw := range []string{"volume", "disc", "disk", "vol", "cd"} {
		j := i
		ok := true
		for _, kr := range kw {
			if j >= n || unicode.ToLower(rs[j]) != kr {
				ok = false
				break
			}
			j++
		}
		if ok {
			i = j
			matched = true
			break
		}
	}
	if !matched {
		return 0, false
	}
	for i < n && unicode.IsSpace(rs[i]) {
		i++
	}
	start := i
	for i < n && rs[i] >= '0' && rs[i] <= '9' {
		i++
	}
	digits := rs[start:i]
	if len(digits) == 0 {
		return 0, false
	}
	for i < n && unicode.IsSpace(rs[i]) {
		i++
	}
	if i != n {
		return 0, false // wholeMatch: nothing may follow
	}
	// Greedy 0* consumes leading zeros but must leave 1–2 digits for the
	// capture; more than 2 non-zero-prefixed digits cannot match at all.
	z := 0
	for z < len(digits) && digits[z] == '0' {
		z++
	}
	k := z
	if k > len(digits)-1 {
		k = len(digits) - 1
	}
	if len(digits)-k > 2 {
		return 0, false
	}
	v, err := strconv.Atoi(string(digits[k:]))
	if err != nil {
		return 0, false
	}
	return v, true
}

// effectiveAlbumPath mirrors MetadataNormalizer.effectiveAlbumPath:
// the album root is the parent folder, or the grandparent when the parent
// is a disc subfolder; backslashes are normalised so Windows-authored
// paths don't flatten to one component.
func effectiveAlbumPath(trackPath string) (albumPath string, disc int) {
	normalized := strings.ReplaceAll(trackPath, "\\", "/")
	components := splitPathComponents(normalized)
	if len(components) < 2 {
		return normalized, 1
	}
	parent := components[len(components)-2]
	if d, ok := discNumberForFolderName(parent); ok && len(components) >= 3 {
		return "/" + strings.Join(components[:len(components)-2], "/"), d
	}
	return "/" + strings.Join(components[:len(components)-1], "/"), 1
}

// splitPathComponents mirrors Swift `split(separator: "/",
// omittingEmptySubsequences: true)`.
func splitPathComponents(p string) []string {
	raw := strings.Split(p, "/")
	out := raw[:0]
	for _, seg := range raw {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// trackNumberFromFilename mirrors MetadataNormalizer.trackNumber(
// fromFilename:) — Swift's trackPrefixRegex /\s*(\d{1,3})[\s._\-–—]+/
// as a prefix match, hand-rolled. The bare-space separator is
// deliberately accepted ("07 Song.flac" → 7); see the package-doc
// divergence note about parseLeadingTrackNumber.
func trackNumberFromFilename(filename string) int {
	rs := []rune(filename)
	n := len(rs)
	i := 0
	for i < n && unicode.IsSpace(rs[i]) {
		i++
	}
	start := i
	for i < n && rs[i] >= '0' && rs[i] <= '9' {
		i++
	}
	d := i - start
	if d < 1 || d > 3 {
		return 0
	}
	if i >= n {
		return 0 // at least one separator must follow
	}
	if r := rs[i]; !unicode.IsSpace(r) && r != '.' && r != '_' && r != '-' && r != '–' && r != '—' {
		return 0
	}
	v, err := strconv.Atoi(string(rs[start:i]))
	if err != nil {
		return 0
	}
	return v
}

// deletePathExtension mirrors NSString.deletingPathExtension for plain
// filenames: "track.flac" → "track", ".hidden" → ".hidden".
func deletePathExtension(name string) string {
	ext := path.Ext(name)
	if ext == "" || ext == name {
		return name
	}
	return strings.TrimSuffix(name, ext)
}

// pathDefaults mirrors MetadataNormalizer.PathDefaults — the
// path-inferred fallbacks assuming .../Artist/Album/Track.ext with disc
// subfolders absorbed.
type pathDefaults struct {
	title       string
	artist      string
	album       string
	trackNumber int
	discNumber  int
}

// pathDefaultsFor mirrors MetadataNormalizer.pathDefaults(fullPath:
// filename:).
func pathDefaultsFor(fullPath, filename string) pathDefaults {
	albumPath, disc := effectiveAlbumPath(fullPath)
	albumComponents := splitPathComponents(albumPath)
	title := deletePathExtension(filename)
	album := "Unknown Album"
	if len(albumComponents) >= 1 {
		album = albumComponents[len(albumComponents)-1]
	}
	artist := "Unknown Artist"
	if len(albumComponents) >= 2 {
		artist = albumComponents[len(albumComponents)-2]
	}
	return pathDefaults{
		title:       cleanDisplayName(title),
		artist:      cleanArtistName(artist),
		album:       cleanDisplayName(album),
		trackNumber: trackNumberFromFilename(filename),
		discNumber:  disc,
	}
}

// nonEmptyTrimmed mirrors BridgeSyncActor.nonEmpty (BridgeSyncActor.swift):
// the tag value TRIMMED, or "" when it trims to nothing — the client
// stores the trimmed form, so the key must derive from it too.
func nonEmptyTrimmed(s string) string {
	return trimSpace(s)
}

// KeyFor computes the client's ContentKey for one manifest row —
// CrossSourceTrackDedup's ContentKey{albumID, disc, track, normTitle}
// with the field values resolved exactly as BridgeSyncActor.
// upsertBridgeTrack resolves them (BridgeSyncActor.swift:693-724):
//
//   - sharePath is "/" + path; path-inferred defaults come from
//     MetadataNormalizer.pathDefaults over it;
//   - title:  tag (trimmed) or the cleaned filename;
//   - artist: cleanArtistName(tag or path default);
//   - albumArtist: cleanArtistName(tag), FALLING BACK TO THE CLEANED
//     ARTIST when the tag is empty (6.7% of the measured library);
//   - album:  tag (trimmed, NOT display-cleaned — cleaning happens
//     inside albumID's bracket gate) or the path default;
//   - track:  tag if present (an explicit 0 is a value), else the
//     Swift filename rule; disc: tag if present, else the disc-folder
//     rule (which yields 1, never 0, when no disc folder matches);
//   - year:   ≤ 0 is absent.
func KeyFor(r Row) Key {
	sharePath := "/" + r.Path
	filename := r.Path
	if idx := strings.LastIndex(filename, "/"); idx >= 0 {
		filename = filename[idx+1:]
	}
	d := pathDefaultsFor(sharePath, filename)

	title := nonEmptyTrimmed(r.Title)
	if title == "" {
		title = d.title
	}
	artist := nonEmptyTrimmed(r.Artist)
	if artist == "" {
		artist = d.artist
	}
	artist = cleanArtistName(artist)
	albumArtist := nonEmptyTrimmed(r.AlbumArtist)
	if albumArtist == "" {
		albumArtist = artist
	} else {
		albumArtist = cleanArtistName(albumArtist)
	}
	album := nonEmptyTrimmed(r.Album)
	if album == "" {
		album = d.album
	}
	track := d.trackNumber
	if r.TrackTagged {
		track = r.Track
	}
	disc := d.discNumber
	if r.DiscTagged {
		disc = r.Disc
	}
	return Key{
		AlbumID:   albumID(albumArtist, album, r.Year),
		Disc:      disc,
		Track:     track,
		NormTitle: normalize(title),
	}
}
