package manifest

// The extraction of WavPack and Monkey's Audio files (backlog B489): what each
// header states, what each reader refuses, and the APEv2 tag's fields. The
// files are wavpack_ape_fixtures_test.go's, built from the formats'
// definitions: no reference encoder runs where this suite runs.

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// extractWVAPE writes data as name in a folder of its own and extracts it with
// ec, returning the track and the extraction's error. The track starts with
// the title a path's guess would give it, so a test sees what a tag replaced.
func extractWVAPE(t *testing.T, name string, data []byte, ec *ExtractContext) (*Track, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	writeFixtureFile(t, p, data)
	tr := &Track{Path: name, Title: "Path Title"}
	return tr, ExtractWithContext(p, tr, ec)
}

// formatFacts is what a test asserts of a track's format: bits 0 for none,
// duration -1 for none.
type formatFacts struct {
	codec    string
	rate     float64
	bits     int
	channels int
	dsd      bool
	kind     string
	duration float64
}

func factsOf(tr *Track) formatFacts {
	f := formatFacts{codec: tr.Codec, kind: tr.SampleFormat, duration: -1}
	if tr.SampleRate != nil {
		f.rate = *tr.SampleRate
	}
	if tr.BitsPerSample != nil {
		f.bits = *tr.BitsPerSample
	}
	if tr.Channels != nil {
		f.channels = *tr.Channels
	}
	if tr.IsDSD != nil {
		f.dsd = *tr.IsDSD
	}
	if tr.Duration != nil {
		f.duration = *tr.Duration
	}
	return f
}

// TestTheWavPackHeaderStatesItsFormat reads each format a WavPack encoder
// writes from its first audio block: the rate from the table or from
// ID_SAMPLE_RATE, the bits stored less those shifted out, float, the channels
// of the flags or of ID_CHANNEL_INFO, WavPack 5's DSD (its rate the byte rate
// times eight, one bit, isDSD, no PCM kind), and the length from the 40-bit
// total, or none where the encoder could not write it.
func TestTheWavPackHeaderStatesItsFormat(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want formatFacts
	}{
		{"16-bit stereo at 44.1 kHz", wvFile(stdWavPack()),
			formatFacts{CodecWavPack, 44100, 16, 2, false, SampleFormatInteger, 1}},
		{"24-bit mono at 96 kHz", wvFile(wvSpec{rate: 96000, bytes: 3, channels: 1, total: 96000 * 3}),
			formatFacts{CodecWavPack, 96000, 24, 1, false, SampleFormatInteger, 3}},
		{"20 bits in three bytes", wvFile(wvSpec{rate: 48000, bytes: 3, shift: 4, channels: 2, total: 48000}),
			formatFacts{CodecWavPack, 48000, 20, 2, false, SampleFormatInteger, 1}},
		{"32-bit float", wvFile(wvSpec{rate: 44100, bytes: 4, float: true, channels: 2, total: 44100}),
			formatFacts{CodecWavPack, 44100, 32, 2, false, SampleFormatFloat, 1}},
		{"a rate the table lacks, in ID_SAMPLE_RATE", wvFile(wvSpec{rate: 352800, bytes: 3, channels: 2, total: 352800, customRate: true}),
			formatFacts{CodecWavPack, 352800, 24, 2, false, SampleFormatInteger, 1}},
		{"an odd rate in ID_SAMPLE_RATE", wvFile(wvSpec{rate: 37800, bytes: 2, channels: 2, total: 37800, customRate: true}),
			formatFacts{CodecWavPack, 37800, 16, 2, false, SampleFormatInteger, 1}},
		{"ID_SAMPLE_RATE beside a table index", wvFileWithSampleRateSub(wvSpec{rate: 44100, bytes: 2, channels: 2, total: 88200}, 88200),
			formatFacts{CodecWavPack, 88200, 16, 2, false, SampleFormatInteger, 88200.0 / 88200}},
		{"six channels in ID_CHANNEL_INFO", wvFile(wvSpec{rate: 48000, bytes: 3, channels: 6, total: 48000}),
			formatFacts{CodecWavPack, 48000, 24, 6, false, SampleFormatInteger, 1}},
		{"DSD64", wvFile(wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 3, channels: 2, total: 352800 * 2}),
			formatFacts{CodecWavPackDSD, 2822400, 1, 2, true, "", 2}},
		{"DSD128 of the 48 kHz family", wvFile(wvSpec{rate: 48000, bytes: 1, dsd: true, dsdPower: 4, channels: 2, total: 768000}),
			formatFacts{CodecWavPackDSD, 6144000, 1, 2, true, "", 1}},
		{"a length the encoder could not write", wvFile(wvSpec{rate: 44100, bytes: 2, channels: 2, total: -1}),
			formatFacts{CodecWavPack, 44100, 16, 2, false, SampleFormatInteger, -1}},
		{"a 40-bit total", wvFile(wvSpec{rate: 192000, bytes: 3, channels: 2, total: 1<<32 + 192000}),
			formatFacts{CodecWavPack, 192000, 24, 2, false, SampleFormatInteger, float64(1<<32+192000) / 192000}},
		{"the total SET_TOTAL_SAMPLES moves past 0xFFFFFFFF", wvFile(wvSpec{rate: 192000, bytes: 3, channels: 2, total: math.MaxUint32}),
			formatFacts{CodecWavPack, 192000, 24, 2, false, SampleFormatInteger, float64(math.MaxUint32) / 192000}},
		{"junk before the first block", append(bytes.Repeat([]byte("junk "), 600), wvFile(stdWavPack())...),
			formatFacts{CodecWavPack, 44100, 16, 2, false, SampleFormatInteger, 1}},
		{"a leading block with no samples", append(leadingWVBlock(stdWavPack()), wvFile(stdWavPack())...),
			formatFacts{CodecWavPack, 44100, 16, 2, false, SampleFormatInteger, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := extractWVAPE(t, "a.wv", c.data, nil)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := factsOf(tr); got != c.want {
				t.Errorf("facts\n %+v\nwant\n %+v", got, c.want)
			}
		})
	}
}

