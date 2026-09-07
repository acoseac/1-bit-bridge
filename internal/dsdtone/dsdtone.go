// Package dsdtone mints small DSF / DSDIFF fixtures from a first-order
// sigma-delta modulator.
//
// It is test-oriented but a normal package on purpose: the manifest
// extractors (which must PARSE what it writes) and the transcode render
// (which must DECODE it) live in different packages, and a `_test.go`
// helper cannot be shared across them — two copies of a container writer
// is exactly the drift this avoids. Nothing in production imports it.
//
// The modulator is deliberately FIRST-ORDER: a few dozen lines, no
// dependencies, and enough to exercise container parsing, decode routing,
// level parity and the clip-guarded gain. Its in-band noise floor sits
// around −45 dBFS, so it cannot answer alias / stopband questions — those
// need the higher-order fixtures of the measurement suite.
//
// Layouts are the ones the bridge's own extractors read (and that ffmpeg's
// dsf / iff demuxers accept — measured 2026-09-07): DSF is little-endian
// with a 28-byte `DSD ` header, a 52-byte `fmt ` chunk and a `data` chunk
// holding LSB-first bits block-interleaved at 4096 bytes per channel; DSDIFF
// is a big-endian FRM8 form with FVER / PROP(SND: FS, CHNL, CMPR) / DSD
// chunks holding MSB-first bits byte-interleaved across channels.
package dsdtone

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Tone describes a fixture: a single sine at AmplitudeDBFS (peak, relative
// to full scale) modulated at RateHz. Every channel carries the identical
// bitstream.
type Tone struct {
	RateHz        int     // nominal DSD rate, e.g. 2822400 for DSD64
	Seconds       float64 // duration
	AmplitudeDBFS float64 // sine peak, e.g. -20
	ToneHz        float64 // 0 → 1000
	Channels      int     // 0 → 2; 1 or 2 supported
}

func (t Tone) normalized() (Tone, error) {
	if t.RateHz <= 0 || t.Seconds <= 0 {
		return t, fmt.Errorf("dsdtone: rate and seconds must be positive (%d, %g)", t.RateHz, t.Seconds)
	}
	if t.ToneHz <= 0 {
		t.ToneHz = 1000
	}
	if t.Channels == 0 {
		t.Channels = 2
	}
	if t.Channels < 1 || t.Channels > 2 {
		return t, fmt.Errorf("dsdtone: %d channels unsupported (1 or 2)", t.Channels)
	}
	return t, nil
}

// Modulate runs a first-order sigma-delta modulator over a sine and returns
// one byte per DSD sample: 1 for +1, 0 for −1.
func Modulate(rateHz int, seconds, amplitude, toneHz float64) []byte {
	n := int(float64(rateHz) * seconds)
	if n <= 0 {
		return nil
	}
	bits := make([]byte, n)
	integ, y := 0.0, 1.0
	w := 2 * math.Pi * toneHz / float64(rateHz)
	for i := range n {
		x := amplitude * math.Sin(w*float64(i))
		integ += x - y
		if integ >= 0 {
			y = 1
		} else {
			y = -1
		}
		if y > 0 {
			bits[i] = 1
		}
	}
	return bits
}

// PackBits packs 1-bit samples into bytes, eight per byte. DSF stores the
// first sample in the LEAST significant bit; DSDIFF in the MOST.
func PackBits(bits []byte, lsbFirst bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b == 0 {
			continue
		}
		if lsbFirst {
			out[i>>3] |= 1 << (i & 7)
		} else {
			out[i>>3] |= 0x80 >> (i & 7)
		}
	}
	return out
}

// dsfBlockSize is the per-channel block size every real encoder writes.
const dsfBlockSize = 4096

