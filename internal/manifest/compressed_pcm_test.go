package manifest

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Tests for naming a compressed AIFF-C or WAV by its encoding (backlog B124,
// B154, ExtractorVersion 21). An AIFF-C names its encoding in the COMM chunk's
// compression type and a WAV in the fmt chunk's format tag; the bridge read
// neither, so every AIFF-C was "AIFF" and every WAV "WAV", both on the
// lossless list, and a µ-law file at 44.1 kHz with no depth landed in CD
// Quality, an ADPCM one at 96 kHz in Hi-Res.

// compressedFixture is one of the real writers' files in testdata/aifc and
// testdata/wav (testdata/gen/compressed_pcm_fixtures.sh) and what its row
// carries: the codec, the rate and depth (0 for none), and the duration (0 for
// none: under the 0.1 s floor, or withheld).
type compressedFixture struct {
	path  string
	codec string
	rate  float64
	bits  int
	dur   float64
}

// compressedFixtures names each encoding as the iOS app names it (its
// canonicalCodec, #2014 and #2028): the compressed AIFF-C types "ULAW",
// "ALAW", "IMA4"; the compressed WAV types "ADPCM" (IMA and MS alike), "GSM",
// "ULAW", "ALAW", "MP2"; the linear ones keep "AIFF" and "WAV" and their
// depth. A WAV format tag the bridge does not name is default-denied, as a DFF
// with an unknown compression is: its container's name and no rate or depth,
// the app's own presentation of a WAV it cannot decode.
var compressedFixtures = []compressedFixture{
	{path: "aifc/af_ulaw.aifc", codec: "ULAW", rate: 44100},
	{path: "aifc/af_alaw.aifc", codec: "ALAW", rate: 44100},
	// numSampleFrames counts 64-frame packets for ima4: 138 of them, 0.2 s.
	{path: "aifc/af_ima4.aifc", codec: "IMA4", rate: 44100, dur: 138 * 64 / 44100.0},
	{path: "aifc/af_ima4_96k.aifc", codec: "IMA4", rate: 96000},
	{path: "aifc/ff_ima4.aifc", codec: "IMA4", rate: 44100, dur: 138 * 64 / 44100.0},
	{path: "aifc/af_twos.aifc", codec: "AIFF", rate: 44100, bits: 16},
	{path: "aifc/af_in24.aifc", codec: "AIFF", rate: 44100, bits: 24},
	{path: "aifc/af_fl32.aifc", codec: "AIFF", rate: 44100, bits: 32},
	{path: "aifc/ff_sowt.aifc", codec: "AIFF", rate: 44100, bits: 16},
	{path: "wav/ima_adpcm.wav", codec: "ADPCM", rate: 44100},
	{path: "wav/ms_adpcm.wav", codec: "ADPCM", rate: 44100},
	{path: "wav/ima_adpcm_96k.wav", codec: "ADPCM", rate: 96000},
	{path: "wav/ms_adpcm_96k.wav", codec: "ADPCM", rate: 96000},
	{path: "wav/alaw.wav", codec: "ALAW", rate: 44100},
	{path: "wav/mulaw.wav", codec: "ULAW", rate: 44100},
	{path: "wav/mp2.wav", codec: "MP2", rate: 44100},
	{path: "wav/gsm.wav", codec: "GSM", rate: 8000},
	{path: "wav/g726.wav", codec: "WAV"},
	{path: "wav/float.wav", codec: "WAV", rate: 44100, bits: 32},
	{path: "wav/s24.wav", codec: "WAV", rate: 44100, bits: 24},
}

// compressedPCMFixture is the bytes of one of those files (rel is below
// testdata), for the fuzz targets' seeds.
func compressedPCMFixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requireCompressedFixture fails unless tr carries what fx says.
func requireCompressedFixture(t *testing.T, fx compressedFixture, tr *Track) {
	t.Helper()
	if tr.Codec != fx.codec {
		t.Errorf("Codec = %q, want %q", tr.Codec, fx.codec)
	}
	var rate float64
	if tr.SampleRate != nil {
		rate = *tr.SampleRate
	}
	if rate != fx.rate {
		t.Errorf("SampleRate = %v, want %v (0 = none)", rate, fx.rate)
	}
	var bits int
	if tr.BitsPerSample != nil {
		bits = *tr.BitsPerSample
	}
	if bits != fx.bits {
		t.Errorf("BitsPerSample = %d, want %d (0 = none)", bits, fx.bits)
	}
	var dur float64
	if tr.Duration != nil {
		dur = *tr.Duration
	}
	if math.Abs(dur-fx.dur) > 1e-9 {
		t.Errorf("Duration = %v, want %v (0 = none)", dur, fx.dur)
	}
}

// TestACompressedAIFCOrWAVIsNamedByItsEncoding is backlog B124 and B154 through
// the extractor, on files afconvert, ffmpeg and sox wrote: a compressed AIFF-C
// or WAV is named by its encoding and carries no depth, a linear one keeps its
// container's name and depth, and an IMA4 AIFF-C's duration counts its
// packets' 64 frames each (afconvert's afinfo reads a 30 s one as 30 s; the
// frame count alone gave 0.4688 s).
func TestACompressedAIFCOrWAVIsNamedByItsEncoding(t *testing.T) {
	for _, fx := range compressedFixtures {
		t.Run(fx.path, func(t *testing.T) {
			var tr Track
			if err := ExtractWithContext(filepath.Join("testdata", fx.path), &tr, nil); err != nil {
				t.Fatalf("extract: %v", err)
			}
			requireCompressedFixture(t, fx, &tr)
		})
	}
}

// TestTheCompressedPCMCodecsAreLossy: the upscale gate's lossy set holds every
// name a compressed AIFF-C or WAV now gets, and none of the names a linear one
// or an unread one keeps. "AIFC" (an AIFF-C whose compression the bridge does
// not know) is neither: the upscale gate still needs a depth, which it lacks.
func TestTheCompressedPCMCodecsAreLossy(t *testing.T) {
	for _, c := range []string{"ULAW", "ALAW", "IMA4", "ADPCM", "GSM", "MP2", "ulaw", " adpcm "} {
		if !IsLossyCodec(c) {
			t.Errorf("IsLossyCodec(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"AIFF", "WAV", "AIFC", "PCM", "FLAC", ""} {
		if IsLossyCodec(c) {
			t.Errorf("IsLossyCodec(%q) = true, want false", c)
		}
	}
}

// TestScanner_V21_ACompressedAIFCOrWAVJoinsTheDelta_ALinearOneOnlyStamps is the
// v21 upgrade end to end. A v20 bridge stored a compressed AIFF-C as "AIFF" and
// a compressed WAV as "WAV", under a stale stamp: such a row re-extracts once,
// gains its codec, advances its indexed_at (the iOS delta) and goes back to the
// enricher (the full-upsert leg does that for any changed row). A linear AIFF-C
// and a PCM WAV re-extract byte-identical and are only stamped.
func TestScanner_V21_ACompressedAIFCOrWAVJoinsTheDelta_ALinearOneOnlyStamps(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{ // name in the library -> fixture
		"ulaw.aifc": "aifc/af_ulaw.aifc",
		"adpcm.wav": "wav/ima_adpcm_96k.wav",
		"twos.aifc": "aifc/af_twos.aifc",
		"float.wav": "wav/float.wav",
	}
	was := map[string]string{"ulaw.aifc": "AIFF", "adpcm.wav": "WAV"} // what v20 stored
	rels := map[string]string{}
	for name, fx := range files {
		data, err := os.ReadFile(filepath.Join("testdata", fx))
		if err != nil {
			t.Fatal(err)
		}
		rels[name] = writeAlone(t, root, name, data)
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	before := map[string]int64{}
	for name, rel := range rels {
		q, args := "UPDATE tracks SET extractor_version = 20, enriched_at = 1 WHERE path = ?", []any{rel}
		if old, ok := was[name]; ok {
			q = `UPDATE tracks SET extractor_version = 20, enriched_at = 1, codec = ?,
				tags_json = json_set(tags_json, '$.codec', ?) WHERE path = ?`
			args = []any{old, old, rel}
		}
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("rewind %s to v20: %v", name, err)
		}
		before[name] = trackIndexedAt(t, store, rel)
	}

	scanOnce(t, sc, "v21")

	for name, rel := range rels {
		got := storedTrack(t, store, rel)
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", name, v, ExtractorVersion)
		}
		after := trackIndexedAt(t, store, rel)
		if old, compressed := was[name]; compressed {
			if got.Codec == old {
				t.Errorf("%s: codec still %q after the re-extract", name, got.Codec)
			}
			if after <= before[name] {
				t.Errorf("%s: indexed_at did not advance (%d -> %d): iOS would keep the lossless claim", name, before[name], after)
			}
			continue
		}
		if after != before[name] {
			t.Errorf("%s: indexed_at moved (%d -> %d): v21 must not put a linear file in the delta", name, before[name], after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: v21 must not re-enrich a linear file", name, v)
		}
	}
}

// aifcWithChunks is a FORM/AIFC file holding chunks, in order.
func aifcWithChunks(chunks ...[]byte) []byte {
	body := []byte("AIFC")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := append([]byte("FORM"), make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[4:8], uint32(len(body)))
	return append(out, body...)
}

// TestTheFirstFormatChunkNamesTheFile: a WAV or AIFF-C carrying two format
// chunks is read from the first, whole, as ffmpeg and TagLib read a WAV and
// TagLib an AIFF, and as the walkers already read their first data and SSND
// chunk (CodeRabbit on #1122). Parsed one on top of the other, the second
// chunk's codec landed beside the first one's depth: an "ADPCM" row with 24
// bits, a "ULAW" row with 16, a lossy name with a lossless claim.
func TestTheFirstFormatChunkNamesTheFile(t *testing.T) {
	pcm := buildWAVFmtChunk(1, 2, 96000, 24)
	adpcm := buildWAVFmtChunkRaw(wavFormatIMAADPCM, 2, 96000, 96000, 2048, 4)
	twos := buildAIFFCOMMChunk(2, 4410, 16, 44100, []byte("twos\x00\x00")...)
	ulaw := buildAIFFCOMMChunk(2, 4410, 16, 44100, []byte("ulaw\x00\x00")...)
	cases := []struct {
		name, file string
		data       []byte
		codec      string
		rate       float64
		bits       int
	}{
		{"PCM, then ADPCM", "x.wav", buildWAVWithID3(t, nil, pcm, adpcm), "WAV", 96000, 24},
		{"ADPCM, then PCM", "x.wav", buildWAVWithID3(t, nil, adpcm, pcm), "ADPCM", 96000, 0},
		{"twos, then ulaw", "x.aifc", aifcWithChunks(twos, ulaw), "AIFF", 44100, 16},
		{"ulaw, then twos", "x.aifc", aifcWithChunks(ulaw, twos), "ULAW", 44100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			var tr Track
			if err := ExtractWithContext(path, &tr, nil); err != nil {
				t.Fatalf("extract: %v", err)
			}
			requireCompressedFixture(t, compressedFixture{codec: tc.codec, rate: tc.rate, bits: tc.bits}, &tr)
			if IsLossyCodec(tr.Codec) && tr.BitsPerSample != nil {
				t.Errorf("a lossy %q row carries a depth of %d", tr.Codec, *tr.BitsPerSample)
			}
		})
	}
}

// mpegFmtChunk is a WAVE_FORMAT_MPEG fmt chunk: a WAVEFORMATEX whose cbSize
// is cb, followed by MPEG1WAVEFORMAT's extension (22 bytes, fwHeadLayer
// first) when cb is 22, or by nothing.
func mpegFmtChunk(cb, layer uint16) []byte {
	payload := make([]byte, 18+int(cb))
	binary.LittleEndian.PutUint16(payload[0:2], wavFormatMPEG)
	binary.LittleEndian.PutUint16(payload[2:4], 2)
	binary.LittleEndian.PutUint32(payload[4:8], 44100)
	binary.LittleEndian.PutUint32(payload[8:12], 24000)
	binary.LittleEndian.PutUint16(payload[12:14], 1)
	binary.LittleEndian.PutUint16(payload[16:18], cb)
	if cb >= 2 {
		binary.LittleEndian.PutUint16(payload[18:20], layer)
	}
	return wrapChunkLE("fmt ", payload)
}

// TestAnMPEGWAVIsNamedByTheLayerItsHeaderDeclares: format code 0x0050 is
// MPEG audio, and MPEG1WAVEFORMAT's fwHeadLayer says which layer (mmreg.h:
// 1, 2 and 4 for layers I, II and III). Layer III is "MP3", as under its own
// tag, 0x0055; anything else, a header without the extension included, is
// "MP2" (ffmpeg writes layer II as 2: testdata/wav/mp2.wav). An extensible
// header's subformat of 0x0050 has WAVEFORMATEXTENSIBLE's fields at those
// offsets, so its "layer" is never read (CodeRabbit on #1122).
func TestAnMPEGWAVIsNamedByTheLayerItsHeaderDeclares(t *testing.T) {
	cases := []struct {
		name  string
		fmt   []byte
		codec string
	}{
		{"layer III", mpegFmtChunk(22, 4), "MP3"},
		{"layer II", mpegFmtChunk(22, 2), "MP2"},
		{"layer I", mpegFmtChunk(22, 1), "MP2"},
		{"no extension (cbSize 0)", mpegFmtChunk(0, 0), "MP2"},
		{"a bare 16-byte header", buildWAVFmtChunkRaw(wavFormatMPEG, 2, 44100, 24000, 1, 0), "MP2"},
		// validBits 4 sits where fwHeadLayer would: the subformat is not
		// MPEG1WAVEFORMAT, so it names no layer.
		{"extensible, subformat 0x0050", buildWAVFmtChunkExtensible(2, 44100, 16, 4, wavFormatMPEG), "MP2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempWAV(t, buildWAVWithID3(t, nil, tc.fmt, buildWAVDataChunk(4)))
			var tr Track
			if err := ExtractWithContext(path, &tr, nil); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if tr.Codec != tc.codec {
				t.Errorf("Codec = %q, want %q", tr.Codec, tc.codec)
			}
			if !IsLossyCodec(tr.Codec) || tr.BitsPerSample != nil {
				t.Errorf("Codec %q lossy = %v, BitsPerSample = %v: want lossy, no depth",
					tr.Codec, IsLossyCodec(tr.Codec), tr.BitsPerSample)
			}
		})
	}
}
