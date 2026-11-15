package testtone

import (
	"math"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/tonebounds"
)

// loudest is the tone's ceiling as this test writes it, apart from the
// package's constant: a constant raised above it fails the test.
const loudest = -30.0

// packings are every container and valid bits the format package's packer
// takes, which bridge output pcm-test's --format and --bits offer
// (pcmTestPacks) and the player's backend opens a subset of.
func packings(t *testing.T) []struct {
	f    format.ALSAFormat
	bits int
} {
	t.Helper()
	var out []struct {
		f    format.ALSAFormat
		bits int
	}
	for _, f := range []format.ALSAFormat{format.S16LE, format.S243LE, format.S24LE, format.S32LE} {
		for _, bits := range []int{16, 24, 32} {
			dst := make([]byte, format.PackedBytes(2, f))
			if _, err := format.Pack(dst, []int32{0, 0}, bits, f); err == nil {
				out = append(out, struct {
					f    format.ALSAFormat
					bits int
				}{f, bits})
			}
		}
	}
	if len(out) != 8 {
		t.Fatalf("the packer takes %d containers and depths, want the eight of B470's table", len(out))
	}
	return out
}

// containers are each container's significant bits and its bytes, and where a
// sample's marker byte sits in them: the top byte of its 24 most significant
// bits, a 16-bit container's top byte.
var containers = map[format.ALSAFormat]struct{ width, size, marker int }{
	format.S16LE:  {16, 2, 1},
	format.S243LE: {24, 3, 2},
	format.S24LE:  {24, 4, 2},
	format.S32LE:  {32, 4, 3},
}

// sample is the nth sample of a tone packed as f carrying bits valid bits,
// read back by arithmetic, apart from the packer: the container's significant
// bytes as a signed value, divided by 2 to the bits below the sample's.
func sample(t *testing.T, packed []byte, n int, f format.ALSAFormat, bits int) int64 {
	t.Helper()
	c := containers[f]
	width, size := c.width, c.size
	b := packed[n*size : (n+1)*size]
	var u uint64
	for j := range width / 8 {
		u |= uint64(b[j]) << (8 * j)
	}
	v := int64(u)
	if u >= 1<<(width-1) {
		v -= 1 << width
	}
	scale := int64(1) << (width - bits)
	if v%scale != 0 {
		t.Fatalf("%s sample %d carrying %d bits is %#x: the bits below the sample's are not zero", f, n, bits, u)
	}
	return v / scale
}

// TestTheToneRefusesALevelAboveMinus30 (B601): the bounds' Check and Build
// refuse every level above -30 dBFS, below -90 and every value that is no
// number, and take -30, -40 and -90, and CheckSeconds every length outside 1
// to 30 seconds; the ceiling is this test's own literal, so a constant raised
// past it fails here.
func TestTheToneRefusesALevelAboveMinus30(t *testing.T) {
	for _, level := range []float64{-29.99, -20, 0, 6, -90.01, math.NaN(), math.Inf(1), math.Inf(-1), math.Nextafter(loudest, 0)} {
		if err := tonebounds.Check(level); err == nil {
			t.Errorf("tonebounds.Check(%g) took it: the tone is never louder than %g dBFS", level, loudest)
		}
		if _, err := Build(48000, 24, format.S243LE, level); err == nil {
			t.Errorf("Build at %g dBFS built a tone: it asks tonebounds.Check first", level)
		}
	}
	for _, level := range []float64{loudest, -40, -90} {
		if err := tonebounds.Check(level); err != nil {
			t.Errorf("tonebounds.Check(%g) refused it: %v", level, err)
		}
		if _, err := Build(48000, 24, format.S243LE, level); err != nil {
			t.Errorf("Build at %g dBFS: %v", level, err)
		}
	}
	if tonebounds.Loudest > loudest {
		t.Errorf("tonebounds.Loudest is %g dBFS, above %g", tonebounds.Loudest, loudest)
	}
	for _, s := range []int{0, -1, 31, 600} {
		if tonebounds.CheckSeconds(s) == nil {
			t.Errorf("tonebounds.CheckSeconds(%d) took it: the console's tone lasts 1 to 30 seconds", s)
		}
	}
	for _, s := range []int{1, 10, 30} {
		if err := tonebounds.CheckSeconds(s); err != nil {
			t.Errorf("tonebounds.CheckSeconds(%d): %v", s, err)
		}
	}
}

