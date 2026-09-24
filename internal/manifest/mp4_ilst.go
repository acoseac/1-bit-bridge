package manifest

// iTunes-metadata repairs for what dhowden/tag v0.0.0-20240417 reads wrong
// or not at all in an MP4 file (ExtractorVersion 15, from a 2026-09-24 user
// report that Rock and Pop listed their DSD files and none of their ALAC):
//
//   - The predefined-genre atom `gnre`. iTunes and Music write a STANDARD
//     genre (Rock, Pop, Jazz, Classical…) as `gnre` — a big-endian uint16
//     holding the ID3v1 genre index PLUS ONE — and only a custom genre as
//     the `©gen` text atom. dhowden's MP4 atom map knows `©gen` and not
//     `gnre`, so it skipped the atom and every such file reached the
//     manifest with no genre. extractMP4PredefinedGenre reads it.
//   - The freeform (`----`) data-atom locale. dhowden's readCustomAtom keeps
//     the data atom's 4-byte locale on every value, so a freeform arrived
//     as "\x00\x00\x00\x00<value>": an M4A's MusicBrainz ids reached the
//     wire NUL-prefixed, and its ReplayGain and ORIGINALDATE / ORIGINALYEAR
//     failed to parse. stripMP4FreeformLocales removes it once, at the source.
//
// The phone's own enrich (`AVTagClassifier`, SMB and on-device sources)
// reads both through AVFoundation and names a `gnre` value from the same
// table, so a file shows the same genre whichever path indexed it.

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// id3v1Genres is dhowden/tag's `id3v2Genres` VERBATIM — the table its ID3
// reader expands a numeric TCON "(17)" through, so an M4A `gnre` and an MP3
// numeric genre name the same genre. Index 141's trailing space is dhowden's
// and is kept (id3v1GenreName trims); the iOS `GenreNormalizer.id3GenreTable`
// mirrors the same table. TestID3v1Genres_MatchDhowdenExpansion runs every
// index through dhowden's own expansion, so a change on either side fails.
var id3v1Genres = [...]string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge",
	"Hip-Hop", "Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B",
	"Rap", "Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska",
	"Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient",
	"Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical",
	"Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel",
	"Noise", "AlternRock", "Bass", "Soul", "Punk", "Space", "Meditative",
	"Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic",
	"Darkwave", "Techno-Industrial", "Electronic", "Pop-Folk",
	"Eurodance", "Dream", "Southern Rock", "Comedy", "Cult", "Gangsta",
	"Top 40", "Christian Rap", "Pop/Funk", "Jungle", "Native American",
	"Cabaret", "New Wave", "Psychedelic", "Rave", "Showtunes", "Trailer",
	"Lo-Fi", "Tribal", "Acid Punk", "Acid Jazz", "Polka", "Retro",
	"Musical", "Rock & Roll", "Hard Rock", "Folk", "Folk-Rock",
	"National Folk", "Swing", "Fast Fusion", "Bebob", "Latin", "Revival",
	"Celtic", "Bluegrass", "Avantgarde", "Gothic Rock", "Progressive Rock",
	"Psychedelic Rock", "Symphonic Rock", "Slow Rock", "Big Band",
	"Chorus", "Easy Listening", "Acoustic", "Humour", "Speech", "Chanson",
	"Opera", "Chamber Music", "Sonata", "Symphony", "Booty Bass", "Primus",
	"Porn Groove", "Satire", "Slow Jam", "Club", "Tango", "Samba",
	"Folklore", "Ballad", "Power Ballad", "Rhythmic Soul", "Freestyle",
	"Duet", "Punk Rock", "Drum Solo", "A capella", "Euro-House", "Dance Hall",
	"Goa", "Drum & Bass", "Club-House", "Hardcore", "Terror", "Indie",
	"Britpop", "Negerpunk", "Polsk Punk", "Beat", "Christian Gangsta Rap",
	"Heavy Metal", "Black Metal", "Crossover", "Contemporary Christian",
	"Christian Rock ", "Merengue", "Salsa", "Thrash Metal", "Anime", "JPop",
	"Synthpop",
	"Christmas", "Art Rock", "Baroque", "Bhangra", "Big Beat", "Breakbeat",
	"Chillout", "Downtempo", "Dub", "EBM", "Eclectic", "Electro",
	"Electroclash", "Emo", "Experimental", "Garage", "Global", "IDM",
	"Illbient", "Industro-Goth", "Jam Band", "Krautrock", "Leftfield", "Lounge",
	"Math Rock", "New Romantic", "Nu-Breakz", "Post-Punk", "Post-Rock", "Psytrance",
	"Shoegaze", "Space Rock", "Trop Rock", "World Music", "Neoclassical", "Audiobook",
	"Audio Theatre", "Neue Deutsche Welle", "Podcast", "Indie Rock", "G-Funk", "Dubstep",
	"Garage Rock", "Psybient",
}

