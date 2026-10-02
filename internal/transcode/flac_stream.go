package transcode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// What a FLAC file the bridge had a tool write actually holds, read from the
// file rather than from what its header claims.
//
// # Why the header is not enough
//
// sox writes a FLAC's STREAMINFO before the audio, and libFLAC rewrites it
// only when the stream FINISHES. When sox knows how long its input is (a
// sox-direct render, a DSD render's Stage C), the STREAMINFO it writes first
// already declares the whole length, as an estimate. A write that fails part
// way (a variants volume that fills: sox 14.4.2 prints "error writing output
// file: No space left on device" and exits 0) leaves that first STREAMINFO in
// place over a file cut short: measured on a full 6 MiB volume, a header
// declaring all 3,840,000 samples of a 20 s render over 6,111,232 bytes that
// hold about a third of them, and ffprobe reports the full 20 s. A piped
// render (the ALAC route) declares 0 until it finishes, so ffprobe reports no
// duration at all, which the old completeness guard read as no verdict.
//
// So the length is read from the frames. A FLAC frame carries its own
// position (the frame number, or for a variable-blocksize stream the first
// sample number) and ends in a CRC-16 over every byte of it. The stream a
// tool finished ends with an intact frame that ends at the sample STREAMINFO
// declares; one cut short ends inside a frame, or (cut exactly between two)
// at a frame that ends before the declared length.

// flacStreamInfo is the STREAMINFO block: the facts of a FLAC stream.
type flacStreamInfo struct {
	minBlock, maxBlock int
	sampleRate         int
	channels           int
	bitsPerSample      int
	// declared is the total samples per channel STREAMINFO declares; 0 is
	// "not declared" (a stream a pipe began and never finished).
	declared uint64
}

// flacStream is what readFLACStream found in a file.
type flacStream struct {
	info flacStreamInfo
	// held is the sample at which the frame that ends the file ends: what the
	// stream holds. 0 when the file does not end with an intact frame.
	held uint64
	// reached is the first sample of the last frame header found in the
	// file's tail, intact or not: about how far a cut stream got. 0 when none
	// was found.
	reached uint64
}

// whole reports whether the stream is one a tool finished: it ends with an
// intact frame, at the sample STREAMINFO declares.
func (s flacStream) whole() bool {
	return s.info.declared > 0 && s.held == s.info.declared
}

// errNotFLAC is readFLACStream's answer for a file that does not begin as a
// FLAC stream: no "fLaC", or a first metadata block that is not a valid
// STREAMINFO.
var errNotFLAC = errors.New("not a FLAC stream")

// flacStreamInfoLen is "fLaC", the first metadata block header and the 34
// bytes of STREAMINFO, which the format requires to be that first block.
const flacStreamInfoLen = 4 + 4 + 34

// readFLACStream reads the STREAMINFO of the FLAC at path and finds the frame
// that ends the file, reading the head and the tail and nothing between: a
// whole render's last frame lies within flacMaxFrameBytes of its end.
func readFLACStream(path string) (flacStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return flacStream{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return flacStream{}, err
	}
	head := make([]byte, flacStreamInfoLen)
	if _, err := io.ReadFull(f, head); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return flacStream{}, fmt.Errorf("%w: %d bytes", errNotFLAC, st.Size())
		}
		return flacStream{}, err
	}
	info, ok := parseFLACStreamInfo(head)
	if !ok {
		return flacStream{}, errNotFLAC
	}
	s := flacStream{info: info}
	window := min(st.Size()-flacStreamInfoLen, int64(flacMaxFrameBytes(info)))
	if window <= 0 {
		return s, nil
	}
	tail := make([]byte, window)
	if _, err := f.ReadAt(tail, st.Size()-window); err != nil {
		return flacStream{}, err
	}
	s.held, s.reached = lastFLACFrame(tail, info)
	return s, nil
}

// parseFLACStreamInfo reads "fLaC" and the STREAMINFO block that must follow
// it, refusing what no encoder writes: another first block, another length,
// a zero rate, a block size under the format's minimum of 16.
func parseFLACStreamInfo(head []byte) (flacStreamInfo, bool) {
	if len(head) < flacStreamInfoLen || string(head[:4]) != "fLaC" {
		return flacStreamInfo{}, false
	}
	// The block header: the last-block flag (ignored), type 0, length 34.
	if head[4]&0x7F != 0 || head[5] != 0 || head[6] != 0 || head[7] != 34 {
		return flacStreamInfo{}, false
	}
	b := head[8:]
	packed := binary.BigEndian.Uint64(b[10:18])
	info := flacStreamInfo{
		minBlock:      int(binary.BigEndian.Uint16(b[0:2])),
		maxBlock:      int(binary.BigEndian.Uint16(b[2:4])),
		sampleRate:    int(packed >> 44),
		channels:      int(packed>>41&0x07) + 1,
		bitsPerSample: int(packed>>36&0x1F) + 1,
		declared:      packed & (1<<36 - 1),
	}
	if info.sampleRate == 0 || info.maxBlock < 16 || info.bitsPerSample < 4 {
		return flacStreamInfo{}, false
	}
	return info, true
}

