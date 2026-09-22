package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	tag "github.com/dhowden/tag"
)

// readIFFChunkBody reads exactly `size` bytes of an IFF/RIFF chunk body from r.
// On a clean read it returns (body, false, nil).
//
// A mid-body truncation (io.EOF / io.ErrUnexpectedEOF) is NOT fatal — the file
// is still worth indexing, and the folder-level cover.jpg fallback that runs at
// the tail of each walker must stay reachable — so it logs a Warn and returns
// (nil, true, nil). The caller MUST break its chunk walk on `truncated` (a bare
// `break` in the AIFF if-chain walker, `break chunkLoop` in the WAV switch
// walker) and fall through to extractLocalArtwork. Any other (genuine I/O) read
// error is returned wrapped for the caller to propagate.
//
// `format` ("wav"/"aiff") + `chunk` ("fmt"/"ID3"/"COMM"/"LIST") shape the log +
// error text so both read exactly as the five per-site strings did before this
// was extracted (B10). Factoring the five near-identical blocks here also keeps
// the SonarCloud new-code duplication gate green.
func readIFFChunkBody(r io.Reader, size uint32, format, chunk, absPath string) (body []byte, truncated bool, err error) {
	body = make([]byte, size)
	if _, rerr := io.ReadFull(r, body); rerr != nil {
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			scanLogger.Warn(format+": "+chunk+" body truncated; stopping chunk walk", "path", absPath, "err", rerr)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("%s: %s body read: %w", format, chunk, rerr)
	}
	return body, false, nil
}

