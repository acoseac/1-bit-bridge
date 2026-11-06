package manifest

// Monkey's Audio (.ape), read from its headers (backlog B489). Nothing is
// decoded.
//
// A file is "MAC " and the file version (the version number times 1000, so
// 3.99 is 3990), at the start or after junk an ID3v2 tag left there. From
// 3.98 (3980) on, an APE_DESCRIPTOR follows (52 bytes, its own length and the
// APE_HEADER's among them), and the APE_HEADER (24 bytes) where the
// descriptor's length says: the compression level, the format flags, the
// blocks per frame, the final frame's blocks, the frame count, the bits per
// sample, the channels and the sample rate. Before 3.98 the APE_HEADER_OLD
// (32 bytes from "MAC ") holds the level, the flags, the channels, the rate,
// the WAV header's and the terminating data's lengths, the frame count and
// the final frame's blocks; its bits are the flags' (8, 24, else 16), and
// its blocks per frame follow from the version and the level as every reader
// derives them (9216; 73728 from 3.90, or from 3.80 at Extra High; 73728 x 4
// from 3.95). The file's length in samples is (frames - 1) x blocks per
// frame + the final frame's blocks.
//
// Versions 3800 to 3990 are the ones read here, the range FFmpeg's reader
// takes; any other is refused (the scanner indexes it by name, backlog B145),
// never read by a guess about a layout this reader does not know. So is a
// header that cannot be read: no frames, a rate of 0 (both before any
// division), no channels, a bit depth or a compression level Monkey's Audio
// does not write, and lengths that do not fit.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The compression levels and format flags the reader takes, as MACLib's
// All.h names them.
const (
	apeLevelFast      = 1000
	apeLevelExtraHigh = 4000
	apeLevelInsane    = 5000

	apeFlag8Bit  = 1
	apeFlag24Bit = 8

	apeMinVersion = 3800
	apeMaxVersion = 3990
	apeNewLayout  = 3980 // the first version with a descriptor

	apeDescriptorBytes = 52
	apeHeaderBytes     = 24
	apeOldHeaderBytes  = 32

	// apeSearchLimit is how far the reader looks for "MAC " past where it
	// starts looking, as MACLib's FindDescriptor does (1 MB).
	apeSearchLimit = 1 << 20
	// apeMaxDescriptorBytes bounds the descriptor's own length, which a
	// later version may grow: 64 KiB is more than any version writes, and
	// the header is read where it ends.
	apeMaxDescriptorBytes = 64 << 10
	// apeMaxChannels is the most channels Monkey's Audio writes (32 since
	// version 10; 2 before).
	apeMaxChannels = 32
)

// errAPENoDescriptor is a file in which "MAC " is not found where MACLib
// looks for it: not a Monkey's Audio file.
var errAPENoDescriptor = errors.New("ape: no \"MAC \" within the search limit")

// apeStream is what a Monkey's Audio file's headers state.
type apeStream struct {
	version     uint16
	compression uint16
	formatFlags uint16
	channels    uint16
	rate        uint32
	bits        uint16
	totalBlocks uint64
}

// duration is the file's length in seconds: its blocks (one sample per
// channel each) over its rate, which readAPEHeader refuses at 0.
func (s apeStream) duration() float64 { return float64(s.totalBlocks) / float64(s.rate) }

// readAPEHeader finds and reads a Monkey's Audio file's headers in r. A read
// that fails comes back as the reader's error; a file that ends before its
// headers do, or holds no header a reader takes, is refused.
func readAPEHeader(r io.ReaderAt) (apeStream, error) {
	start, err := apeJunkLength(r)
	if err != nil {
		return apeStream{}, err
	}
	at, err := findAPEDescriptor(r, start)
	if err != nil {
		return apeStream{}, err
	}
	var head [8]byte
	if err := readAtExactly(r, head[:], at); err != nil {
		return apeStream{}, err
	}
	s := apeStream{version: binary.LittleEndian.Uint16(head[4:6])}
	if s.version < apeMinVersion || s.version > apeMaxVersion {
		return apeStream{}, fmt.Errorf("ape: file version %d is not one this reader takes (%d to %d)",
			s.version, apeMinVersion, apeMaxVersion)
	}
	var blocksPerFrame, finalFrameBlocks, totalFrames uint32
	if s.version >= apeNewLayout {
		blocksPerFrame, finalFrameBlocks, totalFrames, err = s.readNewHeader(r, at)
	} else {
		blocksPerFrame, finalFrameBlocks, totalFrames, err = s.readOldHeader(r, at)
	}
	if err != nil {
		return apeStream{}, err
	}
	if err := s.check(blocksPerFrame, finalFrameBlocks, totalFrames); err != nil {
		return apeStream{}, err
	}
	s.totalBlocks = uint64(totalFrames-1)*uint64(blocksPerFrame) + uint64(finalFrameBlocks)
	return s, nil
}