// flacMaxFrameBytes bounds the bytes of one frame of the stream: a header (at
// most 16 bytes), each channel's subframe stored VERBATIM (an encoder falls
// back to verbatim when nothing compresses better, so no subframe is larger:
// a header byte, a wasted-bits count, and the block's samples at one bit
// more than the stream's, which a side channel carries), the padding to a
// byte, and the CRC-16. For sox's 4096-sample blocks of 24-bit stereo it is
// about 25 KiB; the format's largest (65,535 samples, 8 channels, 32 bits)
// is about 2.2 MB.
func flacMaxFrameBytes(info flacStreamInfo) int {
	perChannel := 1 + 4 + (info.maxBlock*(info.bitsPerSample+1)+7)/8
	return 16 + info.channels*perChannel + 1 + 2
}

// lastFLACFrame finds the frame that ends tail (the end of a FLAC file) and
// returns the sample at which it ends: a frame header at some offset whose
// CRC-16, computed over the bytes from it to the last two, is those last two
// bytes. reached is the first sample of the last frame header in tail, intact
// or not. held is 0 when no frame ends tail.
//
// A sync pattern can occur inside audio data, so a candidate must be a whole
// header that agrees with STREAMINFO and checks its CRC-8, and then the
// CRC-16 must match to the very end; a false candidate passes both with
// about one chance in 2^24.
func lastFLACFrame(tail []byte, info flacStreamInfo) (held, reached uint64) {
	for i := len(tail) - 2; i >= 0; i-- {
		if tail[i] != 0xFF || tail[i+1]&0xFE != 0xF8 {
			continue
		}
		h, ok := parseFLACFrameHeader(tail[i:], info)
		if !ok {
			continue
		}
		first := h.firstSample(info)
		if reached == 0 {
			reached = first
		}
		end := len(tail) - 2
		if end-i < h.length+info.channels {
			continue
		}
		if flacCRC16(tail[i:end]) == binary.BigEndian.Uint16(tail[end:]) {
			return first + uint64(h.blockSize), reached
		}
	}
	return 0, reached
}

// flacFrameHeader is what the completeness check reads of a frame header.
type flacFrameHeader struct {
	// variable is the blocking strategy: the coded number is the first
	// sample's number, not the frame's.
	variable  bool
	number    uint64
	blockSize int
	// length is the header's bytes, its CRC-8 included.
	length int
}

// firstSample is the number of the frame's first sample: the coded number for
// a variable-blocksize stream, the frame number times the stream's block size
// for a fixed one (every block but the last is that size).
func (h flacFrameHeader) firstSample(info flacStreamInfo) uint64 {
	if h.variable {
		return h.number
	}
	return h.number * uint64(info.maxBlock)
}

// flacSampleRates are the frame header's sample rate codes 1 to 11; 0 is
// "STREAMINFO's", 12 to 14 are given after the header, 15 is forbidden.
var flacSampleRates = [12]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}

// flacSampleSizes are the frame header's sample size codes: 0 is
// "STREAMINFO's", 3 is reserved (-1 here).
var flacSampleSizes = [8]int{0, 8, 12, -1, 16, 20, 24, 32}

// parseFLACFrameHeader reads the frame header at the start of b (RFC 9639,
// section 9.1) and checks it against the stream: its channels, rate and bit
// depth agree with STREAMINFO, its block is no larger than STREAMINFO's
// largest, and its CRC-8 is right. ok is false for anything else.
func parseFLACFrameHeader(b []byte, info flacStreamInfo) (h flacFrameHeader, ok bool) {
	if !flacHeaderMatchesStream(b, info) {
		return h, false
	}
	h.variable = b[1]&0x01 == 1
	i, number, ok := flacCodedNumber(b, 4, h.variable)
	if !ok {
		return h, false
	}
	h.number = number
	if h.blockSize, i, ok = flacFrameBlockSize(b, i, int(b[2]>>4)); !ok || h.blockSize > info.maxBlock {
		return h, false
	}
	rate, i, ok := flacFrameSampleRate(b, i, int(b[2]&0x0F), info.sampleRate)
	if !ok || rate != info.sampleRate {
		return h, false
	}
	if i >= len(b) || flacCRC8(b[:i]) != b[i] {
		return h, false
	}
	h.length = i + 1
	return h, true
}

