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
//   - `mp4MaxAtomsPerSearch` (4096) bounds EVERY findAtom call —
//     including the top-level moov search — by count of headers read,
//     NOT byte span. IO is governed by atom headers (8 bytes each);
//     `cursor += atomSize` jumps payloads. A byte-span cap punished
//     legitimately-large moov containers in long audiobooks / DJ
//     mixes (>4 MiB moov), causing silent metadata drop. The 4096
//     ceiling is generous for any realistic file (typical: <100;
//     pathological: <1000); a top-level file with >4096 atoms before
//     `moov` returns an "iteration budget" error, which is the
//     desired safety-net behaviour.
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

// errMP4StructureNotFound wraps the "expected box missing" failures
// from `findSTSD` (moov / trak / mdia / minf / stbl / stsd not
// found). Callers can `errors.Is(err, errMP4StructureNotFound)` to
// distinguish "this isn't a valid MP4 audio file / the file mutated
// between walks" from genuine I/O failures (seek / read /
// atom-iteration-budget) — the former should produce honest
// suppression (return 0 bits, no error), the latter should propagate
// to the caller's `scanLogger.Warn` so operators see real I/O
// problems on their scan log. Per CodeRabbit Major round-2 on PR #237.
var errMP4StructureNotFound = errors.New("mp4: structure not found")

// extractMP4Codec parses the audio codec FourCC from an MP4 file's
// stsd box. Returns one of "ALAC", "AAC", or "" (unknown — caller
// should fall through to extension-derived classification).
func extractMP4Codec(r io.ReadSeeker) (string, error) {
	stsdStart, stsdHdr, stsdSize, err := findSTSD(r)
	if err != nil {
		return "", err
	}

	// stsd payload (after the box header — 8 bytes for normal, 16 for
	// 64-bit largesize):
	//   1 byte: version
	//   3 bytes: flags
	//   4 bytes: entry_count (uint32)
	//   then for each entry:
	//     4 bytes: size (uint32)
	//     4 bytes: format (FourCC)  ← the codec id
	// Bounds guard mirroring extractALACBitDepth: refuse to read the entry
	// header if the declared stsd box is too short to hold it. Without this,
	// a truncated/malformed .m4a surfaces io.EOF / parse errors to the
	// scanner loop instead of a clean "" fallback to extension classification.
	entryStart := stsdStart + stsdHdr + 8
	if entryStart+8 > stsdStart+stsdSize {
		return "", nil
	}
	if _, err := r.Seek(int64(entryStart), io.SeekStart); err != nil {
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

// findSTSD walks the standard MP4 atom hierarchy
// `moov → trak → mdia → minf → stbl → stsd` and returns the stsd
// box's start offset, header size (8 or 16 for 64-bit largesize),
// and total size. Each descent uses the parent's actual header size
// (NOT a hardcoded `mp4HeaderSize`) so 64-bit largesize containers
// (long audiobooks / DJ mixes whose moov / trak exceeds 4 GiB) are
// re-entered at the correct offset rather than 8 bytes into the
// extended header.
//
// Shared by `extractMP4Codec` (reads the first sample entry's
// FourCC) and `extractALACBitDepth` (descends one level further into
// the inner `alac` config atom). Pre-share, the descent chain was
// inlined twice — Gemini medium on PR #237: a duplicated walk over
// NAS / SMB is six extra round-trips per ALAC file scanned, and
// any future change to the descent chain would need two edits in
// lockstep.
//
// Returns an error when any box in the chain is missing — the only
// caller (extractMP4Codec) propagates that, which `extractALACBitDepth`
// upgrades to honest-suppression (returns `(0, nil)`) since the
// caller can't tell apart "this isn't an MP4" from "this is an MP4
// with no audio track" and either way the right behaviour is "leave
// bits nil".
func findSTSD(r io.ReadSeeker) (start, headerSize, size uint64, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, 0, 0, err
	}

	moovStart, moovHdr, moovSize, err := findAtom(r, "moov", 0, mp4MaxHeaderReadBudget)
	if err != nil {
		return 0, 0, 0, err
	}
	if moovSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: moov", errMP4StructureNotFound)
	}
	trakStart, trakHdr, trakSize, err := findAtom(r, "trak", moovStart+moovHdr, moovStart+moovSize)
	if err != nil {
		return 0, 0, 0, err
	}
	if trakSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: trak", errMP4StructureNotFound)
	}
	mdiaStart, mdiaHdr, mdiaSize, err := findAtom(r, "mdia", trakStart+trakHdr, trakStart+trakSize)
	if err != nil {
		return 0, 0, 0, err
	}
	if mdiaSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: mdia", errMP4StructureNotFound)
	}
	minfStart, minfHdr, minfSize, err := findAtom(r, "minf", mdiaStart+mdiaHdr, mdiaStart+mdiaSize)
	if err != nil {
		return 0, 0, 0, err
	}
	if minfSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: minf", errMP4StructureNotFound)
	}
	stblStart, stblHdr, stblSize, err := findAtom(r, "stbl", minfStart+minfHdr, minfStart+minfSize)
	if err != nil {
		return 0, 0, 0, err
	}
	if stblSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: stbl", errMP4StructureNotFound)
	}
	stsdStart, stsdHdr, stsdSize, err := findAtom(r, "stsd", stblStart+stblHdr, stblStart+stblSize)
	if err != nil {
		return 0, 0, 0, err
	}
	if stsdSize <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: stsd", errMP4StructureNotFound)
	}
	return stsdStart, stsdHdr, stsdSize, nil
}

