package decode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The ALAC goldens are written by scripts/player-alac-goldens/generate.py:
// afconvert encodes an integer pattern specified there, which alacPattern
// reproduces, and the manifest's hash of the expected samples checks that
// the two agree. Nothing here derives an expectation from the bridge's own
// decoder.

type alacFixture struct {
	File          string  `json:"file"`
	Encoder       string  `json:"encoder"`
	RateHz        int     `json:"rate_hz"`
	Channels      int     `json:"channels"`
	ValidBits     int     `json:"valid_bits"`
	PatternFrames int     `json:"pattern_frames"`
	StartFrame    int     `json:"start_frame"`
	Frames        int64   `json:"frames"`
	SamplesSHA256 string  `json:"samples_sha256"`
	SeekFrames    []int64 `json:"seek_frames"`
	Refusal       string  `json:"refusal"`
}

// alacFFmpegFixture is an FFmpeg encode from Codex's B470 ALAC evaluation:
// its expected audio is the SHA-256 of the packed little-endian PCM that
// Codex encoded, pcm_bytes long.
type alacFFmpegFixture struct {
	File       string `json:"file"`
	Source     string `json:"source"`
	FileSHA256 string `json:"file_sha256"`
	RateHz     int    `json:"rate_hz"`
	Channels   int    `json:"channels"`
	ValidBits  int    `json:"valid_bits"`
	Frames     int64  `json:"frames"`
	PCMBytes   int64  `json:"pcm_bytes"`
	PCMSHA256  string `json:"pcm_sha256"`
}

type alacGoldens struct {
	FrameLength    int                 `json:"frame_length"`
	Block          int                 `json:"block"`
	Fixtures       []alacFixture       `json:"fixtures"`
	FFmpegFixtures []alacFFmpegFixture `json:"ffmpeg_fixtures"`
}

// -alac-goldens points every ALAC golden test at another directory, such as
// a scratch regeneration (generate.py --out DIR), so a change to the
// generator can be checked without touching the committed fixtures:
//
//	go test ./internal/player/decode -args -alac-goldens=DIR
var alacGoldensDir = flag.String("alac-goldens", filepath.Join("testdata", "alac-goldens"),
	"directory of the ALAC goldens and their manifest")

func loadALACGoldens(t testing.TB) alacGoldens {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(*alacGoldensDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g alacGoldens
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Fixtures) == 0 || g.Block != alacPatternBlock {
		t.Fatalf("ALAC golden manifest: %d fixtures, block %d (want %d)", len(g.Fixtures), g.Block, alacPatternBlock)
	}
	return g
}

func (g alacGoldens) fixture(t testing.TB, name string) (alacFixture, []byte) {
	t.Helper()
	for _, f := range g.Fixtures {
		if f.File == name {
			return f, readALACGolden(t, name)
		}
	}
	t.Fatalf("no ALAC golden %s", name)
	return alacFixture{}, nil
}

