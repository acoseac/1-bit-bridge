package manifest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Backlog B513: the manifest says whether a PCM track's samples are integers
// or floating point, as the file's own header states it, and says nothing
// where the header states neither. These tests read the shapes through the
// extractor, through the header read the backfill makes, through the store
// and through the manifest a client is served.

// ksSubtypePCM and ksSubtypeFloat are KSDATAFORMAT_SUBTYPE_PCM and
// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT as a fmt chunk holds them (Data1, Data2 and
// Data3 little-endian). They are written out here apart from the extractor's
// copy; the captured files (ffmpeg's s24.wav, afconvert's af_i16.wav,
// af_i32.wav and af_f64.wav) pin both against what the writers put there.
var (
	ksSubtypePCM   = [16]byte{0x01, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}
	ksSubtypeFloat = [16]byte{0x03, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}
	// ambisonicPCM is KSDATAFORMAT_SUBTYPE_AMBISONIC_B_FORMAT_PCM: its first
	// two bytes are PCM's format code, and it is not the PCM subformat.
	ambisonicPCM = [16]byte{0x01, 0, 0, 0, 0x21, 0x07, 0xD3, 0x11, 0x86, 0x44, 0xC8, 0xC1, 0xCA, 0, 0, 0}
)

// extensibleFmt is a 40-byte WAVE_FORMAT_EXTENSIBLE fmt chunk, stereo at
// 96 kHz with bits a sample, whose subformat is guid.
func extensibleFmt(bits uint16, guid [16]byte) []byte {
	payload := make([]byte, 40)
	binary.LittleEndian.PutUint16(payload[0:2], wavFormatExtensible)
	binary.LittleEndian.PutUint16(payload[2:4], 2)
	binary.LittleEndian.PutUint32(payload[4:8], 96000)
	binary.LittleEndian.PutUint32(payload[8:12], 96000*2*uint32(bits)/8)
	binary.LittleEndian.PutUint16(payload[12:14], 2*bits/8)
	binary.LittleEndian.PutUint16(payload[14:16], bits)
	binary.LittleEndian.PutUint16(payload[16:18], 22)
	binary.LittleEndian.PutUint16(payload[18:20], bits)
	binary.LittleEndian.PutUint32(payload[20:24], 0x3)
	copy(payload[24:40], guid[:])
	return wrapChunkLE("fmt ", payload)
}

// shortExtensibleFmt is an extensible fmt chunk of 26 bytes: the first two
// bytes of its subformat are PCM's code, and the GUID they begin is cut off.
func shortExtensibleFmt() []byte {
	payload := make([]byte, 26)
	binary.LittleEndian.PutUint16(payload[0:2], wavFormatExtensible)
	binary.LittleEndian.PutUint16(payload[2:4], 2)
	binary.LittleEndian.PutUint32(payload[4:8], 44100)
	binary.LittleEndian.PutUint32(payload[8:12], 44100*4)
	binary.LittleEndian.PutUint16(payload[12:14], 4)
	binary.LittleEndian.PutUint16(payload[14:16], 16)
	binary.LittleEndian.PutUint16(payload[16:18], 8)
	binary.LittleEndian.PutUint16(payload[24:26], wavFormatPCM)
	return wrapChunkLE("fmt ", payload)
}

// aifcCOMM is an AIFF-C COMM chunk, stereo at 44.1 kHz with bits a sample,
// naming compression, with an empty name after it.
func aifcCOMM(bits int16, compression string) []byte {
	return buildAIFFCOMMChunk(2, 4410, bits, 44100, append([]byte(compression), 0, 0)...)
}

// aiffWithChunks is a FORM/AIFF file holding chunks, in order.
func aiffWithChunks(chunks ...[]byte) []byte {
	body := []byte("AIFF")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := append([]byte("FORM"), make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[4:8], uint32(len(body)))
	return append(out, body...)
}

