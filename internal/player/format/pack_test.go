package format_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

func TestPackS16LE(t *testing.T) {
	samples := []int32{-32768, -1, 0, 1, 32767}
	dst := make([]byte, format.PackedBytes(len(samples), format.S16LE))
	n, err := format.Pack(dst, samples, 16, format.S16LE)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("packed %d bytes, want 10", n)
	}
	want := []byte{
		0x00, 0x80, // -32768
		0xFF, 0xFF, // -1
		0x00, 0x00, // 0
		0x01, 0x00, // 1
		0xFF, 0x7F, // 32767
	}
	if !bytes.Equal(dst, want) {
		t.Fatalf("got %x, want %x", dst, want)
	}
}

func TestPackS243LE(t *testing.T) {
	samples := []int32{-8388608, -1, 0, 1, 8388607}
	dst := make([]byte, format.PackedBytes(len(samples), format.S243LE))
	if _, err := format.Pack(dst, samples, 24, format.S243LE); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00, 0x00, 0x80,
		0xFF, 0xFF, 0xFF,
		0x00, 0x00, 0x00,
		0x01, 0x00, 0x00,
		0xFF, 0xFF, 0x7F,
	}
	if !bytes.Equal(dst, want) {
		t.Fatalf("got %x, want %x", dst, want)
	}
}

func TestPackS24LELow24(t *testing.T) {
	samples := []int32{-8388608, -1, 1, 8388607}
	dst := make([]byte, format.PackedBytes(len(samples), format.S24LE))
	if _, err := format.Pack(dst, samples, 24, format.S24LE); err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		got := binary.LittleEndian.Uint32(dst[i*4:])
		want := uint32(s) & 0xFFFFFF
		if got != want {
			t.Fatalf("sample %d: got %#08x, want %#08x (high byte must be zero)", i, got, want)
		}
		if dst[i*4+3] != 0 {
			t.Fatalf("sample %d: high byte %#02x, want 0", i, dst[i*4+3])
		}
	}
}

func TestPackS32LEFrom24ShiftsLeft8(t *testing.T) {
	// Low-bit activity: every source bit must survive in the high 24.
	samples := []int32{0x000001, 0x000002, 0x000004, 0x400000, -1, -8388608}
	dst := make([]byte, format.PackedBytes(len(samples), format.S32LE))
	if _, err := format.Pack(dst, samples, 24, format.S32LE); err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		got := int32(binary.LittleEndian.Uint32(dst[i*4:]))
		want := s << 8
		if got != want {
			t.Fatalf("sample %d: got %#08x, want %#08x", i, uint32(got), uint32(want))
		}
		if dst[i*4] != 0 {
			t.Fatalf("sample %d: low byte %#02x, want 0", i, dst[i*4])
		}
	}
}

func TestPackS32LEFrom32(t *testing.T) {
	samples := []int32{int32(-1 << 31), -1, 0, 1, 0x7FFFFFFF}
	dst := make([]byte, format.PackedBytes(len(samples), format.S32LE))
	if _, err := format.Pack(dst, samples, 32, format.S32LE); err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		got := int32(binary.LittleEndian.Uint32(dst[i*4:]))
		if got != s {
			t.Fatalf("sample %d: got %#08x, want %#08x", i, uint32(got), uint32(s))
		}
	}
}

// TestPackSixteenBitsInEveryWiderContainer pins B735's widening byte by
// byte: 16-bit samples in S24_3LE and S24_LE shifted left eight, the low
// byte zero (and S24_LE's high byte zero, for a negative sample too: its
// sign is bit 23), and in S32_LE shifted left sixteen, the low two bytes
// zero. -0x1235 is 0xedcb, a negative sample whose two bytes differ, so a
// byte out of its place or a shift of the wrong size shows.
func TestPackSixteenBitsInEveryWiderContainer(t *testing.T) {
	samples := []int32{-32768, -0x1235, -1, 0, 1, 0x1234, 32767}
	for _, c := range []struct {
		f    format.ALSAFormat
		want []byte
	}{
		{format.S16LE, []byte{
			0x00, 0x80, 0xcb, 0xed, 0xff, 0xff, 0x00, 0x00, 0x01, 0x00, 0x34, 0x12, 0xff, 0x7f}},
		{format.S243LE, []byte{
			0x00, 0x00, 0x80, 0x00, 0xcb, 0xed, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00,
			0x00, 0x01, 0x00, 0x00, 0x34, 0x12, 0x00, 0xff, 0x7f}},
		{format.S24LE, []byte{
			0x00, 0x00, 0x80, 0x00, 0x00, 0xcb, 0xed, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x01, 0x00, 0x00, 0x00, 0x34, 0x12, 0x00, 0x00, 0xff, 0x7f, 0x00}},
		{format.S32LE, []byte{
			0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0xcb, 0xed, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x34, 0x12, 0x00, 0x00, 0xff, 0x7f}},
	} {
		dst := make([]byte, format.PackedBytes(len(samples), c.f))
		n, err := format.Pack(dst, samples, 16, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.f, err)
		}
		if n != len(c.want) || !bytes.Equal(dst, c.want) {
			t.Errorf("16 bits in %s: % x, want % x", c.f, dst[:n], c.want)
		}
	}
}

