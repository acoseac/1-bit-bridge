package decode

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// DFF (Philips, DSDIFF 1.5): a big-endian "FRM8" form of type "DSD " whose
// chunks each carry an 8-byte size that excludes the 12-byte header and the
// pad byte after an odd body. FVER is the form's first chunk (§3, §3.1): a
// four-byte version whose first byte is the major version. PROP ("SND ")
// holds FS, CHNL and CMPR and precedes the sound data; the "DSD " chunk is
// the uncompressed sound data, byte-interleaved in CHNL order, MSB first.
// Other chunks (COMT, DIIN, ID3, MANF) are skipped by their sizes.
const dffChunkHeader = 12

// dffMajorVersion is the only DSDIFF major version read: FVER's first byte
// (1.5 is 0x01050000, and 1.4 files have the same layout).
const dffMajorVersion = 1

type dffDecoder struct {
	linearRegion
	dsd format.NativeDSD
	// canon[i] is the output channel of the file's i-th interleaved byte.
	canon [dsdChannels]int
}

type dffProp struct {
	rate  uint64
	canon [dsdChannels]int
}

func openDFF(rs io.ReadSeeker) (DSDDecoder, error) {
	var hdr [dffChunkHeader + 4]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return nil, refuse(ReasonTruncated, "DFF header truncated")
	}
	if string(hdr[0:4]) != "FRM8" {
		return nil, refuse(ReasonUnknownContainer, "not a DSDIFF file")
	}
	frmSize := binary.BigEndian.Uint64(hdr[4:12])
	if frmSize > math.MaxInt64-dffChunkHeader {
		return nil, refuse(ReasonUnsupportedLayout, "DFF FRM8 size %d exceeds addressable length", frmSize)
	}
	if frmSize < 4 {
		return nil, refuse(ReasonTruncated, "DFF FRM8 body too small")
	}
	if string(hdr[12:16]) != "DSD " {
		return nil, refuse(ReasonUnsupportedLayout, "DFF form type %q is not DSD", hdr[12:16])
	}
	frmEnd := dffChunkHeader + int64(frmSize)
	pos := int64(len(hdr))
	var (
		prop     *dffProp
		chunkHdr [dffChunkHeader]byte
	)
	for i := 0; i < maxDSDChunks; i++ {
		if pos+dffChunkHeader > frmEnd {
			return nil, refuse(ReasonTruncated, "DFF has no DSD sound data")
		}
		if _, err := io.ReadFull(rs, chunkHdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, refuse(ReasonTruncated, "DFF has no DSD sound data")
			}
			return nil, err
		}
		id := string(chunkHdr[0:4])
		size := binary.BigEndian.Uint64(chunkHdr[4:12])
		body := pos + dffChunkHeader
		// Compare as uint64 before any int64 conversion; the +1 is the
		// pad byte after an odd body.
		if size > uint64(math.MaxInt64-body-1) {
			return nil, refuse(ReasonUnsupportedLayout, "DFF chunk %q size %d exceeds addressable length", id, size)
		}
		end := body + int64(size) + int64(size&1)
		if end > frmEnd {
			return nil, refuse(ReasonTruncated, "DFF chunk %q extends past the FRM8 body", id)
		}
		if i == 0 && id != "FVER" {
			return nil, refuse(ReasonUnsupportedLayout, "DFF's first chunk is %q; FVER must come first", id)
		}
		switch id {
		case "FVER":
			if i != 0 {
				return nil, refuse(ReasonUnsupportedLayout, "DFF has a second FVER chunk")
			}
			if err := readDFFVersion(rs, size); err != nil {
				return nil, err
			}
		case "PROP":
			if prop != nil {
				return nil, refuse(ReasonUnsupportedLayout, "DFF has two PROP chunks")
			}
			if size > maxDFFPropBody {
				return nil, refuse(ReasonUnsupportedLayout, "DFF PROP chunk size %d exceeds %d", size, maxDFFPropBody)
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(rs, b); err != nil {
				return nil, refuse(ReasonTruncated, "DFF PROP chunk truncated")
			}
			p, err := parseDFFProp(b)
			if err != nil {
				return nil, err
			}
			prop = p
		case "DST ":
			return nil, refuse(ReasonUnsupportedComp, "DST-compressed DFF is refused; only uncompressed DSD is played")
		case "DSD ":
			if prop == nil {
				return nil, refuse(ReasonUnsupportedLayout, "DFF sound data before PROP")
			}
			return newDFF(rs, prop, body, size)
		}
		if _, err := rs.Seek(end, io.SeekStart); err != nil {
			return nil, err
		}
		pos = end
	}
	return nil, refuse(ReasonUnsupportedLayout, "DFF has more than %d chunks before its sound data", maxDSDChunks)
}