// extractALACBitDepth walks the same atom chain as extractMP4Codec
// (moov → trak → mdia → minf → stbl → stsd → outer `alac` sample
// entry) and descends one level further into the inner `alac` config
// atom to read the source bit depth from the ALACSpecificConfig.
//
// Returns the source bit depth (typically 16 / 20 / 24 / 32) on
// success, (0, nil) when the layout doesn't match (atypical encoder /
// non-ALAC file / corrupt container) so the caller can skip the
// manifest assignment without erroring, or a non-nil error on I/O
// failure or budget exhaustion in `findAtom`.
//
// MP4 layout reference (validated against `01 Espina.m4a` 2026-05-14):
//
//	stsd payload:
//	  [1 byte version][3 bytes flags][4 bytes entry_count]
//	  entry:
//	    [4 bytes entry_size][4 bytes FourCC = "alac"]
//	    audio sample-entry header (28 bytes):
//	      [6 reserved][2 data_reference_index]
//	      [8 reserved][2 channel_count][2 sample_size]
//	      [2 pre_defined][2 reserved][4 sample_rate (16.16 fixed)]
//	    inner `alac` config atom:
//	      [8 bytes atom header (size + "alac")]
//	      FullBox payload:
//	        [4 bytes version+flags]
//	        ALACSpecificConfig (24 bytes):
//	          [4 frameLength][1 compatibleVersion][1 BIT_DEPTH]...
//
// So bitDepth lives at (inner alac payload offset 4) + 5 = 9. The
// outer sample entry is NOT a FullBox; the inner one IS, hence the
// 4-byte version+flags shift before ALACSpecificConfig.
func extractALACBitDepth(r io.ReadSeeker) (int, error) {
	stsdStart, stsdHdr, stsdSize, err := findSTSD(r)
	if err != nil {
		// Distinguish structural-not-found from genuine I/O failures.
		// Structural cases (moov / trak / .../stsd missing) are
		// suppressed to (0, nil) — the caller already knows the file
		// is ALAC from `extractMP4Codec`, so a chain-failure here
		// means the file mutated between the two walks (NAS hiccup,
		// scanner timing), and the right behaviour is to leave bits
		// nil rather than fail-loud the whole track scan. I/O / seek
		// / atom-walk-budget errors propagate so the caller's
		// `scanLogger.Warn` surfaces real I/O problems on the operator
		// scan log. Per CodeRabbit Major round-2 on PR #237.
		if errors.Is(err, errMP4StructureNotFound) {
			return 0, nil
		}
		return 0, err
	}

	// Skip stsd version+flags (4 bytes) + entry_count (4 bytes) to
	// land on the first sample entry's 4-byte size field.
	entryStart := stsdStart + stsdHdr + 8
	if entryStart+8 > stsdStart+stsdSize {
		return 0, nil
	}
	if _, err := r.Seek(int64(entryStart), io.SeekStart); err != nil {
		return 0, err
	}
	var entrySize uint32
	if err := binary.Read(r, binary.BigEndian, &entrySize); err != nil {
		return 0, err
	}
	var fourcc [4]byte
	if _, err := io.ReadFull(r, fourcc[:]); err != nil {
		return 0, err
	}
	if string(fourcc[:]) != "alac" {
		// Not ALAC — caller doesn't need bits.
		return 0, nil
	}

	// Outer sample-entry box: header is the 8 bytes we just read.
	// Audio sample-entry header is 28 more bytes (see docblock).
	// Inner atoms start at entryStart + 8 (sample-entry header) + 28.
	const audioSampleEntryHeaderBytes = 28
	innerSearchStart := entryStart + 8 + audioSampleEntryHeaderBytes
	// Bound entrySize against the enclosing stsd box. `entrySize` is
	// untrusted container data: a malformed file declaring
	// `entrySize == 0xFFFFFFFF` would otherwise let the inner
	// `findAtom("alac", …)` scan beyond stsd into unrelated mp4 boxes
	// (stts, stsc, stsz, …) — and any 4-byte stretch that happens to
	// spell "alac" would yield a false-positive bit depth instead of
	// honest suppression. Clamp to `min(entryStart + entrySize,
	// stsdEnd)` and bail on underflow / inversion. Per CodeRabbit
	// Major on PR #237.
	entryEnd := entryStart + uint64(entrySize)
	stsdEnd := stsdStart + stsdSize
	if entryEnd < entryStart || entryEnd > stsdEnd {
		// Underflow (overflow wraparound) OR entry claims to extend
		// past stsd — both are corruption signals; return honest 0.
		return 0, nil
	}
	innerSearchEnd := entryEnd
	if innerSearchStart > innerSearchEnd {
		return 0, nil
	}

	innerStart, innerHdr, innerSize, err := findAtom(r, "alac", innerSearchStart, innerSearchEnd)
	if err != nil {
		return 0, err
	}
	if innerSize == 0 {
		// Atypical encoder / corrupt container — caller skips
		// assignment gracefully.
		return 0, nil
	}

	// Inner `alac` is a FullBox. Payload = 4-byte version+flags
	// followed by ALACSpecificConfig. bitDepth = ALACSpecificConfig
	// byte 5 = inner payload byte 9. The ALAC magic cookie ref
	// (ALACMagicCookieDescription.txt): bytes 0-3 frameLength,
	// byte 4 compatibleVersion, BYTE 5 bitDepth.
	const bitDepthPayloadOffset = 9
	if innerSize < innerHdr+bitDepthPayloadOffset+1 {
		// Truncated inner atom — return 0 rather than error so the
		// caller falls through cleanly (matches PR #376's "honest
		// suppression" precedent for ALAC bit-depth uncertainty).
		return 0, nil
	}
	if _, err := r.Seek(int64(innerStart+innerHdr+bitDepthPayloadOffset), io.SeekStart); err != nil {
		return 0, err
	}
	var bitDepth [1]byte
	if _, err := io.ReadFull(r, bitDepth[:]); err != nil {
		return 0, err
	}
	return int(bitDepth[0]), nil
}