// extractAIFFWithContext walks an AIFF / AIFC FORM tree looking for
// an embedded "ID3 " sub-chunk. When present, the chunk body is
// handed to dhowden/tag's ID3v2 parser so APIC artwork and the same
// tag set MP3/M4A/DSF surface today land on the Track. Without an
// embedded ID3 chunk, the function still triggers the folder-level
// cover.jpg / folder.jpg fallback via extractLocalArtwork(m=nil).
//
// dhowden/tag's package-level `ReadFrom` does NOT support AIFF
// containers (see go doc github.com/dhowden/tag — the supported
// formats are MP3 / MP4 / FLAC / OGG). Pre-PR-F the .aif/.aiff
// branch fell through to `extractViaDhowdenWithContext` which always
// returned ErrNoTagsFound, so tagged AIFF files surfaced only
// path-derived defaults.
//
// AIFF and AIFC share the same chunk-walker shape; the FORM type
// FOURCC differs ("AIFF" vs "AIFC") but only affects audio-payload
// codec interpretation — irrelevant here. We accept both.
func extractAIFFWithContext(absPath string, t *Track, ec *ExtractContext) error {
	t.Codec = "AIFF"

	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// FORM outer header: 4 bytes magic + 4 bytes BE size + 4 bytes
	// form type. Note: AIFF uses 32-bit BE size; DSDIFF (also FRM8-
	// based) uses 64-bit BE size. The bridge keeps the two parsers
	// separate rather than sharing a base walker.
	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("aiff: short outer header: %w", err)
	}
	if string(header[0:4]) != "FORM" {
		return fmt.Errorf("aiff: bad FORM magic %q", header[0:4])
	}
	formType := string(header[8:12])
	if formType != "AIFF" && formType != "AIFC" {
		return fmt.Errorf("aiff: not an AIFF/AIFC form (got %q)", formType)
	}

	// Duration inputs, resolved once the walk is over (the spec fixes no
	// chunk order, so COMM may follow SSND): the COMM frame count, and
	// the SSND payload's position so a truncated file — declared audio
	// past the physical end — reports no duration for bytes it does not
	// hold (the DFF `payloadFits` rule; see iffPayloadFits).
	var (
		numSampleFrames uint32
		ssnd            iffPayloadSpan
	)
	physicalSize := physicalFileSize(f)

	// Walk sub-chunks looking for "ID3 ". Each sub-chunk: 4 bytes
	// FOURCC + 4 bytes BE size + payload + pad byte if size is odd
	// (IFF chunk-pad rule).
	var idTagMetadata tag.Metadata
	for {
		var sub [8]byte
		if _, err := io.ReadFull(f, sub[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return fmt.Errorf("aiff: sub-chunk header read: %w", err)
		}
		fourcc := string(sub[0:4])
		size := binary.BigEndian.Uint32(sub[4:8])
		if fourcc == "ID3 " || fourcc == "id3 " {
			const maxID3Size = 32 << 20 // 32 MiB — accommodates APIC up to ~25 MiB plus framing
			if size == 0 {
				continue
			}
			if size > maxID3Size {
				scanLogger.Warn("aiff: ID3 chunk size exceeds sanity limit; skipping",
					"path", absPath, "size", size, "limit", maxID3Size)
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			body, truncated, err := readIFFChunkBody(f, size, "aiff", "ID3", absPath)
			if err != nil {
				return err
			}
			if truncated {
				break // if-chain walker: bare break exits the for-loop
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("aiff: ID3 pad seek: %w", err)
				}
			}
			idTagMetadata = applyEmbeddedID3(body, t, idTagMetadata, absPath, "aiff")
			// Continue walking — operators occasionally embed both an
			// ID3 chunk AND a duplicate; applyEmbeddedID3 keeps the
			// earliest chunk entirely (text + artwork) and ignores the
			// duplicate.
			continue
		}
		if fourcc == "COMM" {
			// The COMM (Common) chunk carries the PCM geometry:
			// numChannels (BE int16), numSampleFrames (BE uint32),
			// sampleSize (BE int16 — bits per sample), and an
			// 80-bit IEEE-754 extended-precision sampleRate. AIFC
			// appends a compressionType FOURCC + pstring after the
			// sampleRate, but the leading 18 bytes are identical, so
			// the same parse serves both form types. 1 KiB cap — a
			// real COMM is 18 bytes (AIFF) or a few dozen (AIFC).
			const minCOMMSize = 18
			const maxCOMMSize = 1 << 10
			if size < minCOMMSize || size > maxCOMMSize {
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			body, truncated, err := readIFFChunkBody(f, size, "aiff", "COMM", absPath)
			if err != nil {
				return err
			}
			if truncated {
				break // if-chain walker: bare break exits the for-loop
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("aiff: COMM pad seek: %w", err)
				}
			}
			numSampleFrames = parseAIFFCOMMChunk(body, t, formType)
			continue
		}
		if fourcc == "SSND" {
			// The audio payload itself is never read — only WHERE it
			// sits, for the fit check. First SSND wins (a second one
			// is malformed; the spec allows exactly one).
			if !ssnd.seen {
				ssnd = ssndSoundSpan(f, size)
			}
			if err := seekPastChunk(f, int64(size)); err != nil {
				return err
			}
			continue
		}
		if err := seekPastChunk(f, int64(size)); err != nil {
			return err
		}
	}

	stampIFFDuration(t, aiffDurationSeconds(numSampleFrames, t.SampleRate), ssnd, physicalSize)

	if ec != nil && ec.ArtworkCacheDir != "" {
		// Pass the dhowden Metadata (or nil if no ID3 chunk surfaced)
		// to extractLocalArtwork — embedded APIC wins if present,
		// folder-level cover.jpg / folder.jpg fallback fires otherwise.
		extractLocalArtwork(absPath, t, idTagMetadata, ec)
	}
	return nil
}

// aiffDurationSeconds is COMM numSampleFrames / sampleRate: frames are
// per-channel sample frames, so channel count does not enter. 0 when
// either input is absent; the plausibility gate is the caller's.
func aiffDurationSeconds(numSampleFrames uint32, sampleRate *float64) float64 {
	if numSampleFrames == 0 || sampleRate == nil || *sampleRate <= 0 {
		return 0
	}
	return float64(numSampleFrames) / *sampleRate
}

