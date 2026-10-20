package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// openPCM opens rs and requires a PCM Decoder; a DSD source is an error.
func openPCM(rs io.ReadSeeker) (decode.Decoder, error) {
	s, err := decode.Open(rs)
	if err != nil {
		return nil, err
	}
	d, ok := s.(decode.Decoder)
	if !ok {
		s.Close()
		return nil, fmt.Errorf("opened %T, want a PCM Decoder", s)
	}
	return d, nil
}

func readAll(t *testing.T, d decode.Decoder) []int32 {
	t.Helper()
	ctx := context.Background()
	var out []int32
	buf := make([]int32, 256)
	for {
		n, err := d.ReadFrames(ctx, buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrames: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadFrames returned 0 without EOF")
		}
	}
	return out
}

func TestWAVStereo16RoundTrip(t *testing.T) {
	samples := []int32{-32768, 32767, -1, 1, 0, 100, -100, 200}
	raw := writeWAV(t, 44100, 2, 16, 16, samples, false, nil)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pcm := d.Format()
	if pcm.ValidBits != 16 || pcm.Channels != 2 || pcm.Container != format.ContainerWAV {
		t.Fatalf("format %+v", pcm)
	}
	got := readAll(t, d)
	if len(got) != len(samples) {
		t.Fatalf("got %d samples, want %d", len(got), len(samples))
	}
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], samples[i])
		}
	}
}

func TestWAVExtensible24(t *testing.T) {
	samples := []int32{-8388608, 8388607, -1, 1, 0x000001, 0x400000}
	raw := writeWAV(t, 48000, 2, 24, 24, samples, true, nil)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := readAll(t, d)
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], samples[i])
		}
	}
}

func TestWAVChunkOrderBextBeforeFmt(t *testing.T) {
	samples := []int32{10, -10, 20, -20}
	bext := make([]byte, 100)
	copy(bext, []byte("bext-metadata-placeholder"))
	raw := writeWAV(t, 44100, 2, 16, 16, samples, false, bext)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := readAll(t, d)
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], samples[i])
		}
	}
}

func TestWAVFloatRefused(t *testing.T) {
	raw := writeWAVFloat32(t, 44100, 2, []float32{0.1, -0.1, 0.2, -0.2})
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonFloatPCM {
		t.Fatalf("got %v, want float refusal", err)
	}
}

func TestWAVSeekExact(t *testing.T) {
	// 8 frames stereo
	var samples []int32
	for i := int32(0); i < 8; i++ {
		samples = append(samples, i, -i)
	}
	raw := writeWAV(t, 8000, 2, 16, 16, samples, false, nil)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := d.Seek(ctx, 3); err != nil {
		t.Fatal(err)
	}
	buf := make([]int32, 4) // 2 frames
	n, err := d.ReadFrames(ctx, buf)
	if n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if buf[0] != 3 || buf[1] != -3 || buf[2] != 4 || buf[3] != -4 {
		t.Fatalf("got %v", buf)
	}
}

func TestAIFFStereo16(t *testing.T) {
	samples := []int32{-32768, 32767, -1, 1}
	raw := writeAIFF(t, 8000, 2, 16, samples, false)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := readAll(t, d)
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], samples[i])
		}
	}
}

func TestAIFCsowt(t *testing.T) {
	samples := []int32{-1000, 1000, -2, 2}
	raw := writeAIFF(t, 8000, 2, 16, samples, true)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := readAll(t, d)
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], samples[i])
		}
	}
}

func TestAIFCFloatRefused(t *testing.T) {
	raw := writeAIFCCompression(t, "fl32")
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonFloatPCM {
		t.Fatalf("got %v, want float refusal", err)
	}
}

func TestRefuseMP3ByContent(t *testing.T) {
	// Minimal MPEG frame sync, not a real frame.
	raw := []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonLossy {
		t.Fatalf("got %v, want lossy refusal", err)
	}
}

func TestRefuseOggByContent(t *testing.T) {
	raw := []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00")
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonLossy {
		t.Fatalf("got %v, want lossy refusal", err)
	}
}

