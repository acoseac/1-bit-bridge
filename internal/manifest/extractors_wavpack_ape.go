package manifest

// The extraction of a WavPack (.wv) or Monkey's Audio (.ape) file (backlog
// B489): the format from the file's own header (wavpack.go, ape.go), then
// its tags from its APEv2 tag (apev2.go), dhowden's ID3v1 read standing in
// only where it has none, then its cover, embedded or the folder's. Every
// read goes through the source openAudioFile hands out, so a read that did
// not complete is the scanner's unread file (backlog B134) and never a row.
//
// The codec names what the bridge established: "WAVPACK" a lossless stream
// (a hybrid one only with its matching correction file, correction.go),
// "WAVPACK-LOSSY" a hybrid stream without it, lossy for every purpose, so no
// bit depth or sample kind; "WAVPACK-DSD" WavPack 5's DSD, its rate the bit
// rate (as a DSF's is), one bit, isDSD; "APE" a Monkey's Audio file. A file
// whose header this cannot read is refused, before anything is stamped, and
// the scanner indexes it by its name (backlog B145).

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
	tag "github.com/dhowden/tag"
)

// The codec tokens of the two formats (Track.Codec).
const (
	CodecWavPack      = "WAVPACK"
	CodecWavPackLossy = "WAVPACK-LOSSY"
	CodecWavPackDSD   = "WAVPACK-DSD"
	CodecAPE          = "APE"
)

// extractWavPackWithContext extracts a .wv: its first audio block's format,
// a hybrid stream's correction file, then its tags and cover.
func extractWavPackWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := openAudioFile(absPath, ec)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	s, err := readWavPackStream(f)
	if err != nil {
		return err
	}
	if err := applyWavPackStream(absPath, t, s, ec); err != nil {
		return err
	}
	readAPEFamilyTags(f, info.Size(), absPath, t, ec)
	return nil
}

// applyWavPackStream stamps a WavPack stream's format on t: the codec first,
// then the depth its codec allows (canSetBitsPerSample), the rate, channels,
// the DSD flag, the sample kind of a lossless PCM stream, and the duration
// its header states, through plausibleDuration. A hybrid stream's
// correction file decides between WAVPACK and WAVPACK-LOSSY; an observation
// of it that did not complete is noted as a read that did not complete, and
// returned, so nothing of the file is written.
func applyWavPackStream(absPath string, t *Track, s wvStream, ec *ExtractContext) error {
	isDSD := s.dsd
	channels := s.channels
	if s.dsd {
		t.Codec = CodecWavPackDSD
		rate := float64(s.rate) * 8
		t.SampleRate = &rate
		if canSetBitsPerSample(t.Codec) {
			bits := 1
			t.BitsPerSample = &bits
		}
	} else {
		lossless := true
		if s.hybrid {
			obs, err := observeCorrection(absPath, s, ec)
			if err != nil {
				if ec != nil {
					ec.reads.note(err)
				}
				return fmt.Errorf("wavpack: the correction file could not be read: %w", err)
			}
			applyCorrection(t, obs)
			lossless = obs.state == correctionMatched
		}
		rate := float64(s.rate)
		t.SampleRate = &rate
		if lossless {
			t.Codec = CodecWavPack
			if canSetBitsPerSample(t.Codec) {
				bits := s.bits
				t.BitsPerSample = &bits
			}
			t.SampleFormat = SampleFormatInteger
			if s.float {
				t.SampleFormat = SampleFormatFloat
			}
		} else {
			t.Codec = CodecWavPackLossy
		}
	}
	t.IsDSD = &isDSD
	t.Channels = &channels
	if d, ok := s.duration(); ok && plausibleDuration(d) {
		t.Duration = &d
	}
	return nil
}

// extractAPEWithContext extracts an .ape: its header's format, then its tags
// and cover.
func extractAPEWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := openAudioFile(absPath, ec)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	s, err := readAPEHeader(f)
	if err != nil {
		return err
	}
	t.Codec = CodecAPE
	if canSetBitsPerSample(t.Codec) {
		bits := int(s.bits)
		t.BitsPerSample = &bits
	}
	// Monkey's Audio below 32 bits is integer PCM, whatever its version; a
	// 32-bit file may hold float since its version 10 SDK, by a format flag
	// this reader does not establish, so it states no kind (absent is
	// unknown, never integer: Track.SampleFormat).
	if s.bits < 32 {
		t.SampleFormat = SampleFormatInteger
	}
	rate := float64(s.rate)
	t.SampleRate = &rate
	isDSD := false
	t.IsDSD = &isDSD
	channels := int(s.channels)
	t.Channels = &channels
	if d := s.duration(); plausibleDuration(d) {
		t.Duration = &d
	}
	readAPEFamilyTags(f, info.Size(), absPath, t, ec)
	return nil
}

