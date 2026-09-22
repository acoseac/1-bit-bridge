package manifest

import (
	"encoding/binary"
	"errors"
	"io"
)

// MP3 carries no separate format header — the sample rate lives in the
// first MPEG audio frame header (4 bytes), and the duration is either
// declared by an encoder-written Xing / Info / VBRI frame or has to be
// estimated from the first frame's bitrate against the audio byte span.
// dhowden/tag reads ID3 tags but NOT the frame geometry, so we parse the
// first frame ourselves to populate Track.SampleRate + Track.Duration.
// Bit depth is not meaningful for a lossy codec, so it stays nil (the
// canSetBitsPerSample gate refuses "MP3" anyway).

// mpegSampleRates is indexed by [versionID][sampleRateIndex]. The 2-bit
// version field maps directly: 00=MPEG2.5, 01=reserved, 10=MPEG2,
// 11=MPEG1. A 0 entry means "reserved / invalid".
var mpegSampleRates = [4][3]int{
	{11025, 12000, 8000},  // MPEG 2.5
	{0, 0, 0},             // reserved
	{22050, 24000, 16000}, // MPEG 2
	{44100, 48000, 32000}, // MPEG 1
}

// mpegBitratesKbps is indexed by [row][bitrateIndex] (kbit/s). Index 0
// (free format) and 15 (invalid) are 0 and rejected by the header parse.
// Row selection: MPEG 1 has one table per layer; MPEG 2 / 2.5 share one
// table for Layers II and III (ISO 11172-3 / 13818-3 Annex).
var mpegBitratesKbps = [5][16]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}, // MPEG 1 Layer I
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},    // MPEG 1 Layer II
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},     // MPEG 1 Layer III
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},    // MPEG 2 / 2.5 Layer I
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},         // MPEG 2 / 2.5 Layer II + III
}

const (
	mpegVersion25 = 0
	mpegVersion2  = 2
	mpegVersion1  = 3
	mpegLayerIII  = 1
	mpegLayerII   = 2
	mpegLayerI    = 3
	// mpegChannelModeMono is the 2-bit channel-mode field value for a
	// single-channel stream — the one case with the shorter Layer III
	// side-info block the Xing offset depends on.
	mpegChannelModeMono = 3
)

// mp3FrameScanWindow bounds how far past the (skipped) ID3v2 tag we scan
// for the first MPEG frame sync. The first frame sits at or just after
// the tag boundary in every well-formed file; 128 KiB tolerates a large
// run of inter-tag padding while keeping the read to a single bounded
// syscall (we never read the whole file).
const mp3FrameScanWindow = 128 << 10

// mp3VBRHeaderProbeBytes is how much of the first frame the duration
// read inspects for a Xing / Info / VBRI header: the latest either can
// sit is 4 (frame header) + 32 (VBRI's fixed offset, or Layer III's
// widest side info) + 4 (tag) + 14 (VBRI's frame-count position) + 4.
// 64 covers both with room; nothing past it is consulted.
const mp3VBRHeaderProbeBytes = 64

// mp3ID3v1TagSize is the fixed size of a trailing ID3v1 tag ("TAG" +
// 125 bytes), subtracted from the audio byte span of a CBR estimate.
const mp3ID3v1TagSize = 128

// mp3Format is what the first MPEG frame reveals about the whole file:
// the sample rate every frame shares, and a duration that is either
// DECLARED (an encoder-written frame count) or ESTIMATED (first-frame
// bitrate over the audio bytes). Duration 0 means "not determinable" —
// the caller stamps nothing.
type mp3Format struct {
	sampleRate float64
	duration   float64
}

// mpegFrame is a validated 4-byte MPEG audio frame header, decoded.
type mpegFrame struct {
	version         byte // 2-bit field: mpegVersion25 / mpegVersion2 / mpegVersion1
	layer           byte // 2-bit field: mpegLayerI / II / III
	sampleRate      int
	bitrateKbps     int
	samplesPerFrame int
	channelMode     byte // 2-bit field; mpegChannelModeMono = mono
}