// parseAIFFCOMMChunk reads the PCM geometry from an AIFF/AIFC COMM
// chunk body and stamps t.SampleRate + t.BitsPerSample. Layout (all
// big-endian):
//
//	[0:2]  numChannels   int16
//	[2:6]  numSampleFrames uint32
//	[6:8]  sampleSize    int16   — bits per sample of the (decompressed) signal
//	[8:18] sampleRate    80-bit IEEE-754 extended
//
// Returns numSampleFrames — the per-channel sample-frame count the
// duration is derived from (0 for a body too short to carry one). It is
// RETURNED rather than stamped because the duration also needs the
// SSND payload to fit the file, which only the walk knows.
//
// SampleRate is always stamped. BitsPerSample is gated TWICE: by
// canSetBitsPerSample (allowlists "AIFF") AND by aiffCOMMHasPCMDepth —
// because `.aifc` is stamped Codec="AIFF" before the COMM is parsed, a
// COMPRESSED AIFC variant (e.g. ima4 / ulaw) would otherwise surface its
// COMM.sampleSize as a real PCM bit depth, the AIFF analog of the iOS
// PR #371 "lossy source reports a container bit depth" regression. For
// AIFC we therefore only set bits when the compressionType is a known
// PCM-like FOURCC. Plain AIFF is uncompressed by definition, so it's
// always eligible.
func parseAIFFCOMMChunk(body []byte, t *Track, formType string) (numSampleFrames uint32) {
	if len(body) < 18 {
		return 0
	}
	numSampleFrames = binary.BigEndian.Uint32(body[2:6])
	sampleSize := int16(binary.BigEndian.Uint16(body[6:8]))
	sampleRate := parseAIFFExtended(body[8:18])
	if sampleRate > 0 {
		t.SampleRate = &sampleRate
	}
	if sampleSize > 0 && canSetBitsPerSample(t.Codec) && aiffCOMMHasPCMDepth(body, formType) {
		bps := int(sampleSize)
		t.BitsPerSample = &bps
	}
	return numSampleFrames
}

// aiffCOMMHasPCMDepth reports whether the COMM chunk's sampleSize is a
// meaningful PCM bit depth. Plain AIFF is always uncompressed PCM. AIFC
// appends a 4-byte compressionType FOURCC at COMM body offset 18; only
// the byte-ordered / float / sized-PCM "compressions" carry a real bit
// depth — every other code is a lossy/compressed scheme whose sampleSize
// describes the pre-compression source, not the stored signal.
func aiffCOMMHasPCMDepth(body []byte, formType string) bool {
	if formType == "AIFF" {
		return true
	}
	if formType != "AIFC" || len(body) < 22 {
		return false
	}
	switch string(body[18:22]) {
	case "NONE", "twos", "sowt", "raw ", "fl32", "fl64", "in24", "in32", "23ni":
		return true
	default:
		return false
	}
}

// parseAIFFExtended decodes a 10-byte 80-bit IEEE-754 extended-precision
// float (the "long double" AIFF stores its sample rate as) into a
// float64. Layout (big-endian): 1 sign bit, 15 exponent bits (bias
// 16383), 64 mantissa bits with an EXPLICIT integer bit (unlike IEEE
// binary64's implicit leading 1). Returns 0 for the zero, Inf, and NaN
// encodings — none are valid sample rates. math.Ldexp is exact for the
// power-of-two scaling, so integer sample rates round-trip without
// floating-point drift.
func parseAIFFExtended(b []byte) float64 {
	if len(b) < 10 {
		return 0
	}
	sign := 1.0
	if b[0]&0x80 != 0 {
		sign = -1.0
	}
	exponent := int(binary.BigEndian.Uint16(b[0:2]) & 0x7FFF)
	mantissa := binary.BigEndian.Uint64(b[2:10])
	switch {
	case exponent == 0 && mantissa == 0:
		return 0
	case exponent == 0x7FFF:
		// Inf / NaN — not a real sample rate.
		return 0
	}
	// value = sign × mantissa × 2^(exponent − 16383 − 63)
	val := sign * math.Ldexp(float64(mantissa), exponent-16383-63)
	// A corrupt COMM chunk with a huge (but non-0x7FFF) exponent can
	// overflow Ldexp to ±Inf. A ±Inf / NaN SampleRate would fail
	// json.Marshal when the Track is persisted, breaking the whole
	// tags_json batch write — so refuse it and leave SampleRate nil
	// rather than let one malformed file derail the scan.
	if math.IsInf(val, 0) || math.IsNaN(val) {
		return 0
	}
	return val
}

