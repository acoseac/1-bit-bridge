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

const (
	maxWAVChunks     = 256
	maxWAVHeaderScan = 8 << 20 // 8 MiB of leading non-data chunks
)

type wavDecoder struct {
	rs            io.ReadSeeker
	pcm           format.PCM
	dataStart     int64
	containerBits int
	frameBytes    int
	posFrame      int64
	byteBuf       []byte
}

func openWAV(rs io.ReadSeeker) (Decoder, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return nil, refuse(ReasonTruncated, "WAVE header truncated")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, refuse(ReasonUnknownContainer, "not a RIFF/WAVE file")
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
		var ch [8]byte
		if _, err := io.ReadFull(rs, ch[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		id := string(ch[0:4])
		size := binary.LittleEndian.Uint32(ch[4:8])
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		scanned += 8 + int64(size) + int64(size&1)
		if !formatSeen && scanned > maxWAVHeaderScan {
			return nil, refuse(ReasonUnsupportedLayout, "WAVE header exceeds scan bound before fmt chunk")
		}

		switch id {
		case "fmt ":
			if formatSeen {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE has more than one fmt chunk")
			}
			if size < 16 {
				return nil, refuse(ReasonTruncated, "WAVE fmt chunk too small")
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
		case "data":
			if dataSeen {
				return nil, refuse(ReasonUnsupportedLayout, "WAVE has more than one data chunk")
			}
			dataStart = pos
			dataSize = size
			dataSeen = true
			if _, err := rs.Seek(int64(size)+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
		default:
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

	containerBits := int(bits)
	vb := int(validBits)
	switch containerBits {
	case 16, 24, 32:
	default:
		return nil, refuse(ReasonUnsupportedBits, "WAVE container %d bits is refused", containerBits)
	}
	if err := acceptPCMShape(int(channels), vb); err != nil {
		// 8/12-bit etc.: acceptPCMShape already refuses. For extensible with
		// validBits in {16,24,32} but odd container, also refuse.
		return nil, err
	}
	fb := int(channels) * (containerBits / 8)
	if blockAlign != 0 && int(blockAlign) != fb {
		return nil, refuse(ReasonUnsupportedLayout, "WAVE block align %d disagrees with %d-byte frames", blockAlign, fb)
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
	if _, err := d.rs.Seek(off, io.SeekStart); err != nil {
		return err
	}
	d.posFrame = frame
	return nil
}

func (d *wavDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
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
	need := framesWant * d.frameBytes
	if cap(d.byteBuf) < need {
		d.byteBuf = make([]byte, need)
	} else {
		d.byteBuf = d.byteBuf[:need]
	}
	n, err := io.ReadFull(d.rs, d.byteBuf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		whole := n / d.frameBytes
		if whole == 0 {
			return 0, io.EOF
		}
		d.byteBuf = d.byteBuf[:whole*d.frameBytes]
		framesWant = whole
		if n%d.frameBytes != 0 {
			err = refuse(ReasonTruncated, "WAVE data ends mid-frame")
		} else {
			err = nil
		}
	} else if err != nil {
		return 0, err
	} else {
		err = nil
	}
	out := 0
	bytesPerSample := d.containerBits / 8
	for f := 0; f < framesWant; f++ {
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
			if d.pcm.ValidBits < d.containerBits {
				u &= (uint32(1) << d.pcm.ValidBits) - 1
			}
			dst[out] = signExtend(u, d.pcm.ValidBits)
			out++
		}
	}
	d.posFrame += int64(framesWant)
	if err == nil && d.posFrame >= d.pcm.Frames {
		err = io.EOF
	}
	return out, err
}