// wvFileWithSampleRateSub is a WavPack file of spec whose first block also
// carries ID_SAMPLE_RATE with rate though its header names a table rate: the
// reference decoder takes the sub-block's.
func wvFileWithSampleRateSub(spec wvSpec, rate uint32) []byte {
	b := wvFirstBlock(spec, false)
	b.subs = append([][]byte{wvSub(fxIDSampleRate, []byte{byte(rate), byte(rate >> 8), byte(rate >> 16)})}, b.subs...)
	return b.bytes()
}

// leadingWVBlock is a block with no samples that holds a RIFF header alone,
// as an encoder may write ahead of the audio.
func leadingWVBlock(spec wvSpec) []byte {
	b := wvFirstBlock(spec, false)
	b.blockSamples = 0
	b.subs = [][]byte{wvSub(fxIDRIFFHeader, []byte("RIFF\x00\x00\x00\x00WAVEfmt "))}
	return b.bytes()
}

// withoutWVSub is spec's first block without the sub-blocks of function id,
// laid out as a file.
func withoutWVSub(spec wvSpec, id byte) []byte {
	b := wvFirstBlock(spec, false)
	var kept [][]byte
	for _, s := range b.subs {
		if s[0]&0x3f != id {
			kept = append(kept, s)
		}
	}
	b.subs = kept
	return b.bytes()
}

// withWVFlags is spec's first block with flags set in its header.
func withWVFlags(spec wvSpec, flags uint32) []byte {
	b := wvFirstBlock(spec, false)
	b.flags |= flags
	return b.bytes()
}