// readNewHeader reads the descriptor at at and the APE_HEADER its length
// places.
func (s *apeStream) readNewHeader(r io.ReaderAt, at int64) (bpf, final, frames uint32, err error) {
	var d [apeDescriptorBytes]byte
	if err := readAtExactly(r, d[:], at); err != nil {
		return 0, 0, 0, err
	}
	descriptorBytes := binary.LittleEndian.Uint32(d[8:12])
	headerBytes := binary.LittleEndian.Uint32(d[12:16])
	if descriptorBytes < apeDescriptorBytes || descriptorBytes > apeMaxDescriptorBytes {
		return 0, 0, 0, fmt.Errorf("ape: a descriptor of %d bytes", descriptorBytes)
	}
	if headerBytes < apeHeaderBytes {
		return 0, 0, 0, fmt.Errorf("ape: a header of %d bytes", headerBytes)
	}
	var h [apeHeaderBytes]byte
	if err := readAtExactly(r, h[:], at+int64(descriptorBytes)); err != nil {
		return 0, 0, 0, err
	}
	s.compression = binary.LittleEndian.Uint16(h[0:2])
	s.formatFlags = binary.LittleEndian.Uint16(h[2:4])
	bpf = binary.LittleEndian.Uint32(h[4:8])
	final = binary.LittleEndian.Uint32(h[8:12])
	frames = binary.LittleEndian.Uint32(h[12:16])
	s.bits = binary.LittleEndian.Uint16(h[16:18])
	s.channels = binary.LittleEndian.Uint16(h[18:20])
	s.rate = binary.LittleEndian.Uint32(h[20:24])
	return bpf, final, frames, nil
}

// readOldHeader reads the APE_HEADER_OLD at at, and derives what the old
// layout does not hold: the bits from the flags, and the blocks per frame
// from the version and the level.
func (s *apeStream) readOldHeader(r io.ReaderAt, at int64) (bpf, final, frames uint32, err error) {
	var h [apeOldHeaderBytes]byte
	if err := readAtExactly(r, h[:], at); err != nil {
		return 0, 0, 0, err
	}
	s.compression = binary.LittleEndian.Uint16(h[6:8])
	s.formatFlags = binary.LittleEndian.Uint16(h[8:10])
	s.channels = binary.LittleEndian.Uint16(h[10:12])
	s.rate = binary.LittleEndian.Uint32(h[12:16])
	frames = binary.LittleEndian.Uint32(h[24:28])
	final = binary.LittleEndian.Uint32(h[28:32])
	switch {
	case s.formatFlags&apeFlag8Bit != 0:
		s.bits = 8
	case s.formatFlags&apeFlag24Bit != 0:
		s.bits = 24
	default:
		s.bits = 16
	}
	switch {
	case s.version >= 3950:
		bpf = 73728 * 4
	case s.version >= 3900 || (s.version >= 3800 && s.compression == apeLevelExtraHigh):
		bpf = 73728
	default:
		bpf = 9216
	}
	return bpf, final, frames, nil
}

// check refuses a header no reader takes: no frames and a rate of 0, before
// any division by either; no channels, or more than Monkey's Audio writes; a
// bit depth it does not write; a compression level that is not one of its
// five; and frame counts that do not fit together.
func (s apeStream) check(bpf, final, frames uint32) error {
	switch {
	case frames == 0:
		return errors.New("ape: no frames")
	case s.rate == 0:
		return errors.New("ape: a sample rate of 0")
	case s.channels == 0 || s.channels > apeMaxChannels:
		return fmt.Errorf("ape: %d channels", s.channels)
	case s.bits != 8 && s.bits != 16 && s.bits != 24 && s.bits != 32:
		return fmt.Errorf("ape: %d bits per sample", s.bits)
	case s.compression < apeLevelFast || s.compression > apeLevelInsane || s.compression%1000 != 0:
		return fmt.Errorf("ape: compression level %d", s.compression)
	case bpf == 0:
		return errors.New("ape: no blocks per frame")
	case final == 0 || final > bpf:
		return fmt.Errorf("ape: a final frame of %d blocks beside %d per frame", final, bpf)
	}
	return nil
}

// apeJunkLength is where MACLib starts looking for "MAC ": past an ID3v2 tag
// at the file's start, and the NUL padding after one with no footer.
func apeJunkLength(r io.ReaderAt) (int64, error) {
	var h [10]byte
	n, err := r.ReadAt(h[:], 0)
	if err != nil && !endOfFile(err) {
		return 0, err
	}
	if n < 10 || string(h[0:3]) != "ID3" {
		return 0, nil
	}
	length := int64(unsyncsafe(h[6:10]))
	footer := h[5]&0x10 != 0
	if footer {
		return length + 20, nil
	}
	start := length + 10
	// The padding after the tag, which MACLib reads past a byte at a time;
	// bounded here by the search limit, a window at a time.
	buf := make([]byte, 4096)
	for limit := start + apeSearchLimit; start < limit; {
		n, err := r.ReadAt(buf, start)
		if err != nil && !endOfFile(err) {
			return 0, err
		}
		for i := 0; i < n; i++ {
			if buf[i] != 0 {
				return start + int64(i), nil
			}
		}
		if n < len(buf) {
			return start + int64(n), nil
		}
		start += int64(n)
	}
	return start, nil
}

// findAPEDescriptor finds "MAC " at or after start, at most apeSearchLimit
// bytes past it, a window at a time.
func findAPEDescriptor(r io.ReaderAt, start int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for pos := start; pos <= start+apeSearchLimit; {
		n, err := r.ReadAt(buf, pos)
		if err != nil && !endOfFile(err) {
			return 0, err
		}
		for i := 0; i+4 <= n; i++ {
			if pos+int64(i) > start+apeSearchLimit {
				return 0, errAPENoDescriptor
			}
			if buf[i] == 'M' && string(buf[i:i+4]) == "MAC " {
				return pos + int64(i), nil
			}
		}
		if n < len(buf) {
			return 0, errAPENoDescriptor
		}
		pos += int64(n - 3)
	}
	return 0, errAPENoDescriptor
}

// readAtExactly reads len(b) bytes at off. A file that ends before them is
// errTruncatedHeader; any other failure is the reader's.
func readAtExactly(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err != nil && !endOfFile(err) {
		return err
	}
	return errTruncatedHeader
}

// errTruncatedHeader is a file that ends inside a header this reader needs.
var errTruncatedHeader = errors.New("the file ends inside its header")
