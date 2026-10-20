package decode

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

const maxAIFFChunks = 256

type aiffDecoder struct {
	rs            io.ReadSeeker
	pcm           format.PCM
	ssndStart     int64 // first sample byte
	ssndEnd       int64 // exclusive end of sample bytes
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
	formSize := binary.BigEndian.Uint32(hdr[4:8])
	formEnd := int64(8) + int64(formSize)
	if formSize < 4 {
		return nil, refuse(ReasonTruncated, "AIFF FORM body too small")
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
		rateExact       float64
		compression     string
		ssndDataStart   int64
		ssndDataBytes   int64
		littleEndian    bool
		scanned         int64
	)
	if !isAIFC {
		compression = "NONE"
	}

	for i := 0; i < maxAIFFChunks; i++ {
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if pos+8 > formEnd {
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
		size := binary.BigEndian.Uint32(ch[4:8])
		chunkDataPos := pos + 8
		chunkEnd := chunkDataPos + int64(size) + int64(size&1)
		if chunkEnd > formEnd {
			return nil, refuse(ReasonTruncated, "AIFF chunk %q extends past FORM body", id)
		}

		switch id {
		case "COMM":
			if commSeen {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF has more than one COMM chunk")
			}
			need := uint32(18)
			if isAIFC {
				need = 22
			}
			if size < need {
				return nil, refuse(ReasonTruncated, "AIFF COMM chunk too small")
			}
			if size > maxHeaderChunkBody {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF COMM chunk size %d exceeds bound", size)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(rs, body); err != nil {
				return nil, refuse(ReasonTruncated, "AIFF COMM chunk truncated")
			}
			channels = int16(binary.BigEndian.Uint16(body[0:2]))
			numSampleFrames = binary.BigEndian.Uint32(body[2:6])
			sampleSize = int16(binary.BigEndian.Uint16(body[6:8]))
			var ok bool
			rateExact, ok = extended80ToFloat(body[8:18])
			if !ok {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF sample rate is not a finite positive value")
			}
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
			scanned += 8 + int64(size) + int64(size&1)
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
			if uint64(offset) > uint64(size)-8 {
				return nil, refuse(ReasonTruncated, "AIFF SSND offset exceeds chunk size")
			}
			ssndDataStart = chunkDataPos + 8 + int64(offset)
			ssndDataBytes = int64(size) - 8 - int64(offset)
			ssndSeen = true
			remain := int64(size) - 8
			if _, err := rs.Seek(remain+int64(size&1), io.SeekCurrent); err != nil {
				return nil, err
			}
		default:
			scanned += 8 + int64(size) + int64(size&1)
			if scanned > maxNonAudioHeaderScan {
				return nil, refuse(ReasonUnsupportedLayout, "AIFF header exceeds scan bound before SSND")
			}
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

	// Require an exact integer rate in the supported range (no silent rounding).
	if rateExact != math.Trunc(rateExact) {
		return nil, refuse(ReasonUnsupportedLayout, "AIFF sample rate %v is not an integer Hz value", rateExact)
	}
	rate := int(rateExact)
	if err := acceptRate(rate); err != nil {
		return nil, err
	}

	bits := int(sampleSize)
	if err := acceptPCMShape(int(channels), bits); err != nil {
		return nil, err
	}
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
	needBytes := int64(numSampleFrames) * int64(fb)
	if ssndDataBytes < needBytes {
		return nil, refuse(ReasonTruncated, "AIFF SSND payload shorter than declared sample frames")
	}
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
		ssndEnd:       ssndDataStart + needBytes,
		littleEndian:  littleEndian,
		containerBits: bits,
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
	off := d.ssndStart + frame*int64(d.frameBytes)
	if off > d.ssndEnd {
		return refuse(ReasonUnsupportedLayout, "AIFF seek past SSND payload")
	}
	if _, err := d.rs.Seek(off, io.SeekStart); err != nil {
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
	out := 0
	for framesWant > 0 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		chunk := framesPerRead(d.frameBytes, framesWant)
		need := chunk * d.frameBytes
		if cap(d.byteBuf) < need {
			d.byteBuf = make([]byte, need)
		} else {
			d.byteBuf = d.byteBuf[:need]
		}
		n, err := io.ReadFull(d.rs, d.byteBuf)
		if cerr := ctx.Err(); cerr != nil {
			return out, cerr
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n == 0 {
				if d.posFrame < d.pcm.Frames {
					return out, refuse(ReasonTruncated, "AIFF SSND ends before declared frame count")
				}
				return out, io.EOF
			}
			whole := n / d.frameBytes
			if whole == 0 || n%d.frameBytes != 0 {
				return out, refuse(ReasonTruncated, "AIFF SSND ends mid-frame")
			}
			d.byteBuf = d.byteBuf[:whole*d.frameBytes]
			chunk = whole
			err = refuse(ReasonTruncated, "AIFF SSND ends before declared frame count")
		} else if err != nil {
			return out, err
		} else {
			err = nil
		}
		bytesPerSample := d.frameBytes / d.pcm.Channels
		for f := 0; f < chunk; f++ {
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

// extended80ToFloat decodes an 80-bit IEEE-754 extended float (AIFF sample rate).
// ok is false for NaN, Inf, negative, or non-finite results.
func extended80ToFloat(b []byte) (float64, bool) {
	if len(b) < 10 {
		return 0, false
	}
	se := binary.BigEndian.Uint16(b[0:2])
	hi := binary.BigEndian.Uint32(b[2:6])
	lo := binary.BigEndian.Uint32(b[6:10])
	sign := int(se >> 15)
	exp := int(se & 0x7FFF)
	mant := (uint64(hi) << 32) | uint64(lo)
	if sign != 0 {
		return 0, false
	}
	if exp == 0 && mant == 0 {
		return 0, true
	}
	if exp == 0 || exp == 0x7FFF {
		return 0, false
	}
	// Normalised extended floats have the explicit integer bit set. A
	// non-zero exponent with that bit clear is unnormalised / pseudo and
	// must not silently become a plausible sample rate (Argon R3 M3).
	if hi&0x80000000 == 0 {
		return 0, false
	}
	f := float64(mant) / (1 << 63)
	f = math.Ldexp(f, exp-16383)
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}
