package smartplaylist

import (
	"strings"
	"testing"
	"time"
)

// xmasCase is one track's tags and the rule MatchChristmas must answer.
type xmasCase struct {
	title, artist, album, genre string
	want                        ChristmasRule
	why                         string
}

func runXmasCases(t *testing.T, cases []xmasCase) {
	t.Helper()
	for _, c := range cases {
		if got := MatchChristmas(c.title, c.artist, c.album, c.genre); got != c.want {
			t.Errorf("MatchChristmas(title %q, artist %q, album %q, genre %q) = %s, want %s: %s",
				c.title, c.artist, c.album, c.genre, got, c.want, c.why)
		}
	}
}

// The fold: NFKD with its marks dropped, lower case, the letters NFKD
// leaves whole spelled out, punctuation a space, and x-mas the word xmas.
func TestChristmasFold(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"Noël":                          "noel",
		"Noe\u0308l":                    "noel", // decomposed: e and a combining diaeresis
		"KOLĘDY":                        "koledy",
		"Kolędy i pastorałki":           "koledy i pastoralki", // ę through NFKD, ł by the map
		"Bóg się rodzi":                 "bog sie rodzi",
		"Mössens julafton":              "mossens julafton",
		"Weiße Weihnacht":               "weisse weihnacht",
		"\u1E9E":                        "ss", // capital sharp s
		"Đurđevdan":                     "durdevdan",
		"Æbleskiver":                    "aebleskiver",
		"Œuvre":                         "oeuvre",
		"Ølstue":                        "olstue",
		"Crăciun":                       "craciun",
		"Astăzi s-a născut":             "astazi s a nascut",
		"X-Mas":                         "xmas",
		"Rock-X-Mas":                    "rock xmas",
		"x\u2013mas":                    "xmas", // an en dash
		"x - mas":                       "x mas",
		"X Mas":                         "x mas",
		"Weihnachts-Oratorium":          "weihnachts oratorium",
		"  Hark!  The   Herald…  ":      "hark the herald",
		"ＣＨＲＩＳＴＭＡＳ":                     "christmas",      // fullwidth, a compatibility form
		"Adeste \uFB01deles":            "adeste fideles", // the fi ligature
		"İstanbul":                      "istanbul",
		"Santa Claus Is Comin' to Town": "santa claus is comin to town",
	}
	for in, want := range cases {
		if got := christmasFold(in); got != want {
			t.Errorf("christmasFold(%q) = %q, want %q", in, got, want)
		}
	}
}

