package librarycat

// The iOS GenreNormalizer mirror.
//
// Fourth entry in this repo's do-not-unify family, after
// internal/dupes (MetadataNormalizer), internal/manifest's
// unicode_lower SQL scalar, and internal/enrich's matchfold. Every
// function here is named after and annotated with its Swift twin in
// com.acoseac.dsdplayer/GenreNormalizer.swift, and the expected values
// in genre_test.go are lifted from that file's own docstrings so an
// iOS-side rule change trips a red test here instead of silently
// splitting one library into two different genre lists.
//
// Do NOT "improve" any of it. A fold that is better than the client's
// is WRONG here — the contract is sameness, not quality.
//
// Divergences honoured on purpose:
//
//   - Swift's Character.isNumber is Unicode-aware; the ASCII-digit
//     guard is written explicitly at each site so a Devanagari digit
//     can't be read as an ID3 reference on one side only.
//   - Swift's split(whereSeparator:) drops empty subsequences, which
//     strings.FieldsFunc also does — they agree, and the per-segment
//     trim makes "; " / ";" / " ; " uniform on both sides.

import (
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// id3GenreTable is the ID3v1 base list (0–79) plus the Winamp
// extensions (80–191), EXTRACTED VERBATIM from dhowden/tag's
// id3v2Genres — not from a published list. That distinction is the
// point: dhowden is the table the bridge's own MP3 extraction expands
// numeric references through, so a reference expanded here groups
// identically to the same file's manifest value. Where dhowden and the
// various published lists diverge (the high Winamp indices, and the
// stray trailing space on index 141 "Christian Rock ") we mirror
// dhowden; the per-segment trim normalises the whitespace quirk away
// on both paths.
//
// TestID3GenreTableMatchesDhowden re-extracts it from the module source
// and compares, so a dependency bump that reshapes the table fails here
// rather than drifting.
var id3GenreTable = [...]string{
	"Blues", "Classic Rock", "Country", "Dance",
	"Disco", "Funk", "Grunge", "Hip-Hop",
	"Jazz", "Metal", "New Age", "Oldies",
	"Other", "Pop", "R&B", "Rap",
	"Reggae", "Rock", "Techno", "Industrial",
	"Alternative", "Ska", "Death Metal", "Pranks",
	"Soundtrack", "Euro-Techno", "Ambient", "Trip-Hop",
	"Vocal", "Jazz+Funk", "Fusion", "Trance",
	"Classical", "Instrumental", "Acid", "House",
	"Game", "Sound Clip", "Gospel", "Noise",
	"AlternRock", "Bass", "Soul", "Punk",
	"Space", "Meditative", "Instrumental Pop", "Instrumental Rock",
	"Ethnic", "Gothic", "Darkwave", "Techno-Industrial",
	"Electronic", "Pop-Folk", "Eurodance", "Dream",
	"Southern Rock", "Comedy", "Cult", "Gangsta",
	"Top 40", "Christian Rap", "Pop/Funk", "Jungle",
	"Native American", "Cabaret", "New Wave", "Psychedelic",
	"Rave", "Showtunes", "Trailer", "Lo-Fi",
	"Tribal", "Acid Punk", "Acid Jazz", "Polka",
	"Retro", "Musical", "Rock & Roll", "Hard Rock",
	"Folk", "Folk-Rock", "National Folk", "Swing",
	"Fast Fusion", "Bebob", "Latin", "Revival",
	"Celtic", "Bluegrass", "Avantgarde", "Gothic Rock",
	"Progressive Rock", "Psychedelic Rock", "Symphonic Rock", "Slow Rock",
	"Big Band", "Chorus", "Easy Listening", "Acoustic",
	"Humour", "Speech", "Chanson", "Opera",
	"Chamber Music", "Sonata", "Symphony", "Booty Bass",
	"Primus", "Porn Groove", "Satire", "Slow Jam",
	"Club", "Tango", "Samba", "Folklore",
	"Ballad", "Power Ballad", "Rhythmic Soul", "Freestyle",
	"Duet", "Punk Rock", "Drum Solo", "A capella",
	"Euro-House", "Dance Hall", "Goa", "Drum & Bass",
	"Club-House", "Hardcore", "Terror", "Indie",
	"Britpop", "Negerpunk", "Polsk Punk", "Beat",
	"Christian Gangsta Rap", "Heavy Metal", "Black Metal", "Crossover",
	"Contemporary Christian", "Christian Rock ", "Merengue", "Salsa",
	"Thrash Metal", "Anime", "JPop", "Synthpop",
	"Christmas", "Art Rock", "Baroque", "Bhangra",
	"Big Beat", "Breakbeat", "Chillout", "Downtempo",
	"Dub", "EBM", "Eclectic", "Electro",
	"Electroclash", "Emo", "Experimental", "Garage",
	"Global", "IDM", "Illbient", "Industro-Goth",
	"Jam Band", "Krautrock", "Leftfield", "Lounge",
	"Math Rock", "New Romantic", "Nu-Breakz", "Post-Punk",
	"Post-Rock", "Psytrance", "Shoegaze", "Space Rock",
	"Trop Rock", "World Music", "Neoclassical", "Audiobook",
	"Audio Theatre", "Neue Deutsche Welle", "Podcast", "Indie Rock",
	"G-Funk", "Dubstep", "Garage Rock", "Psybient"}

// expandNumericRefs mirrors GenreNormalizer.expandNumericRefs.
//
// Semantics (settled in the iOS axis plan — deliberately NOT dhowden's
// concatenating fixpoint loop, whose "(17)Test" → "Rock Test" join
// produces grouping-hostile compound names):
//
//   - "(17)"          → "Rock"           (leading parenthesised refs expand)
//   - "(17)Hard Rock" → "Hard Rock"      (a non-empty text remainder is the
//     ID3v2.3 refinement and WINS over the refs)
//   - "(17)(79)"      → "Rock; Hard Rock" (multiple refs, no refinement →
//     semicolon-joined so genreSegments yields both)
//   - "((17)"         → "(17)"           (the "((" escape marks the
//     refinement start with a literal "(", ID3v2.3 §A.3)
//   - out-of-range parenthesised refs (>= table length) are DROPPED
//   - a BARE numeric expands ONLY when the ENTIRE trimmed string is
//     ASCII digits within table bounds (the ID3v2.4 numeric-string
//     rule) — "1980s", "80s Pop" and "192" stay untouched
func expandNumericRefs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}
	if isAllASCIIDigits(trimmed) {
		if n, err := strconv.Atoi(trimmed); err == nil && n >= 0 && n < len(id3GenreTable) {
			return id3GenreTable[n]
		}
		return trimmed // out-of-range numeric stays a literal name
	}
	if !strings.HasPrefix(trimmed, "(") {
		return trimmed
	}

	var refs []string
	rest := trimmed
	for {
		// Tolerate spaces between refs ("(17) (79)") — the canonical
		// ID3v2.3 form has none, but real taggers insert them.
		atRef := strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(atRef, "(") {
			rest = atRef
			break
		}
		if strings.HasPrefix(atRef, "((") {
			// Escape: the refinement begins with a literal "(".
			rest = atRef[1:]
			break
		}
		afterParen := atRef[1:]
		digits := leadingASCIIDigits(afterParen)
		if digits == "" || !strings.HasPrefix(afterParen[len(digits):], ")") {
			// "(" not opening a numeric ref — the remainder is text.
			rest = atRef
			break
		}
		// Atoi fails on an absurdly long digit run; treated as
		// out-of-range and dropped, same as any ref past the table.
		if n, err := strconv.Atoi(digits); err == nil && n >= 0 && n < len(id3GenreTable) {
			refs = append(refs, id3GenreTable[n])
		}
		rest = afterParen[len(digits)+1:]
	}

	if refinement := strings.TrimSpace(rest); refinement != "" {
		return refinement
	}
	return strings.Join(refs, "; ")
}

