package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
			// ID3 chunk AND a duplicate; first-wins via the empty-field
			// guards inside populateFromTagMetadata.
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

// applyEmbeddedID3 parses an embedded ID3v2 chunk body, merges its tags
// into t (populateFromTagMetadata is first-wins per field), and returns
// the metadata carrier extractLocalArtwork should use after the walk.
// That carrier is ALSO first-wins: the EARLIEST ID3 chunk's metadata is
// kept, so a later text-only chunk can't drop an earlier chunk's APIC
// artwork. On a parse failure it logs (logPrefix names the AIFF / WAV
// caller) and returns `existing` unchanged. Centralised because the AIFF
// and WAV walkers' ID3 handling is otherwise byte-identical — keeping it
// inline duplicated the parse + first-wins + populate sequence across
// both functions.
func applyEmbeddedID3(body []byte, t *Track, existing tag.Metadata, absPath, logPrefix string) tag.Metadata {
	m, err := tag.ReadID3v2Tags(bytes.NewReader(body))
	if err != nil {
		scanLogger.Warn(logPrefix+": embedded ID3v2 parse failed",
			"path", absPath, "err", err)
		return existing
	}
	populateFromTagMetadata(m, t)
	if existing == nil {
		return m
	}
	return existing
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