// flacBytes is a FLAC file of rate and bits, written by mewkiz/flac.
func flacBytes(t *testing.T, rate, bits int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.flac")
	writeMinimalFLAC(t, path, rate, bits, nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mp3Bytes is an MP3 file: an ID3v2.3 tag and one MPEG-1 layer III frame.
func mp3Bytes(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.mp3")
	writeMinimalMP3(t, path, nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sampleFormatCase is one file and what its row carries: the sample kind
// (want, "" for absent), and the codec and depth beside it (bits -1: not
// checked), which the kind must not change.
type sampleFormatCase struct {
	name  string
	file  string // the name it is written under: its extension picks the extractor
	data  []byte
	want  string
	codec string
	bits  int
}

// sampleFormatCases is every shape B513 names. A file a real writer made is
// read from testdata (compressedPCMFixture: compressed_pcm_fixtures.sh,
// sample_format_fixtures.sh, ogg_flac_fixtures.sh, the m4a and dff
// fixtures); the rest are built here.
func sampleFormatCases(t *testing.T) []sampleFormatCase {
	t.Helper()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	wav := func(chunks ...[]byte) []byte { return buildWAVWithID3(t, nil, chunks...) }
	const (
		integer = SampleFormatInteger
		float   = SampleFormatFloat
	)
	return []sampleFormatCase{
		// WAV, integer: format tag 1, and the extensible PCM subformat.
		{"WAV PCM 16, format tag 1", "pcm16.wav", wav(buildWAVFmtChunk(1, 2, 44100, 16), buildWAVDataChunk(4)), integer, "WAV", 16},
		{"WAV PCM 16, extensible (afconvert)", "af_i16.wav", fx("wav/af_i16.wav"), integer, "WAV", 16},
		{"WAV PCM 24, extensible (ffmpeg)", "s24.wav", fx("wav/s24.wav"), integer, "WAV", 24},
		{"WAV PCM 32, extensible (afconvert)", "af_i32.wav", fx("wav/af_i32.wav"), integer, "WAV", 32},
		{"WAV PCM 32, format tag 1", "pcm32.wav", wav(buildWAVFmtChunk(1, 2, 44100, 32), buildWAVDataChunk(8)), integer, "WAV", 32},
		// WAV, float: format tag 3, and the extensible IEEE float subformat.
		{"WAV float 32, format tag 3 (ffmpeg)", "float.wav", fx("wav/float.wav"), float, "WAV", 32},
		{"WAV float 64, format tag 3", "f64.wav", wav(buildWAVFmtChunk(3, 2, 96000, 64), buildWAVDataChunk(16)), float, "WAV", 64},
		{"WAV float 64, extensible (afconvert)", "af_f64.wav", fx("wav/af_f64.wav"), float, "WAV", 64},
		{"WAV float 32, extensible", "xf32.wav", wav(extensibleFmt(32, ksSubtypeFloat), buildWAVDataChunk(8)), float, "WAV", 32},
		// The format chunk after others, which the reads step over: a
		// LIST/INFO, and a JUNK chunk of odd size, with its pad byte.
		{"WAV whose fmt chunk follows a LIST and a JUNK chunk", "late.wav",
			wav(listInfo("INAM", "Late"), riffChunk("JUNK", make([]byte, 1001)), buildWAVFmtChunk(3, 2, 48000, 32), buildWAVDataChunk(8)),
			float, "WAV", 32},
		// The first fmt chunk too short to read is skipped, as the walk
		// skips it, and the next one names the file.
		{"WAV whose first fmt chunk is too short to read", "short_first.wav",
			wav(riffChunk("fmt ", make([]byte, 12)), buildWAVFmtChunk(3, 2, 48000, 32), buildWAVDataChunk(8)),
			float, "WAV", 32},
		// WAV, absent: an extensible subformat that is neither GUID, an
		// extensible header that holds no GUID, the compressed codes, a code
		// the bridge does not know, and headers it cannot read.
		{"WAV extensible, Ambisonic B-format PCM subformat", "amb.wav", wav(extensibleFmt(24, ambisonicPCM), buildWAVDataChunk(6)), "", "WAV", 24},
		{"WAV extensible cut before its GUID", "short_ext.wav", wav(shortExtensibleFmt(), buildWAVDataChunk(4)), "", "WAV", 16},
		{"WAV extensible, IMA ADPCM subformat (ffmpeg)", "ima_adpcm_96k.wav", fx("wav/ima_adpcm_96k.wav"), "", "ADPCM", 0},
		{"WAV A-law (ffmpeg)", "alaw.wav", fx("wav/alaw.wav"), "", "ALAW", 0},
		{"WAV mu-law (ffmpeg)", "mulaw.wav", fx("wav/mulaw.wav"), "", "ULAW", 0},
		{"WAV IMA ADPCM (ffmpeg)", "ima_adpcm.wav", fx("wav/ima_adpcm.wav"), "", "ADPCM", 0},
		{"WAV MS ADPCM (ffmpeg)", "ms_adpcm.wav", fx("wav/ms_adpcm.wav"), "", "ADPCM", 0},
		{"WAV GSM 6.10 (sox)", "gsm.wav", fx("wav/gsm.wav"), "", "GSM", 0},
		{"WAV MPEG audio (ffmpeg)", "mp2.wav", fx("wav/mp2.wav"), "", "MP2", 0},
		{"WAV of a code the bridge does not know, G.726 (ffmpeg)", "g726.wav", fx("wav/g726.wav"), "", "WAV", 0},
		{"WAV cut inside its fmt chunk", "cut.wav", fx("wav/float.wav")[:30], "", "WAV", 0},
		{"WAV with no fmt chunk", "nofmt.wav", wav(buildWAVDataChunk(4)), "", "WAV", 0},
		{"a .wav that is not RIFF", "junk.wav", []byte("this is not a RIFF file at all, only text"), "", "WAV", 0},
		// AIFF: a plain AIFF is integer.
		{"AIFF 16 (afconvert)", "af_be16.aiff", fx("aifc/af_be16.aiff"), integer, "AIFF", 16},
		{"AIFF 24", "be24.aiff", aiffWithChunks(buildAIFFCOMMChunk(2, 4410, 24, 44100)), integer, "AIFF", 24},
		{"AIFF whose COMM follows an ID3 chunk", "id3_first.aiff",
			aiffWithChunks(wrapChunkBE("ID3 ", buildID3v2_3(map[string]string{"title": "First"})), buildAIFFCOMMChunk(2, 4410, 16, 44100)),
			integer, "AIFF", 16},
		{"AIFF cut inside its COMM", "cut.aiff", fx("aifc/af_be16.aiff")[:26], "", "AIFF", 0},
		// AIFF-C by its compression type.
		{"AIFF-C twos (afconvert)", "af_twos.aifc", fx("aifc/af_twos.aifc"), integer, "AIFF", 16},
		{"AIFF-C sowt (ffmpeg)", "ff_sowt.aifc", fx("aifc/ff_sowt.aifc"), integer, "AIFF", 16},
		{"AIFF-C in24 (afconvert)", "af_in24.aifc", fx("aifc/af_in24.aifc"), integer, "AIFF", 24},
		{"AIFF-C NONE", "none.aifc", aifcWithChunks(aifcCOMM(16, "NONE")), integer, "AIFF", 16},
		{"AIFF-C in32", "in32.aifc", aifcWithChunks(aifcCOMM(32, "in32")), integer, "AIFF", 32},
		{"AIFF-C 42ni", "42ni.aifc", aifcWithChunks(aifcCOMM(24, "42ni")), integer, "AIFF", 24},
		{"AIFF-C 23ni", "23ni.aifc", aifcWithChunks(aifcCOMM(32, "23ni")), integer, "AIFF", 32},
		{"AIFF-C raw", "raw.aifc", aifcWithChunks(aifcCOMM(8, "raw ")), integer, "AIFF", 8},
		{"AIFF-C fl32 (afconvert)", "af_fl32.aifc", fx("aifc/af_fl32.aifc"), float, "AIFF", 32},
		{"AIFF-C fl64 (afconvert)", "af_fl64.aifc", fx("aifc/af_fl64.aifc"), float, "AIFF", 64},
		{"AIFF-C FL32", "FL32.aifc", aifcWithChunks(aifcCOMM(32, "FL32")), float, "AIFF", 32},
		{"AIFF-C FL64", "FL64.aifc", aifcWithChunks(aifcCOMM(64, "FL64")), float, "AIFF", 64},
		{"AIFF-C ulaw (afconvert)", "af_ulaw.aifc", fx("aifc/af_ulaw.aifc"), "", "ULAW", 0},
		{"AIFF-C alaw (afconvert)", "af_alaw.aifc", fx("aifc/af_alaw.aifc"), "", "ALAW", 0},
		{"AIFF-C ima4 (afconvert)", "af_ima4.aifc", fx("aifc/af_ima4.aifc"), "", "IMA4", 0},
		{"AIFF-C of a compression the bridge does not know", "mac3.aifc", aifcWithChunks(aifcCOMM(16, "MAC3")), "", "AIFC", 0},
		{"AIFF-C whose COMM names no compression", "bare.aifc", aifcWithChunks(buildAIFFCOMMChunk(2, 4410, 16, 44100)), "", "AIFC", 0},
		// FLAC and ALAC are integer-only.
		{"FLAC (mewkiz)", "x.flac", flacBytes(t, 96000, 24), integer, "FLAC", 24},
		{"Ogg FLAC (libFLAC)", "libflac_picture.oga", fx("ogg/libflac_picture.oga"), integer, "FLAC", -1},
		{"ALAC (afconvert)", "itunes_alac.m4a", fx("m4a/itunes_alac.m4a"), integer, "ALAC", -1},
		// Lossy codecs and DSD state no PCM kind.
		{"AAC (iTunes)", "itunes_aac_pop.m4a", fx("m4a/itunes_aac_pop.m4a"), "", "AAC", 0},
		{"MP3", "x.mp3", mp3Bytes(t), "", "MP3", 0},
		{"Ogg Vorbis", "vorbis.ogg", vorbisFile(2, 44100, 44100), "", "OGG", 0},
		{"Opus", "opus.ogg", opusFile(2, 312, 48000), "", "OGG", 0},
		{"DSF", "x.dsf", minimalDSFBytes(2822400, buildID3v2_3(nil)), "", "DSF", 1},
		{"DSF of a PCM rate (not DSD)", "pcm_rate.dsf", minimalDSFBytes(44100, buildID3v2_3(nil)), "", "DSF", 1},
		{"DFF (TagLib)", "taglib_diin.dff", fx("dff/taglib_diin.dff"), "", "DFF", 1},
		{"DFF, DST", "dst.dff", buildDFF(t, 2822400, "DST "), "", "DFF", 1},
	}
}

// headerReadExt reports whether ext is one a header read answers for: the
// WAV and AIFF families, the formats whose kind only the header tells.
func headerReadExt(ext string) bool {
	switch ext {
	case ".wav", ".aif", ".aiff", ".aifc":
		return true
	}
	return false
}

// TestSampleFormatIsWhatTheHeaderStates reads every shape through the
// extractor (ExtractWithContext, what a scan runs): the kind each header
// states, and absent wherever it states none. The codec and the depth beside
// it are what they were before the field existed: a float WAV stays "WAV"
// with its 32 or 64 bits.
func TestSampleFormatIsWhatTheHeaderStates(t *testing.T) {
	for _, tc := range sampleFormatCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			writeFixtureBytes(t, path, tc.data)
			var tr Track
			if err := ExtractWithContext(path, &tr, nil); err != nil && readFault(err) != nil {
				t.Fatalf("extract: %v", err)
			}
			if tr.SampleFormat != tc.want {
				t.Errorf("SampleFormat = %q, want %q", tr.SampleFormat, tc.want)
			}
			if tr.Codec != tc.codec {
				t.Errorf("Codec = %q, want %q", tr.Codec, tc.codec)
			}
			if tc.bits >= 0 {
				var bits int
				if tr.BitsPerSample != nil {
					bits = *tr.BitsPerSample
				}
				if bits != tc.bits {
					t.Errorf("BitsPerSample = %d, want %d (0 = none)", bits, tc.bits)
				}
			}
			if tr.IsDSD != nil && *tr.IsDSD && tr.SampleFormat != "" {
				t.Errorf("a DSD row carries the PCM kind %q", tr.SampleFormat)
			}
		})
	}
}

// TestTheHeaderReadAgreesWithTheExtractor reads every WAV and AIFF shape
// through the header read the backfill makes (readHeaderSampleFormat), which
// must answer what the extractor answers: a row filled by the backfill and a
// row the scanner writes from the same file carry the same kind.
func TestTheHeaderReadAgreesWithTheExtractor(t *testing.T) {
	for _, tc := range sampleFormatCases(t) {
		ext := strings.ToLower(filepath.Ext(tc.file))
		if !headerReadExt(ext) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			writeFixtureBytes(t, path, tc.data)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := readHeaderSampleFormat(f, ext)
			if err != nil {
				t.Fatalf("header read: %v", err)
			}
			if got != tc.want {
				t.Errorf("the header read says %q, want %q, the extractor's answer (TestSampleFormatIsWhatTheHeaderStates)", got, tc.want)
			}
		})
	}
}