// sideInfoSize is the Layer III side-information block that separates
// the frame header from the main data — and from a Xing / Info header,
// which an encoder writes immediately after it. 0 for Layers I / II
// (they carry no Xing header).
func (f mpegFrame) sideInfoSize() int {
	if f.layer != mpegLayerIII {
		return 0
	}
	if f.version == mpegVersion1 {
		if f.channelMode == mpegChannelModeMono {
			return 17
		}
		return 32
	}
	if f.channelMode == mpegChannelModeMono {
		return 9
	}
	return 17
}

// extractMP3Format returns the sample rate + duration derived from the
// first valid MPEG audio frame, skipping a leading ID3v2 tag if present.
// Returns a zero mp3Format (no error) when no plausible frame is found —
// a malformed file or an unusual layout leaves Track.SampleRate +
// Track.Duration nil rather than failing the scan. Genuine I/O failures
// propagate.
//
// Duration, in order of trust:
//  1. A Xing / Info header in the first frame with the FRAMES flag —
//     LAME and every modern encoder write one for VBR (Xing) and CBR
//     (Info) alike: frames × samplesPerFrame / sampleRate.
//  2. A Fraunhofer VBRI header (fixed 32 bytes after the frame header):
//     the same arithmetic over its frame count.
//  3. The CBR estimate: the audio byte span (file size minus the first
//     frame's offset minus a trailing ID3v1 tag) at the first frame's
//     bitrate. Exact for a constant-bitrate file; for a header-less VBR
//     file it is the classic first-frame estimate every player shows —
//     an honest number for a row that otherwise shows a file size.
//
// The encoder delay / padding a LAME tag records (~50 ms) is deliberately
// not subtracted: the row renders m:ss, and the FLAC / DSF paths' sample
// counts don't subtract their containers' priming either.
func extractMP3Format(r io.ReadSeeker) (mp3Format, error) {
	// An ID3v2 tag, when present, prefixes the audio. Skip it (the FLAC
	// walkers' skipID3v2 — same synchsafe size, same version-gated
	// footer) so the scan starts at (or near) the first real frame
	// rather than inside embedded APIC bytes that could carry a
	// spurious 0xFF 0xFB pattern.
	if err := skipID3v2(r); err != nil {
		return mp3Format{}, err
	}
	frameSearchStart, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return mp3Format{}, err
	}
	buf := make([]byte, mp3FrameScanWindow)
	nn, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return mp3Format{}, err
	}
	frame, at, ok := locateFirstMPEGFrame(buf[:nn])
	if !ok {
		return mp3Format{}, nil
	}
	duration, err := mp3Duration(r, frame, frameSearchStart+int64(at))
	if err != nil {
		return mp3Format{}, err
	}
	return mp3Format{sampleRate: float64(frame.sampleRate), duration: duration}, nil
}

// locateFirstMPEGFrame scans buf for the first byte pair that reads as
// an MPEG frame sync (11 set bits: 0xFF followed by 0b111xxxxx) AND
// validates as a full header, returning the decoded frame and its index
// in buf.
func locateFirstMPEGFrame(buf []byte) (mpegFrame, int, bool) {
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		if frame, ok := parseMPEGFrameHeader(buf[i : i+4]); ok {
			return frame, i, true
		}
	}
	return mpegFrame{}, 0, false
}