// id3v1GenreName names a `gnre` value: the ID3v1 index PLUS ONE, so 18 is
// Rock (ID3v1 17) and 14 is Pop (ID3v1 13). 0 means "none", and a value past
// the table names nothing.
func id3v1GenreName(value uint16) string {
	if value == 0 || int(value) > len(id3v1Genres) {
		return ""
	}
	return strings.TrimSpace(id3v1Genres[value-1])
}

// mp4Box is a located box: its absolute start, its header size (8, or 16
// for a 64-bit `largesize` box) and its total size.
type mp4Box struct{ start, header, size uint64 }

func (b mp4Box) payloadStart() uint64 { return b.start + b.header }

// end is the box's end offset, saturated so a forged size cannot wrap the
// bound of a search inside it.
func (b mp4Box) end() uint64 {
	if e := b.start + b.size; e >= b.start {
		return e
	}
	return ^uint64(0)
}

// findMP4Child locates the first box named `name` inside `parent`, searching
// from `from`. `found` is false — no error — for an absent box, for a
// malformed one (errMP4StructureNotFound), and for one declaring past its
// parent: findAtom checks only that a box's HEADER sits inside the bound,
// the rule findMVHDPayload applies to `mvhd`. Genuine I/O and
// iteration-budget failures propagate.
func findMP4Child(r io.ReadSeeker, name string, from uint64, parent mp4Box) (mp4Box, bool, error) {
	end := parent.end()
	start, header, size, err := findAtom(r, name, from, end)
	if err != nil {
		if errors.Is(err, errMP4StructureNotFound) {
			return mp4Box{}, false, nil
		}
		return mp4Box{}, false, err
	}
	if size == 0 || size > end-start {
		return mp4Box{}, false, nil
	}
	return mp4Box{start: start, header: header, size: size}, true, nil
}

// extractMP4PredefinedGenre reads `moov/udta/meta/ilst/gnre` and returns
// the genre it names, or "" (no error) when there is none to read — no
// moov, no iTunes metadata, no `gnre`, a value outside the table, a data
// atom too short to hold one. Genuine I/O and atom-walk failures propagate,
// the contract the codec / rate / duration walks share, so the caller's
// Warn sees a real read problem and never a structurally absent atom.
func extractMP4PredefinedGenre(r io.ReadSeeker) (string, error) {
	moovStart, moovHeader, moovSize, err := findMoov(r)
	if err != nil {
		if errors.Is(err, errMP4StructureNotFound) {
			return "", nil
		}
		return "", err
	}
	moov := mp4Box{start: moovStart, header: moovHeader, size: moovSize}
	udta, found, err := findMP4Child(r, "udta", moov.payloadStart(), moov)
	if err != nil || !found {
		return "", err
	}
	meta, found, err := findMP4Child(r, "meta", udta.payloadStart(), udta)
	if err != nil || !found {
		return "", err
	}
	childrenStart, err := mp4MetaChildrenStart(r, meta)
	if err != nil {
		return "", err
	}
	ilst, found, err := findMP4Child(r, "ilst", childrenStart, meta)
	if err != nil || !found {
		return "", err
	}
	gnre, found, err := findMP4Child(r, "gnre", ilst.payloadStart(), ilst)
	if err != nil || !found {
		return "", err
	}
	data, found, err := findMP4Child(r, "data", gnre.payloadStart(), gnre)
	if err != nil || !found {
		return "", err
	}
	value, found, err := readMP4GnreValue(r, data)
	if err != nil || !found {
		return "", err
	}
	return id3v1GenreName(value), nil
}

