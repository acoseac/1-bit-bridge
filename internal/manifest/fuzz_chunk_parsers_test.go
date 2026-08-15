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

import "testing"

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
		var tr Track
		parseWAVINFOBlock(b, &tr)
	})
}

func FuzzParseDIINChunks(f *testing.F) {
	// DSDIFF DIIN sub-chunks decode pstrings (1-byte length + N bytes +
	// pad-if-odd) inside a 64-bit-length container: two independent length
	// fields that can disagree.
	f.Add([]byte("DITI\x00\x00\x00\x00\x00\x00\x00\x05\x04abcd"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var tr Track
		parseDIINChunks(b, &tr, "fuzz")
	})
}

func FuzzParsePropChunks(f *testing.F) {
	f.Add([]byte("FS  \x00\x00\x00\x00\x00\x00\x00\x04\x00\x2B\x11\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var tr Track
		_ = parsePropChunks(b, &tr)
	})
}