// extractWAVWithContext is the RIFF/WAVE analog of
// extractAIFFWithContext. RIFF uses little-endian 32-bit chunk
// sizes (vs AIFF's big-endian). The two recognised tag-carrying
// sub-chunks are:
//
//   - "id3 " (lowercase, per the ID3v2 spec for RIFF) — full ID3v2
//     framing including APIC artwork. Routed through dhowden's
//     ReadID3v2Tags.
//   - "LIST" with form type "INFO" — RIFF's native tag scheme.
//     Sub-chunks like INAM (title), IART (artist), IPRD (album),
//     ICRD (year), IGNR (genre). Text-only, no artwork support.
//     Populated only when no ID3 chunk surfaced (ID3 wins).
//
// dhowden/tag's package-level `ReadFrom` does NOT support WAV
// containers; pre-PR-F .wav files fell through to the default
// branch and surfaced only path-derived defaults.
func extractWAVWithContext(absPath string, t *Track, ec *ExtractContext) error {
	t.Codec = "WAV"

	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("wav: short outer header: %w", err)
	}
	if string(header[0:4]) != "RIFF" {
		return fmt.Errorf("wav: bad RIFF magic %q", header[0:4])
	}
	if string(header[8:12]) != "WAVE" {
		return fmt.Errorf("wav: not a WAVE form (got %q)", header[8:12])
	}

	// Duration inputs, resolved once the walk is over (`fmt ` precedes
	// `data` per spec, but the walk does not depend on it): the fmt
	// chunk's bytes-per-second, and the data payload's position + size
	// so a truncated file — or a streaming writer's never-fixed-up
	// 0 / 0xFFFFFFFF size — reports no duration (see iffPayloadFits).
	var (
		bytesPerSecond uint64
		data           iffPayloadSpan
	)
	physicalSize := physicalFileSize(f)

	var idTagMetadata tag.Metadata
	// Labeled so a truncated chunk BODY inside the switch below can break
	// the walk loop (a bare `break` would only exit the switch) and fall
	// through to the extractLocalArtwork tail — see B10 body-read handling.
chunkLoop:
	for {
		var sub [8]byte
		if _, err := io.ReadFull(f, sub[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return fmt.Errorf("wav: sub-chunk header read: %w", err)
		}
		fourcc := string(sub[0:4])
		size := binary.LittleEndian.Uint32(sub[4:8])
		switch {
		case fourcc == "id3 " || fourcc == "ID3 ":
			const maxID3Size = 32 << 20
			if size == 0 {
				continue
			}
			if size > maxID3Size {
				scanLogger.Warn("wav: ID3 chunk size exceeds sanity limit; skipping",
					"path", absPath, "size", size, "limit", maxID3Size)
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			body, truncated, err := readIFFChunkBody(f, size, "wav", "ID3", absPath)
			if err != nil {
				return err
			}
			if truncated {
				break chunkLoop // switch walker: labeled break exits the for-loop
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("wav: ID3 pad seek: %w", err)
				}
			}
			idTagMetadata = applyEmbeddedID3(body, t, idTagMetadata, absPath, "wav")
		case fourcc == "LIST":
			// RIFF LIST chunks come in many flavours — only LIST/INFO
			// carries tag fields. Read the 4-byte form-type plus the
			// remaining body. 64 KiB cap covers real-world INFO blocks
			// (typically <1 KiB) with comfortable headroom.
			const maxLISTSize = 64 << 10
			if size < 4 {
				// Malformed: LIST payload too short to even hold a
				// 4-byte form-type. The bare `continue` would leave
				// the cursor inside the truncated payload and have
				// the next iteration read garbage as a chunk header.
				// Advance past whatever's declared so the walker
				// stays aligned for later valid chunks (CodeRabbit
				// Minor on PR #224).
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			if size > maxLISTSize {
				scanLogger.Warn("wav: LIST chunk size exceeds sanity limit; skipping",
					"path", absPath, "size", size, "limit", maxLISTSize)
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			body, truncated, err := readIFFChunkBody(f, size, "wav", "LIST", absPath)
			if err != nil {
				return err
			}
			if truncated {
				break chunkLoop // switch walker: labeled break exits the for-loop
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("wav: LIST pad seek: %w", err)
				}
			}
			if string(body[0:4]) == "INFO" {
				parseWAVINFOBlock(body[4:], t)
			}
		case fourcc == "fmt ":
			// The fmt chunk carries the PCM geometry (sampleRate +
			// bitsPerSample). Real fmt chunks are 16 (PCM), 18
			// (WAVEFORMATEX), or 40 (WAVE_FORMAT_EXTENSIBLE) bytes; a
			// declared size below 16 can't hold WAVEFORMAT and a wildly
			// large one is corruption — skip both rather than allocate.
			const minFmtSize = 16
			const maxFmtSize = 1 << 10
			if size < minFmtSize || size > maxFmtSize {
				if err := seekPastChunk(f, int64(size)); err != nil {
					return err
				}
				continue
			}
			body, truncated, err := readIFFChunkBody(f, size, "wav", "fmt", absPath)
			if err != nil {
				return err
			}
			if truncated {
				break chunkLoop // switch walker: labeled break exits the for-loop
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("wav: fmt pad seek: %w", err)
				}
			}
			bytesPerSecond = parseWAVFmtChunk(body, t)
		case fourcc == "data":
			// The audio payload itself is never read — only WHERE it
			// sits and how much it declares, for the duration + the
			// fit check. First data chunk wins.
			if !data.seen {
				data = iffPayloadSpanAt(f, size)
			}
			if err := seekPastChunk(f, int64(size)); err != nil {
				return err
			}
		default:
			if err := seekPastChunk(f, int64(size)); err != nil {
				return err
			}
		}
	}

	stampIFFDuration(t, wavDurationSeconds(data, bytesPerSecond), data, physicalSize)

	if ec != nil && ec.ArtworkCacheDir != "" {
		extractLocalArtwork(absPath, t, idTagMetadata, ec)
	}
	return nil
}

