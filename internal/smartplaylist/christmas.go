package smartplaylist

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The Christmas family's match: whether a track belongs on the seasonal mix,
// decided from its genre, its album and its title, never from its artist's
// name. Noel Gallagher, the band Christmas, Natalie and the rapper Jul are
// artists, and a name says nothing about the music; the artist is read only
// to refuse `noel` in an album that is the artist's own name.
//
// It is comparison only. christmasFold's output is never stored, hashed,
// used as a key or put in SQL, and it is not unified with the other folds in
// this tree (internal/enrich/matchfold.go lists them and why each is its
// own): this one folds harder than any of them, since its only question is
// whether a word is one of a few dozen.

// ChristmasRule is the rule a track qualifies for the Christmas family by:
// the first of them that holds, in this order, or ChristmasNone.
type ChristmasRule int

const (
	ChristmasNone ChristmasRule = iota
	// ChristmasByGenre: a segment of the genre is a Christmas genre.
	ChristmasByGenre
	// ChristmasByAlbum: the album holds a Christmas word.
	ChristmasByAlbum
	// ChristmasByTitle: the title, less a version suffix, is a carol in
	// christmasTitles.
	ChristmasByTitle
	// ChristmasByTitleWord: the title holds one of the few words that are
	// Christmas wherever they stand.
	ChristmasByTitleWord
)

func (r ChristmasRule) String() string {
	switch r {
	case ChristmasByGenre:
		return "genre"
	case ChristmasByAlbum:
		return "album"
	case ChristmasByTitle:
		return "title"
	case ChristmasByTitleWord:
		return "title word"
	default:
		return "none"
	}
}

// MatchChristmas reports the rule a track's tags qualify it by for the
// Christmas family, or ChristmasNone. Pure: the same tags give the same
// answer, and nothing but the tags is read.
//
//  1. Genre: one segment of the genre, split where the scanner joins a
//     multi-valued genre ("; ") and on ; / , \ and :, equals a Christmas
//     genre (christmasGenres), or is two words pairing `holiday` with one.
//     Bare `holiday` never counts: it is Hanukkah, Thanksgiving and a travel
//     playlist as often.
//  2. Album: the album holds a Christmas word as a whole word
//     (christmasAlbumWords, or one beginning `christmas` or `weihnacht`),
//     where `noel` does not count when the artist holds `noel` as a word or
//     the word before it is a credit (feat, ft, featuring, with).
//  3. Title: the title, less trailing version suffixes (a parenthetical, a
//     bracket, ` - Live` and the like), equals a carol in christmasTitles.
//     Equals, never begins with: Silent Nightmare is not Silent Night.
//  4. Title word: the title holds `xmas`, `navidad`, `navidades`, or a word
//     beginning `christmas` or `weihnacht`.
//
// `jul` and `natal` count only as a whole genre segment (a rapper and a
// place), `natale`, `craciun` and `noel` never as a word of a title (they are
// surnames), and `holiday`, `winter`, `snow`, `carol`, `merry`, `angel`,
// `star`, `bells` and `santa` never on their own.
func MatchChristmas(title, artist, album, genre string) ChristmasRule {
	if christmasGenre(genre) {
		return ChristmasByGenre
	}
	if christmasAlbum(album, artist) {
		return ChristmasByAlbum
	}
	folded := christmasFold(title)
	carol := folded
	if stripped := stripTitleSuffixes(title); stripped != strings.TrimSpace(title) {
		carol = christmasFold(stripped)
	}
	if christmasTitles[carol] {
		return ChristmasByTitle
	}
	if christmasTitleWord(folded) {
		return ChristmasByTitleWord
	}
	return ChristmasNone
}

// christmasGenres are the genre segments that qualify a track, folded. Two
// words are written with their space (`x mas`, `christmas music`).
var christmasGenres = map[string]bool{
	"christmas": true, "xmas": true, "x mas": true, "christmas music": true,
	"noel":        true,                           // French
	"weihnachten": true, "weihnachtslieder": true, // German
	"navidad": true, "villancicos": true, // Spanish
	"natale":  true,                  // Italian
	"natal":   true,                  // Portuguese; a place and a surname too, so a genre alone
	"craciun": true, "colinde": true, // Romanian
	"jul": true, "julmusik": true, // Scandinavian; Jul is also a rapper, so a genre alone
	"joulu": true, "joululaulut": true, // Finnish
	"kerst": true, "kerstmuziek": true, // Dutch
	"koledy": true, // Polish
}

// christmasAlbumWords are the words an album qualifies a track by, folded,
// besides those beginning `christmas` or `weihnacht` and `noel` (which
// christmasAlbum judges with the artist). `jul` and `natal` are not here:
// they are words of other things in an album's name.
var christmasAlbumWords = map[string]bool{
	"xmas": true, "navidad": true, "navidades": true, "natale": true,
	"craciun": true, "colinde": true, "julvisor": true, "julsanger": true,
	"kerstmis": true, "koledy": true, "joululaulut": true,
}