// TestAWavPackFileItsReaderRefusesIsRefused pins the refusals: each is an
// answer about what the file holds (the scanner indexes the file by its name,
// backlog B145), never a read that did not complete, and none stamps a codec,
// so no such row is reported as lossless WavPack without a header read.
func TestAWavPackFileItsReaderRefusesIsRefused(t *testing.T) {
	std := stdWavPack()
	zeroRate := wvSpec{rate: 0, bytes: 2, channels: 2, total: 44100, customRate: true}
	cases := []struct {
		name string
		data []byte
	}{
		{"no block at all", bytes.Repeat([]byte("not a WavPack stream "), 64)},
		{"a stream version the decoder does not read", func() []byte {
			b := wvFirstBlock(std, false)
			b.version = 0x401
			return b.bytes()
		}()},
		{"a block of more samples than a decoder takes", func() []byte {
			b := wvFirstBlock(std, false)
			b.blockSamples = 3 << 16
			return b.bytes()
		}()},
		{"a sample rate of 0", wvFile(zeroRate)},
		{"a rate index of 15 without its sub-block", withoutWVSub(wvSpec{rate: 37800, bytes: 2, channels: 2, total: 37800, customRate: true}, fxIDSampleRate)},
		{"an audio block without its bitstream", withoutWVSub(std, fxIDWVBitstream)},
		{"a correction file named .wv", wvcFile(wvSpec{rate: 44100, bytes: 2, channels: 2, total: 44100, hybrid: true})},
		{"mono and false stereo at once", withWVFlags(wvSpec{rate: 44100, bytes: 2, channels: 1, total: 44100}, 0x40000000)},
		{"float samples in two bytes", wvFile(wvSpec{rate: 44100, bytes: 2, float: true, channels: 2, total: 44100})},
		{"every stored bit shifted out", wvFile(wvSpec{rate: 44100, bytes: 2, shift: 16, channels: 2, total: 44100})},
		{"DSD without its DSD block", withoutWVSub(wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 3, channels: 2, total: 352800}, fxIDDSDBlock)},
		{"a DSD multiplier past DSD1024", wvFile(wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 11, channels: 2, total: 352800})},
		{"hybrid DSD", wvFile(wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 3, hybrid: true, channels: 2, total: 352800})},
		{"a channel layout of no channels", func() []byte {
			b := wvFirstBlock(std, false)
			b.subs = append([][]byte{wvSub(fxIDChannelInfo, []byte{0, 0x3, 0})}, b.subs...)
			return b.bytes()
		}()},
		{"a channel mask past its count", func() []byte {
			b := wvFirstBlock(std, false)
			b.subs = append([][]byte{wvSub(fxIDChannelInfo, []byte{2, 0x7, 0})}, b.subs...)
			return b.bytes()
		}()},
		{"a sub-block running past its block", func() []byte {
			b := wvFirstBlock(std, false)
			last := append([]byte(nil), b.subs[len(b.subs)-1]...)
			last[1] = 0xff // 510 bytes, in a block that holds fewer
			b.subs[len(b.subs)-1] = last
			data := b.bytes()
			binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
			return data
		}()},
		{"a header cut short", wvFile(std)[:20]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := extractWVAPE(t, "a.wv", c.data, nil)
			if err == nil {
				t.Fatalf("extract took the file: %+v", factsOf(tr))
			}
			if fault := readFault(err); fault != nil {
				t.Fatalf("a refusal answered as a read that did not complete: %v", err)
			}
			if tr.Codec != "" || tr.BitsPerSample != nil || tr.SampleRate != nil {
				t.Errorf("a refused file carries format facts %+v; a refusal stamps none", factsOf(tr))
			}
		})
	}
}