// TestTheToneNeverCarriesADoPMarker (B601; the design's round 1, appendix A):
// at -30 dBFS, at every rate from 44.1 to 384 kHz and in every container and
// depth the packer takes, no frame's marker byte, the top byte of a sample's
// 24 most significant bits (a 16-bit sample's top byte), is 0x05 or 0xFA, the
// two DoP markers: a DAC that detects DoP finds none in the tone.
func TestTheToneNeverCarriesADoPMarker(t *testing.T) {
	for _, rate := range []int{44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000} {
		for _, p := range packings(t) {
			tone, err := Build(rate, p.bits, p.f, loudest)
			if err != nil {
				t.Fatalf("%s/%d at %d Hz: %v", p.f, p.bits, rate, err)
			}
			c := containers[p.f]
			for i := 0; i+c.size <= len(tone.Packed); i += c.size {
				if b := tone.Packed[i+c.marker]; b == 0x05 || b == 0xFA {
					t.Fatalf("%s/%d at %d Hz: sample %d's marker byte is %#02x, a DoP marker", p.f, p.bits, rate, i/c.size, b)
				}
			}
		}
	}
}

// TestTheToneIsTheSineItsLevelNames (B601): Build's samples are this test's
// own sine, its amplitude computed here from the literal level (floor of full
// scale times 10 to the level over 20), each sample rounded, both channels
// alike, in whole 1 kHz cycles that repeat without a seam, in every container
// and depth the packer takes, so no gain hides in the builder.
func TestTheToneIsTheSineItsLevelNames(t *testing.T) {
	for _, level := range []float64{loudest, -40} {
		for _, rate := range []int{44100, 48000, 96000} {
			for _, p := range packings(t) {
				tone, err := Build(rate, p.bits, p.f, level)
				if err != nil {
					t.Fatalf("%s/%d at %d Hz, %g dBFS: %v", p.f, p.bits, rate, level, err)
				}
				full := int64(1)<<(p.bits-1) - 1
				amp := int64(math.Floor(float64(full) * math.Pow(10, level/20)))
				if tone.Amplitude != amp || tone.FullScale != full {
					t.Errorf("%s/%d at %g dBFS: amplitude %d of %d, want %d of %d", p.f, p.bits, level, tone.Amplitude, tone.FullScale, amp, full)
				}
				if tone.Frames*tonebounds.ToneHz%rate != 0 || tone.Frames*1000%rate != 0 {
					t.Errorf("%d frames at %d Hz is not a whole number of 1 kHz cycles", tone.Frames, rate)
				}
				want := func(n int) int64 {
					return int64(math.Round(float64(amp) * math.Sin(2*math.Pi*1000*float64(n)/float64(rate))))
				}
				if want(tone.Frames) != want(0) {
					t.Errorf("%s/%d at %d Hz: the sample after the last is %d, the first %d: the repeat has a seam", p.f, p.bits, rate, want(tone.Frames), want(0))
				}
				if size := 2 * tone.Frames * containers[p.f].size; len(tone.Packed) != size {
					t.Fatalf("%s/%d at %d Hz: %d bytes packed, want %d frames of two samples, %d bytes", p.f, p.bits, rate, len(tone.Packed), tone.Frames, size)
				}
				for n := range tone.Frames {
					l, r := sample(t, tone.Packed, 2*n, p.f, p.bits), sample(t, tone.Packed, 2*n+1, p.f, p.bits)
					if l != want(n) || r != want(n) {
						t.Fatalf("%s/%d at %d Hz, %g dBFS, frame %d: %d and %d, want %d", p.f, p.bits, rate, level, n, l, r, want(n))
					}
				}
			}
		}
	}
}