// creditWords put a name after them: `noel` after one is a person.
var creditWords = map[string]bool{"feat": true, "ft": true, "featuring": true, "with": true}

// christmasTitles are the carols a title qualifies a track by alone, as
// they are written; christmasTitleSet holds them folded. Each is a song no
// other song shares a title with. "Joy to the World" is not here: Three Dog
// Night's hit has that title, and nothing in the tags tells the two apart.
// The list is the design's (B750), and the second column of a row its
// common spellings, which the fold alone does not join: "Oh" for "O", "You"
// for "Ye", "Nowell", the carol's whole first line, a dropped g.
var christmasTitles = foldTitleTable([][]string{
	{"Silent Night", "Silent Night, Holy Night"},
	{"Stille Nacht", "Stille Nacht, heilige Nacht"},
	{"O Holy Night", "Oh Holy Night"},
	{"Cantique de Noël"},
	{"Minuit, Chrétiens"},
	{"Adeste Fideles"},
	{"O Come All Ye Faithful", "Oh Come All Ye Faithful"},
	{"Hark the Herald Angels Sing"},
	{"Away in a Manger"},
	{"O Little Town of Bethlehem", "Oh Little Town of Bethlehem"},
	{"God Rest Ye Merry Gentlemen", "God Rest You Merry Gentlemen"},
	{"The First Noel", "The First Nowell"},
	{"We Three Kings", "We Three Kings of Orient Are"},
	{"Good King Wenceslas"},
	{"Deck the Halls", "Deck the Hall"},
	{"Jingle Bells"},
	{"Santa Claus Is Coming to Town", "Santa Claus Is Comin' to Town"},
	{"Carol of the Bells"},
	{"O Tannenbaum"},
	{"Es ist ein Ros entsprungen"},
	{"Vom Himmel hoch", "Vom Himmel hoch, da komm ich her"},
	{"Il est né le divin enfant"},
	{"Les anges dans nos campagnes"},
	{"Tu scendi dalle stelle"},
	{"Astăzi s-a născut Hristos"},
	{"Bóg się rodzi"},
	{"Lulajże Jezuniu"},
	{"Cicha noc"},
	{"Stilla natt"},
	{"Mössens julafton"},
	{"All I Want for Christmas Is You"},
	{"Feliz Navidad"},
})

// foldTitleTable folds every spelling of every row into one set.
func foldTitleTable(rows [][]string) map[string]bool {
	out := make(map[string]bool)
	for _, row := range rows {
		for _, title := range row {
			out[christmasFold(title)] = true
		}
	}
	return out
}

// titleVersionWords make a trailing " - …" a version of the song rather than
// part of its title: Silent Night - Live and Silent Night - 2009 Remaster are
// Silent Night, Silent Night - Deadly Night is not.
var titleVersionWords = map[string]bool{
	"live": true, "vivo": true, "remaster": true, "remastered": true,
	"instrumental": true, "acoustic": true, "unplugged": true, "version": true,
	"edit": true, "mix": true, "remix": true, "mono": true, "stereo": true,
	"demo": true, "take": true, "single": true, "bonus": true, "reprise": true,
	"karaoke": true, "cappella": true, "acapella": true, "recorded": true,
	"recording": true, "session": true, "sessions": true, "from": true,
	"feat": true, "ft": true, "featuring": true, "with": true,
}

// maxTitleSuffixes bounds the suffixes stripTitleSuffixes takes off, so a
// title made of a thousand parentheticals costs a few scans, not a thousand.
// A real title carries two or three ("Silent Night (Live) [2009 Remaster]").
const maxTitleSuffixes = 8

func christmasGenre(genre string) bool {
	for _, seg := range strings.FieldsFunc(genre, isGenreSeparator) {
		g := christmasFold(seg)
		if christmasGenres[g] {
			return true
		}
		a, b, two := strings.Cut(g, " ")
		if !two || strings.Contains(b, " ") {
			continue
		}
		if (a == "holiday" && christmasGenreWord(b)) || (b == "holiday" && christmasGenreWord(a)) {
			return true
		}
	}
	return false
}

// isGenreSeparator splits a genre into its segments: the scanner joins a
// multi-valued genre with "; " (extractors.go), taggers write ; / , and \,
// and the iTunes Store's genres put a colon between a genre and its kind
// ("Christmas: Pop", "Christmas: Classical", "Holiday: Other").
func isGenreSeparator(r rune) bool {
	switch r {
	case ';', '/', ',', '\\', ':':
		return true
	default:
		return false
	}
}

// christmasGenreWord is a one-word Christmas genre, the partner `holiday`
// needs.
func christmasGenreWord(w string) bool {
	return christmasGenres[w] && !strings.Contains(w, " ")
}

func christmasAlbum(album, artist string) bool {
	words := strings.Fields(christmasFold(album))
	for i, w := range words {
		switch {
		case christmasAlbumWords[w], strings.HasPrefix(w, "christmas"), strings.HasPrefix(w, "weihnacht"):
			return true
		case w == "noel":
			if i > 0 && creditWords[words[i-1]] {
				continue // "Live with Noel Gallagher"
			}
			if hasWord(christmasFold(artist), "noel") {
				continue // the artist's own name, as an album title
			}
			return true
		}
	}
	return false
}