// TestTheAPEHeaderStatesItsFormat reads each layout Monkey's Audio has
// written: the descriptor and header from 3.98 on (a longer descriptor or
// header than this reader knows included), the old header before, its bits
// from the flags and its blocks per frame from the version and the level as
// MACLib derives them, and a file after an ID3v2 tag and its padding.
func TestTheAPEHeaderStatesItsFormat(t *testing.T) {
	std := stdAPE()
	with := func(f func(*apeSpec)) []byte {
		s := stdAPE()
		f(&s)
		return apeFile(s)
	}
	stdLength := float64(9*73728+1000) / 44100
	old := func(version, level, flags uint16, frames, final uint32) apeSpec {
		return apeSpec{version: version, compression: level, formatFlags: flags, channels: 2, rate: 44100,
			totalFrames: frames, finalFrameBlocks: final}
	}
	cases := []struct {
		name string
		data []byte
		want formatFacts
	}{
		{"3.99, 16-bit stereo", apeFile(std), formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, stdLength}},
		{"24-bit", with(func(s *apeSpec) { s.bits = 24; s.formatFlags = fxAPEFlag24Bit }),
			formatFacts{CodecAPE, 44100, 24, 2, false, SampleFormatInteger, stdLength}},
		{"8-bit mono", with(func(s *apeSpec) { s.bits = 8; s.channels = 1; s.formatFlags = fxAPEFlag8Bit }),
			formatFacts{CodecAPE, 44100, 8, 1, false, SampleFormatInteger, stdLength}},
		{"32-bit, whose kind the header does not establish", with(func(s *apeSpec) { s.bits = 32 }),
			formatFacts{CodecAPE, 44100, 32, 2, false, "", stdLength}},
		{"96 kHz, Extra High", with(func(s *apeSpec) { s.rate = 96000; s.compression = fxAPEExtraHigh }),
			formatFacts{CodecAPE, 96000, 16, 2, false, SampleFormatInteger, float64(9*73728+1000) / 96000}},
		{"3.98 with a longer descriptor and header", with(func(s *apeSpec) { s.version = 3980; s.descriptorExtra = 16; s.headerExtra = 8 }),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, stdLength}},
		{"3.97, 24-bit by its flags", apeFile(old(3970, fxAPEHigh, fxAPEFlag24Bit, 3, 500)),
			formatFacts{CodecAPE, 44100, 24, 2, false, SampleFormatInteger, float64(2*73728*4+500) / 44100}},
		{"3.95, 8-bit by its flags", apeFile(old(3950, fxAPENormal, fxAPEFlag8Bit, 2, 1)),
			formatFacts{CodecAPE, 44100, 8, 2, false, SampleFormatInteger, float64(73728*4+1) / 44100}},
		{"3.90", apeFile(old(3900, fxAPENormal, 0, 5, 44100)),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, float64(4*73728+44100) / 44100}},
		{"3.80 at Extra High", apeFile(old(3800, fxAPEExtraHigh, fxAPEFlagPeakLevel|fxAPEFlagSeekElems, 5, 100)),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, float64(4*73728+100) / 44100}},
		{"3.80 at Normal", apeFile(old(3800, fxAPENormal, 0, 50, 9216)),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, float64(50*9216) / 44100}},
		{"after an ID3v2 tag and its padding", with(func(s *apeSpec) { s.junk = id3v2Junk(200, 64) }),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, stdLength}},
		{"after an ID3v2 tag with a footer", with(func(s *apeSpec) { s.junk = id3v2JunkWithFooter(100) }),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, stdLength}},
		{"after junk MACLib scans past", with(func(s *apeSpec) { s.junk = bytes.Repeat([]byte{0x11}, 5000) }),
			formatFacts{CodecAPE, 44100, 16, 2, false, SampleFormatInteger, stdLength}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := extractWVAPE(t, "a.ape", c.data, nil)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := factsOf(tr); got != c.want {
				t.Errorf("facts\n %+v\nwant\n %+v", got, c.want)
			}
		})
	}
}

// id3v2Junk is an ID3v2.4 tag of size bytes (no footer) and pad NULs after
// it, as some taggers leave in front of a Monkey's Audio file.
func id3v2Junk(size, pad int) []byte {
	h := []byte{'I', 'D', '3', 4, 0, 0,
		byte(size>>21) & 0x7f, byte(size>>14) & 0x7f, byte(size>>7) & 0x7f, byte(size) & 0x7f}
	h = append(h, bytes.Repeat([]byte{0x22}, size)...)
	return append(h, make([]byte, pad)...)
}

// id3v2JunkWithFooter is an ID3v2.4 tag of size bytes with its footer.
func id3v2JunkWithFooter(size int) []byte {
	h := []byte{'I', 'D', '3', 4, 0, 0x10,
		byte(size>>21) & 0x7f, byte(size>>14) & 0x7f, byte(size>>7) & 0x7f, byte(size) & 0x7f}
	h = append(h, bytes.Repeat([]byte{0x22}, size)...)
	return append(h, '3', 'D', 'I', 4, 0, 0x10, h[6], h[7], h[8], h[9])
}