// TestPackEverySixteenBitValueKeepsItsValue packs every 16-bit value in
// every container that takes 16 bits and reads it back by arithmetic, apart
// from the packer: the container's signed value is the sample times 2^8 in
// S24_3LE and S24_LE and times 2^16 in S32_LE, which is the same value at
// the container's full scale (lossless widening), with every bit below the
// sample's zero, and S24_LE's top byte zero.
func TestPackEverySixteenBitValueKeepsItsValue(t *testing.T) {
	samples := make([]int32, 0, 1<<16)
	for v := -32768; v <= 32767; v++ {
		samples = append(samples, int32(v))
	}
	for _, f := range []format.ALSAFormat{format.S16LE, format.S243LE, format.S24LE, format.S32LE} {
		dst := make([]byte, format.PackedBytes(len(samples), f))
		if _, err := format.Pack(dst, samples, 16, f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for i, s := range samples {
			var got, want int64
			switch f {
			case format.S16LE:
				got, want = int64(int16(binary.LittleEndian.Uint16(dst[2*i:]))), int64(s)
			case format.S243LE:
				b := dst[3*i : 3*i+3]
				u := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16
				if u >= 1<<23 {
					u -= 1 << 24
				}
				got, want = u, int64(s)*256
				if b[0] != 0 {
					t.Fatalf("%s: %d has a low byte %#x", f, s, b[0])
				}
			case format.S24LE:
				b := dst[4*i : 4*i+4]
				u := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16
				if u >= 1<<23 {
					u -= 1 << 24
				}
				got, want = u, int64(s)*256
				if b[0] != 0 || b[3] != 0 {
					t.Fatalf("%s: %d is % x: the low byte and the high byte are zero", f, s, b)
				}
			case format.S32LE:
				b := dst[4*i : 4*i+4]
				got, want = int64(int32(binary.LittleEndian.Uint32(b))), int64(s)*65536
				if b[0] != 0 || b[1] != 0 {
					t.Fatalf("%s: %d is % x: the low two bytes are zero", f, s, b)
				}
			}
			if got != want {
				t.Fatalf("16 bits in %s: %d packed as %d, want %d", f, s, got, want)
			}
		}
	}
}

func TestPackRefusesMismatchedBits(t *testing.T) {
	dst := make([]byte, 16)
	for _, c := range []struct {
		bits int
		f    format.ALSAFormat
	}{
		{24, format.S16LE}, {32, format.S16LE}, {32, format.S243LE}, {32, format.S24LE},
		{8, format.S16LE}, {8, format.S32LE}, {12, format.S24LE}, {20, format.S243LE}, {20, format.S32LE},
	} {
		if _, err := format.Pack(dst, []int32{1}, c.bits, c.f); err == nil {
			t.Errorf("packed %d-bit samples into %s", c.bits, c.f)
		}
	}
}

func TestPCMValidate(t *testing.T) {
	ok := format.PCM{RateHz: 44100, Channels: 2, ValidBits: 24, Container: format.ContainerFLAC}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []format.PCM{
		{RateHz: 44100, Channels: 1, ValidBits: 16},
		{RateHz: 44100, Channels: 2, ValidBits: 12},
		{RateHz: 0, Channels: 2, ValidBits: 16},
		{RateHz: 44100, Channels: 2, ValidBits: 16, Frames: -1},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Fatalf("expected error for %+v", p)
		}
	}
}