// countingSource is a file that counts the bytes read from it.
type countingSource struct {
	*os.File
	read int64
}

func (c *countingSource) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.File.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

// TestTheHeaderReadStopsAtTheFormatChunk: the backfill reads a header, never
// the tags or the audio. A WAV holds a 1 MiB LIST and a 1 MiB JUNK chunk
// ahead of its fmt chunk and 1 MiB of audio after it; an AIFF-C holds a
// 1 MiB ID3 chunk ahead of its COMM and 1 MiB of SSND after it. Each answer
// costs a few hundred bytes.
func TestTheHeaderReadStopsAtTheFormatChunk(t *testing.T) {
	const big = 1 << 20
	info := append([]byte("INFO"), make([]byte, big)...)
	cases := []struct {
		name, ext string
		data      []byte
		want      string
	}{
		{"WAV", ".wav", buildWAVWithID3(t, nil, riffChunk("LIST", info), riffChunk("JUNK", make([]byte, big)),
			buildWAVFmtChunk(3, 2, 48000, 32), riffChunk("data", make([]byte, big))), SampleFormatFloat},
		{"AIFF-C", ".aifc", aifcWithChunks(wrapChunkBE("ID3 ", make([]byte, big)), aifcCOMM(32, "fl32"),
			wrapChunkBE("SSND", make([]byte, big))), SampleFormatFloat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x"+tc.ext)
			writeFixtureBytes(t, path, tc.data)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			src := &countingSource{File: f}
			got, err := readHeaderSampleFormat(src, tc.ext)
			if err != nil || got != tc.want {
				t.Fatalf("header read = %q, %v; want %q", got, err, tc.want)
			}
			if src.read > 1024 {
				t.Errorf("the header read took %d bytes of a %d-byte file, want the header alone", src.read, len(tc.data))
			}
		})
	}
}

