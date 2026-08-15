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
package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// fuzzExtract drives one extractor entry point against arbitrary bytes written
// to a real file, since every extractor opens by path (several seek, and the
// FLAC path deliberately depends on seek alignment — see the
// flacPictureBlocksSane invariant).
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
		p := filepath.Join(dir, "fuzz"+ext)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Skip() // filesystem refused the input; not what we're testing
		}
		var tr Track
		// An error return is a PASS: "this file is not parseable" is the
		// correct answer for most inputs. Only a panic fails.
		_ = ExtractWithContext(p, &tr, &ExtractContext{})
	})
}

func FuzzExtractAIFF(f *testing.F) {
	fuzzExtract(f, ".aiff", [][]byte{
		// FORM/AIFF carrying a well-formed 18-byte COMM (2ch, 16-bit, 44.1k).
		[]byte("FORM\x00\x00\x00\x12AIFFCOMM\x00\x00\x00\x12\x00\x02\x00\x00\x10\x00\x00\x18\x40\x0E\xAC\x44\x00\x00\x00\x00\x00\x00"),
		[]byte("FORM\x00\x00\x00\x04AIFC"),
	})
}

func FuzzExtractWAV(f *testing.F) {
	fuzzExtract(f, ".wav", [][]byte{
		[]byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x44\xAC\x00\x00\x10\xB1\x02\x00\x04\x00\x10\x00data\x00\x00\x00\x00"),
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
	fuzzExtract(f, ".flac", [][]byte{
		// fLaC + a last-block STREAMINFO header with a zeroed 34-byte body.
		append([]byte("fLaC\x80\x00\x00\x22"), make([]byte, 34)...),
	})
}

func FuzzExtractM4A(f *testing.F) {
	fuzzExtract(f, ".m4a", [][]byte{
		[]byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A mp42isom"),
	})
}

func FuzzExtractMP3(f *testing.F) {
	fuzzExtract(f, ".mp3", [][]byte{
		[]byte("ID3\x03\x00\x00\x00\x00\x00\x0aTIT2\x00\x00\x00\x02\x00\x00\x00a"),
	})
}