// mp3Duration derives the duration for a stream whose first valid frame
// header sits at frameOffset — see extractMP3Format for the ladder.
// Returns 0 when nothing can be derived; the caller's plausibility gate
// discards absurd values (a forged frame count).
func mp3Duration(r io.ReadSeeker, frame mpegFrame, frameOffset int64) (float64, error) {
	if frame.sampleRate <= 0 || frame.samplesPerFrame <= 0 {
		return 0, nil
	}
	if _, err := r.Seek(frameOffset, io.SeekStart); err != nil {
		return 0, err
	}
	var probe [mp3VBRHeaderProbeBytes]byte
	got, err := io.ReadFull(r, probe[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	if frames, ok := mp3VBRFrameCount(probe[:got], frame); ok {
		return float64(frames) * float64(frame.samplesPerFrame) / float64(frame.sampleRate), nil
	}
	return mp3CBRDurationEstimate(r, frame, frameOffset)
}

// mp3VBRFrameCount reads an encoder-declared total frame count out of
// the first frame's bytes (header included, starting at index 0):
// a Xing / Info header after the Layer III side info, else a VBRI
// header at its fixed 32-byte offset. Layers I / II carry neither. A
// Xing header WITHOUT the frames flag ends the search (no encoder
// writes both), so the CBR estimate follows — VBRI is consulted only
// when no Xing / Info tag is present at all.
func mp3VBRFrameCount(first []byte, frame mpegFrame) (uint32, bool) {
	if frame.layer != mpegLayerIII {
		return 0, false
	}
	if frames, ok, present := xingFrameCount(first, 4+frame.sideInfoSize()); present {
		return frames, ok
	}
	return vbriFrameCount(first)
}

// xingFrameCount reads a Xing (VBR) / Info (CBR, LAME's spelling) header
// at `at`: the tag, then a flags word whose bit 0 says a frame count
// follows. `present` reports that a tag was there at all; `ok` that it
// carried a positive frame count.
func xingFrameCount(first []byte, at int) (frames uint32, ok bool, present bool) {
	if len(first) < at+12 {
		return 0, false, false
	}
	tagName := string(first[at : at+4])
	if tagName != "Xing" && tagName != "Info" {
		return 0, false, false
	}
	flags := binary.BigEndian.Uint32(first[at+4 : at+8])
	if flags&0x1 == 0 {
		return 0, false, true
	}
	frames = binary.BigEndian.Uint32(first[at+8 : at+12])
	return frames, frames > 0, true
}

// vbriFrameCount reads a Fraunhofer VBRI header: always 32 bytes after
// the 4-byte frame header, then version u16, delay u16, quality u16,
// bytes u32, frames u32.
func vbriFrameCount(first []byte) (uint32, bool) {
	const vbriAt = 4 + 32
	if len(first) < vbriAt+18 || string(first[vbriAt:vbriAt+4]) != "VBRI" {
		return 0, false
	}
	frames := binary.BigEndian.Uint32(first[vbriAt+14 : vbriAt+18])
	return frames, frames > 0
}

// mp3CBRDurationEstimate is the ladder's last rung: the audio byte span
// (file end minus the first frame's offset, minus a trailing ID3v1
// tag) at the first frame's bitrate. The seek to the end is the one
// whole-file operation on this path, and it is a seek, not a read.
func mp3CBRDurationEstimate(r io.ReadSeeker, frame mpegFrame, frameOffset int64) (float64, error) {
	if frame.bitrateKbps <= 0 {
		return 0, nil
	}
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	audioEnd := fileSize
	if fileSize-mp3ID3v1TagSize >= frameOffset {
		if _, err := r.Seek(fileSize-mp3ID3v1TagSize, io.SeekStart); err != nil {
			return 0, err
		}
		var tag [3]byte
		if _, err := io.ReadFull(r, tag[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, nil
			}
			return 0, err
		}
		if string(tag[:]) == "TAG" {
			audioEnd -= mp3ID3v1TagSize
		}
	}
	audioBytes := audioEnd - frameOffset
	if audioBytes <= 0 {
		return 0, nil
	}
	return float64(audioBytes) * 8 / (float64(frame.bitrateKbps) * 1000), nil
}

// extractMP3SampleRate returns the sample rate (Hz) from the first valid
// MPEG audio frame header, skipping a leading ID3v2 tag if present.
// Returns 0 (no error) when no plausible frame is found.
//
// TEST-ONLY. It is the thin wrapper #935 left behind so the rate tests
// written against the pre-v11 extractor did not have to move, and it has
// had no production caller since: everything that wants a rate now takes
// it from the extractMP3Format result it already holds.
//
// Its docblock used to say it was "kept for the callers that want only
// that", which named callers that do not exist and also stopped being
// true of the function: it now runs the WHOLE duration ladder, and on a
// header-less file mp3CBRDurationEstimate seeks to EOF, so a caller
// reaching for "just the rate" would pay a whole-file seek and get its
// reader back repositioned.
func extractMP3SampleRate(r io.ReadSeeker) (float64, error) {
	info, err := extractMP3Format(r)
	if err != nil {
		return 0, err
	}
	return info.sampleRate, nil
}

// parseMPEGFrameHeader validates a 4-byte MPEG audio frame header and
// decodes its geometry. The validation (sync, non-reserved
// version/layer/sampleRate-index, non-free/non-bad bitrate index) keeps
// a stray 0xFF 0xEx byte pair in random data from being mistaken for a
// real frame.
func parseMPEGFrameHeader(hdr []byte) (mpegFrame, bool) {
	if len(hdr) < 4 {
		return mpegFrame{}, false
	}
	if hdr[0] != 0xFF || hdr[1]&0xE0 != 0xE0 {
		return mpegFrame{}, false
	}
	version := (hdr[1] >> 3) & 0x03
	layer := (hdr[1] >> 1) & 0x03
	if version == 1 || layer == 0 {
		return mpegFrame{}, false // reserved version / layer
	}
	bitrateIndex := (hdr[2] >> 4) & 0x0F
	if bitrateIndex == 0 || bitrateIndex == 0x0F {
		return mpegFrame{}, false // free-format / invalid bitrate — reject false syncs
	}
	srIndex := (hdr[2] >> 2) & 0x03
	if srIndex == 3 {
		return mpegFrame{}, false // reserved sample-rate index
	}
	rate := mpegSampleRates[version][srIndex]
	if rate == 0 {
		return mpegFrame{}, false
	}
	var bitrateRow int
	var samplesPerFrame int
	switch {
	case version == mpegVersion1 && layer == mpegLayerI:
		bitrateRow, samplesPerFrame = 0, 384
	case version == mpegVersion1 && layer == mpegLayerII:
		bitrateRow, samplesPerFrame = 1, 1152
	case version == mpegVersion1: // Layer III
		bitrateRow, samplesPerFrame = 2, 1152
	case layer == mpegLayerI: // MPEG 2 / 2.5
		bitrateRow, samplesPerFrame = 3, 384
	case layer == mpegLayerII:
		bitrateRow, samplesPerFrame = 4, 1152
	default: // MPEG 2 / 2.5 Layer III
		bitrateRow, samplesPerFrame = 4, 576
	}
	bitrate := mpegBitratesKbps[bitrateRow][bitrateIndex]
	if bitrate == 0 {
		return mpegFrame{}, false
	}
	return mpegFrame{
		version:         version,
		layer:           layer,
		sampleRate:      rate,
		bitrateKbps:     bitrate,
		samplesPerFrame: samplesPerFrame,
		channelMode:     (hdr[3] >> 6) & 0x03,
	}, true
}

// mpegFrameSampleRate validates a 4-byte MPEG audio frame header and
// returns its sample rate — parseMPEGFrameHeader's rate alone.
//
// TEST-ONLY, like extractMP3SampleRate above, and for the same reason:
// production reads the whole frame geometry. Kept because the header
// validation cases are worth exercising against the narrow answer.
func mpegFrameSampleRate(hdr []byte) (int, bool) {
	frame, ok := parseMPEGFrameHeader(hdr)
	if !ok {
		return 0, false
	}
	return frame.sampleRate, true
}

// unsyncsafe decodes a 4-byte ID3v2 synchsafe integer (7 bits per byte,
// MSB always 0) into a uint32 — the encoding ID3v2 uses for its tag size.
// The one decoder: skipID3v2 (the FLAC + MP3 tag skip) reads through it.
func unsyncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7F)<<21 | uint32(b[1]&0x7F)<<14 | uint32(b[2]&0x7F)<<7 | uint32(b[3]&0x7F)
}