// TestAnAPEFileItsReaderRefusesIsRefused pins the refusals, each before any
// division by what it refuses: a version outside 3.80 to 3.99, no frames, a
// rate of 0, no channels, a depth or level Monkey's Audio does not write,
// frame counts that do not fit, a descriptor shorter than its own definition,
// a file cut inside its header, and a file with no "MAC " where MACLib looks.
func TestAnAPEFileItsReaderRefusesIsRefused(t *testing.T) {
	with := func(f func(*apeSpec)) []byte {
		s := stdAPE()
		f(&s)
		return apeFile(s)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"version 3.79", with(func(s *apeSpec) { s.version = 3790 })},
		{"version 4.00", with(func(s *apeSpec) { s.version = 4000 })},
		{"no frames", with(func(s *apeSpec) { s.totalFrames = 0 })},
		{"a sample rate of 0", with(func(s *apeSpec) { s.rate = 0 })},
		{"no channels", with(func(s *apeSpec) { s.channels = 0 })},
		{"33 channels", with(func(s *apeSpec) { s.channels = 33 })},
		{"12 bits", with(func(s *apeSpec) { s.bits = 12 })},
		{"compression level 2500", with(func(s *apeSpec) { s.compression = 2500 })},
		{"compression level 6000", with(func(s *apeSpec) { s.compression = 6000 })},
		{"no blocks per frame", with(func(s *apeSpec) { s.blocksPerFrame = 0 })},
		{"a final frame of no blocks", with(func(s *apeSpec) { s.finalFrameBlocks = 0 })},
		{"a final frame longer than a frame", with(func(s *apeSpec) { s.finalFrameBlocks = s.blocksPerFrame + 1 })},
		{"a descriptor shorter than its definition", func() []byte {
			data := apeFile(stdAPE())
			binary.LittleEndian.PutUint32(data[8:12], 20)
			return data
		}()},
		{"a descriptor claiming 4 GiB", func() []byte {
			data := apeFile(stdAPE())
			binary.LittleEndian.PutUint32(data[8:12], math.MaxUint32)
			return data
		}()},
		{"a header shorter than its definition", func() []byte {
			data := apeFile(stdAPE())
			binary.LittleEndian.PutUint32(data[12:16], 8)
			return data
		}()},
		{"a file cut inside its header", apeFile(stdAPE())[:60]},
		{"no \"MAC \" at all", bytes.Repeat([]byte("not a Monkey's Audio file "), 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := extractWVAPE(t, "a.ape", c.data, nil)
			if err == nil {
				t.Fatalf("extract took the file: %+v", factsOf(tr))
			}
			if fault := readFault(err); fault != nil {
				t.Fatalf("a refusal answered as a read that did not complete: %v", err)
			}
			if tr.Codec != "" || tr.BitsPerSample != nil || tr.SampleRate != nil || tr.Duration != nil {
				t.Errorf("a refused file carries format facts %+v; a refusal stamps none", factsOf(tr))
			}
		})
	}
}