// Each rule on its own, and the order: a track qualifies by the first rule
// that holds.
var xmasByRule = []xmasCase{
	// 1. The genre, one segment equal to a Christmas genre.
	{"Song", "A", "Album", "Christmas", ChristmasByGenre, "the genre itself"},
	{"Song", "A", "Album", "CHRISTMAS", ChristmasByGenre, "any case"},
	{"Song", "A", "Album", "Xmas", ChristmasByGenre, "xmas"},
	{"Song", "A", "Album", "X-Mas", ChristmasByGenre, "x-mas is xmas"},
	{"Song", "A", "Album", "X Mas", ChristmasByGenre, "x mas, the two-word entry"},
	{"Song", "A", "Album", "Christmas Music", ChristmasByGenre, "christmas music"},
	{"Song", "A", "Album", "Noël", ChristmasByGenre, "French"},
	{"Song", "A", "Album", "Weihnachten", ChristmasByGenre, "German"},
	{"Song", "A", "Album", "Weihnachtslieder", ChristmasByGenre, "German songs"},
	{"Song", "A", "Album", "Navidad", ChristmasByGenre, "Spanish"},
	{"Song", "A", "Album", "Villancicos", ChristmasByGenre, "Spanish carols"},
	{"Song", "A", "Album", "Natale", ChristmasByGenre, "Italian"},
	{"Song", "A", "Album", "Natal", ChristmasByGenre, "Portuguese, a whole genre segment"},
	{"Song", "A", "Album", "Crăciun", ChristmasByGenre, "Romanian"},
	{"Song", "A", "Album", "Colinde", ChristmasByGenre, "Romanian carols"},
	{"Song", "A", "Album", "Jul", ChristmasByGenre, "Scandinavian, a whole genre segment"},
	{"Song", "A", "Album", "Julmusik", ChristmasByGenre, "Swedish"},
	{"Song", "A", "Album", "Joulu", ChristmasByGenre, "Finnish"},
	{"Song", "A", "Album", "Joululaulut", ChristmasByGenre, "Finnish songs"},
	{"Song", "A", "Album", "Kerst", ChristmasByGenre, "Dutch"},
	{"Song", "A", "Album", "Kerstmuziek", ChristmasByGenre, "Dutch music"},
	{"Song", "A", "Album", "Kolędy", ChristmasByGenre, "Polish"},
	{"Song", "A", "Album", "Christmas Holiday", ChristmasByGenre, "holiday paired with christmas"},
	{"Song", "A", "Album", "Holiday Xmas", ChristmasByGenre, "holiday paired with xmas, either order"},
	{"Song", "A", "Album", "Classical; Christmas", ChristmasByGenre, "the scanner's join of a multi-valued genre"},
	{"Song", "A", "Album", "Jazz/Christmas", ChristmasByGenre, "a slash"},
	{"Song", "A", "Album", "Pop, Noël", ChristmasByGenre, "a comma"},
	{"Song", "A", "Album", `Rock\Weihnachten`, ChristmasByGenre, "a backslash"},
	{"Song", "A", "Album", "Christmas: Pop", ChristmasByGenre, "the iTunes Store's genre and its kind"},
	{"Song", "A", "Album", "Holiday: Christmas", ChristmasByGenre, "the iTunes Store's parent genre"},
	{"Song", "A", "", "Christmas", ChristmasByGenre, "an empty album"},
	{"Silent Night", "A", "Christmas Album", "Christmas", ChristmasByGenre, "the genre is the first rule that holds"},

	// 2. The album, holding a Christmas word.
	{"Track 1", "Mariah Carey", "Merry Christmas", "Pop", ChristmasByAlbum, "christmas"},
	{"Christmas Time Is Here", "Vince Guaraldi Trio", "A Charlie Brown Christmas", "Jazz", ChristmasByAlbum, "the album is the first rule that holds"},
	{"Song", "A", "Xmas Hits", "Pop", ChristmasByAlbum, "xmas"},
	{"Song", "A", "Rock X-Mas", "Rock", ChristmasByAlbum, "x-mas"},
	{"Song", "A", "Christmastide", "Choral", ChristmasByAlbum, "a word beginning christmas"},
	{"Song", "A", "Weihnachtsoratorium, BWV 248", "Classical", ChristmasByAlbum, "a word beginning weihnacht"},
	{"Song", "A", "Weihnachten mit Freunden", "Volksmusik", ChristmasByAlbum, "weihnachten"},
	{"Song", "A", "Navidad en Familia", "Latin", ChristmasByAlbum, "navidad"},
	{"Song", "A", "Navidades Felices", "Latin", ChristmasByAlbum, "navidades, its plural"},
	{"Song", "A", "Natale con Noi", "Pop", ChristmasByAlbum, "natale"},
	{"Song", "A", "Crăciun fericit", "Folk", ChristmasByAlbum, "craciun"},
	{"Song", "A", "Colinde de Crăciun", "Folk", ChristmasByAlbum, "colinde"},
	{"Song", "A", "Julvisor", "Folk", ChristmasByAlbum, "julvisor"},
	{"Song", "A", "Svenska julsånger", "Folk", ChristmasByAlbum, "julsånger, its å through NFKD"},
	{"Song", "A", "Kerstmis met de familie", "Pop", ChristmasByAlbum, "kerstmis"},
	{"Song", "A", "Kolędy polskie", "Choral", ChristmasByAlbum, "kolędy, the demo's album"},
	{"Song", "A", "Kolędy i pastorałki", "Choral", ChristmasByAlbum, "kolędy beside a word the ł map folds"},
	{"Song", "A", "Joululaulut", "Choral", ChristmasByAlbum, "joululaulut"},
	{"Song", "Various Artists", "Noël Acadien", "Folk", ChristmasByAlbum, "noel, by an artist that is no Noel"},

	// 3. The title, a carol in the table, less a version suffix.
	{"Silent Night", "Bing Crosby", "Greatest Hits", "Pop", ChristmasByTitle, "the carol"},
	{"Silent Night (Live)", "A", "Album", "Pop", ChristmasByTitle, "a parenthetical"},
	{"Silent Night [2009 Remaster]", "A", "Album", "Pop", ChristmasByTitle, "a bracket"},
	{"Silent Night (Live) [Remastered]", "A", "Album", "Pop", ChristmasByTitle, "two of them"},
	{"Silent Night - Live", "A", "Album", "Pop", ChristmasByTitle, "a dash suffix"},
	{"Silent Night - Remastered 2009", "A", "Album", "Pop", ChristmasByTitle, "remastered"},
	{"Silent Night - Instrumental", "A", "Album", "Pop", ChristmasByTitle, "instrumental"},
	{"Silent Night \u2013 Live at the BBC", "A", "Album", "Pop", ChristmasByTitle, "an en dash"},
	{"Silent Night - 1957", "A", "Album", "Pop", ChristmasByTitle, "a year"},
	{"Silent Night - Live (Remastered)", "A", "Album", "Pop", ChristmasByTitle, "a dash suffix behind a parenthetical"},
	{"Silent Night", "", "", "", ChristmasByTitle, "no artist, album or genre"},
	{"All I Want for Christmas Is You", "Mariah Carey", "Hits", "Pop", ChristmasByTitle, "the table comes before the title's words"},
	{"Feliz Navidad", "José Feliciano", "Hits", "Latin", ChristmasByTitle, "the table again"},

	// 4. The title, holding one of the four words.
	{"White Christmas", "Bing Crosby", "Hits", "Pop", ChristmasByTitleWord, "christmas"},
	{"Christmastime Is Here", "A", "Album", "Jazz", ChristmasByTitleWord, "a word beginning christmas"},
	{"Merry Xmas Everybody", "Slade", "Hits", "Rock", ChristmasByTitleWord, "xmas"},
	{"Merry X-Mas", "A", "Album", "Pop", ChristmasByTitleWord, "x-mas"},
	{"Happy Xmas (War Is Over)", "John & Yoko", "Hits", "Rock", ChristmasByTitleWord, "xmas, the parenthetical kept for this rule"},
	{"Navidad, Navidad", "A", "Album", "Latin", ChristmasByTitleWord, "navidad"},
	{"Navidades", "A", "Album", "Latin", ChristmasByTitleWord, "navidades"},
	{"Weihnachtslied", "A", "Album", "Folk", ChristmasByTitleWord, "a word beginning weihnacht"},
	{"Hallelujah (Christmas Version)", "A", "Album", "Pop", ChristmasByTitleWord, "a Christmas version of a song"},
}

