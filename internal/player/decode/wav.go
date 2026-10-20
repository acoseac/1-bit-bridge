package decode

import (
	"context"
	"encoding/binary"
	"fmt"
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
	pcmRegion
	containerBits int
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
	pcm := format.PCM{
		RateHz:    int(rate),
		Channels:  int(channels),
		ValidBits: vb,
		Container: format.ContainerWAV,
		Frames:    frames,
	}
	d := &wavDecoder{
		pcmRegion: pcmRegion{
			linearRegion: linearRegion{
				rs:          rs,
				regionStart: dataStart,
				regionEnd:   dataStart + int64(dataSize),
				frameBytes:  fb,
				frames:      pcm.Frames,
				earlyEnd:    "WAVE data ends before declared frame count",
				midFrame:    "WAVE data ends mid-frame",
			},
			pcm: pcm,
		},
		containerBits: containerBits,
	}
	d.unpack = d.unpackWAV
	if _, err := rs.Seek(dataStart, io.SeekStart); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *wavDecoder) Close() error { return nil }

func (d *wavDecoder) Seek(ctx context.Context, frame int64) error {
	return d.seekFrame(ctx, frame,
		fmt.Sprintf("WAVE seek frame %d out of range (0..%d)", frame, d.pcm.Frames),
		"WAVE seek past data chunk")
}

func (d *wavDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	return d.readFrames(ctx, dst)
}

// unpackWAV converts little-endian container samples; extensible valid bits
// are MSB-aligned in the container (shift right before sign-extend).
func (d *wavDecoder) unpackWAV(dst []int32, out int, buf []byte, chunk int) int {
	bytesPerSample := d.containerBits / 8
	shift := d.containerBits - d.pcm.ValidBits
	for f := 0; f < chunk; f++ {
		base := f * d.frameBytes
		for c := 0; c < d.pcm.Channels; c++ {
			off := base + c*bytesPerSample
			var u uint32
			switch d.containerBits {
			case 16:
				u = uint32(binary.LittleEndian.Uint16(buf[off:]))
			case 24:
				u = uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16
			case 32:
				u = binary.LittleEndian.Uint32(buf[off:])
			}
			if shift > 0 {
				u >>= uint(shift)
			}
			dst[out] = signExtend(u, d.pcm.ValidBits)
			out++
		}
	}
	return out
}