// wavDurationSeconds is the data payload's declared byte count over the
// fmt chunk's bytes-per-second. 0 when either is absent; the fit check
// and the plausibility gate are the caller's.
func wavDurationSeconds(data iffPayloadSpan, bytesPerSecond uint64) float64 {
	if !data.seen || data.size == 0 || bytesPerSecond == 0 {
		return 0
	}
	return float64(data.size) / float64(bytesPerSecond)
}

// iffPayloadSpan records where a chunk's payload sits in the file and
// how many bytes it declares, for the truncation check — the audio
// bytes themselves are never read by either walker.
type iffPayloadSpan struct {
	seen   bool
	offset uint64 // absolute offset of the payload's first byte
	size   uint64 // the chunk header's declared payload size
}

// iffPayloadSpanAt records the payload that begins at the file's
// CURRENT position (the walker has just consumed the 8-byte chunk
// header). A failed position read leaves the span unseen — no duration
// rather than one the fit check could not verify.
func iffPayloadSpanAt(f *os.File, size uint32) iffPayloadSpan {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil || pos < 0 {
		return iffPayloadSpan{}
	}
	return iffPayloadSpan{seen: true, offset: uint64(pos), size: uint64(size)}
}

// ssndSoundSpan is iffPayloadSpanAt for an AIFF SSND chunk, whose body
// is NOT all audio: it opens with an 8-byte prefix (`offset` and
// `blockSize`, both uint32 BE), and `offset` counts further padding
// bytes before the first sample frame.
//
// Recording the whole declared size as payload — which is what the plain
// helper does — makes an SSND of exactly 8 bytes look like 8 bytes of
// audio. It holds NONE: the body is the prefix and nothing else. So a
// file whose COMM claims ten minutes and whose SSND claims 8 passed the
// fit check and stamped the ten minutes, which is the zero case one
// prefix along (CodeRabbit on #966).
//
// The span is narrowed rather than merely tested, so the physical bounds
// check still measures the AUDIO against the file: subtracting from
// `size` alone would leave `offset` pointing at the prefix and weaken
// that comparison by 8 + offset bytes.
//
// Read with ReadAt so the walker's own file position is untouched and
// the `seekPastChunk(f, size)` that follows stays correct. A short or
// failed read, a size that cannot hold its own prefix, or a span with no
// sound data at all, all yield an UNSEEN span — no duration rather than
// one nothing verified.
func ssndSoundSpan(f *os.File, size uint32) iffPayloadSpan {
	span := iffPayloadSpanAt(f, size)
	if !span.seen {
		return iffPayloadSpan{}
	}
	var prefix [8]byte
	if _, err := f.ReadAt(prefix[:], int64(span.offset)); err != nil {
		return iffPayloadSpan{}
	}
	skip := uint64(8) + uint64(binary.BigEndian.Uint32(prefix[:4]))
	if span.size <= skip {
		// Cannot hold its own prefix, or holds the prefix and no
		// sound: either way there is no audio to time.
		return iffPayloadSpan{}
	}
	span.offset += skip
	span.size -= skip
	return span
}