func TestMatchChristmasByEachRule(t *testing.T) { runXmasCases(t, xmasByRule) }

// Every spelling in the carol table qualifies by the title alone.
var xmasCarolSpellings = []string{
	"Silent Night", "Silent Night, Holy Night", "Stille Nacht", "Stille Nacht, heilige Nacht",
	"O Holy Night", "Oh Holy Night", "Cantique de Noël", "Minuit, Chrétiens", "Adeste Fideles",
	"O Come, All Ye Faithful", "Oh Come All Ye Faithful", "Hark! The Herald Angels Sing",
	"Away in a Manger", "O Little Town of Bethlehem", "Oh Little Town of Bethlehem",
	"God Rest Ye Merry, Gentlemen", "God Rest You Merry Gentlemen", "The First Noël",
	"The First Nowell", "We Three Kings", "We Three Kings of Orient Are", "Good King Wenceslas",
	"Deck the Halls", "Deck the Hall", "Jingle Bells", "Santa Claus Is Coming to Town",
	"Santa Claus Is Comin' to Town", "Carol of the Bells", "O Tannenbaum",
	"Es ist ein Ros' entsprungen", "Vom Himmel hoch", "Vom Himmel hoch, da komm ich her",
	"Il est né, le divin Enfant", "Les Anges dans nos campagnes", "Tu scendi dalle stelle",
	"Astăzi s-a născut Hristos", "Bóg się rodzi", "Lulajże, Jezuniu", "Cicha noc", "Stilla natt",
	"Mössens julafton", "All I Want for Christmas Is You", "Feliz Navidad",
}