// TestTheAPEv2TagNamesEveryFieldItHolds maps each APEv2 key a tagger writes
// onto the Track as the Vorbis comment reader maps its names: Title, Artist
// (its NUL-separated values joined), Album Artist, Album, Track and Disc
// ("n/m"), Year (a date's year), Genre, Composer, Conductor, Work, the
// compilation flag, the original year, BPM, the MusicBrainz ids, ReplayGain
// and the explicit advisory, keys in any case.
func TestTheAPEv2TagNamesEveryFieldItHolds(t *testing.T) {
	data := append(wvFile(stdWavPack()), apeTag([]apeItem{
		apeText("TITLE", "Song"),
		apeText("Artist", "First\x00Second"),
		apeText("Album Artist", "The Band"),
		apeText("album", "The Album"),
		apeText("Track", "3/12"),
		apeText("Disc", "1/2"),
		apeText("Year", "2019-03-01"),
		apeText("Genre", "Rock"),
		apeText("Composer", "Writer"),
		apeText("Conductor", "Leader"),
		apeText("WORK", "The Work"),
		apeText("Compilation", "1"),
		apeText("ORIGINALYEAR", "1999"),
		apeText("BPM", "120"),
		apeText("MUSICBRAINZ_TRACKID", "11111111-2222-3333-4444-555555555555"),
		apeText("MUSICBRAINZ_ALBUMID", "66666666-7777-8888-9999-000000000000"),
		apeText("REPLAYGAIN_TRACK_GAIN", "-6.50 dB"),
		apeText("replaygain_album_gain", "-7.25 dB"),
		apeText("ITUNESADVISORY", "1"),
	}, true)...)
	tr, err := extractWVAPE(t, "a.wv", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	strs := []struct{ field, got, want string }{
		{"Title", tr.Title, "Song"},
		{"Artist", tr.Artist, "First; Second"},
		{"AlbumArtist", tr.AlbumArtist, "The Band"},
		{"Album", tr.Album, "The Album"},
		{"Genre", tr.Genre, "Rock"},
		{"Composer", tr.Composer, "Writer"},
		{"Conductor", tr.Conductor, "Leader"},
		{"Work", tr.Work, "The Work"},
		{"MusicBrainzTrackID", tr.MusicBrainzTrackID, "11111111-2222-3333-4444-555555555555"},
		{"MusicBrainzAlbumID", tr.MusicBrainzAlbumID, "66666666-7777-8888-9999-000000000000"},
	}
	for _, s := range strs {
		if s.got != s.want {
			t.Errorf("%s = %q, want %q", s.field, s.got, s.want)
		}
	}
	ints := []struct {
		field string
		got   *int
		want  int
	}{
		{"TrackNumber", tr.TrackNumber, 3}, {"DiscNumber", tr.DiscNumber, 1}, {"Year", tr.Year, 2019},
		{"OriginalYear", tr.OriginalYear, 1999}, {"BPM", tr.BPM, 120},
	}
	for _, n := range ints {
		if n.got == nil || *n.got != n.want {
			t.Errorf("%s = %v, want %d", n.field, n.got, n.want)
		}
	}
	if tr.ReplayGainTrackDB == nil || *tr.ReplayGainTrackDB != -6.5 || tr.ReplayGainAlbumDB == nil || *tr.ReplayGainAlbumDB != -7.25 {
		t.Errorf("ReplayGain %v / %v, want -6.5 / -7.25", tr.ReplayGainTrackDB, tr.ReplayGainAlbumDB)
	}
	if !tr.Compilation || !tr.Explicit {
		t.Errorf("Compilation %v, Explicit %v; want both", tr.Compilation, tr.Explicit)
	}
}

// TestTheAPEv2TagJoinsWhatItRepeats pins the multi-value rule: a key the tag
// repeats is joined with "; " in the tag's order, as a FLAC's repeated
// comments are; a value that is empty sets nothing, so the path's guess
// stays; a key holding no value still makes its number present (0); and
// text that is UTF-8 is kept as it is.
func TestTheAPEv2TagJoinsWhatItRepeats(t *testing.T) {
	data := append(apeFile(stdAPE()), apeTag([]apeItem{
		apeText("Artist", "Björk"),
		apeText("ARTIST", "東京事変"),
		apeText("Genre", "Pop"),
		apeText("Genre", "Electronic\x00\x00Art Pop"),
		apeText("Title", "  "),
		apeText("Track", ""),
	}, false)...)
	tr, err := extractWVAPE(t, "a.ape", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Artist != "Björk; 東京事変" {
		t.Errorf("Artist = %q, want the two values joined in the tag's order", tr.Artist)
	}
	if tr.Genre != "Pop; Electronic; Art Pop" {
		t.Errorf("Genre = %q, want every value joined, the empty one dropped", tr.Genre)
	}
	if tr.Title != "Path Title" {
		t.Errorf("Title = %q, want the path's guess kept: an empty value sets nothing", tr.Title)
	}
	if tr.TrackNumber == nil || *tr.TrackNumber != 0 {
		t.Errorf("TrackNumber = %v, want a present 0", tr.TrackNumber)
	}
	if tr.Year != nil || tr.DiscNumber != nil {
		t.Errorf("Year %v, DiscNumber %v; want both absent: the tag holds neither", tr.Year, tr.DiscNumber)
	}
}

// TestTheAPEv2TagIsFoundWhereTaggersPutIt reads the tag with a header or
// without, and before an ID3v1 tag; an APEv1 tag is not read (its text is not
// defined as UTF-8), and a file with no APEv2 tag reads its ID3v1 tag through
// dhowden; an item that is an external locator is passed over.
func TestTheAPEv2TagIsFoundWhereTaggersPutIt(t *testing.T) {
	title := []apeItem{apeText("Title", "From APEv2")}
	apev1 := func() []byte {
		tag := apeTag(title, false)
		binary.LittleEndian.PutUint32(tag[len(tag)-32+8:], 1000)
		return tag
	}
	cases := []struct {
		name, want string
		tail       []byte
	}{
		{"a tag with its header", "From APEv2", apeTag(title, true)},
		{"a footer alone", "From APEv2", apeTag(title, false)},
		{"a tag before an ID3v1 tag", "From APEv2", append(apeTag(title, true), id3v1Tag("From ID3v1", "X")...)},
		{"an ID3v1 tag alone", "From ID3v1", id3v1Tag("From ID3v1", "X")},
		{"an APEv1 tag", "Path Title", apev1()},
		{"an APEv1 tag before an ID3v1 tag", "From ID3v1", append(apev1(), id3v1Tag("From ID3v1", "X")...)},
		{"an external locator", "Path Title", apeTag([]apeItem{{key: "Title", value: []byte("http://example.invalid/t"), flags: 4}}, true)},
		{"no tag", "Path Title", nil},
	}
	for _, c := range cases {
		for _, f := range []struct{ name, ext string }{{"wv", ".wv"}, {"ape", ".ape"}} {
			t.Run(c.name+"/"+f.name, func(t *testing.T) {
				audio := wvFile(stdWavPack())
				if f.ext == ".ape" {
					audio = apeFile(stdAPE())
				}
				tr, err := extractWVAPE(t, "a"+f.ext, append(audio, c.tail...), nil)
				if err != nil {
					t.Fatal(err)
				}
				if tr.Title != c.want {
					t.Errorf("Title = %q, want %q", tr.Title, c.want)
				}
			})
		}
	}
}

// TestTheAPEv2FrontCoverIsTheTracksArtwork pins the cover: the binary front
// cover item is the track's local artwork, ahead of the folder's, and the
// folder's cover stands where the tag holds none.
func TestTheAPEv2FrontCoverIsTheTracksArtwork(t *testing.T) {
	folderCover := append(append([]byte{}, minimalJPEG...), []byte("folder")...)
	embedded := append(append([]byte{}, minimalJPEG...), []byte("embedded")...)
	front := apeItem{key: "Cover Art (Front)", value: append([]byte("cover.jpg\x00"), embedded...), flags: 2}
	cases := []struct {
		name  string
		items []apeItem
		want  []byte
	}{
		{"the tag's front cover", []apeItem{apeText("Title", "T"), front}, embedded},
		{"the folder's cover where the tag holds none", []apeItem{apeText("Title", "T")}, folderCover},
		{"the folder's cover where the file holds no tag", nil, folderCover},
	}
	for _, c := range cases {
		for _, ext := range []string{".wv", ".ape"} {
			t.Run(c.name+"/"+ext, func(t *testing.T) {
				dir := t.TempDir()
				p := filepath.Join(dir, "a"+ext)
				if ext == ".wv" {
					writeWavPack(t, p, stdWavPack(), c.items)
				} else {
					writeAPE(t, p, stdAPE(), c.items)
				}
				if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), folderCover, 0o644); err != nil {
					t.Fatal(err)
				}
				tr := &Track{Path: "a" + ext}
				ec := &ExtractContext{ArtworkCacheDir: t.TempDir(), FolderArtCache: &sync.Map{}}
				if err := ExtractWithContext(p, tr, ec); err != nil {
					t.Fatal(err)
				}
				if want := expectedLocalMBID(c.want); tr.ArtworkMBID != want {
					t.Errorf("ArtworkMBID = %q, want %q", tr.ArtworkMBID, want)
				}
			})
		}
	}
}

// TestAnAPEv2LyricsItemIsALyricsDocument pins the lyrics: an item named as
// the FLAC comment walk names lyrics is a candidate, timed text a synced one.
func TestAnAPEv2LyricsItemIsALyricsDocument(t *testing.T) {
	body := "[00:01.00]Line one\n[00:05.00]Line two\n[00:09.00]Line three"
	data := append(wvFile(stdWavPack()), apeTag([]apeItem{apeText("LYRICS", body)}, true)...)
	tr, err := extractWVAPE(t, "a.wv", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.lyrics == nil || !tr.lyrics.Synced || !strings.Contains(tr.lyrics.Body, "Line two") {
		t.Fatalf("lyrics %+v, want the tag's synced document", tr.lyrics)
	}
}
