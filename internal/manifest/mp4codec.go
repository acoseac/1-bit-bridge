// Minimal MP4 sample-description-box walker that returns the first
// audio track's codec FourCC. Used to disambiguate ALAC from AAC in
// `.m4a` containers — `dhowden/tag` exposes a `FileType()` method on
// its MP4 metadata, but the implementation never actually populates
// the field for ALAC (their source carries a `FIXME: actually
// detect this` comment), so we walk the atoms ourselves.
//
// The MP4 format is a tree of "atoms" (boxes): each starts with an
// 8-byte header (size: uint32, type: FourCC). The audio codec lives
// in `moov/trak/mdia/minf/stbl/stsd` — the Sample Description box —
// whose first entry's 4-byte type code is the codec FourCC. The
// codes we care about:
//   - `alac` → Apple Lossless (lossless integer PCM)
//   - `mp4a` → MPEG-4 Audio (typically AAC, occasionally legacy MP3
//     wrapped in MP4)
//
// **Read budgets**:
//   - `mp4MaxHeaderReadBudget` (4 MiB) caps the *initial* file-position
//     search for the top-level moov. Covers fast-start M4As (moov near
//     head). Non-fast-start files (mdat-before-moov) silently return
//     "moov not found" — separate, pre-existing limitation.
//   - `mp4MaxAtomsPerSearch` (4096) bounds each nested atom-walk by
//     count of headers read, NOT byte span. IO is governed by atom
//     headers (8 bytes each); `cursor += atomSize` jumps payloads. A
//     byte-span cap punished legitimately-large moov containers in long
//     audiobooks / DJ mixes (>4 MiB moov), causing silent metadata drop.
//
// Per Gemini A1 / iOS bug review #1 (Mirror-PR pair with
// `Track.codec` on the iOS side).

package manifest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MP4 atom walker constants.
const (
	mp4HeaderSize          = 8
	mp4MaxHeaderReadBudget = 4 << 20 // 4 MiB — file-position cap for the top-level moov search
	mp4MaxAtomsPerSearch   = 4096    // per-search atom-walk budget (count, not bytes)
)

// extractMP4Codec parses the audio codec FourCC from an MP4 file's
// stsd box. Returns one of "ALAC", "AAC", or "" (unknown — caller
// should fall through to extension-derived classification).
func extractMP4Codec(r io.ReadSeeker) (string, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	// Walk top-level atoms looking for moov.
	moovStart, moovSize, err := findAtom(r, "moov", 0, mp4MaxHeaderReadBudget)
	if err != nil {
		return "", err
	}
	if moovSize <= 0 {
		return "", errors.New("mp4: moov not found")
	}

	// Inside moov, find the first trak.
	trakStart, trakSize, err := findAtom(r, "trak", moovStart+mp4HeaderSize, moovStart+moovSize)
	if err != nil {
		return "", err
	}
	if trakSize <= 0 {
		return "", errors.New("mp4: trak not found")
	}

	// Walk trak/mdia/minf/stbl/stsd.
	mdiaStart, mdiaSize, err := findAtom(r, "mdia", trakStart+mp4HeaderSize, trakStart+trakSize)
	if err != nil {
		return "", err
	}
	if mdiaSize <= 0 {
		return "", errors.New("mp4: mdia not found")
	}
	minfStart, minfSize, err := findAtom(r, "minf", mdiaStart+mp4HeaderSize, mdiaStart+mdiaSize)
	if err != nil {
		return "", err
	}
	if minfSize <= 0 {
		return "", errors.New("mp4: minf not found")
	}
	stblStart, stblSize, err := findAtom(r, "stbl", minfStart+mp4HeaderSize, minfStart+minfSize)
	if err != nil {
		return "", err
	}
	if stblSize <= 0 {
		return "", errors.New("mp4: stbl not found")
	}
	stsdStart, stsdSize, err := findAtom(r, "stsd", stblStart+mp4HeaderSize, stblStart+stblSize)
	if err != nil {
		return "", err
	}
	if stsdSize <= 0 {
		return "", errors.New("mp4: stsd not found")
	}

	// stsd payload (after the 8-byte atom header):
	//   1 byte: version
	//   3 bytes: flags
	//   4 bytes: entry_count (uint32)
	//   then for each entry:
	//     4 bytes: size (uint32)
	//     4 bytes: format (FourCC)  ← the codec id
	if _, err := r.Seek(int64(stsdStart+mp4HeaderSize+8), io.SeekStart); err != nil {
		return "", err
	}
	var entrySize uint32
	if err := binary.Read(r, binary.BigEndian, &entrySize); err != nil {
		return "", err
	}
	var fourcc [4]byte
	if _, err := io.ReadFull(r, fourcc[:]); err != nil {
		return "", err
	}
	switch string(fourcc[:]) {
	case "alac":
		return "ALAC", nil
	case "mp4a":
		// Defaulting to AAC — `mp4a` is the canonical MPEG-4 Audio
		// FourCC and >99% of `.m4a` files in the wild ship AAC. The
		// rare `mp4a`-wrapped-MP3 variant would still surface as
		// "AAC" here; iOS treats both as `.lossy` either way, so
		// the user-visible classification is correct.
		return "AAC", nil
	default:
		// Unknown codec FourCC — caller falls through.
		return "", nil
	}
}

