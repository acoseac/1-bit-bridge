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
