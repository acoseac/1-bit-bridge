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
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("aiff: ID3 body read: %w", err)
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
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("aiff: COMM body read: %w", err)
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("aiff: COMM pad seek: %w", err)
				}
			}
			parseAIFFCOMMChunk(body, t, formType)
			continue
		}
		if err := seekPastChunk(f, int64(size)); err != nil {
			return err
		}
	}

	if ec != nil && ec.ArtworkCacheDir != "" {
		// Pass the dhowden Metadata (or nil if no ID3 chunk surfaced)
		// to extractLocalArtwork — embedded APIC wins if present,
		// folder-level cover.jpg / folder.jpg fallback fires otherwise.
		extractLocalArtwork(absPath, t, idTagMetadata, ec)
	}
	return nil
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
// SampleRate is always stamped. BitsPerSample is gated TWICE: by
// canSetBitsPerSample (allowlists "AIFF") AND by aiffCOMMHasPCMDepth —
// because `.aifc` is stamped Codec="AIFF" before the COMM is parsed, a
// COMPRESSED AIFC variant (e.g. ima4 / ulaw) would otherwise surface its
// COMM.sampleSize as a real PCM bit depth, the AIFF analog of the iOS
// PR #371 "lossy source reports a container bit depth" regression. For
// AIFC we therefore only set bits when the compressionType is a known
// PCM-like FOURCC. Plain AIFF is uncompressed by definition, so it's
// always eligible.
func parseAIFFCOMMChunk(body []byte, t *Track, formType string) {
	if len(body) < 18 {
		return
	}
	sampleSize := int16(binary.BigEndian.Uint16(body[6:8]))
	sampleRate := parseAIFFExtended(body[8:18])
	if sampleRate > 0 {
		t.SampleRate = &sampleRate
	}
	if sampleSize > 0 && canSetBitsPerSample(t.Codec) && aiffCOMMHasPCMDepth(body, formType) {
		bps := int(sampleSize)
		t.BitsPerSample = &bps
	}
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

	var idTagMetadata tag.Metadata
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
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("wav: ID3 body read: %w", err)
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
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("wav: LIST body read: %w", err)
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
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("wav: fmt body read: %w", err)
			}
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("wav: fmt pad seek: %w", err)
				}
			}
			parseWAVFmtChunk(body, t)
		default:
			if err := seekPastChunk(f, int64(size)); err != nil {
				return err
			}
		}
	}

	if ec != nil && ec.ArtworkCacheDir != "" {
		extractLocalArtwork(absPath, t, idTagMetadata, ec)
	}
	return nil
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
		payload := body[8 : 8+size]
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
		advance := uint64(8 + size)
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
func parseWAVFmtChunk(body []byte, t *Track) {
	if len(body) < 16 {
		return
	}
	formatTag := binary.LittleEndian.Uint16(body[0:2])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
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