// readAPEFamilyTags reads the tags of a WavPack or Monkey's Audio file of
// size bytes from f: its APEv2 tag, or where it has none, its ID3v1 tag
// (readAPEFamilyID3v1); then the cover, the tag's front cover ahead of the
// folder's. A read of the tag that did not complete returns, noted by f, so
// the extraction's caller discards what was read.
func readAPEFamilyTags(f extractSource, size int64, absPath string, t *Track, ec *ExtractContext) {
	apeTags, found, err := readAPEv2(f, size, trackLogPath(absPath, t))
	if err != nil {
		return
	}
	if !found {
		readAPEFamilyID3v1(f, size, absPath, t, ec)
		return
	}
	applyAPEv2Tags(apeTags, t)
	if ec != nil && ec.ArtworkCacheDir != "" {
		var pic *tag.Picture
		if len(apeTags.cover) > 0 {
			pic = &tag.Picture{Type: "Cover (front)", Data: apeTags.cover}
		}
		extractLocalArtworkPicture(absPath, t, pic, ec)
	}
}

// readAPEFamilyID3v1 is the fallback for a WavPack or Monkey's Audio file
// with no APEv2 tag: the ID3v1 tag in its last 128 bytes, the one other tag
// either format defines, read by dhowden's ID3v1 reader from those bytes
// alone, which is what dhowden's dispatcher reads for a stream that opens
// with "wvpk" or "MAC ". Nothing else of the file reaches dhowden: an ID3v2
// tag ahead of the stream, which neither format defines, would have it
// allocate by a length the file need not back (dhowden's readBytes sizes a
// buffer by an ID3v2 header's length up to 10 MB; FuzzExtractAPE found a
// 603-byte file allocating 1.3 MB), so it is not read. The 128 bytes are read
// by offset, as the rest of these files is, so the read makes no seek of the
// file, and through a dhowdenReadBuffer, as every dhowden read is (backlog
// B117). Then the folder's cover, as for a file whose tags hold none, and the
// stem's explicit mark for a file with no ID3v1 tag either.
func readAPEFamilyID3v1(f extractSource, size int64, absPath string, t *Track, ec *ExtractContext) {
	var m tag.Metadata
	if size >= apeID3v1Bytes {
		stream, release := newDhowdenReadBuffer(io.NewSectionReader(f, size-apeID3v1Bytes, apeID3v1Bytes))
		md, err := tag.ReadID3v1Tags(stream)
		release()
		if err == nil {
			m = md
		}
	}
	if m == nil {
		applyStemExplicitMark(t)
	} else {
		populateFromTagMetadata(m, t)
	}
	if ec != nil && ec.ArtworkCacheDir != "" {
		extractLocalArtwork(absPath, t, nil, ec)
	}
}

// applyAPEv2Tags writes an APEv2 tag's items into t, by the names the Vorbis
// comment reader takes: each key is normalised as a raw tag key is
// (normaliseRawTagKey: case folded, a space an underscore), and the
// single-valued fields are looked up through namedValueOf and stringOf, as a
// Vorbis comment's are. A field a tag repeats, or holds several values of
// (APEv2 separates them with NUL), is joined with "; " as a FLAC's comment
// block joins it (applyFLACMultiValueArtists): ARTIST and ALBUMARTIST
// uncapped, ALBUM, GENRE, COMPOSER, CONDUCTOR and WORK at 64 values; TITLE is
// its first value. Year, track and disc are present when their key is
// (Some(0) for a value that names no number), read as every other format's
// are (rawDateYear, leadingTagNumber). A value that is empty sets nothing,
// so the path's guess stays.
func applyAPEv2Tags(apeTags apeTagData, t *Track) {
	raw := make(map[string]any, len(apeTags.items))
	var j apeJoins
	for _, it := range apeTags.items {
		prev, _ := raw[it.key].([]string)
		raw[it.key] = append(prev, it.values...)
		j.add(it)
		// Each value a document of its own, as each repeated Vorbis
		// comment is.
		if kind := apeLyricsKey(it.key); kind != "" {
			for _, v := range it.values {
				if c, ok := lyrics.TextCandidate(v, "", kind == "synced", 0); ok {
					t.lyricsCandidates = append(t.lyricsCandidates, c)
				}
			}
		}
	}
	j.apply(t)
	if hasAnyRawKey(raw, yearTagAliases...) {
		y := rawDateYear(raw)
		t.Year = &y
	}
	if hasAnyRawKey(raw, "track", "tracknumber") {
		v, _ := stringOf(raw, "track", "tracknumber")
		n := leadingTagNumber(v)
		t.TrackNumber = &n
	}
	if hasAnyRawKey(raw, "disc", "discnumber", "disk") {
		v, _ := stringOf(raw, "disc", "discnumber", "disk")
		n := leadingTagNumber(v)
		t.DiscNumber = &n
	}
	applyAPEv2Named(raw, t)
	t.Explicit = ExplicitVerdict(ExplicitSignals{
		ItunesAdvisory: j.advisory,
		Explicit:       j.explicit,
		Title:          explicitTrackTitle(j.title, t.Title),
	})
}