func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func leadingASCIIDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// Segment is one (key, display) pair produced by a multi-value fold.
type Segment struct {
	Key     string
	Display string
}

// genreSegments mirrors GenreNormalizer.groupSegments.
//
// expandNumericRefs → split on ";" AND NUL (ID3v2.4 null-delimited
// multi-values; the per-segment trim covers "; " / ";" / " ; "
// uniformly) → drop empties → key on dupes.Normalize. A track tagged
// "Rock; Pop" appears under BOTH groups. Deduped by key within one
// call so "Rock; rock" counts the track once.
//
// Deliberately NO comma split ("Folk, World, & Country" is ONE genre)
// and NO slash split ("Pop/Rock" is one label — only composers get the
// "/" split, per ID3v2.3 TCOM).
func genreSegments(raw string) []Segment {
	return splitSegments(expandNumericRefs(raw),
		func(r rune) bool { return r == ';' || r == '\x00' },
		dupes.Normalize)
}

// composerSegments mirrors GenreNormalizer.composerSegments.
//
// Split on ";", NUL, AND "/" — ID3v2.3 TCOM is a "/"-separated
// composer list ("Lennon/McCartney"). cleanDisplayName is NOT
// re-applied: every composer write site already cleans, and a
// composite could carry structural brackets a re-clean would corrupt.
func composerSegments(raw string) []Segment {
	return splitSegments(raw,
		func(r rune) bool { return r == ';' || r == '\x00' || r == '/' },
		composerGroupKey)
}