func christmasTitleWord(folded string) bool {
	for _, w := range strings.Fields(folded) {
		switch {
		case w == "xmas", w == "navidad", w == "navidades":
			return true
		case strings.HasPrefix(w, "christmas"), strings.HasPrefix(w, "weihnacht"):
			return true
		}
	}
	return false
}

// hasWord reports whether a folded string holds w as a whole word.
func hasWord(folded, w string) bool {
	for _, x := range strings.Fields(folded) {
		if x == w {
			return true
		}
	}
	return false
}

// stripTitleSuffixes takes the trailing version suffixes off a title as it
// is written, before folding removes the brackets and dashes that mark them:
// a parenthetical or a bracket, whatever it holds, and a " - " (or en or em
// dash) suffix holding a titleVersionWords word or only digits (a year).
// What is left is never empty: a title that is all suffix is kept whole.
func stripTitleSuffixes(title string) string {
	t := strings.TrimSpace(title)
	for range maxTitleSuffixes {
		next := stripTitleSuffix(t)
		if next == t || next == "" {
			break
		}
		t = next
	}
	return t
}

func stripTitleSuffix(t string) string {
	if i := trailingGroupStart(t); i > 0 {
		return strings.TrimSpace(t[:i])
	}
	for _, dash := range []string{" - ", " – ", " — "} {
		i := strings.LastIndex(t, dash)
		if i > 0 && versionSuffix(t[i+len(dash):]) {
			return strings.TrimSpace(t[:i])
		}
	}
	return t
}

// trailingGroupStart is the index of the bracket that opens the
// parenthetical or bracket a title ends with, or -1. Bytewise: no byte of a
// multi-byte UTF-8 sequence is a bracket.
func trailingGroupStart(t string) int {
	if t == "" {
		return -1
	}
	var open, closing byte
	switch t[len(t)-1] {
	case ')':
		open, closing = '(', ')'
	case ']':
		open, closing = '[', ']'
	default:
		return -1
	}
	depth := 0
	for i := len(t) - 1; i >= 0; i-- {
		switch t[i] {
		case closing:
			depth++
		case open:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// versionSuffix reports whether what follows a title's dash is a version of
// the song: a titleVersionWords word, or nothing but digits.
func versionSuffix(s string) bool {
	words := strings.Fields(christmasFold(s))
	if len(words) == 0 {
		return false
	}
	digits := true
	for _, w := range words {
		if titleVersionWords[w] {
			return true
		}
		if strings.TrimFunc(w, unicode.IsDigit) != "" {
			digits = false
		}
	}
	return digits
}

// christmasFold reduces a tag to the words the match compares: NFKD with
// its marks removed (Noël and a decomposed Noe\u0308l are noel, a
// compatibility form its letters), lower case, the letters NFKD leaves whole
// spelled out (ł l, ø o, ß ss, đ d, æ ae, œ oe: Kolędy is koledy, Mössens
// mossens), and every rune that is neither a letter nor a digit a space,
// runs of space one. A dash is a space too, except inside `x-mas`, which is
// the word `xmas`: X-Mas is written with its hyphen as often as without,
// while X Más, with a space, is ten and "more".
func christmasFold(s string) string {
	if s == "" {
		return ""
	}
	// Invalid UTF-8 is a space before NFKD sees it: norm leaves the letter
	// after an invalid byte composed ("\xf3Ǚ" kept its Ǚ), which then folded
	// to ǚ once and to u the next time (FuzzMatchChristmas's corpus). A row
	// read back from tags_json is valid already; this is for every caller.
	s = norm.NFKD.String(strings.ToValidUTF8(s, " "))
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		switch {
		case r == 'ł':
			b.WriteByte('l')
		case r == 'ø':
			b.WriteByte('o')
		case r == 'ß':
			b.WriteString("ss")
		case r == 'đ':
			b.WriteByte('d')
		case r == 'æ':
			b.WriteString("ae")
		case r == 'œ':
			b.WriteString("oe")
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.Is(unicode.Pd, r), r == '\u2212':
			b.WriteByte('-')
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		space = false
	}
	out := strings.TrimSpace(b.String())
	if !strings.Contains(out, "-") {
		return out
	}
	// A dash is still inside a word here, so the join can tell x-mas
	// (Rock-X-Mas included) from x - mas, whose dash stands alone.
	words := strings.Fields(out)
	kept := make([]string, 0, len(words)+2)
	for _, w := range words {
		parts := strings.FieldsFunc(w, func(r rune) bool { return r == '-' })
		for i := 0; i < len(parts); i++ {
			if parts[i] == "x" && i+1 < len(parts) && parts[i+1] == "mas" {
				kept = append(kept, "xmas")
				i++
				continue
			}
			kept = append(kept, parts[i])
		}
	}
	return strings.Join(kept, " ")
}
