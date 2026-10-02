// Package flactest writes FLAC streams for tests: what a tool that finished
// its stream leaves, without a tool. The bridge publishes a rendition only
// when its FLAC is whole (internal/transcode, rendition_complete.go), so a
// test whose stand-in sox "renders" writes one of these.
//
// It is test-oriented but a normal package, as internal/dsdtone is: the
// transcode tests and cmd/bridge's both stand in for sox, and a `_test.go`
// helper cannot be shared across packages. Nothing in production imports it.
//
// Its checksums are computed bit by bit, independently of the table-driven
// ones the bridge checks with, so the two cannot agree on a wrong CRC.
package flactest

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Block is the block size of every frame a stream holds but its last: sox's,
// libFLAC's at its default levels.
const Block = 4096

// Stream is a whole FLAC stream: STREAMINFO declaring samples per channel at
// rate / channels / bits, then frames of Block samples (the last shorter)
// whose subframes are CONSTANT at value, every CRC right.
func Stream(rate, channels, bits int, samples uint64, value int64) []byte {
	return stream(rate, channels, bits, samples, value, false)
}

// VariableStream is Stream written with the variable blocking strategy, in
// which a frame codes its first sample's number instead of its own.
func VariableStream(rate, channels, bits int, samples uint64, value int64) []byte {
	return stream(rate, channels, bits, samples, value, true)
}

func stream(rate, channels, bits int, samples uint64, value int64, variable bool) []byte {
	b := StreamInfo(rate, channels, bits, samples)
	for first := uint64(0); first < samples; first += Block {
		n := min(samples-first, Block)
		number := first / Block
		if variable {
			number = first
		}
		b = append(b, Frame(number, int(n), channels, bits, value, variable)...)
	}
	return b
}

// StreamInfo is "fLaC" and a STREAMINFO block, the last metadata block,
// declaring samples (0: not declared) and a block size of Block.
func StreamInfo(rate, channels, bits int, samples uint64) []byte {
	b := []byte("fLaC")
	b = append(b, 0x80, 0, 0, 34)
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], Block)
	binary.BigEndian.PutUint16(si[2:], Block)
	binary.BigEndian.PutUint64(si[10:], uint64(rate)<<44|uint64(channels-1)<<41|uint64(bits-1)<<36|samples)
	return append(b, si...)
}

// Frame is one frame: a header coding number (the frame's, or its first
// sample's when variable) whose rate and bit depth are STREAMINFO's, of
// independent channels, each a CONSTANT subframe at value.
func Frame(number uint64, blockSize, channels, bits int, value int64, variable bool) []byte {
	h := []byte{0xFF, 0xF8}
	if variable {
		h[1] |= 0x01
	}
	bsCode := byte(12) // 4096
	var extra []byte
	if blockSize != Block {
		bsCode = 7 // a 16-bit block size less one, after the coded number
		extra = []byte{byte((blockSize - 1) >> 8), byte(blockSize - 1)}
	}
	h = append(h, bsCode<<4, byte(channels-1)<<4)
	h = append(h, codedNumber(number)...)
	h = append(h, extra...)
	h = append(h, crc8(h))
	var w bitWriter
	for range channels {
		w.put(0, 8) // a zero bit, CONSTANT (000000), no wasted bits
		w.put(uint64(value), bits)
	}
	frame := append(h, w.buf...)
	c := crc16(frame)
	return append(frame, byte(c>>8), byte(c))
}

// ShPrintf renders b as a printf(1) format of octal escapes, so a stand-in
// tool written in sh, with nothing but builtins, can write it:
// `printf '<ShPrintf(b)>' > "$out"`.
func ShPrintf(b []byte) string {
	var s strings.Builder
	for _, x := range b {
		fmt.Fprintf(&s, `\%03o`, x)
	}
	return s.String()
}

// codedNumber codes n in FLAC's UTF-8-like form.
func codedNumber(n uint64) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	for k := 1; k <= 6; k++ {
		if n >= 1<<uint(6*k+6-k) {
			continue
		}
		out := make([]byte, k+1)
		for j := k; j >= 1; j-- {
			out[j] = 0x80 | byte(n&0x3F)
			n >>= 6
		}
		out[0] = byte(0xFF<<uint(7-k)) | byte(n)
		return out
	}
	panic(fmt.Sprintf("flactest: %d needs more than 36 bits", n))
}

// bitWriter packs bit fields, most significant first.
type bitWriter struct {
	buf   []byte
	nbits int
}

func (w *bitWriter) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbits%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> uint(w.nbits%8)
		}
		w.nbits++
	}
}

// crc8 is FLAC's header checksum (x^8+x^2+x+1), bit by bit.
func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// crc16 is FLAC's frame checksum (x^16+x^15+x^2+1), bit by bit.
func crc16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c ^= uint16(x) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}