// physicalFileSize is the on-disk byte count, 0 when Stat fails — an
// unknown bound fails OPEN in iffPayloadFits (typing and duration land),
// parity with the DFF walker and the iOS `fileSizeBound: nil` rule. In
// practice Stat on an open handle does not fail.
func physicalFileSize(f *os.File) uint64 {
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		return uint64(fi.Size())
	}
	return 0
}

// iffUnknownPayloadSize is the 32-bit all-ones a streaming writer puts
// in a RIFF/AIFF size field when it cannot know the length in advance:
// it is writing to a pipe or a socket and can never seek back to patch
// the header. ffmpeg does exactly this (the `-f wav` over a pipe case
// this repo already records, where sox then warns "Premature EOF" on
// every otherwise-successful job).
//
// It is a SENTINEL, not a length. Divided by a CD-rate
// `nAvgBytesPerSec` it works out at about 6.8 hours, comfortably inside
// the week-long plausibility ceiling, so nothing above catches it — and
// a real payload of exactly 4 GiB − 1 is indistinguishable from it
// anyway, which is why RF64 exists for genuinely larger files.
const iffUnknownPayloadSize = 0xFFFFFFFF

// iffPayloadFits reports whether the declared payload physically fits
// inside the file: offset + size <= physicalSize, overflow-safe. An
// unknown bound (0) fails OPEN; an unseen payload, a declared size of
// ZERO, and a declared size that is the unknown-length sentinel, all
// fail CLOSED — a duration nothing can verify must not be stamped. The AIFF / WAV twin of the DFF
// walker's `payloadFits`, kept as its own function because that one is
// a method over the DFF walk's own state.
//
// The sentinel is refused BEFORE the bound is consulted, deliberately.
// With a known physical size it is already refused, by arithmetic: no
// sub-4-GiB file can hold 4 GiB of payload. The case it exists for is
// the one where the bound is UNKNOWN — a Stat that failed on the open
// handle, which the docblock beside physicalFileSize calls rare rather
// than impossible — and there the fail-open would stamp 6.8 hours of
// audio onto a file that never declared a length at all.
func iffPayloadFits(span iffPayloadSpan, physicalSize uint64) bool {
	if !span.seen || span.size == iffUnknownPayloadSize {
		return false
	}
	// A declared payload of ZERO is the sentinel's other half, and it
	// reached here fitting trivially: `0 <= physicalSize - offset` is
	// true for any bound, so a chunk claiming no audio at all passed the
	// check whose whole job is "does the audio fit the file".
	//
	// WAV never noticed, because wavDurationSeconds derives its seconds
	// FROM the data size and returns 0 for an empty chunk, which the
	// plausibility gate then rejects. AIFF derives from COMM instead, so
	// a file with a well-formed COMM (numSampleFrames 26,460,000 at
	// 44100) and an SSND declaring size 0 stamped Duration = 600 on a
	// file holding no audio bytes. #935 claimed the rule for BOTH IFF
	// walkers; it shipped in one.
	//
	// Narrow in practice — a real streaming-writer AIFF has
	// numSampleFrames == 0 too, so it stamps nothing — which is exactly
	// why it needs the gate rather than the coincidence: the shape that
	// reaches it is a corrupt or forged file, and that is the input this
	// function exists for.
	if span.size == 0 {
		return false
	}
	if physicalSize == 0 {
		return true
	}
	if span.offset > physicalSize {
		return false
	}
	return span.size <= physicalSize-span.offset
}

// stampIFFDuration is the single Duration write for the AIFF and WAV
// walkers: the derived seconds land only when the audio payload fits
// the file AND the value passes the shared plausibility gate.
func stampIFFDuration(t *Track, seconds float64, payload iffPayloadSpan, physicalSize uint64) {
	if !iffPayloadFits(payload, physicalSize) || !plausibleDuration(seconds) {
		return
	}
	d := seconds
	t.Duration = &d
}