func splitSegments(s string, isSep func(rune) bool, keyOf func(string) string) []Segment {
	var out []Segment
	seen := map[string]struct{}{}
	for _, seg := range strings.FieldsFunc(s, isSep) {
		display := strings.TrimSpace(seg)
		if display == "" {
			continue
		}
		key := keyOf(display)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Segment{Key: key, Display: display})
	}
	return out
}

// composerGroupKey mirrors GenreNormalizer.composerGroupKey: the
// conservative "Last, First" inversion so "Beethoven, Ludwig van" and
// "Ludwig van Beethoven" land in ONE group. Shapes the KEY only —
// display strings are never inverted.
func composerGroupKey(segment string) string {
	if inverted, ok := commaInvertedForm(segment); ok {
		return dupes.Normalize(inverted)
	}
	return dupes.Normalize(segment)
}

// commaInvertedForm mirrors GenreNormalizer.commaInvertedForm:
// "Last, First" → "First Last" when the conservative guards pass.
//
// The guards are what keep ensembles intact — exactly one comma, both
// sides non-empty after trim, and no "&" and no " and " anywhere in
// the segment — so "Crosby, Stills & Nash" is never mangled. Spanish
// " y " and French " et " ensembles are a documented accepted gap on
// both sides; don't close it here alone.
func commaInvertedForm(segment string) (string, bool) {
	parts := strings.Split(segment, ",")
	if len(parts) != 2 {
		return "", false
	}
	last := strings.TrimSpace(parts[0])
	first := strings.TrimSpace(parts[1])
	if last == "" || first == "" {
		return "", false
	}
	if strings.Contains(segment, "&") {
		return "", false
	}
	if containsFold(segment, " and ") {
		return "", false
	}
	return first + " " + last, true
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// generationalSuffixes are skipped by the fallback surname pick so
// "Johann Strauss II" buckets under S, not I.
var generationalSuffixes = map[string]struct{}{
	"ii": {}, "iii": {}, "jr": {}, "jr.": {}, "sr": {}, "sr.": {},
}

// composerSortName mirrors GenreNormalizer.composerSortName — the
// surname-first SORT/BUCKET string so classical listeners find
// Beethoven under B regardless of tag form. NEVER used for display.
//
//   - If ANY raw segment spelling in the group has the exactly-one-comma
//     inversion shape, that variant wins VERBATIM: a tagger's
//     "Beethoven, Ludwig van" is an authoritative surname statement.
//     rawVariants must be sorted so the pick is deterministic.
//   - Else "<lastWord> <precedingWords>", where the last word skips
//     generational suffixes.
//
// Documented accepted miss, pinned as behaviour rather than fixed:
// multi-word surnames with no comma-form variant — "Ralph Vaughan
// Williams" buckets under W, not V.
func composerSortName(displayName string, rawVariants []string) string {
	for _, variant := range rawVariants {
		if _, ok := commaInvertedForm(variant); ok {
			return variant
		}
	}
	tokens := strings.Fields(displayName)
	if len(tokens) <= 1 {
		return displayName
	}
	idx := len(tokens) - 1
	for idx > 0 {
		if _, isSuffix := generationalSuffixes[strings.ToLower(tokens[idx])]; !isSuffix {
			break
		}
		idx--
	}
	surname := tokens[idx]
	rest := make([]string, 0, len(tokens))
	rest = append(rest, surname)
	rest = append(rest, tokens[:idx]...)
	rest = append(rest, tokens[idx+1:]...)
	return strings.Join(rest, " ")
}