func TestEveryCarolInTheTableQualifiesByItsTitle(t *testing.T) {
	for _, title := range xmasCarolSpellings {
		if got := MatchChristmas(title, "Some Artist", "Some Album", "Pop"); got != ChristmasByTitle {
			t.Errorf("%q answers %s, want %s", title, got, ChristmasByTitle)
		}
	}
}

// What must never qualify: artist names, the words that count only in a
// genre, the words that never count on their own, and the near misses of a
// carol.
var xmasExclusions = []xmasCase{
	// Artist names never qualify a track.
	{"Pretty Boy", "Noel Gallagher's High Flying Birds", "Council Skies", "Rock", ChristmasNone, "Noel Gallagher"},
	{"Wonderwall", "Noel Gallagher", "Live Forever", "Rock", ChristmasNone, "Noel Gallagher again"},
	{"Big Plans", "Christmas", "In Excelsior Dayglo", "Alternative", ChristmasNone, "the band Christmas"},
	{"Torn", "Natalie Imbruglia", "Left of the Middle", "Pop", ChristmasNone, "Natalie"},
	{"Tchikita", "Jul", "Je ne me vois pas briller", "Rap", ChristmasNone, "Jul the French rapper"},
	{"Song", "Noël Christmas Navidad", "Album", "Pop", ChristmasNone, "an artist made of Christmas words"},

	// noel in an album: the artist's own name, or a credit.
	{"Everybody's on the Run", "Noel Gallagher's High Flying Birds", "Noel Gallagher's High Flying Birds", "Rock", ChristmasNone, "the band's self-titled album"},
	{"Song", "Paul Weller", "Live with Noel Gallagher", "Rock", ChristmasNone, "noel after with"},
	{"Song", "A", "Sessions feat. Noel", "Rock", ChristmasNone, "noel after feat"},
	{"Song", "A", "Sessions ft. Noel", "Rock", ChristmasNone, "noel after ft"},
	{"Song", "A", "Sessions featuring Noel", "Rock", ChristmasNone, "noel after featuring"},
	{"Song", "Noël", "Noël", "Pop", ChristmasNone, "a singer named Noël's album named for her (the accepted miss)"},

	// jul and natal count only as a whole genre segment.
	{"Jul", "Jul", "Jul", "Hip-Hop/Rap", ChristmasNone, "jul in a title and an album, the rapper's genre"},
	{"Song", "A", "Jul i Skomakergata", "Pop", ChristmasNone, "jul in an album"},
	{"Natal", "A", "Natal", "Folk", ChristmasNone, "natal in a title and an album"},
	{"Song", "A", "Album", "Jul Rap", ChristmasNone, "jul inside a segment is not the segment"},

	// natale, craciun and noel never count in a title.
	{"Natale", "A", "Album", "Pop", ChristmasNone, "natale is a surname"},
	{"Crăciun", "A", "Album", "Folk", ChristmasNone, "craciun is a surname"},
	{"Noël", "A", "Album", "Folk", ChristmasNone, "noel is a name"},

	// Never on their own.
	{"Holiday", "Madonna", "Madonna", "Pop", ChristmasNone, "Holiday by Madonna"},
	{"Song", "A", "Album", "Holiday", ChristmasNone, "bare holiday"},
	{"Song", "A", "Holiday", "Pop", ChristmasNone, "holiday in an album"},
	{"Song", "A", "Album", "Holiday Jazz", ChristmasNone, "holiday paired with something else"},
	{"Song", "A", "Album", "Holiday; Hanukkah", ChristmasNone, "holiday beside Hanukkah"},
	{"Song", "A", "Album", "Holiday: Other", ChristmasNone, "the iTunes Store's other holidays"},
	{"The Four Seasons, Violin Concerto in F Minor, RV 297 \"Winter\": I. Allegro non molto", "Antonio Vivaldi", "The Four Seasons", "Classical", ChristmasNone, "Winter by Vivaldi"},
	{"Snow (Hey Oh)", "Red Hot Chili Peppers", "Stadium Arcadium", "Rock", ChristmasNone, "Snow (Hey Oh)"},
	{"Winter Wonderland", "A", "Album", "Pop", ChristmasNone, "winter on its own"},
	{"Carol", "A", "Album", "Pop", ChristmasNone, "carol"},
	{"Merry Go 'Round", "A", "Album", "Pop", ChristmasNone, "merry"},
	{"Angel", "A", "Album", "Pop", ChristmasNone, "angel"},
	{"Star", "A", "Album", "Pop", ChristmasNone, "star"},
	{"Bells", "A", "Album", "Pop", ChristmasNone, "bells"},
	{"Santa Monica", "Everclear", "Sparkle and Fade", "Rock", ChristmasNone, "santa"},
	{"Santa Baby", "A", "Album", "Pop", ChristmasNone, "santa, even in a Christmas song the table leaves out"},
	{"Jingle Bell Rock", "A", "Album", "Pop", ChristmasNone, "not a carol in the table; its album or genre must say"},

	// A carol's title is equal or nothing.
	{"Silent Nightmare", "A", "Album", "Metal", ChristmasNone, "begins with a carol"},
	{"Silent Night, Deadly Night", "A", "Album", "Metal", ChristmasNone, "a carol and more"},
	{"Silent Night - Deadly Night", "A", "Album", "Metal", ChristmasNone, "a dash before what is not a version"},
	{"Joy to the World", "Three Dog Night", "Naturally", "Rock", ChristmasNone, "Three Dog Night's hit, left out of the table"},
	{"Xmasque", "A", "Album", "Pop", ChristmasNone, "xmas must be a whole word"},

	// The hyphen joins x-mas; a space does not.
	{"Song", "A", "Vol. X Más Allá", "Latin", ChristmasNone, "ten, and more"},

	// A genre segment is equal or nothing.
	{"Song", "A", "Album", "Christmas Pop", ChristmasNone, "a genre that names Christmas inside it is not the segment"},

	// Nothing at all.
	{"", "", "", "", ChristmasNone, "no tags"},
}

