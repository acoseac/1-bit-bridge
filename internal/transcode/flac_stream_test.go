package transcode

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/flactest"
)

// TestFLACChecksumsAreTheFormatsOwn pins the two checksums to their published
// check values (CRC-8/SMBUS and CRC-16/BUYPASS over "123456789"). The
// fixtures (internal/flactest) compute theirs bit by bit, and pin the same
// values in their own test.
func TestFLACChecksumsAreTheFormatsOwn(t *testing.T) {
	if got := flacCRC8([]byte("123456789")); got != 0xF4 {
		t.Errorf("flacCRC8 = %#02x, want 0xF4", got)
	}
	if got := flacCRC16([]byte("123456789")); got != 0xFEE8 {
		t.Errorf("flacCRC16 = %#04x, want 0xFEE8", got)
	}
}

// writeTestFile writes b to a fresh file and returns its path.
func writeTestFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadFLACStreamReadsWhatLibFLACWrote reads streams libFLAC wrote (through
// sox, committed under testdata/flac): one frame of an uncommon block size,
// mono 24-bit, stereo at the faithful tier's rate. Each is whole, and holds
// the samples its STREAMINFO declares (metaflac --show-total-samples).
func TestReadFLACStreamReadsWhatLibFLACWrote(t *testing.T) {
	for _, tc := range []struct {
		file                 string
		rate, channels, bits int
		samples              uint64
	}{
		{"whole-44100-16-2.flac", 44100, 2, 16, 4052},
		{"whole-96000-24-1.flac", 96000, 1, 24, 19202},
		{"whole-176400-24-2.flac", 176400, 2, 24, 32417},
	} {
		t.Run(tc.file, func(t *testing.T) {
			s, err := readFLACStream(filepath.Join("testdata", "flac", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			i := s.info
			if i.sampleRate != tc.rate || i.channels != tc.channels || i.bitsPerSample != tc.bits || i.declared != tc.samples {
				t.Errorf("STREAMINFO %+v, want %d Hz, %d channel(s), %d bit, %d samples", i, tc.rate, tc.channels, tc.bits, tc.samples)
			}
			if !s.whole() || s.held != tc.samples {
				t.Errorf("held %d of %d (whole %v), want the stream whole", s.held, i.declared, s.whole())
			}
		})
	}
}

// TestACutStreamIsNotWhole cuts streams libFLAC wrote, and fixtures, at every
// byte of their last 300 (of their frames, for a fixture shorter than that):
// a stream a failed write left ends inside a frame, or between two before
// the end STREAMINFO declares, and neither is whole.
func TestACutStreamIsNotWhole(t *testing.T) {
	streams := map[string][]byte{
		"fixed fixture":    flactest.Stream(192000, 2, 24, 3*flactest.Block+1000, 7),
		"variable fixture": flactest.VariableStream(44100, 1, 16, 2*flactest.Block+5, -3),
	}
	for _, f := range []string{"whole-96000-24-1.flac", "whole-176400-24-2.flac"} {
		b, err := os.ReadFile(filepath.Join("testdata", "flac", f))
		if err != nil {
			t.Fatal(err)
		}
		streams[f] = b
	}
	for name, b := range streams {
		t.Run(name, func(t *testing.T) {
			if s, err := readFLACStream(writeTestFile(t, "whole.flac", b)); err != nil || !s.whole() {
				t.Fatalf("uncut: %+v, %v; want whole", s, err)
			}
			for cut := 1; cut <= min(300, len(b)-flacStreamInfoLen); cut++ {
				s, err := readFLACStream(writeTestFile(t, "cut.flac", b[:len(b)-cut]))
				if err != nil {
					t.Fatalf("cut %d: %v", cut, err)
				}
				if s.whole() {
					t.Fatalf("cut %d bytes short: held %d of %d, read as whole", cut, s.held, s.info.declared)
				}
			}
		})
	}
}

// TestAStreamCutBetweenFramesHoldsWhatItsFramesHold cuts a stream exactly
// after a frame: the frame that ends the file is intact, and held is where
// it ends, short of what STREAMINFO declares.
func TestAStreamCutBetweenFramesHoldsWhatItsFramesHold(t *testing.T) {
	const samples = 3*flactest.Block + 100
	b := flactest.Stream(48000, 2, 16, samples, 1)
	last := flactest.Frame(3, 100, 2, 16, 1, false)
	cut := b[:len(b)-len(last)]
	s, err := readFLACStream(writeTestFile(t, "cut.flac", cut))
	if err != nil {
		t.Fatal(err)
	}
	if s.held != 3*flactest.Block || s.whole() {
		t.Errorf("held %d (whole %v), want %d and not whole", s.held, s.whole(), 3*flactest.Block)
	}
}

// TestAFinishedStreamDeclaresItsLength: a stream whose frames are all there
// but whose STREAMINFO declares no length (a piped encode the tool never
// finished, cut exactly between frames) is not whole.
func TestAFinishedStreamDeclaresItsLength(t *testing.T) {
	b := flactest.StreamInfo(44100, 2, 16, 0)
	b = append(b, flactest.Frame(0, flactest.Block, 2, 16, 5, false)...)
	s, err := readFLACStream(writeTestFile(t, "undeclared.flac", b))
	if err != nil {
		t.Fatal(err)
	}
	if s.held != flactest.Block || s.whole() {
		t.Errorf("held %d (whole %v), want %d and not whole", s.held, s.whole(), flactest.Block)
	}
}

// TestReadFLACStreamRefusesWhatIsNotFLAC: a file too short for STREAMINFO, or
// one that does not begin as FLAC, is errNotFLAC.
func TestReadFLACStreamRefusesWhatIsNotFLAC(t *testing.T) {
	whole := flactest.Stream(44100, 2, 16, 10, 0)
	notStreamInfo := append([]byte(nil), whole...)
	notStreamInfo[4] = 0x84 // a first block of type 4 (VORBIS_COMMENT)
	for name, b := range map[string][]byte{
		"empty": {}, "fLaC alone": []byte("fLaC"), "short STREAMINFO": whole[:20],
		"not fLaC": append([]byte("RIFF"), whole[4:]...), "first block not STREAMINFO": notStreamInfo,
	} {
		if _, err := readFLACStream(writeTestFile(t, "x.flac", b)); !errors.Is(err, errNotFLAC) {
			t.Errorf("%s: err %v, want errNotFLAC", name, err)
		}
	}
}

// TestReadFLACStreamOnBytesNoEncoderWrote feeds the reader STREAMINFO heads
// over random tails and random cuts of a whole stream: it must not panic, and
// a random tail must not read as whole (every byte would have to agree).
func TestReadFLACStreamOnBytesNoEncoderWrote(t *testing.T) {
	r := rand.New(rand.NewPCG(264, 1))
	whole := flactest.Stream(96000, 2, 24, 5*flactest.Block+17, 9)
	for i := range 300 {
		tail := make([]byte, r.IntN(40000))
		for j := range tail {
			tail[j] = byte(r.Uint32())
		}
		// Sync patterns, so the header parser runs on random headers.
		for j := 0; j+1 < len(tail); j += 1 + r.IntN(64) {
			tail[j], tail[j+1] = 0xFF, 0xF8|byte(r.IntN(2))
		}
		b := append(append([]byte(nil), whole[:flacStreamInfoLen]...), tail...)
		if s, err := readFLACStream(writeTestFile(t, "random.flac", b)); err != nil || s.whole() {
			t.Fatalf("random tail %d: %+v, %v; want read and not whole", i, s, err)
		}
		cut := whole[:flacStreamInfoLen+r.IntN(len(whole)-flacStreamInfoLen)]
		if s, err := readFLACStream(writeTestFile(t, "cut.flac", cut)); err != nil || s.whole() {
			t.Fatalf("cut to %d of %d: %+v, %v; want read and not whole", len(cut), len(whole), s, err)
		}
	}
}

// TestFLACBlockSizeCodesReadAsTheRFCGivesThem pins every block size code a
// frame header can carry to RFC 9639 section 9.1.1. The fixtures write two
// of them (4096, and a size after the coded number), and a header the
// reader misreads makes a whole rendition read as cut.
func TestFLACBlockSizeCodesReadAsTheRFCGivesThem(t *testing.T) {
	for code, want := range map[int]int{1: 192, 2: 576, 3: 1152, 4: 2304, 5: 4608, 8: 256, 9: 512,
		10: 1024, 11: 2048, 12: 4096, 13: 8192, 14: 16384, 15: 32768} {
		if size, next, ok := flacFrameBlockSize(nil, 7, code); !ok || size != want || next != 7 {
			t.Errorf("block size code %d = %d, %d, %v; want %d, 7, true", code, size, next, ok, want)
		}
	}
	for _, tc := range []struct {
		code       int
		b          []byte
		size, next int
		ok         bool
	}{
		{code: 6, b: []byte{0x7F}, size: 128, next: 1, ok: true},
		{code: 7, b: []byte{0x01, 0x00}, size: 257, next: 2, ok: true},
		{code: 6, b: nil},
		{code: 7, b: []byte{0x01}},
		{code: 0, b: []byte{0x01, 0x00}},
	} {
		size, next, ok := flacFrameBlockSize(tc.b, 0, tc.code)
		if ok != tc.ok || size != tc.size || next != tc.next {
			t.Errorf("block size code %d over %x = %d, %d, %v; want %d, %d, %v", tc.code, tc.b, size, next, ok, tc.size, tc.next, tc.ok)
		}
	}
}

// TestFLACSampleRateCodesReadAsTheRFCGivesThem pins every sample rate code a
// frame header can carry to RFC 9639 section 9.1.2; the fixtures write only
// STREAMINFO's (code 0) and the codes libFLAC picks for 44.1, 96 and
// 176.4 kHz.
func TestFLACSampleRateCodesReadAsTheRFCGivesThem(t *testing.T) {
	for code, want := range map[int]int{0: 12345, 1: 88200, 2: 176400, 3: 192000, 4: 8000, 5: 16000, 6: 22050,
		7: 24000, 8: 32000, 9: 44100, 10: 48000, 11: 96000} {
		if rate, next, ok := flacFrameSampleRate(nil, 3, code, 12345); !ok || rate != want || next != 3 {
			t.Errorf("sample rate code %d = %d, %d, %v; want %d, 3, true", code, rate, next, ok, want)
		}
	}
	for _, tc := range []struct {
		code       int
		b          []byte
		rate, next int
		ok         bool
	}{
		{code: 12, b: []byte{44}, rate: 44000, next: 1, ok: true},
		{code: 13, b: []byte{0xAC, 0x44}, rate: 44100, next: 2, ok: true},
		{code: 14, b: []byte{0x11, 0x3A}, rate: 44100, next: 2, ok: true},
		{code: 12, b: nil},
		{code: 13, b: []byte{0xAC}},
		{code: 14, b: []byte{0x11}},
		{code: 15, b: []byte{0x11, 0x3A}},
	} {
		rate, next, ok := flacFrameSampleRate(tc.b, 0, tc.code, 12345)
		if ok != tc.ok || rate != tc.rate || next != tc.next {
			t.Errorf("sample rate code %d over %x = %d, %d, %v; want %d, %d, %v", tc.code, tc.b, rate, next, ok, tc.rate, tc.next, tc.ok)
		}
	}
}