// readDFFVersion reads FVER's body: exactly four bytes, major version 1.
func readDFFVersion(r io.Reader, size uint64) error {
	if size != 4 {
		return refuse(ReasonUnsupportedLayout, "DFF FVER chunk is %d bytes, want 4", size)
	}
	var v [4]byte
	if _, err := io.ReadFull(r, v[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return refuse(ReasonTruncated, "DFF FVER chunk truncated")
		}
		return err
	}
	if v[0] != dffMajorVersion {
		return refuse(ReasonUnsupportedLayout, "DFF FVER version %d.%d.%d.%d: only major version %d is read",
			v[0], v[1], v[2], v[3], dffMajorVersion)
	}
	return nil
}

// parseDFFProp reads a PROP body held in memory: "SND " and its sub-chunks.
func parseDFFProp(b []byte) (*dffProp, error) {
	if len(b) < 4 || string(b[0:4]) != "SND " {
		return nil, refuse(ReasonUnsupportedLayout, "DFF PROP is not of type SND")
	}
	var fs, chnl, cmpr []byte
	rest := b[4:]
	for i := 0; len(rest) >= dffChunkHeader; i++ {
		if i >= maxDSDChunks {
			return nil, refuse(ReasonUnsupportedLayout, "DFF PROP has more than %d chunks", maxDSDChunks)
		}
		id := string(rest[0:4])
		size := binary.BigEndian.Uint64(rest[4:12])
		if size > uint64(len(rest)-dffChunkHeader) {
			return nil, refuse(ReasonUnsupportedLayout, "DFF PROP chunk %q size %d exceeds PROP", id, size)
		}
		payload := rest[dffChunkHeader : dffChunkHeader+int(size)]
		var dup bool
		switch id {
		case "FS  ":
			dup, fs = fs != nil, payload
		case "CHNL":
			dup, chnl = chnl != nil, payload
		case "CMPR":
			dup, cmpr = cmpr != nil, payload
		}
		if dup {
			return nil, refuse(ReasonUnsupportedLayout, "DFF PROP has two %q chunks", id)
		}
		next := dffChunkHeader + int(size) + int(size&1)
		if next > len(rest) {
			next = len(rest)
		}
		rest = rest[next:]
	}
	for _, c := range []struct {
		name string
		body []byte
	}{{"CMPR", cmpr}, {"CHNL", chnl}, {"FS", fs}} {
		if c.body == nil {
			return nil, refuse(ReasonUnsupportedLayout, "DFF PROP has no %s chunk", c.name)
		}
	}
	if len(cmpr) < 4 {
		return nil, refuse(ReasonUnsupportedLayout, "DFF CMPR chunk too small")
	}
	switch string(cmpr[0:4]) {
	case "DSD ":
	case "DST ":
		return nil, refuse(ReasonUnsupportedComp, "DST-compressed DFF is refused; only uncompressed DSD is played")
	default:
		return nil, refuse(ReasonUnsupportedComp, "DFF compression %q is refused; only uncompressed DSD is played", cmpr[0:4])
	}
	canon, err := parseDFFChannels(chnl)
	if err != nil {
		return nil, err
	}
	if len(fs) != 4 {
		return nil, refuse(ReasonUnsupportedLayout, "DFF FS chunk is %d bytes, want 4", len(fs))
	}
	rate := uint64(binary.BigEndian.Uint32(fs))
	if err := acceptDSDRate("DFF", rate); err != nil {
		return nil, err
	}
	return &dffProp{rate: rate, canon: canon}, nil
}

