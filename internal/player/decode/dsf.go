package decode

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// DSF (Sony, DSF File Format Specification 1.01): a 28-byte "DSD " chunk, a
// 52-byte "fmt " chunk and a "data" chunk, all little-endian, then optional
// ID3v2 metadata. Every chunk size includes its 12-byte header. The data is
// block-interleaved: one blockSize run per channel per block group, the
// final group zero-padded; the sample count, not the data size, ends audio.
const (
	dsfDSDChunkSize = 28
	dsfFmtChunkSize = 52
	dsfChunkHeader  = 12
	dsfBlockSize    = 4096
)

type dsfDecoder struct {
	rs        io.ReadSeeker
	dsd       format.NativeDSD
	dataStart int64
	perCh     int64 // bytes per channel the sample count covers
	lsbFirst  bool
	pos       int64 // next byte per channel
	buf       []byte
	readStuck error
}

type dsfFmt struct {
	rate     uint64
	samples  uint64
	lsbFirst bool
}

func openDSF(rs io.ReadSeeker) (DSDDecoder, error) {
	var hdr [dsfDSDChunkSize]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return nil, refuse(ReasonTruncated, "DSF header truncated")
	}
	if string(hdr[0:4]) != "DSD " {
		return nil, refuse(ReasonUnknownContainer, "not a DSF file")
	}
	if size := binary.LittleEndian.Uint64(hdr[4:12]); size != dsfDSDChunkSize {
		return nil, refuse(ReasonUnsupportedLayout, "DSF DSD chunk size %d, want %d", size, dsfDSDChunkSize)
	}
	// hdr[12:20] is the total file size and hdr[20:28] the metadata pointer.
	// Neither is used: reads end at the sample count inside the data chunk,
	// so the metadata is never read as audio.
	pos := int64(dsfDSDChunkSize)
	var (
		f        dsfFmt
		fmtSeen  bool
		chunkHdr [dsfChunkHeader]byte
	)
	for i := 0; i < maxDSDChunks; i++ {
		if _, err := io.ReadFull(rs, chunkHdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if fmtSeen {
					return nil, refuse(ReasonTruncated, "DSF has no data chunk")
				}
				return nil, refuse(ReasonTruncated, "DSF has no fmt chunk")
			}
			return nil, err
		}
		id := string(chunkHdr[0:4])
		size := binary.LittleEndian.Uint64(chunkHdr[4:12])
		if size < dsfChunkHeader {
			return nil, refuse(ReasonUnsupportedLayout, "DSF chunk %q size %d is smaller than its header", id, size)
		}
		// Compare as uint64 before any int64 conversion: a size with the
		// high bit set must not turn negative.
		if size > uint64(math.MaxInt64-pos) {
			return nil, refuse(ReasonUnsupportedLayout, "DSF chunk %q size %d exceeds addressable length", id, size)
		}
		body := size - dsfChunkHeader
		switch id {
		case "fmt ":
			if fmtSeen {
				return nil, refuse(ReasonUnsupportedLayout, "DSF has two fmt chunks")
			}
			if size != dsfFmtChunkSize {
				return nil, refuse(ReasonUnsupportedLayout, "DSF fmt chunk size %d, want %d", size, dsfFmtChunkSize)
			}
			var b [dsfFmtChunkSize - dsfChunkHeader]byte
			if _, err := io.ReadFull(rs, b[:]); err != nil {
				return nil, refuse(ReasonTruncated, "DSF fmt chunk truncated")
			}
			parsed, err := parseDSFFmt(b[:])
			if err != nil {
				return nil, err
			}
			f, fmtSeen = parsed, true
		case "data":
			if !fmtSeen {
				return nil, refuse(ReasonUnsupportedLayout, "DSF data chunk before fmt")
			}
			return newDSF(rs, f, pos+dsfChunkHeader, body)
		default:
			if _, err := rs.Seek(int64(body), io.SeekCurrent); err != nil {
				return nil, err
			}
		}
		pos += int64(size)
	}
	return nil, refuse(ReasonUnsupportedLayout, "DSF has more than %d chunks before its data", maxDSDChunks)
}

func parseDSFFmt(b []byte) (dsfFmt, error) {
	version := binary.LittleEndian.Uint32(b[0:4])
	formatID := binary.LittleEndian.Uint32(b[4:8])
	chanType := binary.LittleEndian.Uint32(b[8:12])
	chanNum := binary.LittleEndian.Uint32(b[12:16])
	rate := binary.LittleEndian.Uint32(b[16:20])
	bits := binary.LittleEndian.Uint32(b[20:24])
	samples := binary.LittleEndian.Uint64(b[24:32])
	block := binary.LittleEndian.Uint32(b[32:36])
	if version != 1 {
		return dsfFmt{}, refuse(ReasonUnsupportedLayout, "DSF format version %d, want 1", version)
	}
	if formatID != 0 {
		return dsfFmt{}, refuse(ReasonUnsupportedComp, "DSF format id %d is not DSD raw", formatID)
	}
	if err := acceptDSDChannels(uint64(chanNum)); err != nil {
		return dsfFmt{}, err
	}
	if chanType != 2 { // 2 is stereo
		return dsfFmt{}, refuse(ReasonUnsupportedLayout, "DSF channel type %d disagrees with 2 channels", chanType)
	}
	if err := acceptDSDRate("DSF", uint64(rate)); err != nil {
		return dsfFmt{}, err
	}
	if bits != 1 && bits != 8 {
		return dsfFmt{}, refuse(ReasonUnsupportedBits, "DSF bits per sample %d, want 1 (LSB first) or 8 (MSB first)", bits)
	}
	if block != dsfBlockSize {
		return dsfFmt{}, refuse(ReasonUnsupportedLayout, "DSF block size %d, want %d", block, dsfBlockSize)
	}
	return dsfFmt{rate: uint64(rate), samples: samples, lsbFirst: bits == 1}, nil
}