// WriteDSF writes a DSF file whose every channel carries `packed` (LSB-first
// packed bits). sampleCount is the per-channel sample count the `fmt ` chunk
// declares; the trailing partial block is zero-padded, and readers that
// honour sampleCount (ffmpeg's demuxer does) never decode the padding.
func WriteDSF(w io.Writer, rateHz, channels int, packed []byte, sampleCount int64) error {
	if channels < 1 || channels > 6 {
		return fmt.Errorf("dsdtone: dsf channels %d out of range", channels)
	}
	nblocks := (len(packed) + dsfBlockSize - 1) / dsfBlockSize
	padded := make([]byte, nblocks*dsfBlockSize)
	copy(padded, packed)
	dataLen := uint64(len(padded)) * uint64(channels)
	dataChunkLen := 12 + dataLen
	const fmtLen = 52
	total := 28 + fmtLen + dataChunkLen

	bw := bufio.NewWriterSize(w, 1<<20)
	le := binary.LittleEndian
	var hdr [28]byte
	copy(hdr[0:4], "DSD ")
	le.PutUint64(hdr[4:12], 28)
	le.PutUint64(hdr[12:20], total)
	le.PutUint64(hdr[20:28], 0) // no metadata (ID3) chunk
	if _, err := bw.Write(hdr[:]); err != nil {
		return err
	}
	channelType := uint32(1) // mono
	if channels == 2 {
		channelType = 2 // stereo
	}
	var fmtChunk [fmtLen]byte
	copy(fmtChunk[0:4], "fmt ")
	le.PutUint64(fmtChunk[4:12], fmtLen)
	le.PutUint32(fmtChunk[12:16], 1) // format version
	le.PutUint32(fmtChunk[16:20], 0) // format id: DSD raw
	le.PutUint32(fmtChunk[20:24], channelType)
	le.PutUint32(fmtChunk[24:28], uint32(channels))
	le.PutUint32(fmtChunk[28:32], uint32(rateHz))
	le.PutUint32(fmtChunk[32:36], 1) // bits per sample
	le.PutUint64(fmtChunk[36:44], uint64(sampleCount))
	le.PutUint32(fmtChunk[44:48], dsfBlockSize)
	le.PutUint32(fmtChunk[48:52], 0) // reserved
	if _, err := bw.Write(fmtChunk[:]); err != nil {
		return err
	}
	var dataHdr [12]byte
	copy(dataHdr[0:4], "data")
	le.PutUint64(dataHdr[4:12], dataChunkLen)
	if _, err := bw.Write(dataHdr[:]); err != nil {
		return err
	}
	for b := 0; b < nblocks; b++ {
		block := padded[b*dsfBlockSize : (b+1)*dsfBlockSize]
		for c := 0; c < channels; c++ {
			if _, err := bw.Write(block); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// WriteDFF writes an uncompressed DSDIFF file whose every channel carries
// `packed` (MSB-first packed bits), byte-interleaved across channels.
func WriteDFF(w io.Writer, rateHz, channels int, packed []byte) error {
	if channels < 1 || channels > 2 {
		return fmt.Errorf("dsdtone: dff channels %d out of range", channels)
	}
	be := binary.BigEndian
	chunk := func(id string, payload []byte) []byte {
		out := make([]byte, 0, 12+len(payload)+1)
		out = append(out, id...)
		out = be.AppendUint64(out, uint64(len(payload)))
		out = append(out, payload...)
		if len(payload)%2 == 1 {
			out = append(out, 0) // IFF pad byte
		}
		return out
	}
	fver := chunk("FVER", be.AppendUint32(nil, 0x01050000))
	fs := chunk("FS  ", be.AppendUint32(nil, uint32(rateHz)))
	ids := []string{"SLFT", "SRGT"}[:channels]
	chnlBody := be.AppendUint16(nil, uint16(channels))
	for _, id := range ids {
		chnlBody = append(chnlBody, id...)
	}
	chnl := chunk("CHNL", chnlBody)
	name := "not compressed"
	cmprBody := append([]byte("DSD "), byte(len(name)))
	cmprBody = append(cmprBody, name...)
	if len(cmprBody)%2 == 1 {
		cmprBody = append(cmprBody, 0)
	}
	cmpr := chunk("CMPR", cmprBody)
	propBody := append([]byte("SND "), fs...)
	propBody = append(propBody, chnl...)
	propBody = append(propBody, cmpr...)
	prop := chunk("PROP", propBody)

	dataLen := uint64(len(packed)) * uint64(channels)
	bodyLen := uint64(4+len(fver)+len(prop)) + 12 + dataLen + dataLen%2

	bw := bufio.NewWriterSize(w, 1<<20)
	var frm8 [12]byte
	copy(frm8[0:4], "FRM8")
	be.PutUint64(frm8[4:12], bodyLen)
	if _, err := bw.Write(frm8[:]); err != nil {
		return err
	}
	if _, err := bw.WriteString("DSD "); err != nil {
		return err
	}
	for _, part := range [][]byte{fver, prop} {
		if _, err := bw.Write(part); err != nil {
			return err
		}
	}
	var dsdHdr [12]byte
	copy(dsdHdr[0:4], "DSD ")
	be.PutUint64(dsdHdr[4:12], dataLen)
	if _, err := bw.Write(dsdHdr[:]); err != nil {
		return err
	}
	for _, b := range packed {
		for c := 0; c < channels; c++ {
			if err := bw.WriteByte(b); err != nil {
				return err
			}
		}
	}
	if dataLen%2 == 1 {
		if err := bw.WriteByte(0); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// MintDSF modulates t and writes it as a DSF file at path. Returns the
// per-channel sample count the header declares.
func MintDSF(path string, t Tone) (int64, error) {
	t, err := t.normalized()
	if err != nil {
		return 0, err
	}
	bits := Modulate(t.RateHz, t.Seconds, math.Pow(10, t.AmplitudeDBFS/20), t.ToneHz)
	if len(bits) == 0 {
		return 0, errors.New("dsdtone: empty tone")
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	if err := WriteDSF(f, t.RateHz, t.Channels, PackBits(bits, true), int64(len(bits))); err != nil {
		_ = f.Close()
		return 0, err
	}
	return int64(len(bits)), f.Close()
}

// MintDFF modulates t and writes it as an uncompressed DSDIFF file at path.
// Returns the per-channel sample count (the bit count; the container does
// not declare one).
func MintDFF(path string, t Tone) (int64, error) {
	t, err := t.normalized()
	if err != nil {
		return 0, err
	}
	bits := Modulate(t.RateHz, t.Seconds, math.Pow(10, t.AmplitudeDBFS/20), t.ToneHz)
	if len(bits) == 0 {
		return 0, errors.New("dsdtone: empty tone")
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	if err := WriteDFF(f, t.RateHz, t.Channels, PackBits(bits, false)); err != nil {
		_ = f.Close()
		return 0, err
	}
	return int64(len(bits)), f.Close()
}
