package dsdtone

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestModulateTracksTheSineOnAverage(t *testing.T) {
	// A first-order SDM's running mean follows the input: over one cycle of
	// a −20 dBFS sine the +1 density must average ~0.5 (zero mean), and the
	// first quarter-cycle must lean positive.
	rate := 2822400
	bits := Modulate(rate, 1.0/1000.0, 0.1, 1000) // exactly one 1 kHz cycle
	if len(bits) != 2822 {
		t.Fatalf("one cycle at DSD64 = 2822 samples, got %d", len(bits))
	}
	ones := 0
	for _, b := range bits {
		ones += int(b)
	}
	if mean := float64(ones) / float64(len(bits)); mean < 0.48 || mean > 0.52 {
		t.Errorf("full-cycle +1 density = %.3f, want ≈ 0.5 (zero-mean sine)", mean)
	}
	quarter := bits[:len(bits)/4]
	q := 0
	for _, b := range quarter {
		q += int(b)
	}
	if qm := float64(q) / float64(len(quarter)); qm <= 0.5 {
		t.Errorf("first quarter-cycle +1 density = %.3f, want > 0.5 (the sine is positive there)", qm)
	}
}

func TestPackBitsOrder(t *testing.T) {
	bits := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}
	if got := PackBits(bits, true); !bytes.Equal(got, []byte{0x01, 0x01}) {
		t.Errorf("LSB-first pack = %x, want 01 01", got)
	}
	if got := PackBits(bits, false); !bytes.Equal(got, []byte{0x80, 0x80}) {
		t.Errorf("MSB-first pack = %x, want 80 80", got)
	}
}

func TestMintDSFLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tone.dsf")
	n, err := MintDSF(path, Tone{RateHz: 2822400, Seconds: 0.01, AmplitudeDBFS: -20})
	if err != nil {
		t.Fatal(err)
	}
	if n != 28224 {
		t.Errorf("sample count = %d, want 28224", n)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if string(b[0:4]) != "DSD " || le.Uint64(b[4:12]) != 28 || le.Uint64(b[12:20]) != uint64(len(b)) || le.Uint64(b[20:28]) != 0 {
		t.Errorf("DSD header wrong: %x", b[:28])
	}
	fmtChunk := b[28:80]
	if string(fmtChunk[0:4]) != "fmt " || le.Uint64(fmtChunk[4:12]) != 52 {
		t.Errorf("fmt header wrong: %x", fmtChunk[:12])
	}
	if le.Uint32(fmtChunk[24:28]) != 2 || le.Uint32(fmtChunk[28:32]) != 2822400 || le.Uint32(fmtChunk[32:36]) != 1 {
		t.Errorf("fmt geometry wrong: channels=%d rate=%d bits=%d", le.Uint32(fmtChunk[24:28]), le.Uint32(fmtChunk[28:32]), le.Uint32(fmtChunk[32:36]))
	}
	if le.Uint64(fmtChunk[36:44]) != 28224 || le.Uint32(fmtChunk[44:48]) != 4096 {
		t.Errorf("fmt sampleCount/block wrong: %d / %d", le.Uint64(fmtChunk[36:44]), le.Uint32(fmtChunk[44:48]))
	}
	data := b[80:]
	if string(data[0:4]) != "data" {
		t.Errorf("data chunk magic wrong: %q", data[0:4])
	}
	// 28224 bits = 3528 bytes → one 4096-byte block per channel, two channels.
	if want := uint64(12 + 2*4096); le.Uint64(data[4:12]) != want || uint64(len(data)) != want {
		t.Errorf("data chunk size = %d (file has %d), want %d", le.Uint64(data[4:12]), len(data), want)
	}
	// Both channels carry the identical block.
	if !bytes.Equal(data[12:12+4096], data[12+4096:12+8192]) {
		t.Error("channel blocks differ; every channel must carry the same bitstream")
	}
}

func TestMintDFFLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tone.dff")
	n, err := MintDFF(path, Tone{RateHz: 2822400, Seconds: 0.01, AmplitudeDBFS: -20})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	be := binary.BigEndian
	if string(b[0:4]) != "FRM8" || be.Uint64(b[4:12]) != uint64(len(b)-12) || string(b[12:16]) != "DSD " {
		t.Errorf("FRM8 header wrong: %x", b[:16])
	}
	if !bytes.Contains(b, []byte("FVER")) || !bytes.Contains(b, []byte("PROP")) || !bytes.Contains(b, []byte("SND ")) ||
		!bytes.Contains(b, []byte("FS  ")) || !bytes.Contains(b, []byte("CHNL")) || !bytes.Contains(b, []byte("CMPR")) {
		t.Error("a required chunk is missing from the DSDIFF body")
	}
	// The DSD sound chunk holds bits/8 bytes per channel, interleaved.
	idx := bytes.LastIndex(b, []byte("DSD "))
	if idx < 0 {
		t.Fatal("no DSD sound chunk")
	}
	if got, want := be.Uint64(b[idx+4:idx+12]), uint64(n/8*2); got != want {
		t.Errorf("DSD chunk size = %d, want %d", got, want)
	}
	payload := b[idx+12:]
	if len(payload) < 2 || payload[0] != payload[1] {
		t.Error("channels must be byte-interleaved with identical bytes")
	}
}

func TestToneValidation(t *testing.T) {
	if _, err := MintDSF(filepath.Join(t.TempDir(), "x.dsf"), Tone{RateHz: 0, Seconds: 1}); err == nil {
		t.Error("a zero rate must be refused")
	}
	if _, err := MintDFF(filepath.Join(t.TempDir(), "x.dff"), Tone{RateHz: 2822400, Seconds: 0.001, Channels: 3}); err == nil {
		t.Error("three channels are unsupported by the DFF writer")
	}
}
