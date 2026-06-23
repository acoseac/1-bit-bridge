package manifest

import (
	"errors"
	"io"
)

// MP3 carries no separate format header — the sample rate lives in the
// first MPEG audio frame header (4 bytes). dhowden/tag reads ID3 tags
// but NOT the frame geometry, so we parse the first frame ourselves to
// populate Track.SampleRate. Bit depth is not meaningful for a lossy
// codec, so it stays nil (the canSetBitsPerSample gate refuses "MP3"
// anyway).

// mpegSampleRates is indexed by [versionID][sampleRateIndex]. The 2-bit
// version field maps directly: 00=MPEG2.5, 01=reserved, 10=MPEG2,
// 11=MPEG1. A 0 entry means "reserved / invalid".
var mpegSampleRates = [4][3]int{
	{11025, 12000, 8000},  // MPEG 2.5
	{0, 0, 0},             // reserved
	{22050, 24000, 16000}, // MPEG 2
	{44100, 48000, 32000}, // MPEG 1
}

// mp3FrameScanWindow bounds how far past the (skipped) ID3v2 tag we scan
// for the first MPEG frame sync. The first frame sits at or just after
// the tag boundary in every well-formed file; 128 KiB tolerates a large
// run of inter-tag padding while keeping the read to a single bounded
// syscall (we never read the whole file).
const mp3FrameScanWindow = 128 << 10

// extractMP3SampleRate returns the sample rate (Hz) from the first valid
// MPEG audio frame header, skipping a leading ID3v2 tag if present.
// Returns 0 (no error) when no plausible frame is found — a malformed
// file or an unusual layout leaves Track.SampleRate nil rather than
// failing the scan. Genuine I/O failures propagate.
func extractMP3SampleRate(r io.ReadSeeker) (float64, error) {
	// An ID3v2 tag, when present, prefixes the audio. Skip it so the
	// scan starts at (or near) the first real frame rather than inside
	// embedded APIC bytes that could carry a spurious 0xFF 0xFB pattern.
	var idHeader [10]byte
	n, err := io.ReadFull(r, idHeader[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	var frameSearchStart int64
	if n >= 10 && string(idHeader[0:3]) == "ID3" {
		tagLen := int64(10) + int64(unsyncsafe(idHeader[6:10]))
		if idHeader[5]&0x10 != 0 {
			// Footer-present flag (ID3v2.4) — the footer is another 10 bytes.
			tagLen += 10
		}
		frameSearchStart = tagLen
	}
	if _, err := r.Seek(frameSearchStart, io.SeekStart); err != nil {
		return 0, err
	}

	buf := make([]byte, mp3FrameScanWindow)
	nn, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	buf = buf[:nn]
	for i := 0; i+4 <= len(buf); i++ {
		// A frame sync is 11 set bits: 0xFF followed by 0b111xxxxx.
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		if rate, ok := mpegFrameSampleRate(buf[i : i+4]); ok {
			return float64(rate), nil
		}
	}
	return 0, nil
}

// mpegFrameSampleRate validates a 4-byte MPEG audio frame header and
// returns its sample rate. The validation (sync, non-reserved
// version/layer/sampleRate-index, non-free/non-bad bitrate index) keeps
// a stray 0xFF 0xEx byte pair in random data from being mistaken for a
// real frame.
func mpegFrameSampleRate(hdr []byte) (int, bool) {
	if len(hdr) < 3 {
		return 0, false
	}
	if hdr[0] != 0xFF || hdr[1]&0xE0 != 0xE0 {
		return 0, false
	}
	version := (hdr[1] >> 3) & 0x03
	layer := (hdr[1] >> 1) & 0x03
	if version == 1 || layer == 0 {
		return 0, false // reserved version / layer
	}
	bitrateIndex := (hdr[2] >> 4) & 0x0F
	if bitrateIndex == 0 || bitrateIndex == 0x0F {
		return 0, false // free-format / invalid bitrate — reject false syncs
	}
	srIndex := (hdr[2] >> 2) & 0x03
	if srIndex == 3 {
		return 0, false // reserved sample-rate index
	}
	rate := mpegSampleRates[version][srIndex]
	if rate == 0 {
		return 0, false
	}
	return rate, true
}

// unsyncsafe decodes a 4-byte ID3v2 synchsafe integer (7 bits per byte,
// MSB always 0) into a uint32 — the encoding ID3v2 uses for its tag size.
func unsyncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7F)<<21 | uint32(b[1]&0x7F)<<14 | uint32(b[2]&0x7F)<<7 | uint32(b[3]&0x7F)
}