func newDSF(rs io.ReadSeeker, f dsfFmt, dataStart int64, dataBytes uint64) (DSDDecoder, error) {
	perCh := f.samples/8 + boolToUint64(f.samples%8 != 0)
	groups := perCh/dsfBlockSize + boolToUint64(perCh%dsfBlockSize != 0)
	// perCh <= 2^61, so groups*blockSize*channels <= 2^62 + 8192: no overflow.
	need := groups * dsfBlockSize * dsdChannels
	if need > dataBytes {
		return nil, refuse(ReasonTruncated, "DSF data chunk holds %d bytes; %d samples per channel need %d", dataBytes, f.samples, need)
	}
	if f.samples > math.MaxInt64 {
		return nil, refuse(ReasonUnsupportedLayout, "DSF sample count %d exceeds addressable length", f.samples)
	}
	dsd := format.NativeDSD{
		BitRateHz: int(f.rate),
		Channels:  dsdChannels,
		Container: format.ContainerDSF,
		Frames:    int64(f.samples),
	}
	// The decoder's end is the length Format reports, from one function, so
	// a caller's seek to BytesPerChannel is always the decoder's end.
	return &dsfDecoder{
		rs:        rs,
		dsd:       dsd,
		dataStart: dataStart,
		perCh:     dsd.BytesPerChannel(),
		lsbFirst:  f.lsbFirst,
	}, nil
}

func boolToUint64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func (d *dsfDecoder) Format() format.NativeDSD { return d.dsd }

func (d *dsfDecoder) Close() error { return nil }

// Seek moves to byte pos of each channel. Every read seeks to its own block
// offset, so there is nothing to do here but check and record pos.
func (d *dsfDecoder) Seek(ctx context.Context, pos int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pos < 0 || pos > d.perCh {
		return refuse(ReasonUnsupportedLayout, "DSF seek byte %d out of range (0..%d)", pos, d.perCh)
	}
	d.pos = pos
	d.readStuck = nil
	return nil
}

// ReadDSD reads within one block group at a time: from channel 0's offset to
// the last channel's, one contiguous span. Each span read seeks to its own
// absolute offset first, so a cancel or an I/O error after a read leaves
// nothing to undo: pos only moves for bytes handed to dst, and the next read
// seeks again. A failed seek sticks until a successful Seek.
func (d *dsfDecoder) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
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
	left := d.perCh - d.pos
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(want) > left {
		want = int(left)
	}
	out := 0
	for want > 0 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		inBlock := int(d.pos % dsfBlockSize)
		k := dsfBlockSize - inBlock
		if k > want {
			k = want
		}
		span := (dsdChannels-1)*dsfBlockSize + k
		off := d.dataStart + (d.pos/dsfBlockSize)*dsfBlockSize*dsdChannels + int64(inBlock)
		if _, err := d.rs.Seek(off, io.SeekStart); err != nil {
			d.readStuck = err
			return out, err
		}
		if cap(d.buf) < span {
			d.buf = make([]byte, span)
		}
		buf := d.buf[:span]
		n, rerr := io.ReadFull(d.rs, buf)
		if cerr := ctx.Err(); cerr != nil {
			return out, cerr
		}
		got := k
		switch {
		case rerr == io.EOF || rerr == io.ErrUnexpectedEOF:
			// The file ends inside this block group: every channel has
			// what reached the last channel's run, which may be nothing.
			got = n - (dsdChannels-1)*dsfBlockSize
			if got < 0 {
				got = 0
			}
		case rerr != nil:
			return out, rerr
		}
		for c := 0; c < dsdChannels; c++ {
			src := buf[c*dsfBlockSize : c*dsfBlockSize+got]
			to := dst[c][out : out+got]
			if d.lsbFirst {
				for i, b := range src {
					to[i] = bitReverse[b]
				}
			} else {
				copy(to, src)
			}
		}
		out += got
		d.pos += int64(got)
		want -= got
		if rerr != nil {
			return out, refuse(ReasonTruncated, "DSF data ends before its sample count")
		}
	}
	if d.pos >= d.perCh {
		return out, io.EOF
	}
	return out, nil
}