func readALACGolden(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(*alacGoldensDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decoded returns the fixtures that decode, skipping the refusals.
func (g alacGoldens) decoded() []alacFixture {
	var out []alacFixture
	for _, f := range g.Fixtures {
		if f.Refusal == "" {
			out = append(out, f)
		}
	}
	return out
}

// want is the fixture's expected audio: the pattern from its start frame.
func (f alacFixture) want(t testing.TB) []int32 {
	t.Helper()
	all := alacPattern(f.PatternFrames, f.ValidBits)
	want := all[2*f.StartFrame : 2*(int64(f.StartFrame)+f.Frames)]
	if got := int32SHA256(want); got != f.SamplesSHA256 {
		t.Fatalf("%s: the Go pattern hashes to %s, the generator's to %s", f.File, got, f.SamplesSHA256)
	}
	return want
}

// alacPatternBlock and alacPattern reproduce generate.py's pattern(): the
// kind changes every block of frames, and every operation is on integers so
// the two languages agree to the sample.
const alacPatternBlock = 1499

func alacPattern(frames, bits int) []int32 {
	lo, hi := -(int64(1) << (bits - 1)), int64(1)<<(bits-1)-1
	xl, xr := uint32(0x9E3779B9), uint32(0x85EBCA6B)
	xorshift := func(x uint32) uint32 {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return x
	}
	toSigned := func(u uint32) int64 {
		v := int64(u)
		if v >= int64(1)<<(bits-1) {
			v -= int64(1) << bits
		}
		return v
	}
	tri := func(i, period, amp int64) int64 {
		p, half := i%period, period/2
		if p < half {
			return -amp + (2*amp*p)/half
		}
		return amp - (2*amp*(p-half))/(period-half)
	}
	out := make([]int32, 0, 2*frames)
	for n := 0; n < frames; n++ {
		i := int64(n)
		kind, j := (i/alacPatternBlock)%8, i%alacPatternBlock
		xl, xr = xorshift(xl), xorshift(xr)
		var left, right int64
		switch kind {
		case 0:
			left, right = i%16-8, 7-i%15
		case 1:
			left, right = hi, lo
			if i%2 == 0 {
				left = lo
			}
			if (i/3)%2 == 0 {
				right = hi
			}
		case 2:
			left, right = toSigned(xl>>(32-bits)), toSigned(xr>>(32-bits))
		case 3:
			left, right = tri(i, 97, hi*4/5), tri(i+13, 61, hi/2)
		case 4:
			left = tri(i, 173, hi/3)
			right = left + (i*7)%33 - 16
		case 5:
			if j == 100 {
				left = hi
			}
			if j == 700 {
				right = lo
			}
		case 6:
			left, right = hi-int64(xl&0xFF), lo+int64(xr&0xFF)
		default:
			left, right = -1, j%64-32
			if i%2 == 1 {
				left = 1
			}
		}
		out = append(out, int32(left), int32(right))
	}
	return out
}

func int32SHA256(samples []int32) string {
	h := sha256.New()
	var le [4]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint32(le[:], uint32(s))
		h.Write(le[:])
	}
	return hexLower(h.Sum(nil))
}

// readAllFrames reads d to io.EOF with reads of chunk samples.
func readAllFrames(t *testing.T, d decode.Decoder, chunk int) []int32 {
	t.Helper()
	var out []int32
	buf := make([]int32, chunk)
	for i := 0; ; i++ {
		n, err := d.ReadFrames(context.Background(), buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrames after %d samples: n=%d err=%v", len(out), n, err)
		}
		if n == 0 {
			t.Fatal("ReadFrames returned 0 without EOF")
		}
	}
}

// requireSameSamples reports the first frame where got and want differ.
func requireSameSamples(t *testing.T, got, want []int32) {
	t.Helper()
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			t.Fatalf("frame %d channel %d: got %d want %d", i/2, i%2, got[i], want[i])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
}

func TestALACGoldensMatchPattern(t *testing.T) {
	g := loadALACGoldens(t)
	for _, f := range g.decoded() {
		f := f
		t.Run(f.File, func(t *testing.T) {
			raw := readALACGolden(t, f.File)
			want := f.want(t)
			// One packet, a read that straddles every boundary at a new
			// offset, one frame, and more than the stream.
			for _, chunk := range []int{2 * g.FrameLength, 2 * 777, 2, 1 << 17} {
				d, err := openPCM(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				got := d.Format()
				wantFmt := format.PCM{
					RateHz:    f.RateHz,
					Channels:  f.Channels,
					ValidBits: f.ValidBits,
					Container: format.ContainerALAC,
					Frames:    f.Frames,
				}
				if got != wantFmt {
					t.Fatalf("format %+v, want %+v", got, wantFmt)
				}
				if err := got.Validate(); err != nil {
					t.Fatal(err)
				}
				requireSameSamples(t, readAllFrames(t, d, chunk), want)
				d.Close()
			}
		})
	}
}

func TestALACGoldensExactSeek(t *testing.T) {
	g := loadALACGoldens(t)
	for _, f := range g.decoded() {
		f := f
		t.Run(f.File, func(t *testing.T) {
			want := f.want(t)
			d, err := openPCM(bytes.NewReader(readALACGolden(t, f.File)))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx := context.Background()
			// Forward, then backward, so a seek that works only from the
			// start or only moves forward fails.
			order := append([]int64{}, f.SeekFrames...)
			for i := len(f.SeekFrames) - 1; i >= 0; i-- {
				order = append(order, f.SeekFrames[i])
			}
			buf := make([]int32, 2*16)
			for _, frame := range order {
				if err := d.Seek(ctx, frame); err != nil {
					t.Fatalf("seek %d: %v", frame, err)
				}
				n, err := d.ReadFrames(ctx, buf)
				left := f.Frames - frame
				if left == 0 {
					if n != 0 || !errors.Is(err, io.EOF) {
						t.Fatalf("seek to the end: n=%d err=%v, want 0, EOF", n, err)
					}
					continue
				}
				// An in-range seek must return audio: a broken seek that
				// yields (0, EOF) must not pass.
				wantN := 2 * int(min64(16, left))
				if n != wantN {
					t.Fatalf("seek %d: n=%d err=%v, want %d", frame, n, err, wantN)
				}
				if err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("seek %d: %v", frame, err)
				}
				for k := 0; k < n; k++ {
					if buf[k] != want[2*frame+int64(k)] {
						t.Fatalf("seek %d: frame %d channel %d: got %d want %d",
							frame, frame+int64(k/2), k%2, buf[k], want[2*frame+int64(k)])
					}
				}
			}
			if err := d.Seek(ctx, f.Frames+1); !decode.IsRefusal(err) {
				t.Fatalf("seek past the end: %v, want a refusal", err)
			}
			if err := d.Seek(ctx, -1); !decode.IsRefusal(err) {
				t.Fatalf("seek before the start: %v, want a refusal", err)
			}
		})
	}
}

// One read larger than the stream returns exactly the presented frames and
// io.EOF, the edited timeline's end included, and the next read is (0, EOF).
func TestALACGoldensEOFAtSampleCount(t *testing.T) {
	g := loadALACGoldens(t)
	for _, f := range g.decoded() {
		f := f
		t.Run(f.File, func(t *testing.T) {
			d, err := openPCM(bytes.NewReader(readALACGolden(t, f.File)))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			buf := make([]int32, 2*f.Frames+1000)
			n, err := d.ReadFrames(context.Background(), buf)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("one read of the whole stream: n=%d err=%v, want EOF", n, err)
			}
			requireSameSamples(t, buf[:n], f.want(t))
			if n, err := d.ReadFrames(context.Background(), buf); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read after the end: n=%d err=%v", n, err)
			}
		})
	}
}

