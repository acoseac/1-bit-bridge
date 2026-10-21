package decode

import (
	"encoding/binary"
	"io"
	"math"
)

type sniffKind int

const (
	sniffUnknown sniffKind = iota
	sniffFLAC
	sniffWAV
	sniffAIFF
	sniffALAC
	sniffDSF
	sniffDFF
	sniffMP3
	sniffAAC
	sniffOgg
)

type sniffResult struct {
	kind  sniffKind
	start int64 // absolute byte offset of the container origin
}

// sniff peeks at rs. It never decides by file extension. start is non-zero
// when an ID3v2 tag precedes a recognised lossless container.
func sniff(rs io.ReadSeeker) (sniffResult, error) {
	var hdr [12]byte
	n, err := io.ReadFull(rs, hdr[:])
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		res, _ := sniffFrom(hdr[:n], nil, 0)
		return res, nil
	}
	if err != nil {
		return sniffResult{}, err
	}
	return sniffFrom(hdr[:], rs, 0)
}

func sniffFrom(hdr []byte, rs io.ReadSeeker, base int64) (sniffResult, error) {
	if len(hdr) >= 3 && string(hdr[:3]) == "ID3" {
		if rs == nil {
			return sniffResult{kind: sniffMP3, start: base}, nil
		}
		return sniffAfterID3(rs, hdr, base)
	}
	return sniffContainer(hdr, rs, base)
}

