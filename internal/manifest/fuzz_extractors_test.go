// Fuzz coverage for the whole-file extractor entry points.
//
// # Why these exist
//
// `runScanWorker` carries a per-iteration `recover()` precisely BECAUSE these
// parsers can panic on malformed input — dhowden/tag and the project's own
// DSF/DFF walkers both have. That recover is a mitigation, not a fix: a
// panicked file logs, increments `Scanner.panickedCnt`, and is SKIPPED, so it
// never reaches the manifest and never reaches a client. A crash found here is
// therefore a real defect (one silently unindexed track per malformed file),
// not merely a robustness nicety.
//
// The input is genuinely untrusted in the sense that matters operationally: an
// operator's library is whatever landed on disk — half-finished rclone
// uploads, truncated NAS copies, files a tagger wrote badly. The v0.1.7
// truncated-B2-upload case (PRs #448/#449) is exactly this shape.
//
// These drive the REAL entry point through a real file on disk rather than the
// pure sub-parsers, so the chunk-walk arithmetic that stitches them together —
// the part no unit test covers end to end — is in scope.
//
// Seeds are minimal well-formed containers so the fuzzer starts inside the
// parse rather than bouncing off the magic check. Without `-fuzz` these run as
// ordinary seed-corpus tests, so they cost the normal suite ~nothing.
//
// # The allocation property
//
// Every target here also fails when one extraction allocates more than
// extractionAllocLimit allows for the input's size: a length read out of a
// file must never size a buffer the file cannot back (the v11 rule, and
// backlog B99). Without it the fuzzer found such an input and could not say
// so: dhowden allocated 4 GiB for a 40-byte FLAC, the runtime handed out the
// address space, and the four workers re-zeroing it took the nightly runner's
// memory until the runner itself was killed, with nothing saved. Measured
// here, the same input is an ordinary crasher, written to testdata/fuzz with
// the size it asked for.
package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// fuzzExtract drives one extractor entry point against arbitrary bytes written
// to a real file, since every extractor opens by path (several seek, and the
// FLAC path deliberately depends on seek alignment).
//
// The ExtractContext carries no artwork cache dir: artwork extraction writes
// files and is covered by its own tests, and leaving it empty keeps each
// execution to a single open + parse so the fuzzer gets throughput.
func fuzzExtract(f *testing.F, ext string, seeds [][]byte) {
	f.Helper()
	for _, s := range seeds {
		f.Add(s)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzExtractOnce(t, dir, ext, b)
	})
}

// fuzzExtractOnce extracts b as a file with extension ext, and fails on the
// allocation property (see the file's docblock). An error return is a PASS:
// "this file is not parseable" is the correct answer for most inputs.
func fuzzExtractOnce(t *testing.T, dir, ext string, b []byte) {
	p := filepath.Join(dir, "fuzz"+ext)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Skip() // filesystem refused the input; not what we're testing
	}
	var tr Track
	before := heapAllocated()
	_ = ExtractWithContext(p, &tr, &ExtractContext{})
	if got, limit := heapAllocated()-before, extractionAllocLimit(len(b)); got > limit {
		t.Fatalf("extracting a %d-byte %s file allocated %d bytes (limit %d): a length read from the file sized a buffer the file cannot back",
			len(b), ext, got, limit)
	}
}