// storedSampleFormat is path's tracks.sample_format column, NULL included.
func storedSampleFormat(t *testing.T, s *Store, path string) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := s.db.QueryRow(`SELECT sample_format FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("sample_format(%s): %v", path, err)
	}
	return v
}

// tagsJSONKeys is the keys of path's stored tags_json.
func tagsJSONKeys(t *testing.T, s *Store, path string) map[string]any {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(`SELECT tags_json FROM tracks WHERE path = ?`, path).Scan(&raw); err != nil {
		t.Fatalf("tags_json(%s): %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode tags_json(%s): %v", path, err)
	}
	return m
}

// TestSampleFormatRidesItsColumnAndNeverTagsJSON: a track write stores the
// kind in tracks.sample_format (the empty string where the row has none), never inside
// tags_json, and every reader that serves a Track splices it back: GetTrack,
// and the list, stream and page readers the manifest is built from.
func TestSampleFormatRidesItsColumnAndNeverTagsJSON(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	pcm := false
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	want := map[string]string{
		"A/float.wav":   SampleFormatFloat,
		"A/int.flac":    SampleFormatInteger,
		"A/lossy.mp3":   "",
		"A/single.aiff": SampleFormatFloat,
	}
	batch := []*Track{
		{Path: "A/float.wav", Size: 1, ModTime: at, Codec: "WAV", IsDSD: &pcm, SampleFormat: SampleFormatFloat},
		{Path: "A/int.flac", Size: 1, ModTime: at, Codec: "FLAC", IsDSD: &pcm, SampleFormat: SampleFormatInteger},
		{Path: "A/lossy.mp3", Size: 1, ModTime: at, Codec: "MP3"},
	}
	if err := s.UpsertTrackBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "A/single.aiff", Size: 1, ModTime: at, Codec: "AIFF", SampleFormat: SampleFormatFloat}); err != nil {
		t.Fatal(err)
	}
	check := func(reader, path, got string) {
		t.Helper()
		if got != want[path] {
			t.Errorf("%s: %s SampleFormat = %q, want %q", reader, path, got, want[path])
		}
	}
	for path, w := range want {
		if col := storedSampleFormat(t, s, path); !col.Valid || col.String != w {
			t.Errorf("%s: sample_format = %+v, want %q", path, col, w)
		}
		if _, ok := tagsJSONKeys(t, s, path)["sampleFormat"]; ok {
			t.Errorf("%s: tags_json carries sampleFormat; the column is its one home", path)
		}
		got, err := s.GetTrack(ctx, path)
		if err != nil || got == nil {
			t.Fatalf("GetTrack(%s): %v", path, err)
		}
		check("GetTrack", path, got.SampleFormat)
	}
	listed, err := s.ListServedTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range listed {
		check("ListServedTracks", tr.Path, tr.SampleFormat)
	}
	err = s.StreamServedTracks(ctx, nil, func(tr *Track) error {
		check("StreamServedTracks", tr.Path, tr.SampleFormat)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListServedTracksPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != len(want) || len(listed) != len(want) {
		t.Fatalf("readers returned %d and %d rows, want %d", len(listed), len(page), len(want))
	}
	for _, tr := range page {
		check("ListServedTracksPage", tr.Path, tr.SampleFormat)
	}
}

// TestADSDRowNeverCarriesASampleFormat: a PCM kind never reaches an isDSD
// row. A write that names one stores none, a column that holds one anyway (a
// hand edit, an older binary's bug) is not served, and the open-time fill
// never stamps a DSD row by its codec.
func TestADSDRowNeverCarriesASampleFormat(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dsd := true
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	const path = "A/x.dsf"
	if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 1, ModTime: at, Codec: "DSF", IsDSD: &dsd, SampleFormat: SampleFormatInteger}); err != nil {
		t.Fatal(err)
	}
	if col := storedSampleFormat(t, s, path); !col.Valid || col.String != "" {
		t.Errorf("the write stored %+v on a DSD row, want ''", col)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET sample_format = 'integer' WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTrack(ctx, path)
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if got.SampleFormat != "" {
		t.Errorf("GetTrack serves %q on a DSD row", got.SampleFormat)
	}
	listed, err := s.ListServedTracks(ctx, nil)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListServedTracks: %d rows, %v", len(listed), err)
	}
	if listed[0].SampleFormat != "" {
		t.Errorf("the manifest reader serves %q on a DSD row", listed[0].SampleFormat)
	}
	// A FLAC codec on a DSD row is no file's, and it is the shape that tells
	// the codec rule from the DSD rule: the fill decides the row has no kind.
	if _, err := s.db.Exec(`UPDATE tracks SET sample_format = NULL, codec = 'FLAC' WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	if col := storedSampleFormat(t, s, path); !col.Valid || col.String != "" {
		t.Errorf("the open-time fill stamped %+v on a DSD row, want ''", col)
	}
}