func TestMatchChristmasExclusions(t *testing.T) { runXmasCases(t, xmasExclusions) }

// Scripts other than Latin never match by accident, and the decomposed and
// compatibility forms of a Latin word do.
var xmasForms = []xmasCase{
	{"Song", "A", "Album", "Рождество", ChristmasNone, "Russian is in no list"},
	{"Song", "A", "クリスマス・ソング", "Pop", ChristmasNone, "Japanese is in no list"},
	{"Χριστούγεννα", "A", "Album", "Pop", ChristmasNone, "Greek is in no list"},
	{"Song", "A", "\u0421hristmas Songs", "Pop", ChristmasNone, "a Cyrillic С is not a C"},
	{"Song", "A", "Noe\u0308l Acadien", "Folk", ChristmasByAlbum, "a decomposed ë"},
	{"Song", "A", "Album", "ＣＨＲＩＳＴＭＡＳ", ChristmasByGenre, "fullwidth letters"},
	{"Ｓｉｌｅｎｔ\u3000Ｎｉｇｈｔ", "A", "Album", "Pop", ChristmasByTitle, "fullwidth, an ideographic space"},
	{"Adeste \uFB01deles", "A", "Album", "Choral", ChristmasByTitle, "the fi ligature"},
	{"Song", "A", "KOLĘDY", "Choral", ChristmasByAlbum, "Kolędy, ę through NFKD"},
	{"BÓG SIĘ RODZI", "A", "Album", "Choral", ChristmasByTitle, "Bóg się rodzi in capitals"},
	{"Lulajz\u0307e Jezuniu", "A", "Album", "Choral", ChristmasByTitle, "a decomposed ż"},
	{"Song", "A", "WEIHNACHTSLIEDER", "Folk", ChristmasByAlbum, "German in capitals"},
	{"Weiße Weihnacht", "A", "Album", "Folk", ChristmasByTitleWord, "ß, and a word beginning weihnacht"},
	{"Mo\u0308ssens julafton", "A", "Album", "Folk", ChristmasByTitle, "a decomposed ö"},
}

