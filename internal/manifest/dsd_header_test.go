package manifest

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

func TestReadDSFAudioHeaderChecksTheDataChunk(t *testing.T) {
	const rate = 2822400
	samples := int64(rate)
	packed := bytes.Repeat([]byte{0x69}, int(samples/8))
	var well bytes.Buffer
	if err := dsdtone.WriteDSF(&well, rate, 2, packed, samples); err != nil {
		t.Fatal(err)
	}
	h, err := ReadDSFAudioHeader(bytes.NewReader(well.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Check(); err != nil {
		t.Fatal(err)
	}
	if math.Abs(h.DurationSec()-1) > 1e-9 {
		t.Fatalf("duration %.6f", h.DurationSec())
	}

	declared := samples + int64(8*(4096*2+1))
	var bad bytes.Buffer
	if err := dsdtone.WriteDSF(&bad, rate, 2, packed, declared); err != nil {
		t.Fatal(err)
	}
	h, err = ReadDSFAudioHeader(bytes.NewReader(bad.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Check(); !errors.Is(err, ErrDSFAudioMalformed) {
		t.Fatalf("check: %v", err)
	}
	if math.Abs(h.DurationSec()-float64(declared)/rate) > 1e-6 {
		t.Fatalf("declared duration %.6f", h.DurationSec())
	}
}

func TestReadDFFAudioHeaderIgnoresATrailingID3(t *testing.T) {
	const rate = 2822400
	packed := bytes.Repeat([]byte{0x69}, rate/8)
	var body bytes.Buffer
	if err := dsdtone.WriteDFF(&body, rate, 2, packed); err != nil {
		t.Fatal(err)
	}
	raw := append(body.Bytes(), []byte("ID3 ")...)
	raw = append(raw, make([]byte, 8+100)...)
	h, err := ReadDFFAudioHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := h.DurationSec()
	if !ok || math.Abs(d-1) > 1e-9 {
		t.Fatalf("duration %.6f ok=%v", d, ok)
	}
}

func TestAHeaderWithNoAudioDataIsMalformed(t *testing.T) {
	raw := dsfHeaderBytes(2, 2822400, 1, 1000, 4096, 12)
	h, err := ReadDSFAudioHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h.DataBytes != 0 {
		t.Fatalf("data bytes %d", h.DataBytes)
	}
	if err := h.Check(); !errors.Is(err, ErrDSFAudioMalformed) {
		t.Fatalf("check: %v", err)
	}
}

func TestAZeroPayloadDFFNamesNoDuration(t *testing.T) {
	var body bytes.Buffer
	if err := dsdtone.WriteDFF(&body, 2822400, 2, nil); err != nil {
		t.Fatal(err)
	}
	h, err := ReadDFFAudioHeader(bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := h.DurationSec(); ok {
		t.Fatalf("duration %.6f", d)
	}
}

func TestAnUnknownDFFCompressionNamesNoDuration(t *testing.T) {
	raw := dffWithCompression(t, "MP4 ")
	// PROP's declared size is odd, so the chunk is padded before DSD.
	raw = append(raw, 0)
	chunk := make([]byte, 12)
	copy(chunk, "DSD ")
	var size [8]byte
	size[5] = 0x15
	size[6] = 0x8c // 1_411_200 bytes, 2 s of stereo DSD64
	copy(chunk[4:], size[:])
	raw = append(raw, chunk...)
	h, err := ReadDFFAudioHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := h.DurationSec(); ok {
		t.Fatalf("unknown compression reported %.6f", d)
	}
}

func TestReadDFFAudioHeaderLeavesDSTWithoutAChunkDuration(t *testing.T) {
	raw := dffWithCompression(t, "DST ")
	h, err := ReadDFFAudioHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !h.DST {
		t.Fatal("DST was not named")
	}
	if _, ok := h.DurationSec(); ok {
		t.Fatal("DST reported a chunk duration")
	}
}

func dsfHeaderBytes(channels, rate, bits uint32, samples uint64, block uint32, dataSize uint64) []byte {
	buf := make([]byte, 92)
	copy(buf[0:4], "DSD ")
	buf[4] = 28
	copy(buf[28:32], "fmt ")
	buf[32] = 52
	putLE32(buf[52:56], channels)
	putLE32(buf[56:60], rate)
	putLE32(buf[60:64], bits)
	putLE64(buf[64:72], samples)
	putLE32(buf[72:76], block)
	copy(buf[80:84], "data")
	putLE64(buf[84:92], dataSize)
	return buf
}

func putLE32(b []byte, n uint32) {
	b[0] = byte(n)
	b[1] = byte(n >> 8)
	b[2] = byte(n >> 16)
	b[3] = byte(n >> 24)
}

func putLE64(b []byte, n uint64) {
	putLE32(b[0:4], uint32(n))
	putLE32(b[4:8], uint32(n>>32))
}

func dffWithCompression(t *testing.T, fourcc string) []byte {
	t.Helper()
	be := func(n uint64) []byte {
		var b [8]byte
		b[0] = byte(n >> 56)
		b[1] = byte(n >> 48)
		b[2] = byte(n >> 40)
		b[3] = byte(n >> 32)
		b[4] = byte(n >> 24)
		b[5] = byte(n >> 16)
		b[6] = byte(n >> 8)
		b[7] = byte(n)
		return b[:]
	}
	fs := append([]byte("FS  "), be(4)...)
	fs = append(fs, 0x00, 0x2b, 0x11, 0x00) // 2_822_400
	chnl := append([]byte("CHNL"), be(6)...)
	chnl = append(chnl, 0, 2, 'S', 'L', 'F', 'T')
	cmprBody := append([]byte(fourcc), 0)
	cmpr := append([]byte("CMPR"), be(uint64(len(cmprBody)))...)
	cmpr = append(cmpr, cmprBody...)
	propBody := append([]byte("SND "), fs...)
	propBody = append(propBody, chnl...)
	propBody = append(propBody, cmpr...)
	prop := append([]byte("PROP"), be(uint64(len(propBody)))...)
	prop = append(prop, propBody...)
	form := append([]byte("FRM8"), be(uint64(4+len(prop)))...)
	form = append(form, "DSD "...)
	return append(form, prop...)
}
