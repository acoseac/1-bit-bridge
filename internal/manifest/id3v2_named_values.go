package manifest

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/dhowden/tag"
)

// An ID3v2 tag files most of its values under a frame id of its own (TIT2,
// TCOM, TDOR, …), and that id is the key dhowden's raw map holds the value
// under, which stringOf matches. Two kinds of frame file a value under a NAME:
// a user text frame (TXXX, and TXX in version 2.2) names its value by its
// description, and a unique file identifier (UFID, UFI) by its owner. That is
// where the MusicBrainz ids Picard writes, and ReplayGain, live in an ID3v2
// tag: TXXX "MusicBrainz Album Id" for the release, TXXX
// "REPLAYGAIN_TRACK_GAIN" and "REPLAYGAIN_ALBUM_GAIN" (upper case from Picard,
// lower case from other writers, so matched without regard to case), and the
// recording id as the UFID owned by http://musicbrainz.org.
//
// dhowden stores a TXXX frame as a *tag.Comm under the key TXXX, the next one
// under TXXX_0, then TXXX_1 and so on (its renaming of a repeated frame id,
// which the ID3v2 guard bounds), and a UFID frame as a *tag.UFID under UFID,
// UFID_0, …. A lookup by key never sees a description or an owner, so until
// backlog B116 no MP3, DSF, AIFF or WAV reached the manifest with its
// MusicBrainz ids or its ReplayGain: the enricher searched by text for releases
// the file names exactly, and an analysis-derived loudness stood in for a
// ReplayGain the file carries.
//
// The named values answer the same aliases a Vorbis comment and an MP4
// freeform atom do, normalised the same way (normaliseRawTagKey), and with the
// same precedence (namedValueOf). They serve only the fields whose ID3v2 home
// is a TXXX or UFID frame, which are the ones the app's ID3v2Parser reads there
// too. A field ID3v2 gives a frame of its own (the compilation flag, the
// composer, the conductor, the work, the original year, the tempo) is read from
// that frame on both sides, and never from a TXXX: the app pins that neither
// side reads TXXX:COMPILATION (test_TXXXCompilationAndV22TCP_areNotRead), since
// a flagged file with no album artist is keyed into "Various Artists" on both.
// ITUNESADVISORY and EXPLICIT are the exceptions that have no frame of
// their own: each is a TXXX of that description, and namedValueOf reads
// it, matching the paired app. TXXX:COMPILATION stays unread.

// id3v2Named is one value an ID3v2 tag files under a name: the name,
// normalised as a raw tag key is, and the value.
type id3v2Named struct {
	name  string
	value string
}

// musicBrainzUFIDName is the name the MusicBrainz UFID's identifier answers to.
// It is the recording id, which Picard writes to that UFID in an ID3v2 tag, to
// MUSICBRAINZ_TRACKID in a Vorbis comment and to the "MusicBrainz Track Id"
// freeform atom in an MP4, and which Track.MusicBrainzTrackID holds (the Atlas
// lyrics tier asks for /v1/atlas/recording/{it}). Picard's TXXX "MusicBrainz
// Release Track Id" names the release's track, a different entity, and answers
// no alias.
const musicBrainzUFIDName = "musicbrainz_trackid"

// id3v2NamedValues returns the values the ID3v2 tag dhowden read into raw files
// under a name, in the order a lookup must see them: the MusicBrainz UFIDs
// first, since the recording id's own frame outranks a TXXX claiming the same
// name, then the TXXX frames; each kind in the tag's order, which dhowden's
// renaming leaves in the key (TXXX, then TXXX_0, TXXX_1, …); and by key where
// that ties (a v2.2 TXX beside a TXXX, which no one tag holds). A strict order,
// never the map's: two frames under one name resolve the same way on every
// scan. A frame whose value is empty is left out, so a later one of the same
// name can answer, as stringOf passes over an empty value. A tag without such
// frames, and every other format's raw map, returns nil.
func id3v2NamedValues(raw map[string]any) []id3v2Named {
	var all []namedFrame
	for key, v := range raw {
		if f, ok := namedFrameOf(key, v); ok {
			all = append(all, f)
		}
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].before(all[j]) })
	named := make([]id3v2Named, len(all))
	for i, f := range all {
		named[i] = f.named
	}
	return named
}

// namedFrame is one frame id3v2NamedValues keeps, with what orders it: whether
// it is a UFID, its place among the frames of its id, and its key.
type namedFrame struct {
	ufid  bool
	index int
	key   string
	named id3v2Named
}

