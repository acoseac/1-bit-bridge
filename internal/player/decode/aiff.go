package decode

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

const (
	maxAIFFChunks     = 256
	maxAIFFHeaderScan = 8 << 20
)

type aiffDecoder struct {
	rs            io.ReadSeeker
	pcm           format.PCM
	ssndStart     int64 // first sample byte
	littleEndian  bool
	containerBits int
	frameBytes    int
	posFrame      int64
	byteBuf       []byte
}

func openAIFF(rs io.ReadSeeker) (Decoder, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return nil, refuse(ReasonTruncated, "AIFF header truncated")
	}
	if string(hdr[0:4]) != "FORM" {
		return nil, refuse(ReasonUnknownContainer, "not a FORM file")
	}
	formType := string(hdr[8:12])
	isAIFC := formType == "AIFC"
	if formType != "AIFF" && !isAIFC {
		return nil, refuse(ReasonUnknownContainer, "FORM type %q is not AIFF/AIFC", formType)
	}

	var (
		commSeen        bool
		ssndSeen        bool
		channels        int16
		numSampleFrames uint32
		sampleSize      int16
		rate            int
		compression     string // AIFC only; AIFF is implicitly NONE
		ssndDataStart   int64
		littleEndian    bool
		scanned         int64
	)
	if !isAIFC {
		compression = "NONE"
	}

	for i := 0; i < maxAIFFChunks; i++ {
		var ch [8]byte
		if _, err := io.ReadFull(rs, ch[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		id := string(ch[0:4])
		size := binary.BigEndian.Uint32(ch[4:8])
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		scanned += 8 + int64(size) + int64(size&1)
		if !commSeen && scanned > maxAIFFHeaderScan {
			return nil, refuse(ReasonUnsupportedLayout, "AIFF header exceeds scan bound before COMM chunk")
		}

		switch id {
		case "COMM":
			if commSeen {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF has more than one COMM chunk")
			}
			need := uint32(18)
			if isAIFC {
				need = 22 // 18 + 4-char compression type
			}
			if size < need {
				return nil, refuse(ReasonTruncated, "AIFF COMM chunk too small")
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(rs, body); err != nil {
				return nil, refuse(ReasonTruncated, "AIFF COMM chunk truncated")
			}
			channels = int16(binary.BigEndian.Uint16(body[0:2]))
			numSampleFrames = binary.BigEndian.Uint32(body[2:6])
			sampleSize = int16(binary.BigEndian.Uint16(body[6:8]))
			rate = int(math.Round(extended80ToFloat(body[8:18])))
			if isAIFC {
				if len(body) < 22 {
					return nil, refuse(ReasonTruncated, "AIFF-C COMM missing compression type")
				}
				compression = string(body[18:22])
			}
			switch compression {
			case "NONE", "none":
				littleEndian = false
			case "sowt":
				littleEndian = true
			case "fl32", "FL32", "fl64", "FL64":
				return nil, refuse(ReasonFloatPCM, "AIFF-C floating compression %q is refused", compression)
			case "ulaw", "ULAW", "alaw", "ALAW", "ima4", "IMA4":
				return nil, refuse(ReasonUnsupportedComp, "AIFF-C compression %q is refused", compression)
			default:
				return nil, refuse(ReasonUnsupportedComp, "AIFF-C compression %q is refused", compression)
			}
			commSeen = true
			if size&1 != 0 {
				if _, err := rs.Seek(1, io.SeekCurrent); err != nil {
					return nil, err
				}
			}
		case "SSND":
			if ssndSeen {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF has more than one SSND chunk")
			}
			if size < 8 {
				return nil, refuse(ReasonTruncated, "AIFF SSND chunk too small")
			}
			var offBlock [8]byte
			if _, err := io.ReadFull(rs, offBlock[:]); err != nil {
				return nil, refuse(ReasonTruncated, "AIFF SSND header truncated")
			}
			offset := binary.BigEndian.Uint32(offBlock[0:4])
			// blockSize := binary.BigEndian.Uint32(offBlock[4:8]) — ignored for PCM
			ssndDataStart = pos + 8 + int64(offset)
			ssndSeen = true
			remain := int64(size) - 8
			if remain < 0 {
				return nil, refuse(ReasonTruncated, "AIFF SSND size inconsistent")
			}
			if _, err := rs.Seek(remain+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
		default:
			if _, err := rs.Seek(int64(size)+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
		}
		if commSeen && ssndSeen {
			break
		}
	}
	if !commSeen {
		return nil, refuse(ReasonTruncated, "AIFF missing COMM chunk")
	}
	if !ssndSeen {
		return nil, refuse(ReasonTruncated, "AIFF missing SSND chunk")
	}

	bits := int(sampleSize)
	if err := acceptPCMShape(int(channels), bits); err != nil {
		return nil, err
	}
	containerBits := bits
	// AIFF packs sampleSize bits into the next whole number of bytes.
	bytesPerSample := (bits + 7) / 8
	switch bytesPerSample {
	case 2, 3, 4:
	default:
		return nil, refuse(ReasonUnsupportedBits, "AIFF %d-bit packing is refused", bits)
	}
	if bits != 16 && bits != 24 && bits != 32 {
		return nil, refuse(ReasonUnsupportedBits, "AIFF %d-bit PCM is refused", bits)
	}
	fb := int(channels) * bytesPerSample
	d := &aiffDecoder{
		rs: rs,
		pcm: format.PCM{
			RateHz:    rate,
			Channels:  int(channels),
			ValidBits: bits,
			Container: format.ContainerAIFF,
			Frames:    int64(numSampleFrames),
		},
		ssndStart:     ssndDataStart,
		littleEndian:  littleEndian,
		containerBits: containerBits,
		frameBytes:    fb,
	}
	if _, err := rs.Seek(ssndDataStart, io.SeekStart); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *aiffDecoder) Format() format.PCM { return d.pcm }
func (d *aiffDecoder) Close() error       { return nil }

func (d *aiffDecoder) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if frame < 0 || frame > d.pcm.Frames {
		return refuse(ReasonUnsupportedLayout, "AIFF seek frame %d out of range (0..%d)", frame, d.pcm.Frames)
	}
	if _, err := d.rs.Seek(d.ssndStart+frame*int64(d.frameBytes), io.SeekStart); err != nil {
		return err
	}
	d.posFrame = frame
	return nil
}

func (d *aiffDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
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
			err = refuse(ReasonTruncated, "AIFF SSND ends mid-frame")
		} else {
			err = nil
		}
	} else if err != nil {
		return 0, err
	} else {
		err = nil
	}
	bytesPerSample := d.frameBytes / d.pcm.Channels
	out := 0
	for f := 0; f < framesWant; f++ {
		base := f * d.frameBytes
		for c := 0; c < d.pcm.Channels; c++ {
			off := base + c*bytesPerSample
			var u uint32
			switch bytesPerSample {
			case 2:
				if d.littleEndian {
					u = uint32(binary.LittleEndian.Uint16(d.byteBuf[off:]))
				} else {
					u = uint32(binary.BigEndian.Uint16(d.byteBuf[off:]))
				}
			case 3:
				if d.littleEndian {
					u = uint32(d.byteBuf[off]) | uint32(d.byteBuf[off+1])<<8 | uint32(d.byteBuf[off+2])<<16
				} else {
					u = uint32(d.byteBuf[off])<<16 | uint32(d.byteBuf[off+1])<<8 | uint32(d.byteBuf[off+2])
				}
			case 4:
				if d.littleEndian {
					u = binary.LittleEndian.Uint32(d.byteBuf[off:])
				} else {
					u = binary.BigEndian.Uint32(d.byteBuf[off:])
				}
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

// extended80ToFloat decodes an 80-bit IEEE-754 extended float (AIFF sample rate).
func extended80ToFloat(b []byte) float64 {
	if len(b) < 10 {
		return 0
	}
	se := binary.BigEndian.Uint16(b[0:2])
	hi := binary.BigEndian.Uint32(b[2:6])
	lo := binary.BigEndian.Uint32(b[6:10])
	sign := int(se >> 15)
	exp := int(se & 0x7FFF)
	mant := (uint64(hi) << 32) | uint64(lo)
	if exp == 0 && mant == 0 {
		return 0
	}
	// Integer bit is explicit in extended precision.
	f := float64(mant) / (1 << 63)
	f = math.Ldexp(f, exp-16383)
	if sign != 0 {
		f = -f
	}
	return f
}
