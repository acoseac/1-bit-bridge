package manifest

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/dhowden/tag"
)

// containerText is the text a file keeps in chunks of its own container
// rather than in an ID3 tag: a DSDIFF DIIN chunk's title (DITI) and artist
// (DIAR), and a RIFF LIST/INFO chunk's title (INAM), artist (IART), album
// (IPRD) and genre (IGNR).
//
// The scanner fills a track's title, album and artist from its path
// (fillFromPath: the file name, the folder, the folder above) BEFORE it
// extracts, so this text is applied as every tag reader's value is: a field it
// has a value for replaces what the track holds (applyUnder). Both walkers
// wrote a field only while it was EMPTY until ExtractorVersion 20, which in a
// scan is never, so a DFF's DIIN and a WAV's LIST/INFO title, album and artist
// never reached a row, while their extractor tests, which start from an empty
// Track, passed (backlog B140; TestNoExtractorFillsAPathGuessedFieldOnlyWhenEmpty
// keeps the package from growing a third).
type containerText struct {
	title, artist, album, genre string
}

// keepFirst gives each field c has no value for the value next has: the first
// chunk to name a field keeps it, in a file that carries two DIIN or two
// LIST/INFO chunks, or two of one text chunk.
func (c *containerText) keepFirst(next containerText) {
	if c.title == "" {
		c.title = next.title
	}
	if c.artist == "" {
		c.artist = next.artist
	}
	if c.album == "" {
		c.album = next.album
	}
	if c.genre == "" {
		c.genre = next.genre
	}
}

// applyUnder writes c onto t beneath the file's ID3 tag (id3; nil when the
// file has none, or none that could be read): each field c has a value for
// replaces what t holds (the path's guess, in a scan) unless the ID3 tag has a
// value for that field, which populateFromTagMetadata has written into t (the
// same test: a value that is not empty once trimmed). So the ID3 tag answers
// each field it has a value for and the container's own text the rest,
// whichever chunk comes first in the file. It is how TagLib reads a DSDIFF file
// holding both an ID3 and a DIIN chunk (its tag answers from the ID3 chunk
// first, field by field; measured with TagLib 2.0.2 on
// testdata/dff/diin_then_id3.dff and id3_then_diin.dff) and how the WAV walk
// always ranked an ID3 chunk over LIST/INFO.
//
// Called once, after the walk: a chunk read later must not decide the answer by
// coming later.
func (c containerText) applyUnder(t *Track, id3 tag.Metadata) {
	var tagTitle, tagArtist, tagAlbum, tagGenre string
	if id3 != nil {
		tagTitle = strings.TrimSpace(id3.Title())
		tagArtist = strings.TrimSpace(id3.Artist())
		tagAlbum = strings.TrimSpace(id3.Album())
		tagGenre = strings.TrimSpace(id3.Genre())
	}
	if c.title != "" && tagTitle == "" {
		t.Title = c.title
		// The container title replaces the path guess. Recompute so a
		// marker on the filename does not stay once a title was read,
		// and a marker on this title does count.
		var advisory, explicitField string
		if id3 != nil && id3.Raw() != nil {
			raw := id3.Raw()
			named := id3v2NamedValues(raw)
			advisory, _ = namedValueOf(raw, named, "itunesadvisory")
			explicitField, _ = namedValueOf(raw, named, "explicit")
		}
		t.Explicit = ExplicitVerdict(ExplicitSignals{
			ItunesAdvisory: advisory,
			Explicit:       explicitField,
			Title:          c.title,
		})
	}
	if c.artist != "" && tagArtist == "" {
		t.Artist = c.artist
	}
	if c.album != "" && tagAlbum == "" {
		t.Album = c.album
	}
	if c.genre != "" && tagGenre == "" {
		t.Genre = c.genre
	}
}

// diinText is the text of a DITI or DIAR chunk's counted bytes: up to the
// first NUL (a writer that terminates or pads its text inside the count leaves
// nothing after it), as UTF-8 when the bytes are valid UTF-8 and as ISO-8859-1
// otherwise, trimmed of spaces. The DSDIFF 1.5 specification calls the text
// characters and names no encoding; TagLib writes ISO-8859-1 (0xE9 for é,
// measured on testdata/dff/taglib_diin.dff), where a raw copy of the bytes is
// invalid UTF-8 and would reach the wire as U+FFFD. Every byte of ISO-8859-1 is
// the code point of the same number.
func diinText(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if utf8.Valid(b) {
		return strings.TrimSpace(string(b))
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return strings.TrimSpace(string(runes))
}
