package decode

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// WAVE format tags.
const (
	waveFormatPCM        = 0x0001
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xFFFE
)

// KSDATAFORMAT_SUBTYPE_PCM GUID: 00000001-0000-0010-8000-00aa00389b71
var guidPCM = [16]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}

// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT: 00000003-0000-0010-8000-00aa00389b71
var guidFloat = [16]byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}

const maxWAVChunks = 256

type wavDecoder struct {
	rs            io.ReadSeeker
	pcm           format.PCM
	dataStart     int64
	dataEnd       int64 // exclusive; confines reads to the data chunk
	containerBits int
	frameBytes    int
	posFrame      int64
	byteBuf       []byte
	// readStuck is set when a seek-back after a failed/cancelled read fails;
	// later ReadFrames return it until a successful Seek clears it.
	readStuck error
}

func openWAV(rs io.ReadSeeker) (Decoder, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return nil, refuse(ReasonTruncated, "WAVE header truncated")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, refuse(ReasonUnknownContainer, "not a RIFF/WAVE file")
	}
	riffSize := binary.LittleEndian.Uint32(hdr[4:8])
	// RIFF size covers WAVE+chunks; absolute end of the declared RIFF body.
	riffEnd := int64(8) + int64(riffSize)
	if riffSize < 4 {
		return nil, refuse(ReasonTruncated, "WAVE RIFF body too small")
	}

	var (
		formatSeen bool
		dataSeen   bool
		channels   uint16
		rate       uint32
		bits       uint16
		validBits  uint16
		blockAlign uint16
		dataStart  int64
		dataSize   uint32
		scanned    int64
	)

	for i := 0; i < maxWAVChunks; i++ {
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if pos+8 > riffEnd {
			break
		}
		var ch [8]byte
		if _, err := io.ReadFull(rs, ch[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		id := string(ch[0:4])
		size := binary.LittleEndian.Uint32(ch[4:8])
		chunkDataPos := pos + 8
		chunkEnd := chunkDataPos + int64(size) + int64(size&1)
		if chunkEnd > riffEnd {
			return nil, refuse(ReasonTruncated, "WAVE chunk %q extends past RIFF body", id)
		}

		switch id {
		case "fmt ":
			if formatSeen {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE has more than one fmt chunk")
			}
			if size < 16 {
				return nil, refuse(ReasonTruncated, "WAVE fmt chunk too small")
			}
			if size > maxHeaderChunkBody {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE fmt chunk size %d exceeds bound", size)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(rs, body); err != nil {
				return nil, refuse(ReasonTruncated, "WAVE fmt chunk truncated")
			}
			audioFmt := binary.LittleEndian.Uint16(body[0:2])
			channels = binary.LittleEndian.Uint16(body[2:4])
			rate = binary.LittleEndian.Uint32(body[4:8])
			blockAlign = binary.LittleEndian.Uint16(body[12:14])
			bits = binary.LittleEndian.Uint16(body[14:16])
			validBits = bits
			switch audioFmt {
			case waveFormatPCM:
				// ok
			case waveFormatIEEEFloat:
				return nil, refuse(ReasonFloatPCM, "floating-point WAVE is refused")
			case waveFormatExtensible:
				if size < 40 {
					return nil, refuse(ReasonTruncated, "WAVE_FORMAT_EXTENSIBLE fmt chunk too small")
				}
				validBits = binary.LittleEndian.Uint16(body[18:20])
				var sub [16]byte
				copy(sub[:], body[24:40])
				switch {
				case sub == guidPCM:
					// ok
				case sub == guidFloat:
					return nil, refuse(ReasonFloatPCM, "floating-point WAVE (extensible) is refused")
				default:
					return nil, refuse(ReasonUnsupportedLayout, "WAVE_FORMAT_EXTENSIBLE sub-format is not PCM")
				}
				if validBits == 0 {
					validBits = bits
				}
				if int(validBits) > int(bits) {
					return nil, refuse(ReasonUnsupportedLayout, "WAVE valid bits %d exceed container %d", validBits, bits)
				}
			default:
				return nil, refuse(ReasonUnsupportedComp, "WAVE format tag %#x is not integer PCM", audioFmt)
			}
			formatSeen = true
			if size&1 != 0 {
				if _, err := rs.Seek(1, io.SeekCurrent); err != nil {
					return nil, err
				}
			}
			scanned += 8 + int64(size) + int64(size&1)
		case "data":
			if dataSeen {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE has more than one data chunk")
			}
			dataStart = chunkDataPos
			dataSize = size
			dataSeen = true
			if _, err := rs.Seek(int64(size)+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
			// data payload does not count toward the non-audio header scan.
		default:
			scanned += 8 + int64(size) + int64(size&1)
			if scanned > maxNonAudioHeaderScan {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE header exceeds scan bound before data")
			}
			if _, err := rs.Seek(int64(size)+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
		}
		if formatSeen && dataSeen {
			break
		}
	}
	if !formatSeen {
		return nil, refuse(ReasonTruncated, "WAVE missing fmt chunk")
	}
	if !dataSeen {
		return nil, refuse(ReasonTruncated, "WAVE missing data chunk")
	}
	if err := acceptRate(int(rate)); err != nil {
		return nil, err
	}

	containerBits := int(bits)
	vb := int(validBits)
	switch containerBits {
	case 16, 24, 32:
	default:
		return nil, refuse(ReasonUnsupportedBits, "WAVE container %d bits is refused", containerBits)
	}
	if err := acceptPCMShape(int(channels), vb); err != nil {
		return nil, err
	}
	fb := int(channels) * (containerBits / 8)
	if blockAlign != 0 && int(blockAlign) != fb {
		return nil, refuse(ReasonUnsupportedLayout, "WAVE block align %d disagrees with %d-byte frames", blockAlign, fb)
	}
	if dataSize%uint32(fb) != 0 {
		return nil, refuse(ReasonTruncated, "WAVE data size %d is not a whole number of frames", dataSize)
	}
	frames := int64(dataSize) / int64(fb)
	d := &wavDecoder{
		rs: rs,
		pcm: format.PCM{
			RateHz:    int(rate),
			Channels:  int(channels),
			ValidBits: vb,
			Container: format.ContainerWAV,
			Frames:    frames,
		},
		dataStart:     dataStart,
		dataEnd:       dataStart + int64(dataSize),
		containerBits: containerBits,
		frameBytes:    fb,
	}
	if _, err := rs.Seek(dataStart, io.SeekStart); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *wavDecoder) Format() format.PCM { return d.pcm }

func (d *wavDecoder) Close() error { return nil }

func (d *wavDecoder) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if frame < 0 || frame > d.pcm.Frames {
		return refuse(ReasonUnsupportedLayout, "WAVE seek frame %d out of range (0..%d)", frame, d.pcm.Frames)
	}
	off := d.dataStart + frame*int64(d.frameBytes)
	if off > d.dataEnd {
		return refuse(ReasonUnsupportedLayout, "WAVE seek past data chunk")
	}
	if _, err := d.rs.Seek(off, io.SeekStart); err != nil {
		return err
	}
	d.posFrame = frame
	d.readStuck = nil
	return nil
}

func (d *wavDecoder) bytePos() int64 {
	return d.dataStart + d.posFrame*int64(d.frameBytes)
}

// seekToPos rewinds the underlying reader to posFrame so a cancelled or
// failed read that consumed bytes cannot leave a gap into trailing chunks.
func (d *wavDecoder) seekToPos() error {
	if _, err := d.rs.Seek(d.bytePos(), io.SeekStart); err != nil {
		d.readStuck = err
		return err
	}
	return nil
}

func (d *wavDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	if d.readStuck != nil {
		return 0, d.readStuck
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := checkReadBuf(dst, d.pcm.Channels); err != nil {
		return 0, err
	}
	left := d.pcm.Frames - d.posFrame
	if left <= 0 {
		return 0, io.EOF
	}
	framesWant := len(dst) / d.pcm.Channels
	if int64(framesWant) > left {
		framesWant = int(left)
	}
	out := 0
	for framesWant > 0 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		chunk := framesPerRead(d.frameBytes, framesWant)
		// Second guard: never read past the declared data chunk.
		regionLeft := d.dataEnd - d.bytePos()
		if regionLeft < 0 {
			regionLeft = 0
		}
		maxByRegion := int(regionLeft / int64(d.frameBytes))
		if chunk > maxByRegion {
			chunk = maxByRegion
		}
		if chunk == 0 {
			if d.posFrame < d.pcm.Frames {
				return out, refuse(ReasonTruncated, "WAVE data ends before declared frame count")
			}
			return out, io.EOF
		}
		need := chunk * d.frameBytes
		if cap(d.byteBuf) < need {
			d.byteBuf = make([]byte, need)
		} else {
			d.byteBuf = d.byteBuf[:need]
		}
		n, err := io.ReadFull(d.rs, d.byteBuf)
		// Check cancellation after the read: a single chunk can cover the
		// whole dst, so a pre-read check alone would miss a cancel that
		// fires during ReadFull (Codex M8). Bytes already consumed must be
		// seeked back so posFrame and the reader stay aligned (R6).
		if cerr := ctx.Err(); cerr != nil {
			if e := d.seekToPos(); e != nil {
				return out, e
			}
			return out, cerr
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n == 0 {
				if d.posFrame < d.pcm.Frames {
					return out, refuse(ReasonTruncated, "WAVE data ends before declared frame count")
				}
				return out, io.EOF
			}
			whole := n / d.frameBytes
			if whole == 0 || n%d.frameBytes != 0 {
				if e := d.seekToPos(); e != nil {
					return out, e
				}
				return out, refuse(ReasonTruncated, "WAVE data ends mid-frame")
			}
			d.byteBuf = d.byteBuf[:whole*d.frameBytes]
			chunk = whole
			err = refuse(ReasonTruncated, "WAVE data ends before declared frame count")
		} else if err != nil {
			if e := d.seekToPos(); e != nil {
				return out, e
			}
			return out, err
		} else {
			err = nil
		}
		bytesPerSample := d.containerBits / 8
		shift := d.containerBits - d.pcm.ValidBits
		for f := 0; f < chunk; f++ {
			base := f * d.frameBytes
			for c := 0; c < d.pcm.Channels; c++ {
				off := base + c*bytesPerSample
				var u uint32
				switch d.containerBits {
				case 16:
					u = uint32(binary.LittleEndian.Uint16(d.byteBuf[off:]))
				case 24:
					u = uint32(d.byteBuf[off]) | uint32(d.byteBuf[off+1])<<8 | uint32(d.byteBuf[off+2])<<16
				case 32:
					u = binary.LittleEndian.Uint32(d.byteBuf[off:])
				}
				// WAVE_FORMAT_EXTENSIBLE stores valid bits MSB-aligned
				// (left-justified) in the container; low bits are padding.
				if shift > 0 {
					u >>= uint(shift)
				}
				dst[out] = signExtend(u, d.pcm.ValidBits)
				out++
			}
		}
		d.posFrame += int64(chunk)
		framesWant -= chunk
		if err != nil {
			return out, err
		}
	}
	if d.posFrame >= d.pcm.Frames {
		return out, io.EOF
	}
	return out, nil
}