// applyAPEv2Named writes the fields a tag names by a key of its own: the
// compilation flag (and the album artist it stands in for), the original
// year, the tempo, the MusicBrainz ids and ReplayGain.
func applyAPEv2Named(raw map[string]any, t *Track) {
	if comp, ok := stringOf(raw, "compilation"); ok && comp == "1" {
		t.Compilation = true
		if t.AlbumArtist == "" {
			t.AlbumArtist = "Various Artists"
		}
	}
	if v, ok := stringOf(raw, "originalyear", "originaldate"); ok {
		if y, err := parseYearPrefix(v); err == nil {
			t.OriginalYear = &y
		}
	}
	if v, ok := stringOf(raw, "bpm"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			t.BPM = &n
		}
	}
	if v, ok := namedValueOf(raw, nil, "musicbrainz_trackid", "musicbrainz_track_id"); ok {
		t.MusicBrainzTrackID = v
	}
	if v, ok := namedValueOf(raw, nil, "musicbrainz_albumid", "musicbrainz_album_id"); ok {
		t.MusicBrainzAlbumID = v
	}
	if v, ok := namedValueOf(raw, nil, "replaygain_track_gain"); ok {
		if g := parseReplayGain(v); g != nil {
			t.ReplayGainTrackDB = g
		}
	}
	if v, ok := namedValueOf(raw, nil, "replaygain_album_gain"); ok {
		if g := parseReplayGain(v); g != nil {
			t.ReplayGainAlbumDB = g
		}
	}
}

// apeJoins gathers the fields a tag's items join, in the tag's order, and
// the first value of the title and of each explicit field.
type apeJoins struct {
	title, advisory, explicit                         string
	artists, albumArtists                             []string
	albums, genres, composers, conductors, works      []string
	nAlbums, nGenres, nComposers, nConductors, nWorks int
}

func (j *apeJoins) add(it apeTagItem) {
	switch explicitFieldName(it.key) {
	case "advisory":
		if j.advisory == "" && len(it.values) > 0 {
			j.advisory = it.values[0]
		}
	case "explicit":
		if j.explicit == "" && len(it.values) > 0 {
			j.explicit = it.values[0]
		}
	}
	for _, v := range it.values {
		switch normaliseRawTagKey(it.key) {
		case "title":
			if j.title == "" {
				j.title = v
			}
		case "artist":
			j.artists = append(j.artists, v)
		case "albumartist", "album_artist":
			j.albumArtists = append(j.albumArtists, v)
		case "album":
			j.albums = appendVorbisField(j.albums, &j.nAlbums, v)
		case "genre":
			j.genres = appendVorbisField(j.genres, &j.nGenres, v)
		case "composer":
			j.composers = appendVorbisField(j.composers, &j.nComposers, v)
		case "conductor":
			j.conductors = appendVorbisField(j.conductors, &j.nConductors, v)
		case "work":
			j.works = appendVorbisField(j.works, &j.nWorks, v)
		}
	}
}

// apply writes what the items gave over the path's guess, each field only
// where the tag held a value.
func (j *apeJoins) apply(t *Track) {
	if j.title != "" {
		t.Title = j.title
	}
	for _, f := range []struct {
		dst    *string
		values []string
	}{
		{&t.Artist, j.artists}, {&t.AlbumArtist, j.albumArtists}, {&t.Album, j.albums},
		{&t.Genre, j.genres}, {&t.Composer, j.composers}, {&t.Conductor, j.conductors},
		{&t.Work, j.works},
	} {
		if len(f.values) > 0 {
			*f.dst = strings.Join(f.values, "; ")
		}
	}
}