func TestRefuseMono(t *testing.T) {
	samples := []int32{1, 2, 3, 4}
	raw := writeWAV(t, 44100, 1, 16, 16, samples, false, nil)
	_, err := decode.Open(bytes.NewReader(raw))
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != decode.ReasonUnsupportedChannels {
		t.Fatalf("got %v, want channel refusal", err)
	}
}

// --- writers ---

func writeWAV(t *testing.T, rate, ch, containerBits, validBits int, samples []int32, extensible bool, bext []byte) []byte {
	t.Helper()
	bytesPerSample := containerBits / 8
	data := make([]byte, len(samples)*bytesPerSample)
	for i, s := range samples {
		off := i * bytesPerSample
		switch containerBits {
		case 16:
			binary.LittleEndian.PutUint16(data[off:], uint16(int16(s)))
		case 24:
			u := uint32(s) & 0xFFFFFF
			data[off] = byte(u)
			data[off+1] = byte(u >> 8)
			data[off+2] = byte(u >> 16)
		case 32:
			binary.LittleEndian.PutUint32(data[off:], uint32(s))
		}
	}
	var fmtBody []byte
	blockAlign := uint16(ch * bytesPerSample)
	byteRate := uint32(rate) * uint32(blockAlign)
	if extensible {
		fmtBody = make([]byte, 40)
		binary.LittleEndian.PutUint16(fmtBody[0:], 0xFFFE)
		binary.LittleEndian.PutUint16(fmtBody[2:], uint16(ch))
		binary.LittleEndian.PutUint32(fmtBody[4:], uint32(rate))
		binary.LittleEndian.PutUint32(fmtBody[8:], byteRate)
		binary.LittleEndian.PutUint16(fmtBody[12:], blockAlign)
		binary.LittleEndian.PutUint16(fmtBody[14:], uint16(containerBits))
		binary.LittleEndian.PutUint16(fmtBody[16:], 22)
		binary.LittleEndian.PutUint16(fmtBody[18:], uint16(validBits))
		binary.LittleEndian.PutUint32(fmtBody[20:], 0) // channel mask
		copy(fmtBody[24:], []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
	} else {
		fmtBody = make([]byte, 16)
		binary.LittleEndian.PutUint16(fmtBody[0:], 1)
		binary.LittleEndian.PutUint16(fmtBody[2:], uint16(ch))
		binary.LittleEndian.PutUint32(fmtBody[4:], uint32(rate))
		binary.LittleEndian.PutUint32(fmtBody[8:], byteRate)
		binary.LittleEndian.PutUint16(fmtBody[12:], blockAlign)
		binary.LittleEndian.PutUint16(fmtBody[14:], uint16(containerBits))
	}
	var body []byte
	if bext != nil {
		body = append(body, chunkLE("bext", bext)...)
	}
	body = append(body, chunkLE("fmt ", fmtBody)...)
	body = append(body, chunkLE("data", data)...)
	out := append([]byte("RIFF"), make([]byte, 4)...)
	out = append(out, []byte("WAVE")...)
	out = append(out, body...)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func writeWAVFloat32(t *testing.T, rate, ch int, samples []float32) []byte {
	t.Helper()
	data := make([]byte, len(samples)*4)
	for i, s := range samples {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(s))
	}
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:], 3)
	binary.LittleEndian.PutUint16(fmtBody[2:], uint16(ch))
	binary.LittleEndian.PutUint32(fmtBody[4:], uint32(rate))
	binary.LittleEndian.PutUint16(fmtBody[12:], uint16(ch*4))
	binary.LittleEndian.PutUint16(fmtBody[14:], 32)
	body := append(chunkLE("fmt ", fmtBody), chunkLE("data", data)...)
	out := append([]byte("RIFF"), make([]byte, 4)...)
	out = append(out, []byte("WAVE")...)
	out = append(out, body...)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func writeAIFF(t *testing.T, rate, ch, bits int, samples []int32, sowt bool) []byte {
	t.Helper()
	bytesPerSample := (bits + 7) / 8
	frames := len(samples) / ch
	ssndPayload := make([]byte, 8+len(samples)*bytesPerSample)
	// offset + blockSize already zero
	for i, s := range samples {
		off := 8 + i*bytesPerSample
		switch bytesPerSample {
		case 2:
			if sowt {
				binary.LittleEndian.PutUint16(ssndPayload[off:], uint16(int16(s)))
			} else {
				binary.BigEndian.PutUint16(ssndPayload[off:], uint16(int16(s)))
			}
		case 3:
			u := uint32(s) & 0xFFFFFF
			if sowt {
				ssndPayload[off] = byte(u)
				ssndPayload[off+1] = byte(u >> 8)
				ssndPayload[off+2] = byte(u >> 16)
			} else {
				ssndPayload[off] = byte(u >> 16)
				ssndPayload[off+1] = byte(u >> 8)
				ssndPayload[off+2] = byte(u)
			}
		}
	}
	comm := make([]byte, 18)
	binary.BigEndian.PutUint16(comm[0:], uint16(ch))
	binary.BigEndian.PutUint32(comm[2:], uint32(frames))
	binary.BigEndian.PutUint16(comm[6:], uint16(bits))
	copy(comm[8:], floatToExtended80(float64(rate)))
	formType := "AIFF"
	if sowt {
		formType = "AIFC"
		comm = append(comm, []byte("sowt")...)
		comm = append(comm, 0) // empty pascal name
	}
	body := append([]byte(formType), chunkBE("COMM", comm)...)
	body = append(body, chunkBE("SSND", ssndPayload)...)
	out := append([]byte("FORM"), make([]byte, 4)...)
	out = append(out, body...)
	binary.BigEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func writeAIFCCompression(t *testing.T, comp string) []byte {
	t.Helper()
	comm := make([]byte, 18)
	binary.BigEndian.PutUint16(comm[0:], 2)
	binary.BigEndian.PutUint32(comm[2:], 1)
	binary.BigEndian.PutUint16(comm[6:], 16)
	copy(comm[8:], floatToExtended80(8000))
	comm = append(comm, []byte(comp)...)
	comm = append(comm, 0)
	ssnd := make([]byte, 8+4)
	body := append([]byte("AIFC"), chunkBE("COMM", comm)...)
	body = append(body, chunkBE("SSND", ssnd)...)
	out := append([]byte("FORM"), make([]byte, 4)...)
	out = append(out, body...)
	binary.BigEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func chunkLE(id string, payload []byte) []byte {
	b := append([]byte(id), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	b = append(b, payload...)
	if len(payload)&1 != 0 {
		b = append(b, 0)
	}
	return b
}

func chunkBE(id string, payload []byte) []byte {
	b := append([]byte(id), make([]byte, 4)...)
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload)))
	b = append(b, payload...)
	if len(payload)&1 != 0 {
		b = append(b, 0)
	}
	return b
}

func floatToExtended80(f float64) []byte {
	b := make([]byte, 10)
	if f == 0 {
		return b
	}
	sign := 0
	if f < 0 || math.Signbit(f) {
		sign = 1
		if !math.IsInf(f, 0) && !math.IsNaN(f) {
			f = -f
		}
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		// IEEE-754 extended: exp all-ones; Inf has zero mantissa, NaN non-zero.
		se := uint16(sign<<15) | 0x7FFF
		binary.BigEndian.PutUint16(b[0:], se)
		if math.IsNaN(f) {
			b[2] = 0xc0 // quiet NaN payload
		}
		return b
	}
	frac, exp := math.Frexp(f)
	// frac in [0.5, 1); extended wants [1, 2) with explicit integer bit
	frac *= 2
	exp--
	mant := uint64(frac * (1 << 63))
	se := uint16(sign<<15) | uint16(exp+16383)
	binary.BigEndian.PutUint16(b[0:], se)
	binary.BigEndian.PutUint32(b[2:], uint32(mant>>32))
	binary.BigEndian.PutUint32(b[6:], uint32(mant))
	return b
}