// pagedTracksByPath is the first page BuildManifestPage builds for store's
// root, marshalled as the handler writes it and decoded with each track's
// keys as the wire carries them, by path.
func pagedTracksByPath(t *testing.T, store *Store, root string) map[string]map[string]any {
	t.Helper()
	page, err := BuildManifestPage(context.Background(), store, []string{root}, "", 100)
	if err != nil {
		t.Fatalf("BuildManifestPage: %v", err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var mf struct {
		Tracks []map[string]any `json:"tracks"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("decode the page: %v", err)
	}
	byPath := make(map[string]map[string]any, len(mf.Tracks))
	for _, tr := range mf.Tracks {
		if p, ok := tr["path"].(string); ok {
			byPath[p] = tr
		}
	}
	return byPath
}

// TestTheManifestCarriesSampleFormatOnlyWhereAHeaderStatesIt scans a library
// of real writers' files and reads the manifest a client is served, streamed
// (WriteManifest) and paged (BuildManifestPage): a float row carries
// "sampleFormat":"float", an integer row "integer", and a row whose header
// states no kind carries no key at all, neither null nor "".
func TestTheManifestCarriesSampleFormatOnlyWhereAHeaderStatesIt(t *testing.T) {
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	files := []struct {
		name string
		data []byte
		want string
	}{
		{"float.wav", fx("wav/float.wav"), SampleFormatFloat},
		{"af_f64.wav", fx("wav/af_f64.wav"), SampleFormatFloat},
		{"s24.wav", fx("wav/s24.wav"), SampleFormatInteger},
		{"af_fl32.aifc", fx("aifc/af_fl32.aifc"), SampleFormatFloat},
		{"af_be16.aiff", fx("aifc/af_be16.aiff"), SampleFormatInteger},
		{"lossless.flac", flacBytes(t, 44100, 16), SampleFormatInteger},
		{"alaw.wav", fx("wav/alaw.wav"), ""},
		{"lossy.mp3", mp3Bytes(t), ""},
		{"dsd.dsf", minimalDSFBytes(2822400, buildID3v2_3(nil)), ""},
	}
	root := t.TempDir()
	rels := map[string]string{}
	for _, f := range files {
		rels[f.name] = writeAlone(t, root, f.name, f.data)
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	for body, served := range map[string]map[string]map[string]any{
		"streamed": servedTracksByPath(t, store, root),
		"paged":    pagedTracksByPath(t, store, root),
	} {
		for _, f := range files {
			row, ok := served[rels[f.name]]
			if !ok {
				t.Fatalf("the %s manifest does not serve %s", body, rels[f.name])
			}
			got, present := row["sampleFormat"]
			switch {
			case f.want == "" && present:
				t.Errorf("%s manifest, %s: carries \"sampleFormat\": %#v, want the key absent", body, f.name, got)
			case f.want != "" && got != f.want:
				t.Errorf("%s manifest, %s: \"sampleFormat\" = %#v (present %v), want %q", body, f.name, got, present, f.want)
			}
		}
	}
	for _, f := range files {
		if _, ok := tagsJSONKeys(t, store, rels[f.name])["sampleFormat"]; ok {
			t.Errorf("%s: the scan stored sampleFormat inside tags_json", f.name)
		}
	}
}

// TestAVariantCarriesTheKindItsFormatIs: every rendition the bridge writes is
// FLAC, which is integer-only, so each variant on the wire says "integer"; a
// format the bridge does not know says nothing.
func TestAVariantCarriesTheKindItsFormatIs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	const src = "A/01.flac"
	if err := s.UpsertTrack(ctx, &Track{Path: src, Size: 1, ModTime: at, Codec: "FLAC"}); err != nil {
		t.Fatal(err)
	}
	rows := []VariantRow{
		{SourcePath: src, VariantID: "upscaled-v2-176400-24", SidecarPath: "/v/a.flac", Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1, SourceMTimeNS: at.UnixNano(), SourceSize: 1, CreatedAt: 2},
		{SourcePath: src, VariantID: "optimized-v2-48000-16", SidecarPath: "/v/b.wav", Format: "wav",
			SampleRate: 48000, BitsPerSample: 16, SizeBytes: 1, SourceMTimeNS: at.UnixNano(), SourceSize: 1, CreatedAt: 1},
	}
	for _, r := range rows {
		if err := s.UpsertVariant(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := s.ListServedTracks(ctx, nil)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListServedTracks: %d rows, %v", len(listed), err)
	}
	raw, err := json.Marshal(listed[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Variants []map[string]any `json:"variants"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"upscaled-v2-176400-24": SampleFormatInteger, "optimized-v2-48000-16": ""}
	if len(wire.Variants) != len(want) {
		t.Fatalf("variants on the wire: %v", wire.Variants)
	}
	for _, v := range wire.Variants {
		id, _ := v["id"].(string)
		got, present := v["sampleFormat"]
		switch w, ok := want[id]; {
		case !ok:
			t.Errorf("an unexpected variant %q", id)
		case w == "" && present:
			t.Errorf("%s (format %v) carries \"sampleFormat\": %#v, want the key absent", id, v["format"], got)
		case w != "" && got != w:
			t.Errorf("%s: \"sampleFormat\" = %#v (present %v), want %q", id, got, present, w)
		}
	}
}