func TestMatchChristmasAcrossScriptsAndForms(t *testing.T) { runXmasCases(t, xmasForms) }

// A very long title costs a few scans, and the suffixes come off a bounded
// number of times.
func TestMatchChristmasOnAVeryLongTitle(t *testing.T) {
	long := strings.Repeat("Silent Night ", 100_000)
	start := time.Now()
	if got := MatchChristmas(long, "A", "Album", "Pop"); got != ChristmasNone {
		t.Errorf("a 1.3 MB title of repeated carols answers %s, want %s", got, ChristmasNone)
	}
	nested := "Silent Night" + strings.Repeat(" (Live)", 50_000)
	if got := MatchChristmas(nested, "A", "Album", "Pop"); got != ChristmasNone {
		t.Errorf("a carol under 50,000 parentheticals answers %s, want %s (the strip is bounded)", got, ChristmasNone)
	}
	deep := "Silent Night (" + strings.Repeat("(", 50_000) + strings.Repeat(")", 50_000) + ")"
	if got := MatchChristmas(deep, "A", "Album", "Pop"); got != ChristmasByTitle {
		t.Errorf("a carol with one deeply nested parenthetical answers %s, want %s", got, ChristmasByTitle)
	}
	christmas := strings.Repeat("a ", 200_000) + "Christmas"
	if got := MatchChristmas(christmas, "A", "Album", "Pop"); got != ChristmasByTitleWord {
		t.Errorf("a long title ending in Christmas answers %s, want %s", got, ChristmasByTitleWord)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("four very long titles took %s", d)
	}
	if got := MatchChristmas("Silent Night"+strings.Repeat(" (Live)", maxTitleSuffixes), "A", "Album", "Pop"); got != ChristmasByTitle {
		t.Errorf("a carol under %d parentheticals answers %s, want %s", maxTitleSuffixes, got, ChristmasByTitle)
	}
}

// The match never panics, is the same twice, folds idempotently, never
// changes with an artist's name unless the album holds noel, and never with
// the case of an ASCII letter.
func FuzzMatchChristmas(f *testing.F) {
	seeds := [][4]string{
		{"Silent Night (Live)", "Bing Crosby", "Hits", "Pop"},
		{"Song", "Noel Gallagher", "Noel Gallagher's High Flying Birds", "Rock"},
		{"Song", "A", "Live with Noel Gallagher", "Rock"},
		{"Song", "A", "Noël Acadien", "Folk"},
		{"Song", "A", "Album", "Classical; Christmas"},
		{"Song", "A", "Album", "Holiday"},
		{"Bóg się rodzi", "A", "Kolędy i pastorałki", ""},
		{"Silent Night - Deadly Night", "A", "X-Mas", "x - mas"},
		{"((((Silent Night))))", "", "", "\\"},
		{"\xff\xfeNo\xcc\x88el", "\x80", "\xe2\x80", "Christmas:"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1], s[2], s[3])
	}
	f.Fuzz(func(t *testing.T, title, artist, album, genre string) {
		got := MatchChristmas(title, artist, album, genre)
		if got < ChristmasNone || got > ChristmasByTitleWord {
			t.Fatalf("rule %d is outside the enum", got)
		}
		if again := MatchChristmas(title, artist, album, genre); again != got {
			t.Fatalf("the same tags answered %s, then %s", got, again)
		}
		for _, s := range []string{title, artist, album, genre} {
			if once := christmasFold(s); christmasFold(once) != once {
				t.Fatalf("christmasFold(%q) = %q, and folding it again gives %q", s, once, christmasFold(once))
			}
		}
		if !hasWord(christmasFold(album), "noel") {
			if other := MatchChristmas(title, "", album, genre); other != got {
				t.Fatalf("the artist %q decided the match (%s, %s with none) for an album without noel", artist, got, other)
			}
		}
		up := func(s string) string {
			return strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' {
					return r - 'a' + 'A'
				}
				return r
			}, s)
		}
		if upper := MatchChristmas(up(title), up(artist), up(album), up(genre)); upper != got {
			t.Fatalf("ASCII capitals changed the answer from %s to %s", got, upper)
		}
	})
}