// flacHeaderMatchesStream reads a frame header's first four bytes: the sync
// code, a clear reserved bit, no reserved or forbidden code, and the channel
// count and bit depth STREAMINFO gives.
func flacHeaderMatchesStream(b []byte, info flacStreamInfo) bool {
	if len(b) < 6 || b[0] != 0xFF || b[1]&0xFE != 0xF8 || b[3]&0x01 != 0 {
		return false
	}
	bsCode, srCode := b[2]>>4, b[2]&0x0F
	chCode, ssCode := int(b[3]>>4), int(b[3]>>1&0x07)
	if bsCode == 0 || srCode == 15 || chCode > 10 || flacSampleSizes[ssCode] < 0 {
		return false
	}
	channels := chCode + 1
	if chCode >= 8 {
		channels = 2 // left/side, right/side, mid/side
	}
	bits := flacSampleSizes[ssCode]
	if bits == 0 {
		bits = info.bitsPerSample
	}
	return channels == info.channels && bits == info.bitsPerSample
}

// flacFrameBlockSize reads the block size a frame header codes as bsCode,
// for codes 6 and 7 from the bytes at b[i] after the coded number, and
// returns the index past what it read.
func flacFrameBlockSize(b []byte, i, bsCode int) (size, next int, ok bool) {
	switch {
	case bsCode < 1:
		return 0, i, false
	case bsCode == 1:
		return 192, i, true
	case bsCode <= 5:
		return 576 << (bsCode - 2), i, true
	case bsCode <= 7:
		// The block size less one: one byte for code 6, two for code 7.
		n := bsCode - 5
		if i+n > len(b) {
			return 0, i, false
		}
		v := 0
		for _, x := range b[i : i+n] {
			v = v<<8 | int(x)
		}
		return v + 1, i + n, true
	}
	return 256 << (bsCode - 8), i, true
}

// flacFrameSampleRate reads the sample rate a frame header codes as srCode:
// STREAMINFO's for 0, a table entry for 1 to 11, and for 12 to 14 the bytes
// at b[i] after the block size (kHz, Hz, tens of Hz). It returns the index
// past what it read; 15 is forbidden.
func flacFrameSampleRate(b []byte, i, srCode, streamRate int) (rate, next int, ok bool) {
	switch {
	case srCode == 0:
		return streamRate, i, true
	case srCode <= 11:
		return flacSampleRates[srCode], i, true
	case srCode == 12:
		if i+1 > len(b) {
			return 0, i, false
		}
		return int(b[i]) * 1000, i + 1, true
	case srCode <= 14:
		if i+2 > len(b) {
			return 0, i, false
		}
		rate = int(b[i])<<8 | int(b[i+1])
		if srCode == 14 {
			rate *= 10
		}
		return rate, i + 2, true
	}
	return 0, i, false
}

// flacCodedNumber reads the frame or sample number coded at b[i] in the
// UTF-8-like form FLAC uses: one to six bytes for a frame number (31 bits),
// to seven for a sample number (36 bits).
func flacCodedNumber(b []byte, i int, variable bool) (next int, n uint64, ok bool) {
	if i >= len(b) {
		return 0, 0, false
	}
	c := b[i]
	var more int
	switch {
	case c&0x80 == 0:
		n = uint64(c)
	case c&0xE0 == 0xC0:
		n, more = uint64(c&0x1F), 1
	case c&0xF0 == 0xE0:
		n, more = uint64(c&0x0F), 2
	case c&0xF8 == 0xF0:
		n, more = uint64(c&0x07), 3
	case c&0xFC == 0xF8:
		n, more = uint64(c&0x03), 4
	case c&0xFE == 0xFC:
		n, more = uint64(c&0x01), 5
	case c == 0xFE && variable:
		more = 6
	default:
		return 0, 0, false
	}
	i++
	if i+more > len(b) {
		return 0, 0, false
	}
	for _, x := range b[i : i+more] {
		if x&0xC0 != 0x80 {
			return 0, 0, false
		}
		n = n<<6 | uint64(x&0x3F)
	}
	return i + more, n, true
}

// The two checksums of a FLAC frame: CRC-8 (polynomial x^8+x^2+x+1) over its
// header, CRC-16 (x^16+x^15+x^2+1) over the whole frame; both start at zero
// and are not reflected.
var (
	flacCRC8Table  = makeFLACCRC8Table()
	flacCRC16Table = makeFLACCRC16Table()
)

func makeFLACCRC8Table() (t [256]byte) {
	for i := range t {
		c := byte(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

func makeFLACCRC16Table() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

func flacCRC8(b []byte) byte {
	var c byte
	for _, x := range b {
		c = flacCRC8Table[c^x]
	}
	return c
}

func flacCRC16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c = c<<8 ^ flacCRC16Table[byte(c>>8)^x]
	}
	return c
}