// sniffContainer classifies a non-ID3 payload at base. Shared by the top-level
// sniff and the post-ID3 path so ID3-prefixed DSF/DFF/RF64/MP4 match the
// unprefixed reasons (Argon R3 M1).
func sniffContainer(hdr []byte, rs io.ReadSeeker, base int64) (sniffResult, error) {
	if len(hdr) >= 4 && string(hdr[:4]) == "fLaC" {
		return sniffResult{kind: sniffFLAC, start: base}, nil
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "RIFF" && string(hdr[8:12]) == "WAVE" {
		return sniffResult{kind: sniffWAV, start: base}, nil
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "RF64" && string(hdr[8:12]) == "WAVE" {
		return sniffResult{}, refuse(ReasonUnsupportedLayout, "RF64 WAVE is not supported")
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "FORM" && (string(hdr[8:12]) == "AIFF" || string(hdr[8:12]) == "AIFC") {
		return sniffResult{kind: sniffAIFF, start: base}, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "DSD " {
		return sniffResult{kind: sniffDSF, start: base}, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "FRM8" {
		return sniffResult{kind: sniffDFF, start: base}, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "OggS" {
		return sniffResult{kind: sniffOgg, start: base}, nil
	}
	if len(hdr) >= 8 && string(hdr[4:8]) == "ftyp" {
		if rs == nil {
			return sniffResult{kind: sniffUnknown, start: base}, nil
		}
		return sniffMP4Brand(rs, base)
	}
	if len(hdr) >= 2 && hdr[0] == 0xFF && hdr[1]&0xE0 == 0xE0 {
		return sniffResult{kind: sniffMP3, start: base}, nil
	}
	return sniffResult{kind: sniffUnknown, start: base}, nil
}

func sniffAfterID3(rs io.ReadSeeker, hdr []byte, base int64) (sniffResult, error) {
	if len(hdr) < 10 {
		var more [10]byte
		copy(more[:], hdr)
		if _, err := io.ReadFull(rs, more[len(hdr):]); err != nil {
			return sniffResult{kind: sniffMP3, start: base}, nil
		}
		hdr = more[:]
	}
	size := int(hdr[6]&0x7F)<<21 | int(hdr[7]&0x7F)<<14 | int(hdr[8]&0x7F)<<7 | int(hdr[9]&0x7F)
	if size < 0 || size > 16<<20 {
		return sniffResult{kind: sniffMP3, start: base}, nil
	}
	// ID3v2 size covers frames (and an extended header, when present). A
	// v2.4 footer (flag 0x10) sits after that size and is not included.
	start := base + int64(10+size)
	if hdr[5]&0x10 != 0 {
		start += 10
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return sniffResult{}, err
	}
	var next [12]byte
	n, err := io.ReadFull(rs, next[:])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		res, _ := sniffContainer(next[:n], nil, start)
		if res.kind == sniffUnknown && n >= 2 && next[0] == 0xFF && next[1]&0xE0 == 0xE0 {
			return sniffResult{kind: sniffMP3, start: start}, nil
		}
		return res, nil
	}
	if err != nil {
		return sniffResult{}, err
	}
	// A second ID3 tag is not a nested skip — refuse rather than recurse.
	if string(next[:3]) == "ID3" {
		return sniffResult{}, refuse(ReasonUnsupportedLayout, "stacked ID3v2 tags are not supported")
	}
	res, err := sniffContainer(next[:], rs, start)
	if err != nil {
		return sniffResult{}, err
	}
	if res.kind == sniffUnknown {
		return sniffResult{}, refuse(ReasonUnsupportedLayout, "ID3v2 tag not followed by a supported lossless container")
	}
	return res, nil
}

func sniffMP4Brand(rs io.ReadSeeker, base int64) (sniffResult, error) {
	if _, err := rs.Seek(base, io.SeekStart); err != nil {
		return sniffResult{}, err
	}
	alac, aac, err := scanMP4Codecs(rs)
	if err != nil {
		return sniffResult{}, err
	}
	if alac {
		return sniffResult{kind: sniffALAC, start: base}, nil
	}
	if aac {
		return sniffResult{kind: sniffAAC, start: base}, nil
	}
	return sniffResult{kind: sniffUnknown, start: base}, nil
}

// scanMP4Codecs walks ISO-BMFF boxes looking for stsd sample-entry fourccs.
func scanMP4Codecs(rs io.ReadSeeker) (alac, aac bool, err error) {
	for i := 0; i < maxMP4Boxes; i++ {
		var hdr [8]byte
		n, rerr := io.ReadFull(rs, hdr[:])
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF || n == 0 {
			return alac, aac, nil
		}
		if rerr != nil {
			return false, false, rerr
		}
		size := uint64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		headerLen := uint64(8)
		if size == 1 {
			var big [8]byte
			if _, err := io.ReadFull(rs, big[:]); err != nil {
				return alac, aac, nil
			}
			size = binary.BigEndian.Uint64(big[:])
			headerLen = 16
		}
		if size == 0 {
			return alac, aac, nil
		}
		if size < headerLen {
			return false, false, refuse(ReasonUnsupportedLayout, "MP4 box size %d is smaller than its header", size)
		}
		payload := size - headerLen
		// Seek offsets and slice lengths are int64/int; a file-supplied size
		// that does not fit must not be converted before the bound check.
		if payload > uint64(math.MaxInt64) {
			return false, false, refuse(ReasonUnsupportedLayout, "MP4 box size %d exceeds addressable length", size)
		}
		switch typ {
		case "mdat", "free", "skip", "wide":
			if _, err := rs.Seek(int64(payload), io.SeekCurrent); err != nil {
				return alac, aac, nil
			}
		case "moov", "moof":
			// Cap the in-memory walk; a large covr or sample table must not
			// discard codecs already found earlier in the same moov.
			truncated := payload > maxMoovBytes
			scan := payload
			if truncated {
				scan = maxMoovBytes
			}
			buf := make([]byte, scan)
			n, _ := io.ReadFull(rs, buf)
			buf = buf[:n]
			// A file that ends inside moov is cut there the way the cap
			// cuts a large one: the box that crosses the end is a boundary,
			// so the codec found before it decides the refusal, and the ALAC
			// demuxer reports the file as cut short.
			truncated = truncated || uint64(n) < scan
			boxCount := 0
			a, c, werr := codecsInBoxes(buf, 0, &boxCount, truncated)
			if werr != nil {
				return false, false, werr
			}
			alac = alac || a
			aac = aac || c
			remain := int64(payload) - int64(n)
			if remain > 0 {
				if _, err := rs.Seek(remain, io.SeekCurrent); err != nil {
					return alac, aac, nil
				}
			}
			if alac {
				return true, aac, nil
			}
		default:
			if _, err := rs.Seek(int64(payload), io.SeekCurrent); err != nil {
				return alac, aac, nil
			}
		}
	}
	return alac, aac, nil
}

// isoBMFFContainers are boxes whose payload is a sequence of child boxes.
// meta is a FullBox (version/flags before children) and is handled separately.
var isoBMFFContainers = map[string]bool{
	"moov": true, "moof": true, "trak": true, "mdia": true,
	"minf": true, "stbl": true, "edts": true, "dinf": true,
	"mvex": true, "traf": true, "udta": true,
}

// codecsInBoxes walks nested ISO-BMFF boxes looking for stsd sample entries.
// depth and boxCount cap recursion against crafted moov buffers (Argon R3 M2).
// truncated is true when the top-level moov/moof read hit maxMoovBytes: a child
// whose declared size runs past the buffer is the scan boundary (descend into
// containers with the flag still set, then stop), not UnsupportedLayout.
func codecsInBoxes(buf []byte, depth int, boxCount *int, truncated bool) (alac, aac bool, err error) {
	if depth > maxMP4BoxDepth {
		return false, false, nil
	}
	err = walkBoxes(buf, truncated, func(typ string, payload []byte, trunc bool) error {
		if *boxCount >= maxMP4Boxes {
			return nil
		}
		*boxCount++
		switch {
		case typ == "stsd":
			a, c := parseSTSD(payload)
			alac = alac || a
			aac = aac || c
		case typ == "meta":
			// FullBox: 1 byte version + 3 bytes flags, then child boxes.
			if len(payload) < 4 {
				return nil
			}
			a, c, e := codecsInBoxes(payload[4:], depth+1, boxCount, trunc)
			alac = alac || a
			aac = aac || c
			return e
		case isoBMFFContainers[typ]:
			a, c, e := codecsInBoxes(payload, depth+1, boxCount, trunc)
			alac = alac || a
			aac = aac || c
			return e
		}
		return nil
	})
	return alac, aac, err
}

func parseSTSD(payload []byte) (alac, aac bool) {
	// stsd: 1 byte version, 3 flags, 4 entry count, then entries.
	if len(payload) < 8 {
		return false, false
	}
	rest := payload[8:]
	for len(rest) >= 8 {
		esz := uint64(binary.BigEndian.Uint32(rest[0:4]))
		if esz < 8 || esz > uint64(len(rest)) {
			return alac, aac
		}
		entryType := string(rest[4:8])
		switch entryType {
		case "alac":
			alac = true
		case "mp4a":
			aac = true
		}
		rest = rest[esz:]
	}
	return alac, aac
}

// walkBoxes iterates ISO-BMFF boxes in buf. Box sizes are compared as uint64
// against uint64(len(buf)) before any conversion used for slicing — a size
// with the high bit set (e.g. 0x8000000000000000) must not become a negative
// int that passes int(size) > len(buf) and then panics on the slice.
//
// When truncated is set (moov/moof scan hit maxMoovBytes), a size past the
// buffer end is the scan boundary: fn receives the partial payload with
// trunc=true and walking stops. Complete boxes (size ≤ len(buf)) are handed
// to fn with trunc=false so invalid sizes inside a finished buffer still refuse.
func walkBoxes(buf []byte, truncated bool, fn func(typ string, payload []byte, trunc bool) error) error {
	for len(buf) >= 8 {
		size := uint64(binary.BigEndian.Uint32(buf[0:4]))
		typ := string(buf[4:8])
		headerLen := uint64(8)
		if size == 1 {
			if uint64(len(buf)) < 16 {
				return nil
			}
			size = binary.BigEndian.Uint64(buf[8:16])
			headerLen = 16
		}
		if size == 0 {
			return nil
		}
		if size < headerLen {
			return refuse(ReasonUnsupportedLayout, "MP4 box %q size %d is smaller than its header", typ, size)
		}
		if size > uint64(len(buf)) {
			return crossingBox(buf, typ, size, headerLen, truncated, fn)
		}
		if err := fn(typ, buf[headerLen:size], false); err != nil {
			return err
		}
		buf = buf[size:]
	}
	return nil
}

// crossingBox handles a box whose declared size runs past buf. In a complete
// buffer that is a layout error. When the moov/moof read was capped, the
// partial payload of the box that crosses the 4 MiB cut is walked with
// trunc=true, and the walk then stops. walkBoxes guarantees headerLen <=
// len(buf): its loop needs 8 bytes, and a 16-byte header returns earlier.
func crossingBox(buf []byte, typ string, size, headerLen uint64, truncated bool, fn func(typ string, payload []byte, trunc bool) error) error {
	if !truncated {
		return refuse(ReasonUnsupportedLayout, "MP4 box %q size %d exceeds buffer (%d bytes)", typ, size, len(buf))
	}
	return fn(typ, buf[headerLen:], true)
}
