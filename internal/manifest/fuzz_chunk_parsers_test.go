// Fuzz coverage for the pure chunk-body parsers.
//
// Siblings of fuzz_extractors_test.go, one layer down. The whole-file targets
// exercise the walk; these exercise the arithmetic INSIDE a single chunk body,
// where a declared length and a real length can disagree — the shape behind
// every historical parser panic in this codebase.
//
// They are separate from the whole-file targets because the walk gates what
// reaches them (size caps, magic checks), so a body the walk would never hand
// over is still worth testing directly: the caps are a policy that can change,
// and the parser must not depend on them for memory safety.
package manifest

import (
	"testing"
	"unicode/utf8"
)

func FuzzParseAIFFCOMMChunk(f *testing.F) {
	// A real 18-byte COMM: 2 channels, 0x1000 frames, 24-bit, 44100 Hz as an
	// 80-bit IEEE-754 extended.
	f.Add([]byte{0, 2, 0, 0, 0x10, 0x00, 0, 24, 0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		var tr Track
		// Both form types: AIFC appends a compression FOURCC after the shared
		// 18-byte prefix, so the two take different lengths through the parse.
		parseAIFFCOMMChunk(b, &tr, "AIFF")
		parseAIFFCOMMChunk(b, &tr, "AIFC")
	})
}

func FuzzParseAIFFExtended(f *testing.F) {
	// 80-bit extended float — a hand-rolled decode with an exponent shift,
	// which is where a malformed value turns into a huge or NaN sample rate.
	f.Add([]byte{0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { _ = parseAIFFExtended(b) })
}

func FuzzParseWAVFmtChunk(f *testing.F) {
	// PCM, 2ch, 44100, 16-bit.
	f.Add([]byte{1, 0, 2, 0, 0x44, 0xAC, 0, 0, 0, 0, 0, 0, 4, 0, 0x10, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		var tr Track
		parseWAVFmtChunk(b, &tr)
	})
}

func FuzzParseWAVINFOBlock(f *testing.F) {
	// LIST/INFO carries its own nested length-prefixed sub-chunks — a second
	// layer of declared lengths inside an already length-bounded body.
	f.Add([]byte("INFOINAM\x04\x00\x00\x00abc\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = parseWAVINFOBlock(b)
	})
}

func FuzzParseDIINChunks(f *testing.F) {
	// A DSDIFF DIIN chunk's text chunks hold a 4-byte count and the text
	// (DSDIFF 1.5; backlog B140) inside a 64-bit-length chunk: two length
	// fields that can disagree, beside the edited master's id and markers.
	f.Add([]byte("DITI\x00\x00\x00\x00\x00\x00\x00\x08\x00\x00\x00\x04abcd"))
	// The body TagLib 2 writes (testdata/dff/taglib_diin.dff): an ISO-8859-1
	// title of odd size, its pad byte, and an artist.
	f.Add([]byte("DITI\x00\x00\x00\x00\x00\x00\x00\x1f\x00\x00\x00\x1bPr\xe9lude \xe0 la nuit, premi\xe8re\x00" +
		"DIAR\x00\x00\x00\x00\x00\x00\x00\x11\x00\x00\x00\x0dEnsemble DIIN\x00"))
	// EMID and MARK ahead of the text, and a count past its chunk.
	f.Add(append(append(append(dffChunk("EMID", []byte("an edited master's id")),
		dffChunk("MARK", make([]byte, 22))...),
		dffChunk("DITI", []byte("\x00\x00\x01\x00short"))...),
		buildDIINSubChunk("DIAR", "After")...))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := parseDIINChunks(b, "fuzz")
		for _, v := range []string{c.title, c.artist} {
			if !utf8.ValidString(v) {
				t.Fatalf("parseDIINChunks returned text that is not UTF-8: %q", v)
			}
		}
		if c.album != "" || c.genre != "" {
			t.Fatalf("a DIIN has no album or genre, and parseDIINChunks returned %q / %q", c.album, c.genre)
		}
	})
}

func FuzzParsePropChunks(f *testing.F) {
	f.Add([]byte("FS  \x00\x00\x00\x00\x00\x00\x00\x04\x00\x2B\x11\x00"))
	// CHNL + CMPR("DST ") seeds so the gather-only rewrite's new arms
	// stay in the fuzz corpus.
	f.Add([]byte("CHNL\x00\x00\x00\x00\x00\x00\x00\x02\x00\x02"))
	f.Add([]byte("CMPR\x00\x00\x00\x00\x00\x00\x00\x05DST \x00\x00"))
	// An ID3 chunk nested among the properties (backlog B140), in both
	// spellings, the second one ignored.
	f.Add(append(dffChunk("ID3 ", []byte("ID3\x04\x00\x00\x00\x00\x00\x00")),
		dffChunk("id3 ", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"))...))
	f.Fuzz(func(t *testing.T, b []byte) {
		info := parsePropChunks(b)
		if len(info.id3) > len(b) {
			t.Fatalf("parsePropChunks returned a %d-byte ID3 body from %d bytes", len(info.id3), len(b))
		}
	})
}
