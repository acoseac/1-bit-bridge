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

func TestPackRefusesMismatchedBits(t *testing.T) {
	dst := make([]byte, 16)
	if _, err := format.Pack(dst, []int32{1}, 24, format.S16LE); err == nil {
		t.Fatal("expected error packing 24-bit into S16_LE")
	}
	if _, err := format.Pack(dst, []int32{1}, 16, format.S243LE); err == nil {
		t.Fatal("expected error packing 16-bit into S24_3LE")
	}
	if _, err := format.Pack(dst, []int32{1}, 16, format.S32LE); err == nil {
		t.Fatal("expected error packing 16-bit into S32_LE")
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