// findAtom scans MP4 atoms starting at `start` (absolute byte offset
// in the reader) up to `endBound` (exclusive) for one with type
// matching `name` (4-character ASCII). Returns the atom's start offset
// AND its full size (header + payload). Size of 0 means not found.
//
// Single-pass, non-recursive — caller drives the descent into nested
// containers. `endBound` is exclusive of the byte at that position
// (so an atom that ends EXACTLY at endBound is included, but an atom
// whose header would start at endBound is not).
func findAtom(r io.ReadSeeker, name string, start, endBound uint64) (offset, size uint64, err error) {
	if len(name) != 4 {
		return 0, 0, fmt.Errorf("mp4: findAtom: name %q must be 4 chars", name)
	}
	cursor := start
	iterations := 0
	for cursor+mp4HeaderSize <= endBound {
		if iterations >= mp4MaxAtomsPerSearch {
			return 0, 0, fmt.Errorf("mp4: atom walk exceeded %d-atom iteration budget at cursor %d", mp4MaxAtomsPerSearch, cursor)
		}
		iterations++
		if _, err := r.Seek(int64(cursor), io.SeekStart); err != nil {
			return 0, 0, err
		}
		var header [mp4HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Truncated atom near end-of-search-range; treat as
				// not-found rather than propagating EOF up.
				return 0, 0, nil
			}
			return 0, 0, err
		}
		atomSize := uint64(binary.BigEndian.Uint32(header[0:4]))
		atomType := string(header[4:8])
		// MP4 large-size variant: size==1 means the next 8 bytes
		// hold the real size (uint64). size==0 means "to EOF" (rare
		// in practice; we treat it as the remainder of endBound).
		switch atomSize {
		case 0:
			atomSize = endBound - cursor
		case 1:
			var bigSize [8]byte
			if _, err := io.ReadFull(r, bigSize[:]); err != nil {
				return 0, 0, err
			}
			atomSize = binary.BigEndian.Uint64(bigSize[:])
		}
		if atomSize < mp4HeaderSize {
			// Malformed — atom claims smaller than its own header.
			return 0, 0, fmt.Errorf("mp4: atom %q at %d has size %d < header", atomType, cursor, atomSize)
		}
		if atomType == name {
			return cursor, atomSize, nil
		}
		next := cursor + atomSize
		if next <= cursor {
			// Wraparound / zero-size loop guard.
			return 0, 0, fmt.Errorf("mp4: atom walk stalled at %d", cursor)
		}
		cursor = next
	}
	return 0, 0, nil
}