func TestALACGoldensRefused(t *testing.T) {
	g := loadALACGoldens(t)
	seen := 0
	for _, f := range g.Fixtures {
		if f.Refusal == "" {
			continue
		}
		seen++
		_, err := decode.Open(bytes.NewReader(readALACGolden(t, f.File)))
		r, ok := decode.AsRefusal(err)
		if !ok || string(r.Reason) != f.Refusal {
			t.Errorf("%s: got %v, want refusal %s", f.File, err, f.Refusal)
		}
	}
	if seen != 3 {
		t.Fatalf("%d refusal fixtures, want 3", seen)
	}
}

// Each golden decodes both kinds of ALAC packet: a compressed channel pair
// and an escape (uncompressed) one, read from the packets' own element
// headers, so neither decode path goes untested by a regeneration.
func TestALACGoldensDecodeBothPacketKinds(t *testing.T) {
	g := loadALACGoldens(t)
	names := []string{}
	for _, f := range g.decoded() {
		names = append(names, f.File)
	}
	for _, f := range g.FFmpegFixtures {
		names = append(names, f.File)
	}
	for _, name := range names {
		raw := readALACGolden(t, name)
		offs, sizes := m4aPacketSpans(t, parseM4ANodes(t, raw))
		kinds := map[byte]int{}
		for i, off := range offs {
			if sizes[i] < 3 {
				t.Fatalf("%s: packet %d is %d bytes", name, i, sizes[i])
			}
			hdr := uint32(raw[off])<<16 | uint32(raw[off+1])<<8 | uint32(raw[off+2])
			if tag := hdr >> 21; tag != 1 {
				t.Fatalf("%s: packet %d opens with element %d, want a channel pair", name, i, tag)
			}
			kinds[byte(hdr>>1&1)]++
		}
		if kinds[0] == 0 || kinds[1] == 0 {
			t.Errorf("%s: %d compressed and %d escape packets, want both", name, kinds[0], kinds[1])
		}
	}
}

// The FFmpeg encodes decode to the PCM Codex encoded, packed little-endian
// at the stream's depth, byte for byte by SHA-256.
func TestALACFFmpegFixtures(t *testing.T) {
	g := loadALACGoldens(t)
	if len(g.FFmpegFixtures) != 2 {
		t.Fatalf("%d FFmpeg fixtures, want 2", len(g.FFmpegFixtures))
	}
	for _, f := range g.FFmpegFixtures {
		f := f
		t.Run(f.File, func(t *testing.T) {
			raw := readALACGolden(t, f.File)
			if sum := sha256.Sum256(raw); hexLower(sum[:]) != f.FileSHA256 {
				t.Fatalf("the fixture's bytes hash to %x, the manifest records %s", sum, f.FileSHA256)
			}
			d, err := openPCM(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			fm := d.Format()
			if fm.RateHz != f.RateHz || fm.Channels != f.Channels || fm.ValidBits != f.ValidBits || fm.Frames != f.Frames {
				t.Fatalf("format %+v, want %d Hz, %d channels, %d bits, %d frames", fm, f.RateHz, f.Channels, f.ValidBits, f.Frames)
			}
			pcm := packLE(readAllFrames(t, d, 4096), f.ValidBits)
			if int64(len(pcm)) != f.PCMBytes {
				t.Fatalf("%d PCM bytes, want %d", len(pcm), f.PCMBytes)
			}
			if sum := sha256.Sum256(pcm); hexLower(sum[:]) != f.PCMSHA256 {
				t.Fatalf("PCM hashes to %x, want %s", sum, f.PCMSHA256)
			}
		})
	}
}

// packLE packs samples as little-endian integers of bits/8 bytes, the form
// FFmpeg's s16le, s24le and s32le write.
func packLE(samples []int32, bits int) []byte {
	width := bits / 8
	out := make([]byte, 0, len(samples)*width)
	for _, s := range samples {
		for b := 0; b < width; b++ {
			out = append(out, byte(uint32(s)>>(8*b)))
		}
	}
	return out
}