// parseDFFChannels maps the file's channel order onto left, right.
func parseDFFChannels(chnl []byte) ([dsdChannels]int, error) {
	var canon [dsdChannels]int
	if len(chnl) < 2 {
		return canon, refuse(ReasonUnsupportedLayout, "DFF CHNL chunk too small")
	}
	n := int(binary.BigEndian.Uint16(chnl[0:2]))
	if len(chnl) != 2+4*n {
		return canon, refuse(ReasonUnsupportedLayout, "DFF CHNL lists %d channels in %d bytes", n, len(chnl))
	}
	if err := acceptDSDChannels(uint64(n)); err != nil {
		return canon, err
	}
	ids := [dsdChannels]string{string(chnl[2:6]), string(chnl[6:10])}
	switch ids {
	case [dsdChannels]string{"SLFT", "SRGT"}:
		canon = [dsdChannels]int{0, 1}
	case [dsdChannels]string{"SRGT", "SLFT"}:
		canon = [dsdChannels]int{1, 0}
	default:
		return canon, refuse(ReasonUnsupportedLayout, "DFF stereo channels %q, %q are not SLFT and SRGT", ids[0], ids[1])
	}
	return canon, nil
}

func newDFF(rs io.ReadSeeker, p *dffProp, dataStart int64, size uint64) (DSDDecoder, error) {
	if size%dsdChannels != 0 {
		return nil, refuse(ReasonTruncated, "DFF sound data size %d is not a whole number of frames", size)
	}
	perCh := size / dsdChannels
	if perCh > math.MaxInt64/8 {
		return nil, refuse(ReasonUnsupportedLayout, "DFF sound data size %d exceeds addressable length", size)
	}
	d := &dffDecoder{
		linearRegion: linearRegion{
			rs:          rs,
			regionStart: dataStart,
			regionEnd:   dataStart + int64(size),
			frameBytes:  dsdChannels,
			frames:      int64(perCh),
			earlyEnd:    "DFF sound data ends before its declared size",
			midFrame:    "DFF sound data ends mid-frame",
		},
		dsd: format.NativeDSD{
			BitRateHz: int(p.rate),
			Channels:  dsdChannels,
			Container: format.ContainerDFF,
			Frames:    int64(perCh) * 8,
		},
		canon: p.canon,
	}
	if _, err := rs.Seek(dataStart, io.SeekStart); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *dffDecoder) Format() format.NativeDSD { return d.dsd }

func (d *dffDecoder) Close() error { return nil }

func (d *dffDecoder) Seek(ctx context.Context, pos int64) error {
	return d.seekFrame(ctx, pos,
		fmt.Sprintf("DFF seek byte %d out of range (0..%d)", pos, d.frames),
		"DFF seek past the sound data")
}

// ReadDSD de-interleaves whole frames (one byte per channel) through the
// linear region WAV and AIFF use: reads capped at the sound data, ctx checked
// after each, and the reader seeked back on a cancel or an I/O error.
func (d *dffDecoder) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if d.readStuck != nil {
		return 0, d.readStuck
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	want, err := checkDSDBuf(dst, dsdChannels)
	if err != nil {
		return 0, err
	}
	first, second := dst[d.canon[0]], dst[d.canon[1]]
	return d.readLinear(ctx, want, func(buf []byte, chunk, out int) {
		for i := 0; i < chunk; i++ {
			first[out+i] = buf[2*i]
			second[out+i] = buf[2*i+1]
		}
	})
}