// before is id3v2NamedValues' order: the UFIDs, then by place, then by key. The
// key is unique in a map, so the order is strict.
func (a namedFrame) before(b namedFrame) bool {
	if a.ufid != b.ufid {
		return a.ufid
	}
	if a.index != b.index {
		return a.index < b.index
	}
	return a.key < b.key
}

// namedFrameOf reads what raw holds under key as a named frame: a TXXX or TXX
// frame, named by its description, or a UFID or UFI frame owned by
// MusicBrainz, named musicBrainzUFIDName. Anything else, and a frame whose
// value is empty, is none.
func namedFrameOf(key string, v any) (namedFrame, bool) {
	switch v := v.(type) {
	case *tag.Comm:
		index, ok := renamedFrameIndex(key, "TXXX", "TXX")
		if !ok || v == nil {
			return namedFrame{}, false
		}
		value := id3v2TextValue(v.Text)
		if value == "" {
			return namedFrame{}, false
		}
		return namedFrame{index: index, key: key,
			named: id3v2Named{name: normaliseRawTagKey(v.Description), value: value}}, true
	case *tag.UFID:
		index, ok := renamedFrameIndex(key, "UFID", "UFI")
		if !ok || v == nil || !isMusicBrainzUFIDOwner(v.Provider) {
			return namedFrame{}, false
		}
		value := id3v2TextValue(string(v.Identifier))
		if value == "" {
			return namedFrame{}, false
		}
		return namedFrame{ufid: true, index: index, key: key,
			named: id3v2Named{name: musicBrainzUFIDName, value: value}}, true
	}
	return namedFrame{}, false
}

// renamedFrameIndex reports whether key is one of dhowden's keys for a frame
// whose id is one of ids, and which of them in the tag's order: the id itself
// is the first (0), and the renamed id_n the (n+2)th. Only the renaming's own
// spelling counts (strconv.Itoa's), so no other name reads as such a key.
func renamedFrameIndex(key string, ids ...string) (int, bool) {
	for _, id := range ids {
		if key == id {
			return 0, true
		}
		suffix, ok := strings.CutPrefix(key, id+"_")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 0 || strconv.Itoa(n) != suffix {
			continue
		}
		return n + 1, true
	}
	return 0, false
}

// isMusicBrainzUFIDOwner reports whether a UFID frame's owner is MusicBrainz,
// by the app's rule (ID3v2Parser.applyUFID: the owner, lower-cased, contains
// "musicbrainz.org"), so the two sides take a recording id from the same
// frames. Picard, beets and Mp3tag write exactly http://musicbrainz.org.
func isMusicBrainzUFIDOwner(owner string) bool {
	return strings.Contains(strings.ToLower(owner), "musicbrainz.org")
}

// id3v2TextValue is the value of an ID3v2 text field as dhowden hands it over:
// the first of its values that is not empty, trimmed. dhowden decodes the whole
// field and keeps what separates and ends its values, so every writer measured
// (mutagen, and so Picard; ffmpeg) leaves a terminating NUL on the value
// ("<id>\x00"), a version 2.4 field holds its values NUL-separated, and a UTF-16
// value after the first keeps its byte order mark as a leading U+FEFF (dhowden
// strips only the field's first). Kept, the NUL makes an album id no UUID (the
// enricher drops it, with a Warn) and a ReplayGain value no number.
func id3v2TextValue(s string) string {
	for {
		value, rest, more := strings.Cut(s, "\x00")
		if value = strings.TrimFunc(value, isSpaceOrByteOrderMark); value != "" {
			return value
		}
		if !more {
			return ""
		}
		s = rest
	}
}

// byteOrderMark is U+FEFF, which unicode.IsSpace does not count.
const byteOrderMark rune = 0xFEFF

func isSpaceOrByteOrderMark(r rune) bool { return unicode.IsSpace(r) || r == byteOrderMark }

// namedValueOf looks a value up by name everywhere a tag files one by name: the
// raw map's keys (a Vorbis comment's name, an MP4 freeform atom's: stringOf),
// then the ID3v2 tag's named values (id3v2NamedValues). keys are the aliases in
// priority order, normalised as stringOf's are. The first alias that answers
// wins, wherever its value sits, and under one alias the raw map answers before
// the named values, which answer in their own order. A raw map holds one tag
// format's values, so for a Vorbis or MP4 file (no named values) this is
// stringOf(raw, keys...) exactly, and for an ID3v2 tag the raw map answers none
// of these aliases (its keys are frame ids).
func namedValueOf(raw map[string]any, named []id3v2Named, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := stringOf(raw, k); ok {
			return v, true
		}
		for _, n := range named {
			if n.name == k {
				return n.value, true
			}
		}
	}
	return "", false
}