// findAtom scans MP4 atoms starting at `start` (absolute byte offset
// in the reader) up to `endBound` (exclusive) for one with type
// matching `name` (4-character ASCII). Returns the atom's start
// offset, its header size (8 for normal boxes, 16 for 64-bit
// `largesize` boxes — size==1, where the next 8 bytes hold the real
// uint64 size), and its full size (header + payload). A returned
// `size` of 0 means not found.
//
// Single-pass, non-recursive — caller drives the descent into nested
// containers. Callers descending into the payload MUST use
// `offset + headerSize` (not `+ mp4HeaderSize`) so a 64-bit container
// (e.g. a `moov`/`trak` exceeding 4 GiB in long audiobooks / DJ
// mixes) doesn't have its descent collide with the 8-byte largesize
// field. `endBound` is exclusive of the byte at that position (so an
// atom that ends EXACTLY at endBound is included, but an atom whose
// header would start at endBound is not).
func findAtom(r io.ReadSeeker, name string, start, endBound uint64) (offset, headerSize, size uint64, err error) {
	if len(name) != 4 {
		return 0, 0, 0, fmt.Errorf("mp4: findAtom: name %q must be 4 chars", name)
	}
	cursor := start
	iterations := 0
	for cursor+mp4HeaderSize <= endBound {
		if iterations >= mp4MaxAtomsPerSearch {
			return 0, 0, 0, fmt.Errorf("mp4: atom walk exceeded %d-atom iteration budget at cursor %d", mp4MaxAtomsPerSearch, cursor)
		}
		iterations++
		if _, err := r.Seek(int64(cursor), io.SeekStart); err != nil {
			return 0, 0, 0, err
		}
		var header [mp4HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Truncated atom near end-of-search-range; treat as
				// not-found rather than propagating EOF up.
				return 0, 0, 0, nil
			}
			return 0, 0, 0, err
		}
		atomSize := uint64(binary.BigEndian.Uint32(header[0:4]))
		atomType := string(header[4:8])
		atomHeaderSize := uint64(mp4HeaderSize)
		// MP4 large-size variant: size==1 means the next 8 bytes
		// hold the real size (uint64). size==0 means "to EOF" (rare
		// in practice; we treat it as the remainder of endBound).
		switch atomSize {
		case 0:
			atomSize = endBound - cursor
		case 1:
			var bigSize [8]byte
			if _, err := io.ReadFull(r, bigSize[:]); err != nil {
				return 0, 0, 0, err
			}
			atomSize = binary.BigEndian.Uint64(bigSize[:])
			atomHeaderSize = 16
		}
		if atomSize < atomHeaderSize {
			// Malformed — atom claims smaller than its own header.
			return 0, 0, 0, fmt.Errorf("mp4: atom %q at %d has size %d < header (%d)", atomType, cursor, atomSize, atomHeaderSize)
		}
		if atomType == name {
			return cursor, atomHeaderSize, atomSize, nil
		}
		next := cursor + atomSize
		if next <= cursor {
			// Wraparound / zero-size loop guard.
			return 0, 0, 0, fmt.Errorf("mp4: atom walk stalled at %d", cursor)
		}
		cursor = next
	}
	return 0, 0, 0, nil
}
