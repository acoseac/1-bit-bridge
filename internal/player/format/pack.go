package format

import (
	"encoding/binary"
	"fmt"
)

// ALSAFormat names an ALSA PCM sample format the packers emit.
type ALSAFormat string

const (
	// S16LE is two little-endian bytes per signed sample.
	S16LE ALSAFormat = "S16_LE"
	// S243LE is three little-endian bytes per signed sample.
	S243LE ALSAFormat = "S24_3LE"
	// S24LE is a four-byte container with the low 24 bits significant.
	S24LE ALSAFormat = "S24_LE"
	// S32LE is four little-endian bytes per signed sample. When packing a
	// 24-bit source, samples are shifted left eight bits and the low byte is
	// zero so every source bit is preserved under lossless widening.
	S32LE ALSAFormat = "S32_LE"
)

// Pack writes interleaved int32 samples (sign-extended valid bits) into dst
// in the named ALSA layout. n is the number of sample values (channels ×
// frames). dst must have room for the packed size; use PackedBytes.
//
// Lossless widening preserves the normalized value and every source bit.
func Pack(dst []byte, samples []int32, validBits int, alsa ALSAFormat) (int, error) {
	if err := checkValidBits(validBits); err != nil {
		return 0, err
	}
	need := PackedBytes(len(samples), alsa)
	if need < 0 {
		return 0, fmt.Errorf("player format: unknown ALSA format %q", alsa)
	}
	if len(dst) < need {
		return 0, fmt.Errorf("player format: dst has %d bytes, need %d", len(dst), need)
	}
	switch alsa {
	case S16LE:
		if validBits != 16 {
			return 0, fmt.Errorf("player format: S16_LE requires 16 valid bits, got %d", validBits)
		}
		for i, s := range samples {
			binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(s)))
		}
	case S243LE:
		if validBits != 24 {
			return 0, fmt.Errorf("player format: S24_3LE requires 24 valid bits, got %d", validBits)
		}
		for i, s := range samples {
			v := uint32(s) & 0xFFFFFF
			off := i * 3
			dst[off] = byte(v)
			dst[off+1] = byte(v >> 8)
			dst[off+2] = byte(v >> 16)
		}
	case S24LE:
		if validBits != 24 {
			return 0, fmt.Errorf("player format: S24_LE requires 24 valid bits, got %d", validBits)
		}
		for i, s := range samples {
			// Four-byte container, low 24 bits significant; high byte zero
			// (LSB-aligned). Sign is in bit 23.
			binary.LittleEndian.PutUint32(dst[i*4:], uint32(s)&0xFFFFFF)
		}
	case S32LE:
		switch validBits {
		case 24:
			for i, s := range samples {
				// Shift left eight; low byte zero. Preserves all 24 source bits.
				binary.LittleEndian.PutUint32(dst[i*4:], uint32(s)<<8)
			}
		case 32:
			for i, s := range samples {
				binary.LittleEndian.PutUint32(dst[i*4:], uint32(s))
			}
		default:
			return 0, fmt.Errorf("player format: S32_LE packing accepts 24 or 32 valid bits, got %d", validBits)
		}
	default:
		return 0, fmt.Errorf("player format: unknown ALSA format %q", alsa)
	}
	return need, nil
}

// PackedBytes returns the byte length of n sample values in alsa layout, or
// -1 for an unknown format.
func PackedBytes(n int, alsa ALSAFormat) int {
	switch alsa {
	case S16LE:
		return n * 2
	case S243LE:
		return n * 3
	case S24LE, S32LE:
		return n * 4
	default:
		return -1
	}
}

func checkValidBits(v int) error {
	switch v {
	case 16, 24, 32:
		return nil
	default:
		return fmt.Errorf("player format: want 16, 24 or 32 valid bits, got %d", v)
	}
}