// mp4MetaChildrenStart returns where `meta`'s children begin. `meta` is a
// FullBox in ISO BMFF and in every iTunes file (4 bytes of version + flags
// before its first child) but a plain container in QuickTime files, and its
// first child is the mandatory `hdlr`. So "hdlr" at payload bytes 4..8 means
// the plain form (bytes 0..4 are hdlr's own size); anything else is the
// FullBox form, which is what iTunes writes and what dhowden assumes. Read as
// a size, "hdlr" would be a 1.7 GB box, so the two cannot be confused.
func mp4MetaChildrenStart(r io.ReadSeeker, meta mp4Box) (uint64, error) {
	const fullBoxHeader = 4
	start := meta.payloadStart()
	if meta.end()-start < 8 {
		return start + fullBoxHeader, nil
	}
	if _, err := r.Seek(int64(start), io.SeekStart); err != nil {
		return 0, err
	}
	var peek [8]byte
	if _, err := io.ReadFull(r, peek[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return start + fullBoxHeader, nil
		}
		return 0, err
	}
	if string(peek[4:8]) == "hdlr" {
		return start, nil
	}
	return start + fullBoxHeader, nil
}

// readMP4GnreValue reads the uint16 a `gnre` data atom holds. A data atom's
// payload is a 4-byte type indicator (version + class; 0, "implicit", for
// `gnre`), a 4-byte locale, then the value. A payload too short for two
// value bytes, or one cut short by EOF, is absent rather than an error.
func readMP4GnreValue(r io.ReadSeeker, data mp4Box) (uint16, bool, error) {
	const valueOffset = 8
	if data.size-data.header < valueOffset+2 {
		return 0, false, nil
	}
	if _, err := r.Seek(int64(data.payloadStart()+valueOffset), io.SeekStart); err != nil {
		return 0, false, err
	}
	var value [2]byte
	if _, err := io.ReadFull(r, value[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return binary.BigEndian.Uint16(value[:]), true, nil
}

// mp4DataLocale is the 4-byte locale a `data` atom carries between its type
// indicator and its value. iTunes, mutagen (Picard) and every other tagger
// in the wild write 0.
const mp4DataLocale = "\x00\x00\x00\x00"

// stripMP4FreeformLocales removes the data-atom locale dhowden/tag leaves on
// every iTunes freeform (`----`) value. Its readCustomAtom slices a data
// atom's body at [4:] — past the type indicator, not the locale — and joins
// several data atoms with ";", so a freeform arrives as
// "\x00\x00\x00\x00<a>;\x00\x00\x00\x00<b>". Standard atoms go
// through readAtomData, which skips both fields, so only a freeform value
// can START with the locale: the prefix is the discriminator, and a value
// without it is left alone. The map is dhowden's own (metadataMP4.Raw
// returns it), so its accessors and every raw reader after this one see the
// clean value.
func stripMP4FreeformLocales(raw map[string]any) {
	for key, v := range raw {
		s, ok := v.(string)
		if !ok || !strings.HasPrefix(s, mp4DataLocale) {
			continue
		}
		raw[key] = strings.ReplaceAll(s, ";"+mp4DataLocale, ";")[len(mp4DataLocale):]
	}
}