// parseWAVINFOBlock walks the body of a RIFF LIST/INFO chunk and
// populates Track text fields from common INFO sub-chunks. Each
// sub-chunk: 4 bytes ID + 4 bytes LE size + N bytes ASCII text
// (often null-terminated) + pad byte if size is odd.
//
// INFO is text-only — no artwork field exists in the spec. Fields
// only fill when not already populated (ID3 chunk, if present,
// wins via populateFromTagMetadata's empty-field guards).
//
// Common INFO sub-chunk IDs (per the RIFF spec):
//   - INAM: title
//   - IART: artist
//   - IPRD: album/product
//   - ICRD: creation date (often "YYYY-MM-DD" — we don't parse Year)
//   - IGNR: genre
//
// Composer / Conductor / Work atoms aren't in the standard RIFF
// INFO set, so PR-D's classical metadata flow only reaches WAV/AIFF
// tracks via their embedded ID3v2 chunks. Acceptable since WAV is
// rarely used for classical libraries today.
func parseWAVINFOBlock(body []byte, t *Track) {
	for len(body) >= 8 {
		id := string(body[0:4])
		size := binary.LittleEndian.Uint32(body[4:8])
		if uint64(size) > uint64(len(body)-8) {
			break
		}
		// Widen BEFORE adding: `8 + size` in uint32 would wrap for
		// size >= 0xFFFFFFF8 (low>high → slice panic). Unreachable given
		// the guard above + the 64 KiB LIST cap, but kept consistent with
		// the be64 walkers as defense-in-depth (Q18).
		payload := body[8 : 8+uint64(size)]
		// RIFF INFO values are null-terminated C-strings. Truncate at
		// the FIRST NUL before converting — some encoders pad the
		// declared size with non-NUL junk after the terminator
		// (e.g. ['H','i',0x00,0xAA,0xBB,0x00]), and a trailing-only
		// TrimRight("\x00") would leave that interior garbage embedded
		// in the string, corrupting the Track field (and downstream
		// JSON / iOS rendering). Cutting at the first NUL drops
		// everything past the terminator in one pass.
		if i := bytes.IndexByte(payload, 0); i >= 0 {
			payload = payload[:i]
		}
		text := strings.TrimSpace(string(payload))
		switch id {
		case "INAM":
			if text != "" && t.Title == "" {
				t.Title = text
			}
		case "IART":
			if text != "" && t.Artist == "" {
				t.Artist = text
			}
		case "IPRD":
			if text != "" && t.Album == "" {
				t.Album = text
			}
		case "IGNR":
			if text != "" && t.Genre == "" {
				t.Genre = text
			}
		}
		// Widen BEFORE adding (not `uint64(8 + size)`): the inner `8 + size`
		// evaluates in uint32 and would wrap for size >= 0xFFFFFFF8, yielding
		// advance==0 → an infinite loop. Guarded unreachable today; consistent
		// with the be64 walkers (Q18).
		advance := 8 + uint64(size)
		if advance%2 == 1 {
			advance++
		}
		if advance > uint64(len(body)) {
			break
		}
		body = body[advance:]
	}
}

// WAVE format tags (the `wFormatTag` field at fmt-chunk offset 0). Only
// PCM and IEEE-float carry a meaningful integer/float bit depth; A-law,
// mu-law, ADPCM, MP3-in-WAV (0x55), etc. are compressed and their
// bitsPerSample is a container artefact, not a signal depth.
const (
	wavFormatPCM        = 0x0001
	wavFormatIEEEFloat  = 0x0003
	wavFormatExtensible = 0xFFFE
)

