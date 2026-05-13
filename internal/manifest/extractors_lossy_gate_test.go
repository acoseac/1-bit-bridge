package manifest

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeMinimalDSFWithBits is a fixture variant of writeMinimalDSF
// (testdata_test.go) that accepts an arbitrary `bitsPerSample` value
// for the DSF fmt chunk. Used by PR-A2 tests to exercise the
// anomalous-bits + PCM-like-rate matrix that writeMinimalDSF (which
// hardcodes bits=1) can't reach.
func writeMinimalDSFWithBits(t *testing.T, sampleRate, bitsPerSample uint32) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.dsf")

	var buf bytes.Buffer
	var fmtChunk [52]byte
	copy(fmtChunk[0:4], []byte("fmt "))
	binary.LittleEndian.PutUint64(fmtChunk[4:12], 52)
	binary.LittleEndian.PutUint32(fmtChunk[12:16], 1)
	binary.LittleEndian.PutUint32(fmtChunk[16:20], 0)
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 2)
	binary.LittleEndian.PutUint32(fmtChunk[24:28], 2)
	binary.LittleEndian.PutUint32(fmtChunk[28:32], sampleRate)
	binary.LittleEndian.PutUint32(fmtChunk[32:36], bitsPerSample)
	binary.LittleEndian.PutUint64(fmtChunk[36:44], uint64(sampleRate*5))
	binary.LittleEndian.PutUint32(fmtChunk[44:48], 4096)

	var dataHeader [12]byte
	copy(dataHeader[0:4], []byte("data"))
	binary.LittleEndian.PutUint64(dataHeader[4:12], 12)

	totalSize := uint64(28 + 52 + 12)
	metadataPointer := uint64(0)

	var dsd [28]byte
	copy(dsd[0:4], []byte("DSD "))
	binary.LittleEndian.PutUint64(dsd[4:12], 28)
	binary.LittleEndian.PutUint64(dsd[12:20], totalSize)
	binary.LittleEndian.PutUint64(dsd[20:28], metadataPointer)

	buf.Write(dsd[:])
	buf.Write(fmtChunk[:])
	buf.Write(dataHeader[:])

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write DSF fixture: %v", err)
	}
	return path
}

// TestIsLossyCodec_TruthTable pins the canonical lossy / lossless
// classification used by every `t.BitsPerSample` write site as a
// defense-in-depth gate against the iOS PR #371 "M4A 32-bit" chip
// regression.
func TestIsLossyCodec_TruthTable(t *testing.T) {
	t.Helper()
	lossy := []string{"AAC", "MP3", "OGG", "OPUS", "WMA", "aac", "mp3"}
	lossless := []string{"FLAC", "ALAC", "DSF", "DFF", "WAV", "AIFF", "", "PCM"}
	for _, c := range lossy {
		if !isLossyCodec(c) {
			t.Errorf("isLossyCodec(%q) = false, want true", c)
		}
	}
	for _, c := range lossless {
		if isLossyCodec(c) {
			t.Errorf("isLossyCodec(%q) = true, want false", c)
		}
	}
}

// TestIsValidDSDSampleRate_TruthTable pins the DSD-rate sanity floor
// the DSF extractor uses to refuse the default-true IsDSD flip on
// PCM-like sample rates.
func TestIsValidDSDSampleRate_TruthTable(t *testing.T) {
	validDSDRates := []uint32{
		2_822_400,  // DSD64 (44.1k base)
		3_072_000,  // DSD64 (48k base)
		5_644_800,  // DSD128 (44.1k base)
		6_144_000,  // DSD128 (48k base)
		11_289_600, // DSD256 (44.1k base)
		12_288_000, // DSD256 (48k base)
		22_579_200, // DSD512 (44.1k base)
		24_576_000, // DSD512 (48k base)
		45_158_400, // DSD1024 (44.1k base)
	}
	pcmRates := []uint32{
		0,
		8_000,
		11_025,
		22_050,
		44_100,
		48_000,
		88_200,
		96_000,
		176_400,
		192_000,
		352_800,
		384_000,
	}
	for _, sr := range validDSDRates {
		if !isValidDSDSampleRate(sr) {
			t.Errorf("isValidDSDSampleRate(%d) = false, want true", sr)
		}
	}
	for _, sr := range pcmRates {
		if isValidDSDSampleRate(sr) {
			t.Errorf("isValidDSDSampleRate(%d) = true, want false (PCM-like rate)", sr)
		}
	}
}

// TestDSF_IsDSDDefaultsTrue_OnAnomalousBitsPerSample exercises the
// PR-A2 default-true policy: a DSF file with bitsPerSample != 1 in
// its fmt chunk but a VALID DSD sample rate should still classify
// as DSD (since .dsf is structurally DSD), and a scanLogger.Warn
// should fire for operator visibility.
func TestDSF_IsDSDDefaultsTrue_OnAnomalousBitsPerSample(t *testing.T) {
	// Build a DSF with bitsPerSample=8 in fmt chunk + valid DSD64
	// rate (2.8224 MHz). Pre-PR-A2 this would set IsDSD=false; new
	// policy keeps IsDSD=true.
	path := writeMinimalDSFWithBits(t, 2_822_400, 8)
	track := &Track{}
	if err := extractDSFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDSFWithContext: %v", err)
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want true (anomalous bits but valid DSD rate)", track.IsDSD)
	}
	if track.SampleRate == nil || *track.SampleRate != 2_822_400 {
		t.Errorf("SampleRate = %v, want 2_822_400", track.SampleRate)
	}
}

// TestDSF_IsDSDFalse_OnPCMLikeSampleRate locks the PCM-like-rate
// rejection: a `.dsf` file declaring a non-DSD rate (44100) is
// almost certainly a mislabeled PCM container, so IsDSD must be
// false to avoid the iOS decoder attempting a DoP lock.
func TestDSF_IsDSDFalse_OnPCMLikeSampleRate(t *testing.T) {
	path := writeMinimalDSFWithBits(t, 44_100, 1)
	track := &Track{}
	if err := extractDSFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDSFWithContext: %v", err)
	}
	if track.IsDSD == nil || *track.IsDSD {
		t.Errorf("IsDSD = %v, want false (PCM-like rate must classify as non-DSD)", track.IsDSD)
	}
}

// TestDSF_BitsPerSamplePersistsForLosslessDSF locks the gate's
// allow-side: a lossless DSF must still surface bitsPerSample=1
// (the existing behavior); the gate only suppresses lossy codecs.
func TestDSF_BitsPerSamplePersistsForLosslessDSF(t *testing.T) {
	path := writeMinimalDSFWithBits(t, 2_822_400, 1)
	track := &Track{}
	if err := extractDSFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDSFWithContext: %v", err)
	}
	if track.Codec != "DSF" {
		t.Errorf("Codec = %q, want %q", track.Codec, "DSF")
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 1 {
		t.Errorf("BitsPerSample = %v, want pointer to 1", track.BitsPerSample)
	}
}