func FuzzExtractAIFF(f *testing.F) {
	fuzzExtract(f, ".aiff", [][]byte{
		// FORM/AIFF carrying a well-formed 18-byte COMM (2ch, 16-bit, 44.1k).
		[]byte("FORM\x00\x00\x00\x12AIFFCOMM\x00\x00\x00\x12\x00\x02\x00\x00\x10\x00\x00\x18\x40\x0E\xAC\x44\x00\x00\x00\x00\x00\x00"),
		// The same COMM followed by a 12-byte SSND (8-byte header + 4 audio
		// bytes) — the duration walk's fit check (v11) has a payload to check.
		[]byte("FORM\x00\x00\x00\x26AIFFCOMM\x00\x00\x00\x12\x00\x02\x00\x00\x10\x00\x00\x18\x40\x0E\xAC\x44\x00\x00\x00\x00\x00\x00SSND\x00\x00\x00\x0c\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		[]byte("FORM\x00\x00\x00\x04AIFC"),
	})
}

func FuzzExtractWAV(f *testing.F) {
	fuzzExtract(f, ".wav", [][]byte{
		[]byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x44\xAC\x00\x00\x10\xB1\x02\x00\x04\x00\x10\x00data\x00\x00\x00\x00"),
		// Four bytes of audio in the data chunk — a non-zero duration path.
		[]byte("RIFF\x28\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x44\xAC\x00\x00\x10\xB1\x02\x00\x04\x00\x10\x00data\x04\x00\x00\x00\x00\x00\x00\x00"),
		[]byte("RIFF\x04\x00\x00\x00WAVE"),
	})
}

func FuzzExtractDFF(f *testing.F) {
	fuzzExtract(f, ".dff", [][]byte{
		// FRM8/DSD with a PROP/SND container holding an FS chunk (2.8224 MHz).
		[]byte("FRM8\x00\x00\x00\x00\x00\x00\x00\x20DSD PROP\x00\x00\x00\x00\x00\x00\x00\x10SND FS  \x00\x00\x00\x00\x00\x00\x00\x04\x00\x2B\x11\x00"),
	})
}

func FuzzExtractDSF(f *testing.F) {
	fuzzExtract(f, ".dsf", [][]byte{
		[]byte("DSD \x1c\x00\x00\x00\x00\x00\x00\x00"),
	})
}

func FuzzExtractFLAC(f *testing.F) {
	seeds := [][]byte{
		// fLaC + a last-block STREAMINFO header with a zeroed 34-byte body.
		append([]byte("fLaC\x80\x00\x00\x22"), make([]byte, 34)...),
	}
	// Every way the old guard let dhowden allocate a picture past the bytes
	// present (backlog B99), so the ordinary suite runs each through the
	// allocation property. testdata/fuzz/FuzzExtractFLAC holds the input the
	// fuzzer itself found.
	for _, c := range pictureBombShapes() {
		seeds = append(seeds, c.data)
	}
	fuzzExtract(f, ".flac", seeds)
}

// FuzzExtractOGG fuzzes the Ogg path through what dhowden reads of it: the
// comment packet. A mutated page fails dhowden's CRC and ends its read at
// once, so the fuzzer mutates the PACKETS and the harness lays them out as
// pages with valid CRCs (pageData picks how the packet splits across pages,
// which the reader has to join). The seeds carry a METADATA_BLOCK_PICTURE,
// whose declared data length dhowden allocates before reading.
func FuzzExtractOGG(f *testing.F) {
	cover := make([]byte, 64)
	f.Add(vorbisIdent(), vorbisCommentPacket("TITLE=t"), uint16(65025))
	f.Add(vorbisIdent(), vorbisCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover)), uint16(255))
	f.Add(append([]byte("OpusHead"), make([]byte, 11)...), opusCommentPacket(mbpComment("metadata_block_picture", pictureDataLen, nil)), uint16(510))
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, ident, comment []byte, pageData uint16) {
		// Bound what one execution writes: the property is about what a
		// file's declared lengths make the extractor allocate, not the
		// file's own size.
		if len(ident)+len(comment) > 1<<20 {
			t.Skip()
		}
		fuzzExtractOnce(t, dir, ".ogg", oggStream(int(pageData), ident, comment))
	})
}

func FuzzExtractM4A(f *testing.F) {
	fuzzExtract(f, ".m4a", [][]byte{
		[]byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A mp42isom"),
		// ftyp + a moov holding only a version-0 mvhd (timescale 600,
		// duration 144300) — the duration walk (v11) parses it.
		append([]byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x74moov\x00\x00\x00\x6cmvhd\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x58\x00\x02\x33\xAC"),
			make([]byte, 80)...),
	})
}

func FuzzExtractMP3(f *testing.F) {
	fuzzExtract(f, ".mp3", [][]byte{
		[]byte("ID3\x03\x00\x00\x00\x00\x00\x0aTIT2\x00\x00\x00\x02\x00\x00\x00a"),
		// One MPEG 1 Layer III 128 kbit/s 44.1 kHz frame carrying a Xing
		// header (32 bytes of side info, flags=frames, 100 frames) — the
		// duration ladder's first rung (v11).
		append(append([]byte("\xFF\xFB\x90\x00"), make([]byte, 32)...), []byte("Xing\x00\x00\x00\x01\x00\x00\x00\x64")...),
	})
}