// parseWAVFmtChunk reads the PCM geometry from a RIFF/WAVE fmt chunk
// body and stamps t.SampleRate + t.BitsPerSample. Layout (all
// little-endian): [0:2] wFormatTag, [2:4] nChannels, [4:8] nSamplesPerSec,
// [8:12] nAvgBytesPerSec, [12:14] nBlockAlign, [14:16] wBitsPerSample.
//
// WAVE_FORMAT_EXTENSIBLE (0xFFFE) wraps the real format code in the
// first 2 bytes of the SubFormat GUID (offset 24); wBitsPerSample at
// [14:16] is then the container width (the value iOS / the composition
// bar want), with the valid-bits count at [18:20]. BitsPerSample is set
// only for PCM / IEEE-float and gated by canSetBitsPerSample (allowlists
// "WAV") as defense-in-depth, matching every other bits-write site.
//
// Returns the stream's bytes-per-second for the duration derivation —
// `nAvgBytesPerSec` as written, which is defined for compressed WAV
// formats too (ADPCM, MP3-in-WAV), where `nSamplesPerSec × nBlockAlign`
// would be a compressed block size and wrong; that product is used only
// as a fallback for a PCM-like file whose writer left the field 0
// (widened to 64 bits so a forged rate × block-align cannot wrap into a
// small, plausible-looking number). 0 means "cannot derive".
func parseWAVFmtChunk(body []byte, t *Track) (bytesPerSecond uint64) {
	if len(body) < 16 {
		return 0
	}
	formatTag := binary.LittleEndian.Uint16(body[0:2])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
	avgBytesPerSec := binary.LittleEndian.Uint32(body[8:12])
	blockAlign := binary.LittleEndian.Uint16(body[12:14])
	bitsPerSample := binary.LittleEndian.Uint16(body[14:16])

	effectiveFormat := formatTag
	if formatTag == wavFormatExtensible && len(body) >= 26 {
		effectiveFormat = binary.LittleEndian.Uint16(body[24:26])
	}

	if sampleRate > 0 {
		sr := float64(sampleRate)
		t.SampleRate = &sr
	}
	isPCMLike := effectiveFormat == wavFormatPCM || effectiveFormat == wavFormatIEEEFloat
	if isPCMLike && bitsPerSample > 0 && canSetBitsPerSample(t.Codec) {
		bps := int(bitsPerSample)
		t.BitsPerSample = &bps
	}
	if avgBytesPerSec > 0 {
		return uint64(avgBytesPerSec)
	}
	if isPCMLike {
		return uint64(sampleRate) * uint64(blockAlign)
	}
	return 0
}

// applyEmbeddedID3 parses an embedded ID3v2 chunk body, merges its tags
// into t, and returns the metadata carrier extractLocalArtwork should
// use after the walk.
//
// Policy: the EARLIEST ID3 chunk wins ENTIRELY. Once a chunk has been
// applied (`existing != nil`), any later chunk is skipped whole — no
// re-parse, no re-populate. This keeps a track's text fields and its
// APIC artwork carrier consistent. Pre-fix, populateFromTagMetadata ran
// unconditionally on every chunk, and its `if v != ""` guards are
// last-non-empty-wins (NOT first-wins), so a second ID3 chunk overwrote
// the text fields while the returned carrier stayed the first chunk — a
// torn state where text came from the last chunk but artwork from the
// first. The short-circuit below is what enforces first-wins; it is NOT
// enforced inside populateFromTagMetadata.
//
// On a parse failure it logs (logPrefix names the AIFF / WAV caller) and
// returns `existing` unchanged. Centralised because the AIFF and WAV
// walkers' ID3 handling is otherwise byte-identical.
func applyEmbeddedID3(body []byte, t *Track, existing tag.Metadata, absPath, logPrefix string) tag.Metadata {
	// Earliest chunk wins entirely — ignore any duplicate ID3 chunk.
	if existing != nil {
		return existing
	}
	m, err := tag.ReadID3v2Tags(bytes.NewReader(body))
	if err != nil {
		scanLogger.Warn(logPrefix+": embedded ID3v2 parse failed",
			"path", absPath, "err", err)
		return existing
	}
	populateFromTagMetadata(m, t)
	return m
}

// seekPastChunk advances the file cursor past `size` bytes plus
// one pad byte when size is odd (IFF / RIFF alignment rule).
// Centralised so AIFF and WAV walkers stay consistent on the
// odd-payload alignment behaviour.
func seekPastChunk(f *os.File, size int64) error {
	skip := size
	if skip%2 == 1 {
		skip++
	}
	if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
		return fmt.Errorf("seek past chunk: %w", err)
	}
	return nil
}
